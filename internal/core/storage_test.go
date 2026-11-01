package core

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// The resize warning for a non-expandable class is not repeated every reconcile.
func TestStorageResizeWarningRateLimited(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Storage.Size = resource.MustParse("20Gi")
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fixed"}, Provisioner: "x", AllowVolumeExpansion: ptr.To(false)}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("fixed"),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
	}
	rc := testContext(t, f, sc, pvc)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	for range 3 {
		if _, err := ReconcileStorage(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	if got := drainEvents(rec); len(got) != 1 || !strings.Contains(got[0], "StorageResizeUnsupported") {
		t.Fatalf("want a single StorageResizeUnsupported event, got %v", got)
	}
	// A different requested size is a new problem, reported at once.
	f.Spec.Storage.Size = resource.MustParse("30Gi")
	if _, err := ReconcileStorage(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if got := drainEvents(rec); len(got) != 1 {
		t.Fatalf("new size: want one event, got %v", got)
	}
}
