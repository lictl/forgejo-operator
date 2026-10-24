package forgejo

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

func TestParsePostgresImage(t *testing.T) {
	const repo = "ghcr.io/cloudnative-pg/postgresql:"
	for _, tt := range []struct {
		image        string
		major, minor int
		os           string
		ok           bool
	}{
		{repo + "18-minimal-trixie", 18, -1, DebianTrixie, true},
		{repo + "18.6-minimal-trixie", 18, 6, DebianTrixie, true},
		{repo + "18.6-202510091234-standard-bookworm", 18, 6, DebianBookworm, true},
		{repo + "17-system-bullseye", 17, -1, DebianBullseye, true},
		{repo + "18-minimal-trixie@sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", 18, -1, DebianTrixie, true},
		{repo + "19rc1-minimal-trixie", 19, -1, DebianTrixie, true},
		// The deprecated rolling and immutable tags of the legacy system images are bullseye.
		{repo + "17", 17, -1, DebianBullseye, true},
		{repo + "17.6", 17, 6, DebianBullseye, true},
		{repo + "17.6-3", 17, 6, DebianBullseye, true},
		{"localhost:5000/postgresql:16-custom", 16, -1, "", true},
		{"postgres:18", 18, -1, DebianBullseye, true},
		{repo + "latest", 0, -1, "", false},
		{"ghcr.io/cloudnative-pg/postgresql@sha256:0123", 0, -1, "", false},
		{"localhost:5000/postgresql", 0, -1, "", false},
	} {
		got := ParsePostgresImage(tt.image)
		if got.OK != tt.ok || (tt.ok && (got.Major != tt.major || got.Minor != tt.minor || got.OS != tt.os)) {
			t.Errorf("%s: %+v, want major %d minor %d os %q ok %v", tt.image, got, tt.major, tt.minor, tt.os, tt.ok)
		}
	}
}

// The default image is a verified major on Debian stable, and the policy covers it.
func TestPostgresPolicy(t *testing.T) {
	ref := ParsePostgresImage(forgejov1.DefaultPostgresImage)
	if !ref.OK || !Postgres.IsVerified(ref.Major) || ref.OS != DebianTrixie {
		t.Fatalf("default image %s: %+v", forgejov1.DefaultPostgresImage, ref)
	}
	for major, in := range map[int]bool{13: false, 14: true, 16: true, 17: true, 18: true, 19: false} {
		if Postgres.InRange(major) != in {
			t.Errorf("InRange(%d) = %v", major, !in)
		}
	}
	if Postgres.IsVerified(16) || Postgres.VerifiedList() != "17 and 18" {
		t.Errorf("verified %v (%s)", Postgres.Verified, Postgres.VerifiedList())
	}
	// CloudNativePG 1.30 upgrades in place only to 16 or newer, so 14 → 15 is the one upgrade
	// within the range it refuses; every verified major is a target it accepts.
	if Postgres.MinUpgradeTarget != 16 || !Postgres.InRange(Postgres.MinUpgradeTarget) || Postgres.Verified[0] < Postgres.MinUpgradeTarget {
		t.Errorf("MinUpgradeTarget %d", Postgres.MinUpgradeTarget)
	}
}

func TestReadCNPGClusterAndRunningImage(t *testing.T) {
	const pg17, pg18, image = "pg:17-minimal-trixie", "pg:18-minimal-trixie", "image"
	u := &unstructured.Unstructured{Object: map[string]any{
		"spec": map[string]any{"imageName": pg18, "instances": int64(2), "storage": map[string]any{"size": "8Gi"}},
		"status": map[string]any{
			"phase": CNPGMajorUpgradePhase, image: pg18, "readyInstances": int64(0), "systemID": "7001",
			"pgDataImageInfo":       map[string]any{image: pg17, "majorVersion": int64(17)},
			"targetPgDataImageInfo": map[string]any{image: pg18, "majorVersion": int64(18)},
		},
	}}
	u.SetGroupVersionKind(CNPGClusterGVK)
	u.SetName("fj-db")
	c := ReadCNPGCluster(u)
	if c.SpecImage != pg18 || c.SpecInstances != 2 || c.StorageSize != "8Gi" || c.DataMajor != 17 ||
		c.TargetMajor != 18 || c.DataImage != pg17 || c.SystemID != "7001" || c.Healthy() {
		t.Fatalf("%+v", c)
	}
	if c.RunningImage() != pg17 {
		t.Errorf("running image %s, want the image of the data", c.RunningImage())
	}

	f := &forgejov1.Forgejo{}
	f.Spec.Database.CNPG.ImageName = pg18
	if got := RunningPostgresImage(f); got != pg18 {
		t.Errorf("before status.database: %s", got)
	}
	f.Status.Database = &forgejov1.DatabaseStatus{Image: pg17}
	if got := RunningPostgresImage(f); got != pg17 {
		t.Errorf("CNPG: %s, want status.database.image", got)
	}
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	if got := RunningPostgresImage(f); got != pg18 {
		t.Errorf("External: %s, want spec.database.cnpg.imageName", got)
	}

	f.Status.Database.Upgrade = &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeVerifying}
	if !DatabaseUpgradeInFlight(f) || DatabaseUpgradeHoldsWorkload(f) {
		t.Error("Verifying is in flight")
	}
	now := metav1.Now()
	f.Status.Database.Upgrade = &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeFailed, RollbackRestoreFailed: true, CompletionTime: &now}
	if DatabaseUpgradeInFlight(f) || !DatabaseUpgradeHoldsWorkload(f) {
		t.Error("a failed rollback restore holds the workload and is not in flight")
	}
}
