package forgejo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// PostgreSQL images and versions of the operator-managed CloudNativePG clusters. Major
// upgrades are orchestrated by internal/upgrade (docs/upgrades.md, "PostgreSQL major upgrades").

// Labels and values of the objects CloudNativePG creates for a Cluster.
const (
	// CNPGClusterLabel names the Cluster on its pods, PVCs, Jobs and Secrets.
	CNPGClusterLabel = "cnpg.io/cluster"
	// CNPGJobRoleLabel is the role of a CNPG Job; CNPGMajorUpgradeJobRole runs pg_upgrade.
	CNPGJobRoleLabel        = "cnpg.io/jobRole"
	CNPGMajorUpgradeJobRole = "major-upgrade"
	// CNPGMajorUpgradeContainer is the container of that Job that runs pg_upgrade (with the new
	// image); its init container "prepare" copies the old binaries.
	CNPGMajorUpgradeContainer = "major-upgrade"
	// CNPGHealthyPhase is the phase of a Cluster whose instances all run.
	CNPGHealthyPhase = "Cluster in healthy state"
	// CNPGMajorUpgradePhase is the phase while CNPG upgrades the major version in place.
	CNPGMajorUpgradePhase = "Upgrading Postgres major version"
)

// PostgresPolicy is the operator's PostgreSQL version policy for CNPG clusters.
type PostgresPolicy struct {
	// Min is the oldest major Forgejo supports (its database preparation guide asks for 14).
	Min int
	// Max is the newest major that the CloudNativePG release the operator supports (1.30)
	// supports. Newer ones, release candidates included, are refused.
	Max int
	// Verified majors run Forgejo 15 and 17 in the operator's tests (fresh installs, upgrades
	// of Forgejo and of PostgreSQL, backups and restores). Others in [Min, Max] are allowed with
	// an UnsupportedVersion warning.
	Verified []int
}

// Postgres is the operator's PostgreSQL version policy.
var Postgres = PostgresPolicy{Min: 14, Max: 18, Verified: []int{17, 18}}

// InRange reports whether major may run Forgejo with the supported CloudNativePG release.
func (p PostgresPolicy) InRange(major int) bool { return major >= p.Min && major <= p.Max }

// IsVerified reports whether major is one of the verified majors.
func (p PostgresPolicy) IsVerified(major int) bool { return slices.Contains(p.Verified, major) }

// VerifiedList names the verified majors for messages ("17 and 18").
func (p PostgresPolicy) VerifiedList() string {
	parts := make([]string, 0, len(p.Verified))
	for _, m := range p.Verified {
		parts = append(parts, strconv.Itoa(m))
	}
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// Debian releases of the CloudNativePG images.
const (
	DebianTrixie   = "trixie"
	DebianBookworm = "bookworm"
	DebianBullseye = "bullseye"
)

// PostgresImageRef is what the operator reads from the tag of a PostgreSQL image.
type PostgresImageRef struct {
	// Major and Minor are the PostgreSQL version; Minor is -1 when the tag names none.
	Major, Minor int
	// OS is the Debian release of the image: the tag's last part (trixie, bookworm, bullseye),
	// bullseye for the deprecated tags without one (MM, MM.mm and MM.mm-N, the legacy system
	// images), and "" when the tag ends in anything else.
	OS string
	// OK is false when the tag does not start with a version (a digest alone, latest), so the
	// major is unknown. CloudNativePG reads the major from the tag the same way.
	OK bool
}

// ParsePostgresImage reads the version and the Debian release from the tag of a
// CloudNativePG PostgreSQL image (MM-TYPE-OS, MM.mm-TYPE-OS, MM.mm-TS-TYPE-OS, the
// deprecated MM and MM.mm, release candidates such as 19rc1-minimal-trixie). A digest after
// the tag is ignored.
func ParsePostgresImage(image string) PostgresImageRef {
	tag := ImageTag(image)
	ref := PostgresImageRef{Minor: -1}
	n := leadingDigits(tag)
	if n == 0 {
		return ref
	}
	major, err := strconv.Atoi(tag[:n])
	if err != nil {
		return ref
	}
	ref.Major, ref.OK = major, true
	rest := tag[n:]
	if strings.HasPrefix(rest, ".") {
		if m := leadingDigits(rest[1:]); m > 0 {
			ref.Minor, _ = strconv.Atoi(rest[1 : 1+m])
			rest = rest[1+m:]
		}
	}
	i := strings.LastIndex(rest, "-")
	switch last := rest[i+1:]; {
	case i < 0, leadingDigits(last) == len(last):
		// MM, MM.mm, MM.mm-N: the deprecated tags of the legacy system images.
		ref.OS = DebianBullseye
	case last == DebianTrixie, last == DebianBookworm, last == DebianBullseye:
		ref.OS = last
	}
	return ref
}

func leadingDigits(s string) int {
	n := 0
	for n < len(s) && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	return n
}

// CNPGCluster is what the operator reads of a CloudNativePG Cluster.
type CNPGCluster struct {
	Name     string
	UID      types.UID
	Created  metav1.Time
	Deleting bool
	// SpecImage is spec.imageName, SpecInstances spec.instances, StorageSize spec.storage.size.
	SpecImage     string
	SpecInstances int64
	StorageSize   string
	// Phase and PhaseReason are status.phase and status.phaseReason.
	Phase, PhaseReason string
	// Image is status.image: the requested image, also while a major upgrade runs.
	Image string
	// DataImage and DataMajor are status.pgDataImageInfo: the image and major version of the
	// data, the old ones until pg_upgrade has succeeded. DataMajor is 0 while CNPG has not
	// initialised the cluster.
	DataImage string
	DataMajor int
	// TargetMajor is status.targetPgDataImageInfo.majorVersion while a major upgrade runs.
	TargetMajor int
	// SystemID is status.systemID.
	SystemID                      string
	ReadyInstances                int64
	CurrentPrimary, TargetPrimary string
}

// ReadCNPGCluster reads the fields the operator uses from a Cluster.
func ReadCNPGCluster(u *unstructured.Unstructured) *CNPGCluster {
	str := func(path ...string) string {
		s, _, _ := unstructured.NestedString(u.Object, path...)
		return s
	}
	num := func(path ...string) int64 {
		if n, found, err := unstructured.NestedInt64(u.Object, path...); found && err == nil {
			return n
		}
		if f, found, err := unstructured.NestedFloat64(u.Object, path...); found && err == nil {
			return int64(f)
		}
		return 0
	}
	c := &CNPGCluster{
		Name:           u.GetName(),
		UID:            u.GetUID(),
		Created:        u.GetCreationTimestamp(),
		Deleting:       u.GetDeletionTimestamp() != nil,
		SpecImage:      str("spec", "imageName"),
		SpecInstances:  num("spec", "instances"),
		StorageSize:    str("spec", "storage", "size"),
		Phase:          str("status", "phase"),
		PhaseReason:    str("status", "phaseReason"),
		Image:          str("status", "image"),
		DataImage:      str("status", "pgDataImageInfo", "image"),
		DataMajor:      int(num("status", "pgDataImageInfo", "majorVersion")),
		TargetMajor:    int(num("status", "targetPgDataImageInfo", "majorVersion")),
		SystemID:       str("status", "systemID"),
		ReadyInstances: num("status", "readyInstances"),
		CurrentPrimary: str("status", "currentPrimary"),
		TargetPrimary:  str("status", "targetPrimary"),
	}
	if c.SpecInstances == 0 {
		c.SpecInstances = 1
	}
	return c
}

// Healthy reports whether CNPG reports the cluster healthy with every instance ready.
func (c *CNPGCluster) Healthy() bool {
	return c != nil && !c.Deleting && c.Phase == CNPGHealthyPhase && c.ReadyInstances >= c.SpecInstances
}

// cnpgPrimaryBlockedPhases are CNPG phases during which the primary does not accept
// connections (or is about to change).
var cnpgPrimaryBlockedPhases = map[string]bool{
	"Setting up primary":                                       true,
	"Failing over":                                             true,
	"Switchover in progress":                                   true,
	"Primary instance is being restarted in-place":             true,
	"Primary instance is being restarted without a switchover": true,
	CNPGMajorUpgradePhase:                                      true,
}

// CNPGPrimaryUnavailable explains why the primary of a CNPG cluster (status.phase,
// currentPrimary, targetPrimary and readyInstances) cannot serve, or returns "" when it can:
// a primary that is not changing, in a phase that lets it accept connections, whose pod
// (read with r) is ready. Replicas do not count.
func CNPGPrimaryUnavailable(ctx context.Context, r client.Reader, namespace, phase, current, target string, ready int64) (string, error) {
	switch {
	case current == "":
		return "no primary yet", nil
	case target != current:
		return fmt.Sprintf("primary changing from %s to %s", current, target), nil
	case cnpgPrimaryBlockedPhases[phase]:
		return "primary unavailable", nil
	case ready < 1:
		return "no instance ready", nil
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: current}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return "primary pod " + current + " not found", nil
		}
		return "", fmt.Errorf("primary pod %s: %w", current, err)
	}
	if !pod.DeletionTimestamp.IsZero() || !PodReady(pod) {
		return "primary pod " + current + " is not ready", nil
	}
	return "", nil
}

// PodReady reports whether the pod's Ready condition is True.
func PodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// RunningImage is the image of the cluster's data, or spec.imageName before CNPG reports one.
func (c *CNPGCluster) RunningImage() string {
	if c.DataImage != "" {
		return c.DataImage
	}
	return c.SpecImage
}

// RunningPostgresImage is the PostgreSQL image backup and restore Jobs run pg_dump and
// pg_restore from: for a CNPG cluster the image of its data (status.database.image, the old
// image until a major upgrade has succeeded), else, and before the database step has reported
// it, spec.database.cnpg.imageName.
func RunningPostgresImage(f *forgejov1.Forgejo) string {
	if DatabaseType(f) == forgejov1.DatabaseTypeCNPG {
		if st := f.Status.Database; st != nil && st.Image != "" {
			return st.Image
		}
	}
	return PostgresImage(f)
}

// DatabaseUpgrade returns status.database.upgrade, or nil.
func DatabaseUpgrade(f *forgejov1.Forgejo) *forgejov1.DatabaseUpgradeStatus {
	if f.Status.Database == nil {
		return nil
	}
	return f.Status.Database.Upgrade
}

// DatabaseUpgradeInFlightPhase reports whether p is a phase of a PostgreSQL major upgrade
// that has not finished.
func DatabaseUpgradeInFlightPhase(p forgejov1.DatabaseUpgradePhase) bool {
	switch p {
	case forgejov1.DatabaseUpgradePending, forgejov1.DatabaseUpgradeBackingUp, forgejov1.DatabaseUpgradeUpgrading,
		forgejov1.DatabaseUpgradeVerifying, forgejov1.DatabaseUpgradeReverting, forgejov1.DatabaseUpgradeRollingBack:
		return true
	}
	return false
}

// DatabaseUpgradeInFlight reports whether status.database.upgrade records a PostgreSQL major
// upgrade that has not finished. Backups, restores, runner registrations and Forgejo
// upgrades wait for it (except the upgrade's own backup and restore).
func DatabaseUpgradeInFlight(f *forgejov1.Forgejo) bool {
	u := DatabaseUpgrade(f)
	return u != nil && DatabaseUpgradeInFlightPhase(u.Phase)
}

// DatabaseUpgradeHoldsWorkload reports a PostgreSQL major upgrade whose rollback restore
// failed: the recreated database may be empty or half-restored, so Forgejo stays stopped until
// a later ForgejoRestore has restored a backup.
func DatabaseUpgradeHoldsWorkload(f *forgejov1.Forgejo) bool {
	u := DatabaseUpgrade(f)
	return u != nil && u.Phase == forgejov1.DatabaseUpgradeFailed && u.RollbackRestoreFailed
}
