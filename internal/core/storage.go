package core

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileStorage ensures the data PVC.
func ReconcileStorage(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if f.Spec.Storage.ExistingClaim != "" {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Spec.Storage.ExistingClaim}, pvc); err != nil {
			return forgejo.StepResult{}, fmt.Errorf("storage.existingClaim %q: %w", f.Spec.Storage.ExistingClaim, err)
		}
		return forgejo.StepResult{}, nil
	}

	want := forgejo.QuantityOr(f.Spec.Storage.Size, "10Gi")
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace}}
	err := apply(ctx, rc, pvc, rc.OwnerRefsEnabled(), func() error {
		pvc.Labels = mergeMaps(pvc.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		if pvc.ResourceVersion == "" {
			pvc.Spec.AccessModes = f.Spec.Storage.AccessModes
			if len(pvc.Spec.AccessModes) == 0 {
				pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
			}
			pvc.Spec.StorageClassName = f.Spec.Storage.StorageClassName
			pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: want}
			return nil
		}
		// Only grow: autopilot may already have expanded the claim beyond the spec.
		cur := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
		if want.Cmp(cur) > 0 && expansionAllowed(ctx, rc, pvc, want) {
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = want
		}
		return nil
	})
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("data PVC: %w", err)
	}
	return forgejo.StepResult{}, nil
}

// expansionAllowed reports whether the claim's StorageClass supports resizing, and
// records a warning event when it does not: once per requested size, repeated hourly while
// the spec asks for more than the claim can get.
func expansionAllowed(ctx context.Context, rc *forgejo.ReconcileContext, pvc *corev1.PersistentVolumeClaim, want resource.Quantity) bool {
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return false
	}
	sc := &storagev1.StorageClass{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Name: *pvc.Spec.StorageClassName}, sc); err != nil {
		return false
	}
	if sc.AllowVolumeExpansion == nil || !*sc.AllowVolumeExpansion {
		warnEvery(rc, "resize-unsupported/"+pvc.Name+"/"+want.String(), warnInterval, "StorageResizeUnsupported", "ResizeVolume",
			"cannot grow PVC %s to %s: StorageClass %s does not allow volume expansion", pvc.Name, want.String(), sc.Name)
		return false
	}
	return true
}
