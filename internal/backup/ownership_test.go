package backup

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Switching retainPolicy must change the ownership of existing backups and of the backup PVC,
// or deleting the instance garbage-collects backups the user meant to keep (and vice versa).
func TestReconcileOwnershipFollowsRetainPolicy(t *testing.T) {
	s := FakeScheme(t)
	ctx := context.Background()
	reasons := map[string]forgejov1.BackupReason{
		"sched":       forgejov1.BackupReasonScheduled,
		"pre":         forgejov1.BackupReasonPreUpgrade,
		"pre-restore": forgejov1.BackupReasonPreRestore,
		"manual":      forgejov1.BackupReasonManual,
	}

	// Created under Delete: scheduled and pre-upgrade backups and the PVC are owned.
	deleteF := ownedInstance(forgejov1.RetainPolicyDelete)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: "ns"}}
	if err := controllerutil.SetControllerReference(deleteF, pvc, s); err != nil {
		t.Fatal(err)
	}
	objs := append(make([]client.Object, 0, 1+len(reasons)), pvc)
	for name, reason := range reasons {
		b := reasonBackup(name, reason)
		if err := controllerutil.SetOwnerReference(deleteF, b, s); err != nil { // even a manual one
			t.Fatal(err)
		}
		objs = append(objs, b)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	refsOf := func(name string) []metav1.OwnerReference {
		b := &forgejov1.ForgejoBackup{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, b); err != nil {
			t.Fatal(err)
		}
		return b.OwnerReferences
	}
	pvcController := func() *metav1.OwnerReference {
		got := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: testBackupsPVC}, got); err != nil {
			t.Fatal(err)
		}
		return metav1.GetControllerOf(got)
	}

	// Delete -> Retain: every reference to the instance goes.
	retainF := ownedInstance(forgejov1.RetainPolicyRetain)
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: retainF}
	if _, err := ReconcileOwnership(ctx, rc); err != nil {
		t.Fatal(err)
	}
	for name := range reasons {
		if refs := refsOf(name); len(refs) != 0 {
			t.Errorf("Retain: backup %s still has owner references %v", name, refs)
		}
	}
	if ref := pvcController(); ref != nil {
		t.Errorf("Retain: backup PVC is still controlled by %v", ref)
	}

	// Retain -> Delete: scheduled and pre-upgrade backups and the PVC are owned again;
	// manual backups are left alone.
	rc.Instance = ownedInstance(forgejov1.RetainPolicyDelete)
	if _, err := ReconcileOwnership(ctx, rc); err != nil {
		t.Fatal(err)
	}
	for name, reason := range reasons {
		refs := refsOf(name)
		owned := len(refs) == 1 && refs[0].UID == testForgejoUID
		if want := reason != forgejov1.BackupReasonManual; owned != want {
			t.Errorf("Delete: backup %s owned=%v, want %v (%v)", name, owned, want, refs)
		}
	}
	if ref := pvcController(); ref == nil || ref.UID != testForgejoUID {
		t.Errorf("Delete: backup PVC controller = %v, want the instance", ref)
	}

	// Idempotent: nothing left to change.
	b := &forgejov1.ForgejoBackup{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "sched"}, b); err != nil {
		t.Fatal(err)
	}
	if changed, err := SyncBackupOwnership(ctx, c, s, rc.Instance, b); err != nil || changed {
		t.Errorf("second sync changed=%v err=%v", changed, err)
	}
}
