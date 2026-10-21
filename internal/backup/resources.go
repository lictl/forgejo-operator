package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Files written into "<backup-pvc>/<backup-name>/".
const (
	BackupMountPath = "/backups"
	DBDumpFile      = "db.dump"
	DataArchiveFile = "data.tar.gz"
	ManifestFile    = "manifest.json"
)

// DataExcludes are paths (relative to the data volume) left out of data archives: temp files,
// caches, logs, queues and search indexes, all of which Forgejo recreates or rebuilds.
var DataExcludes = []string{
	"lost+found",
	"gitea/tmp",
	"gitea/repo-archive",
	"gitea/log",
	"gitea/queues",
	"gitea/indexers",
}

// DataClaimName is the PVC holding the instance's data volume.
func DataClaimName(f *forgejov1.Forgejo) string {
	if f.Spec.Storage.ExistingClaim != "" {
		return f.Spec.Storage.ExistingClaim
	}
	return forgejo.DataPVCName(f)
}

// BackupClaimName is the PVC backups of the instance are written to.
func BackupClaimName(f *forgejov1.Forgejo) string {
	if f.Spec.Backup.Storage.ExistingClaim != "" {
		return f.Spec.Backup.Storage.ExistingClaim
	}
	return forgejo.BackupPVCName(f)
}

// CurrentImage is the Forgejo image currently deployed (falls back to the spec image).
func CurrentImage(f *forgejov1.Forgejo) string {
	if f.Status.Image != "" {
		return f.Status.Image
	}
	return forgejo.Image(f)
}

// IncludeData resolves spec.includeData against the instance default.
func IncludeData(b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) bool {
	if b.Spec.IncludeData != nil {
		return *b.Spec.IncludeData
	}
	return forgejo.BackupIncludeData(f)
}

// Reason returns the backup reason, defaulting to Manual.
func Reason(b *forgejov1.ForgejoBackup) forgejov1.BackupReason {
	if b.Spec.Reason == "" {
		return forgejov1.BackupReasonManual
	}
	return b.Spec.Reason
}

// BackupDir is the backup's directory as seen by jobs.
func BackupDir(backupName string) string { return BackupMountPath + "/" + backupName }

// BackupJobName, CleanupJobName and RestoreJobName are the Jobs run for each object. Every
// kind has its own suffix (kept on the hashed path too), so a ForgejoBackup and a
// ForgejoRestore of the same name never share a Job. The controllers additionally refuse
// Jobs they do not control (CreateJob, CheckOwned).
func BackupJobName(b *forgejov1.ForgejoBackup) string   { return childName(b.Name, "-backup") }
func CleanupJobName(b *forgejov1.ForgejoBackup) string  { return childName(b.Name, "-cleanup") }
func RestoreJobName(r *forgejov1.ForgejoRestore) string { return childName(r.Name, "-restore") }

// childName appends suffix to base, shortening base with a hash so the result is a valid
// 63-character DNS label (Job names end up in pod labels).
func childName(base, suffix string) string {
	const maxLen = 63
	if len(base)+len(suffix) <= maxLen {
		return base + suffix
	}
	sum := sha256.Sum256([]byte(base))
	keep := maxLen - len(suffix) - 9
	return strings.TrimRight(base[:keep], "-.") + "-" + hex.EncodeToString(sum[:4]) + suffix
}

// IsBackupActive reports whether a backup has not reached a terminal phase.
func IsBackupActive(b *forgejov1.ForgejoBackup) bool {
	return b.Status.Phase != forgejov1.BackupPhaseCompleted && b.Status.Phase != forgejov1.BackupPhaseFailed
}

// IsRestoreActive reports whether a restore has not reached a terminal phase.
func IsRestoreActive(r *forgejov1.ForgejoRestore) bool {
	return r.Status.Phase != forgejov1.RestorePhaseCompleted && r.Status.Phase != forgejov1.RestorePhaseFailed
}

// NeedsMaintenance reports whether a running restore requires the instance to be scaled to
// zero. ScalingUp is excluded: the restore is done and waits for Forgejo to come back.
func NeedsMaintenance(r *forgejov1.ForgejoRestore) bool {
	return r.Status.Phase == forgejov1.RestorePhaseScalingDown || r.Status.Phase == forgejov1.RestorePhaseRestoring
}

const (
	// ConditionDataModified on a ForgejoRestore records whether its Job may already have
	// changed the instance: True once the database transaction committed or the data step
	// started, Unknown while a pod is past verification and the outcome is not known, False
	// when the Job failed without changing anything.
	ConditionDataModified = "DataModified"
	// AnnotationReleaseMaintenance="true" on a failed ForgejoRestore that holds the instance
	// in maintenance lets Forgejo start again on whatever the restore left behind.
	AnnotationReleaseMaintenance = "forgejo.lucas.icu/release-maintenance"
)

// Modification is how far a restore Job may have changed the instance.
type Modification int

const (
	Unmodified Modification = iota
	// MaybeModified: a pod got past verification, so the database or the data volume may be
	// changing, and it is not known whether anything was committed.
	MaybeModified
	// Modified: the database restore committed or the data step started.
	Modified
)

// RecordedModification reads the DataModified condition of a restore.
func RecordedModification(r *forgejov1.ForgejoRestore) Modification {
	c := meta.FindStatusCondition(r.Status.Conditions, ConditionDataModified)
	switch {
	case c == nil:
		return Unmodified
	case c.Status == metav1.ConditionTrue:
		return Modified
	case c.Status == metav1.ConditionUnknown:
		return MaybeModified
	}
	return Unmodified
}

// ModificationCondition is the DataModified condition for m.
func ModificationCondition(m Modification, generation int64, msg string) metav1.Condition {
	c := metav1.Condition{Type: ConditionDataModified, ObservedGeneration: generation, Message: msg}
	switch m {
	case Modified:
		c.Status, c.Reason = metav1.ConditionTrue, "Modified"
	case MaybeModified:
		c.Status, c.Reason = metav1.ConditionUnknown, "MaybeModified"
	default:
		c.Status, c.Reason = metav1.ConditionFalse, "Unchanged"
	}
	if c.Message == "" {
		c.Message = c.Reason
	}
	return c
}

// HoldsMaintenance reports whether a failed restore had (or may have) already changed the
// database or the data volume, so the instance must stay scaled to zero until someone
// releases it (annotation, deleting the restore, or a later restore that succeeds).
func HoldsMaintenance(r *forgejov1.ForgejoRestore) bool {
	return r.Status.Phase == forgejov1.RestorePhaseFailed &&
		RecordedModification(r) != Unmodified &&
		r.DeletionTimestamp.IsZero() &&
		r.Annotations[AnnotationReleaseMaintenance] != "true"
}

// BlocksBackups reports whether a restore must finish before a backup of the instance starts.
func BlocksBackups(r *forgejov1.ForgejoRestore) bool {
	return IsRestoreActive(r) && r.Status.Phase != forgejov1.RestorePhaseScalingUp
}

// ListBackups returns the instance's backups, oldest first.
func ListBackups(ctx context.Context, c client.Client, namespace, instance string) ([]forgejov1.ForgejoBackup, error) {
	list := &forgejov1.ForgejoBackupList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]forgejov1.ForgejoBackup, 0, len(list.Items))
	for _, b := range list.Items {
		if b.Spec.ForgejoRef.Name == instance {
			out = append(out, b)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return olderThan(&out[i].ObjectMeta, &out[j].ObjectMeta) })
	return out, nil
}

// ListRestores returns the instance's restores, oldest first.
func ListRestores(ctx context.Context, c client.Client, namespace, instance string) ([]forgejov1.ForgejoRestore, error) {
	list := &forgejov1.ForgejoRestoreList{}
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]forgejov1.ForgejoRestore, 0, len(list.Items))
	for _, r := range list.Items {
		if r.Spec.ForgejoRef.Name == instance {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return olderThan(&out[i].ObjectMeta, &out[j].ObjectMeta) })
	return out, nil
}

func olderThan(a, b *metav1.ObjectMeta) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// DatabaseReady reports whether the instance's database can be dumped or restored. Before
// the database step has reported anything, the credentials secret is the only signal.
func DatabaseReady(f *forgejov1.Forgejo) bool {
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseReady)
	return c == nil || c.Status == metav1.ConditionTrue
}

// EnsureBackupPVC creates "<name>-backups" (or checks spec.backup.storage.existingClaim) and
// returns the claim name. A larger spec.backup.storage.size grows an existing operator-managed
// claim when its StorageClass allows expansion (never shrinks it); warn reports why it cannot.
func EnsureBackupPVC(ctx context.Context, c client.Client, scheme *runtime.Scheme, f *forgejov1.Forgejo, warn Warner) (string, error) {
	name := BackupClaimName(f)
	if f.Spec.Backup.Storage.ExistingClaim != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, pvc); err != nil {
			return "", fmt.Errorf("backup claim %q: %w", name, err)
		}
		return name, nil
	}

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, pvc, func() error {
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		maps.Copy(pvc.Labels, forgejo.Labels(f, forgejo.ComponentBackup))
		if pvc.CreationTimestamp.IsZero() {
			pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			pvc.Spec.StorageClassName = f.Spec.Backup.Storage.StorageClassName
			pvc.Spec.Resources.Requests = corev1.ResourceList{
				corev1.ResourceStorage: BackupClaimSize(f),
			}
		} else if want, grow := needsGrowth(pvc, BackupClaimSize(f)); grow && expansionAllowed(ctx, c, pvc, warn) {
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		}
		if forgejo.RetainData(f) {
			if metav1.IsControlledBy(pvc, f) {
				return controllerutil.RemoveControllerReference(f, pvc, scheme)
			}
			return nil
		}
		return controllerutil.SetControllerReference(f, pvc, scheme)
	})
	if err != nil {
		return "", fmt.Errorf("backup claim %q: %w", name, err)
	}
	return name, nil
}

// Warner reports a problem the caller should surface as a Warning event; it may be nil.
type Warner func(reason, msg string)

// BackupClaimSize is the requested size of the operator-managed backup PVC.
func BackupClaimSize(f *forgejov1.Forgejo) resource.Quantity {
	return forgejo.QuantityOr(f.Spec.Backup.Storage.Size, "20Gi")
}

// GrowBackupPVC raises the request of an existing operator-managed backup PVC to
// spec.backup.storage.size, so a resize does not wait for the next backup. It does nothing
// when the claim does not exist yet (it is created on first use), for an existingClaim, when
// the claim is already as large (autopilot or a person may have grown it further), or when
// its StorageClass does not allow expansion (warn says so). It reports whether it resized.
func GrowBackupPVC(ctx context.Context, c client.Client, f *forgejov1.Forgejo, warn Warner) (bool, error) {
	if f.Spec.Backup.Storage.ExistingClaim != "" {
		return false, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.BackupPVCName(f)}, pvc); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	want, grow := needsGrowth(pvc, BackupClaimSize(f))
	if !grow || !pvc.DeletionTimestamp.IsZero() || !expansionAllowed(ctx, c, pvc, warn) {
		return false, nil
	}
	orig := pvc.DeepCopy()
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
	if err := c.Patch(ctx, pvc, client.MergeFrom(orig)); err != nil {
		return false, fmt.Errorf("growing backup claim %s to %s: %w", pvc.Name, want.String(), err)
	}
	return true, nil
}

// needsGrowth reports whether the claim requests less than want. It never asks to shrink.
func needsGrowth(pvc *corev1.PersistentVolumeClaim, want resource.Quantity) (resource.Quantity, bool) {
	if pvc.Spec.Resources.Requests == nil {
		pvc.Spec.Resources.Requests = corev1.ResourceList{}
	}
	cur, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return want, ok && want.Cmp(cur) > 0
}

// expansionAllowed reports whether the claim's StorageClass supports resizing.
func expansionAllowed(ctx context.Context, c client.Client, pvc *corev1.PersistentVolumeClaim, warn Warner) bool {
	if warn == nil {
		warn = func(string, string) {}
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		warn("StorageResizeUnsupported", fmt.Sprintf("cannot grow backup PVC %s: it has no StorageClass", pvc.Name))
		return false
	}
	sc := &storagev1.StorageClass{}
	if err := c.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		warn("StorageResizeUnsupported", fmt.Sprintf("cannot grow backup PVC %s: StorageClass %s: %v", pvc.Name, *pvc.Spec.StorageClassName, err))
		return false
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		warn("StorageResizeUnsupported", fmt.Sprintf("cannot grow backup PVC %s: StorageClass %s does not allow volume expansion", pvc.Name, sc.Name))
		return false
	}
	return true
}

// Placement returns the affinity that lets a job mount the data volume: next to running
// Forgejo pods (an RWO volume is attached to their node), otherwise on a node the data
// volume's PV allows. nil means no constraint.
func Placement(ctx context.Context, c client.Client, f *forgejov1.Forgejo) (*corev1.Affinity, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(forgejo.SelectorLabels(f))); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" || p.DeletionTimestamp != nil ||
			p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		return &corev1.Affinity{PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: forgejo.SelectorLabels(f)},
				TopologyKey:   corev1.LabelHostname,
			}},
		}}, nil
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: DataClaimName(f)}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if pvc.Spec.VolumeName == "" {
		return nil, nil
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return nil, nil
	}
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: pv.Spec.NodeAffinity.Required.DeepCopy(),
	}}, nil
}

// ServerPodsRunning counts Forgejo server pods that still exist (including terminating ones).
func ServerPodsRunning(ctx context.Context, c client.Client, f *forgejov1.Forgejo) (int, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels(forgejo.SelectorLabels(f))); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			n++
		}
	}
	return n, nil
}
