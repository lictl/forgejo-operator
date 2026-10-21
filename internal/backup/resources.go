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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
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

// BackupJobName, CleanupJobName and RestoreJobName are the Jobs run for each object.
func BackupJobName(b *forgejov1.ForgejoBackup) string   { return childName(b.Name, "-job") }
func CleanupJobName(b *forgejov1.ForgejoBackup) string  { return childName(b.Name, "-cleanup") }
func RestoreJobName(r *forgejov1.ForgejoRestore) string { return childName(r.Name, "-job") }

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

// NeedsMaintenance reports whether a restore requires the instance to be scaled to zero.
// ScalingUp is excluded: the restore is done and waits for Forgejo to come back.
func NeedsMaintenance(r *forgejov1.ForgejoRestore) bool {
	return r.Status.Phase == forgejov1.RestorePhaseScalingDown || r.Status.Phase == forgejov1.RestorePhaseRestoring
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
// returns the claim name. The size is only applied at creation; growing it is left to the
// storage auto-expansion.
func EnsureBackupPVC(ctx context.Context, c client.Client, scheme *runtime.Scheme, f *forgejov1.Forgejo) (string, error) {
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
				corev1.ResourceStorage: forgejo.QuantityOr(f.Spec.Backup.Storage.Size, "20Gi"),
			}
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
