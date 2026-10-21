package backup

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Ownership of backups follows spec.storage.retainPolicy, which may change at any time:
//   - Delete: scheduled and pre-upgrade ForgejoBackups and the operator-managed backup PVC are
//     owned by the instance and garbage-collected with it.
//   - Retain: no ForgejoBackup and no backup PVC carries a reference to the instance, so
//     deleting the instance keeps every backup.
//
// Manual backups are never given a reference; under Retain one to the instance is removed.

// ReconcileOwnership is a pipeline step that brings the owner references of the instance's
// ForgejoBackups and of its backup PVC in line with the current retainPolicy. It needs
// neither the database nor the workload, so it belongs early in the pipeline.
func ReconcileOwnership(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !f.DeletionTimestamp.IsZero() {
		return forgejo.StepResult{}, nil
	}
	backups, err := ListBackups(ctx, rc.Client, f.Namespace, f.Name)
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("listing backups: %w", err)
	}
	retry := false
	for i := range backups {
		if _, err := SyncBackupOwnership(ctx, rc.Client, rc.Scheme, f, &backups[i]); err != nil {
			if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
				retry = true
				continue
			}
			return forgejo.StepResult{}, fmt.Errorf("owner reference of backup %s: %w", backups[i].Name, err)
		}
	}
	if _, err := SyncBackupPVCOwnership(ctx, rc.Client, rc.Scheme, f); err != nil {
		if !apierrors.IsConflict(err) {
			return forgejo.StepResult{}, err
		}
		retry = true
	}
	if retry {
		rc.Requeue(5 * time.Second)
	}
	return forgejo.StepResult{}, nil
}

// SyncBackupOwnership adds or removes the instance's owner reference on b according to the
// retain policy (see above) and reports whether it patched b. b must belong to f.
func SyncBackupOwnership(ctx context.Context, c client.Client, scheme *runtime.Scheme, f *forgejov1.Forgejo, b *forgejov1.ForgejoBackup) (bool, error) {
	if !b.DeletionTimestamp.IsZero() || !f.DeletionTimestamp.IsZero() || b.Spec.ForgejoRef.Name != f.Name || b.Namespace != f.Namespace {
		return false, nil
	}
	orig := b.DeepCopy()
	switch r := Reason(b); {
	case forgejo.RetainData(f):
		b.OwnerReferences = withoutInstanceRefs(b.OwnerReferences, f)
	case r == forgejov1.BackupReasonScheduled || r == forgejov1.BackupReasonPreUpgrade:
		if !hasRefTo(b.OwnerReferences, f.UID) {
			if err := controllerutil.SetOwnerReference(f, b, scheme); err != nil {
				return false, err
			}
		}
	}
	if equality.Semantic.DeepEqual(orig.OwnerReferences, b.OwnerReferences) {
		return false, nil
	}
	if err := c.Patch(ctx, b, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		b.OwnerReferences = orig.OwnerReferences
		return false, err
	}
	return true, nil
}

// SyncBackupPVCOwnership applies the retain policy to the operator-managed backup PVC (not to
// spec.backup.storage.existingClaim) and reports whether it patched the claim. Without it the
// claim would keep the ownership it had when the last backup started.
func SyncBackupPVCOwnership(ctx context.Context, c client.Client, scheme *runtime.Scheme, f *forgejov1.Forgejo) (bool, error) {
	if f.Spec.Backup.Storage.ExistingClaim != "" || !f.DeletionTimestamp.IsZero() {
		return false, nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.BackupPVCName(f)}, pvc); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return false, nil
	}
	orig := pvc.DeepCopy()
	if err := setBackupPVCOwnership(pvc, f, scheme); err != nil {
		return false, err
	}
	if equality.Semantic.DeepEqual(orig.OwnerReferences, pvc.OwnerReferences) {
		return false, nil
	}
	if err := c.Patch(ctx, pvc, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("owner reference of backup claim %s: %w", pvc.Name, err)
	}
	return true, nil
}

// withoutInstanceRefs drops every reference to f: by UID, and to an earlier Forgejo of the
// same name (garbage collection would delete the object once that owner is gone).
func withoutInstanceRefs(refs []metav1.OwnerReference, f *forgejov1.Forgejo) []metav1.OwnerReference {
	var out []metav1.OwnerReference
	for _, r := range refs {
		if r.UID == f.UID || IsForgejoRef(r) && r.Name == f.Name {
			continue
		}
		out = append(out, r)
	}
	return out
}

func hasRefTo(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}

// IsForgejoRef reports whether an owner reference points to a Forgejo.
func IsForgejoRef(r metav1.OwnerReference) bool {
	gv, err := schema.ParseGroupVersion(r.APIVersion)
	return err == nil && gv.Group == forgejov1.GroupVersion.Group && r.Kind == "Forgejo"
}

// ForgejoOwner returns the owner reference of obj that points to a Forgejo, or nil.
func ForgejoOwner(obj metav1.Object) *metav1.OwnerReference {
	refs := obj.GetOwnerReferences()
	for i := range refs {
		if IsForgejoRef(refs[i]) {
			return &refs[i]
		}
	}
	return nil
}

// ControlledByUID reports whether obj's controller reference has the given UID.
func ControlledByUID(obj metav1.Object, uid types.UID) bool {
	ref := metav1.GetControllerOf(obj)
	return ref != nil && ref.UID == uid
}
