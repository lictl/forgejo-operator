package upgrade

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPreBackup       = "pre"
	testRollbackRestore = "rollback"
)

const (
	imgA = "codeberg.org/forgejo/forgejo:15"
	imgB = "codeberg.org/forgejo/forgejo:16"
)

func ago(d time.Duration) *metav1.Time {
	return &metav1.Time{Time: time.Now().Add(-d).Truncate(time.Second)}
}

// fixture is an instance that runs imgA with spec.image imgB and the given upgrade status.
func fixture(u *forgejov1.UpgradeStatus) *forgejov1.Forgejo {
	return &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: "uid"},
		Spec:       forgejov1.ForgejoSpec{Image: imgB},
		Status:     forgejov1.ForgejoStatus{Image: imgA, Upgrade: u},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func reconcile(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) (*forgejo.ReconcileContext, client.Client) {
	t.Helper()
	c := newClient(t, objs...)
	return reconcileWith(t, c, c.Scheme(), f), c
}

func reconcileWith(t *testing.T, c client.Client, s *runtime.Scheme, f *forgejov1.Forgejo) *forgejo.ReconcileContext {
	t.Helper()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, Instance: f, TargetImage: f.Status.Image}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

func restore(name string, phase forgejov1.RestorePhase, created *metav1.Time) *forgejov1.ForgejoRestore {
	return &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", CreationTimestamp: *created},
		Spec:       forgejov1.ForgejoRestoreSpec{ForgejoRef: forgejov1.ForgejoRef{Name: "fj"}, BackupName: testPreBackup},
		Status:     forgejov1.ForgejoRestoreStatus{Phase: phase},
	}
}

func serverPod(f *forgejov1.Forgejo, name, image string, created *metav1.Time, init corev1.ContainerState, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: forgejo.SelectorLabels(f), CreationTimestamp: *created},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: serverContainer, Image: image}}},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: migrateContainer, State: init, RestartCount: restarts}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: serverContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}},
		},
	}
}

func TestFailedRollbackRestoreKeepsForgejoStopped(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingBack, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, RestoreName: testRollbackRestore,
		StartTime: ago(time.Hour), RolloutStartTime: ago(50 * time.Minute),
	})
	rc, c := reconcile(t, f, restore("rollback", forgejov1.RestorePhaseFailed, ago(40*time.Minute)))
	u := f.Status.Upgrade
	if u.Phase != forgejov1.UpgradePhaseFailed || !u.RollbackRestoreFailed {
		t.Fatalf("phase %s, rollbackRestoreFailed %v", u.Phase, u.RollbackRestoreFailed)
	}
	if rc.TargetImage != imgA || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 || rc.Upgrading {
		t.Fatalf("target %s, override %v, upgrading %v", rc.TargetImage, rc.ReplicasOverride, rc.Upgrading)
	}

	// Still stopped while idle and no later restore has restored anything.
	u.CompletionTime = &metav1.Time{Time: u.CompletionTime.Truncate(time.Second)} // as persisted
	if err := c.Create(context.Background(), restore("manual", forgejov1.RestorePhaseRestoring, ptr.To(metav1.Now()))); err != nil {
		t.Fatal(err)
	}
	if rc = reconcileWith(t, c, c.Scheme(), f); rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 {
		t.Fatalf("override %v while the manual restore runs", rc.ReplicasOverride)
	}

	// A later restore that reached ScalingUp lifts the hold for good.
	r := &forgejov1.ForgejoRestore{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "manual"}, r); err != nil {
		t.Fatal(err)
	}
	r.Status.Phase = forgejov1.RestorePhaseScalingUp
	if err := c.Update(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if rc = reconcileWith(t, c, c.Scheme(), f); rc.ReplicasOverride != nil || rc.TargetImage != imgA || f.Status.Upgrade.RollbackRestoreFailed {
		t.Fatalf("after restore: override %v, target %s", rc.ReplicasOverride, rc.TargetImage)
	}
}

func TestFailedRollbackRestoreReleasedBySpecImage(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseFailed, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, RestoreName: testRollbackRestore,
		StartTime: ago(time.Hour), RolloutStartTime: ago(50 * time.Minute), CompletionTime: ago(time.Minute),
		FailedImage: imgB, RollbackRestoreFailed: true,
	})
	f.Spec.Image = imgA
	rc, _ := reconcile(t, f)
	if rc.ReplicasOverride != nil || rc.TargetImage != imgA {
		t.Fatalf("override %v, target %s", rc.ReplicasOverride, rc.TargetImage)
	}
}

func TestKeptFailureHoldsSingleReplica(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingOut, FromImage: imgA, ToImage: imgB,
		StartTime: ago(time.Hour), RolloutStartTime: ago(20 * time.Minute),
	})
	f.Spec.Replicas = ptr.To[int32](3)
	rc, c := reconcile(t, f)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseFailed || rc.Upgrading {
		t.Fatalf("phase %s, upgrading %v", f.Status.Upgrade.Phase, rc.Upgrading)
	}
	if rc.TargetImage != imgB || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 1 || !forgejo.UpgradeHoldsWorkload(f) {
		t.Fatalf("target %s, override %v", rc.TargetImage, rc.ReplicasOverride)
	}
	if rc = reconcileWith(t, c, c.Scheme(), f); rc.ReplicasOverride == nil || *rc.ReplicasOverride != 1 {
		t.Fatalf("idle override %v", rc.ReplicasOverride)
	}

	// The kept image became available: the hold ends.
	f.Status.Image = imgB
	if rc = reconcileWith(t, c, c.Scheme(), f); rc.ReplicasOverride != nil || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseSucceeded {
		t.Fatalf("override %v, phase %s", rc.ReplicasOverride, f.Status.Upgrade.Phase)
	}
}

func TestBackingUpGivesUpOnPodsThatDoNotStop(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, StartTime: ago(16 * time.Minute),
	})
	stuck := serverPod(f, "stuck", imgA, ago(time.Hour), corev1.ContainerState{}, 0)
	stuck.Status.Phase = corev1.PodRunning
	rc, _ := reconcile(t, f, stuck)
	u := f.Status.Upgrade
	if u.Phase != forgejov1.UpgradePhaseFailed || u.RolloutStartTime != nil || rc.ReplicasOverride != nil || rc.TargetImage != imgA {
		t.Fatalf("phase %s, override %v, target %s", u.Phase, rc.ReplicasOverride, rc.TargetImage)
	}
}

func TestFreshUpgradeDeploysWithRolloutStartTime(t *testing.T) {
	f := fixture(nil)
	f.Spec.Upgrade.BackupBeforeUpgrade = ptr.To(false)
	rc, c := reconcile(t, f)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhasePending || rc.TargetImage != imgA {
		t.Fatalf("Pending deploys %s", rc.TargetImage)
	}
	rc = reconcileWith(t, c, c.Scheme(), f)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingOut || u.RolloutStartTime == nil || rc.TargetImage != imgB {
		t.Fatalf("phase %s, target %s", u.Phase, rc.TargetImage)
	}
}

func TestCancelBeforeDeploy(t *testing.T) {
	pending := func() *forgejov1.Forgejo {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, StartTime: ago(time.Minute),
		})
		f.Spec.Image = imgA
		return f
	}
	dep := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: serverContainer, Image: image}}}}},
		}
	}
	bk := &forgejov1.ForgejoBackup{
		ObjectMeta: metav1.ObjectMeta{Name: testPreBackup, Namespace: "ns"},
		Status:     forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhaseRunning},
	}

	// A running backup finishes first, with Forgejo stopped.
	f := pending()
	rc, c := reconcile(t, f, dep(imgA), bk)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseBackingUp || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 {
		t.Fatalf("phase %s, override %v", f.Status.Upgrade.Phase, rc.ReplicasOverride)
	}
	bk.Status.Phase = forgejov1.BackupPhaseCompleted
	if err := c.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	rc = reconcileWith(t, c, c.Scheme(), f)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseCancelled || u.FailedImage != "" || rc.TargetImage != imgA || rc.ReplicasOverride != nil {
		t.Fatalf("phase %s, failedImage %q, target %s", u.Phase, u.FailedImage, rc.TargetImage)
	}
	if rc = reconcileWith(t, c, c.Scheme(), f); rc.TargetImage != imgA || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseCancelled {
		t.Fatalf("idle after cancel: target %s", rc.TargetImage)
	}

	// The rollout already started in a reconcile whose status write was lost: not cancelled.
	f = pending()
	bk.Status.Phase = forgejov1.BackupPhaseCompleted
	bk.ResourceVersion = ""
	rc, _ = reconcile(t, f, dep(imgB), bk)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseRollingOut || rc.TargetImage != imgB {
		t.Fatalf("phase %s, target %s", f.Status.Upgrade.Phase, rc.TargetImage)
	}
}

func TestRetryKeepsMigratingPodAndDeletesFailingOne(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingOut, FromImage: imgA, ToImage: imgB, Retry: true,
		StartTime: ago(time.Minute), RolloutStartTime: ago(time.Minute),
	})
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	crashing := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
	migrating := serverPod(f, "migrating", imgB, ago(time.Hour), running, 5)
	failed := serverPod(f, "failed", imgB, ago(time.Hour), crashing, 5)
	_, c := reconcile(t, f, migrating, failed)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingOut {
		t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
	}
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "migrating" {
		t.Fatalf("pods left: %v", pods.Items)
	}
}
