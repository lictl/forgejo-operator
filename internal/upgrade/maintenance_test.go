package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// reconcileInMaintenance reconciles f as the pipeline does while the maintenance step reports
// a restore (rc.Maintenance).
func reconcileInMaintenance(t *testing.T, c client.Client, f *forgejov1.Forgejo) *forgejo.ReconcileContext {
	t.Helper()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image,
		Maintenance: true, MaintenanceReason: "ForgejoRestore manual is restoring backup b"}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

// deployment is core's Deployment with one replica of image, available or not.
func deployment(image string, available bool) *appsv1.Deployment {
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: serverContainer, Image: image}}}}},
		Status: appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1},
	}
	if available {
		d.Status.AvailableReplicas, d.Status.ReadyReplicas = 1, 1
	}
	return d
}

func replace(t *testing.T, c client.Client, obj client.Object) {
	t.Helper()
	if err := c.Delete(context.Background(), obj); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	obj.SetResourceVersion("")
	if err := c.Create(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

func TestSucceedsOnlyOnceDeploymentRolledOut(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseVerifying, FromImage: imgA, ToImage: imgB,
		StartTime: ago(10 * time.Minute), RolloutStartTime: ago(5 * time.Minute),
	})
	f.Status.Image = imgB

	// status.image alone is not enough: no Deployment yet.
	_, c := reconcile(t, f)
	if p := f.Status.Upgrade.Phase; p != forgejov1.UpgradePhaseVerifying {
		t.Fatalf("without a Deployment: phase %s", p)
	}
	for _, d := range []*appsv1.Deployment{deployment(imgB, false), deployment(imgA, true)} {
		replace(t, c, d)
		if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseVerifying {
			t.Fatalf("Deployment %s (available %d): phase %s", deploymentImage(d), d.Status.AvailableReplicas, f.Status.Upgrade.Phase)
		}
	}
	stale := deployment(imgB, true)
	stale.Generation, stale.Status.ObservedGeneration = 2, 1
	replace(t, c, stale)
	if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseVerifying {
		t.Fatalf("unobserved generation: phase %s", f.Status.Upgrade.Phase)
	}

	// A rolled-out Deployment alone is not enough either.
	replace(t, c, deployment(imgB, true))
	f.Status.Image = imgA
	if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseVerifying {
		t.Fatalf("status.image %s: phase %s", f.Status.Image, f.Status.Upgrade.Phase)
	}

	f.Status.Image = imgB
	rc := reconcileWith(t, c, c.Scheme(), f)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseSucceeded || rc.TargetImage != imgB || rc.Upgrading {
		t.Fatalf("phase %s, target %s, upgrading %v", u.Phase, rc.TargetImage, rc.Upgrading)
	}
}

func TestMaintenancePausesPendingAndBackingUp(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhasePending, FromImage: imgA, ToImage: imgB, BackupName: "pre", StartTime: ago(time.Minute),
		})
		c := newClient(t)
		rc := reconcileInMaintenance(t, c, f)
		u := f.Status.Upgrade
		if u.Phase != forgejov1.UpgradePhasePending || rc.TargetImage != imgA || !rc.Upgrading || !strings.HasPrefix(u.Message, "Paused") {
			t.Fatalf("phase %s, target %s, upgrading %v, message %q", u.Phase, rc.TargetImage, rc.Upgrading, u.Message)
		}
		if reconcileWith(t, c, c.Scheme(), f); u.Phase != forgejov1.UpgradePhaseBackingUp {
			t.Fatalf("after maintenance: phase %s", u.Phase)
		}
	})

	t.Run("no backup is taken", func(t *testing.T) {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: "pre", StartTime: ago(time.Minute),
		})
		c := newClient(t)
		rc := reconcileInMaintenance(t, c, f)
		if rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 || rc.TargetImage != imgA || backups(t, c) != 0 {
			t.Fatalf("override %v, target %s, backups %d", rc.ReplicasOverride, rc.TargetImage, backups(t, c))
		}
		if reconcileWith(t, c, c.Scheme(), f); backups(t, c) != 1 || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseBackingUp {
			t.Fatalf("after maintenance: backups %d, phase %s", backups(t, c), f.Status.Upgrade.Phase)
		}
	})

	t.Run("stop timeout does not fire", func(t *testing.T) {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: "pre", StartTime: ago(16 * time.Minute),
		})
		stuck := serverPod(f, "stuck", imgA, ago(time.Hour), corev1.ContainerState{}, 0)
		stuck.Status.Phase = corev1.PodRunning
		reconcileInMaintenance(t, newClient(t, stuck), f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseBackingUp || u.FailureReason != "" {
			t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
		}
	})

	t.Run("completed backup does not start the rollout", func(t *testing.T) {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: "pre", StartTime: ago(time.Minute),
		})
		bk := &forgejov1.ForgejoBackup{
			ObjectMeta: metav1.ObjectMeta{Name: "pre", Namespace: "ns"},
			Status:     forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhaseCompleted},
		}
		c := newClient(t, bk)
		rc := reconcileInMaintenance(t, c, f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseBackingUp || u.RolloutStartTime != nil || rc.TargetImage != imgA {
			t.Fatalf("phase %s, rolloutStartTime %v, target %s", u.Phase, u.RolloutStartTime, rc.TargetImage)
		}
		rc = reconcileWith(t, c, c.Scheme(), f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingOut || u.RolloutStartTime == nil || rc.TargetImage != imgB {
			t.Fatalf("after maintenance: phase %s, target %s", u.Phase, rc.TargetImage)
		}
	})

	t.Run("cancellation still applies", func(t *testing.T) {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhasePending, FromImage: imgA, ToImage: imgB, BackupName: "pre", StartTime: ago(time.Minute),
		})
		f.Spec.Image = imgA
		rc := reconcileInMaintenance(t, newClient(t), f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseCancelled || rc.TargetImage != imgA || rc.Upgrading {
			t.Fatalf("phase %s, target %s", u.Phase, rc.TargetImage)
		}
	})
}

func TestMaintenancePausesRolloutTimeout(t *testing.T) {
	// The timeout (15m) would have expired five minutes ago.
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseVerifying, FromImage: imgA, ToImage: imgB, BackupName: "pre",
		StartTime: ago(30 * time.Minute), RolloutStartTime: ago(20 * time.Minute),
	})
	c := newClient(t)
	rc := reconcileInMaintenance(t, c, f)
	u := f.Status.Upgrade
	if u.Phase != forgejov1.UpgradePhaseRollingOut || u.FailureReason != "" || rc.TargetImage != imgB || !rc.Upgrading {
		t.Fatalf("phase %s (%s), target %s", u.Phase, u.FailureReason, rc.TargetImage)
	}
	if time.Since(u.RolloutStartTime.Time) > time.Minute {
		t.Fatalf("rolloutStartTime %s was not moved to the end of maintenance", u.RolloutStartTime)
	}

	// Maintenance has ended: the timeout counts from there.
	u.RolloutStartTime = ago(time.Minute)
	if reconcileWith(t, c, c.Scheme(), f); u.Phase != forgejov1.UpgradePhaseRollingOut || u.FailureReason != "" {
		t.Fatalf("after maintenance: phase %s (%s)", u.Phase, u.FailureReason)
	}
	u.RolloutStartTime = ago(16 * time.Minute)
	if reconcileWith(t, c, c.Scheme(), f); u.Phase != forgejov1.UpgradePhaseRollingBack {
		t.Fatalf("timeout after maintenance: phase %s", u.Phase)
	}
}

func TestMaintenancePausesRollback(t *testing.T) {
	rollingBack := func() *forgejov1.Forgejo {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseRollingBack, FromImage: imgA, ToImage: imgB, BackupName: "pre", RestoreName: "rollback",
			StartTime: ago(time.Hour), RolloutStartTime: ago(50 * time.Minute), FailureReason: "crashed",
		})
		f.Status.Backup = &forgejov1.BackupStatusSummary{ActiveRestore: "manual"}
		return f
	}

	t.Run("restore is not created", func(t *testing.T) {
		f := rollingBack()
		c := newClient(t)
		reconcileInMaintenance(t, c, f)
		if n := restores(t, c); n != 0 || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseRollingBack {
			t.Fatalf("restores %d, phase %s", n, f.Status.Upgrade.Phase)
		}
		f.Status.Backup.ActiveRestore = ""
		reconcileWith(t, c, c.Scheme(), f)
		if n := restores(t, c); n != 1 {
			t.Fatalf("after maintenance: restores %d", n)
		}
	})

	t.Run("wait for the old image does not time out", func(t *testing.T) {
		f := rollingBack()
		done := restore("rollback", forgejov1.RestorePhaseCompleted, ago(40*time.Minute))
		done.Status.CompletionTime = ago(20 * time.Minute)
		c := newClient(t, done, deployment(imgA, false))
		reconcileInMaintenance(t, c, f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingBack {
			t.Fatalf("phase %s: %s", u.Phase, u.Message)
		}
		f.Status.Backup.ActiveRestore = ""
		if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseRollingBack {
			t.Fatalf("after maintenance: phase %s: %s", f.Status.Upgrade.Phase, f.Status.Upgrade.Message)
		}
		replace(t, c, deployment(imgA, true))
		if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseRolledBack {
			t.Fatalf("old image available: phase %s", f.Status.Upgrade.Phase)
		}
	})

	t.Run("own restore does not pause", func(t *testing.T) {
		f := rollingBack()
		f.Status.Backup.ActiveRestore = "rollback" // the failed restore holds maintenance
		c := newClient(t, restore("rollback", forgejov1.RestorePhaseFailed, ago(40*time.Minute)))
		rc := reconcileInMaintenance(t, c, f)
		if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseFailed || !u.RollbackRestoreFailed || rc.TargetImage != imgA {
			t.Fatalf("phase %s, rollbackRestoreFailed %v, target %s", u.Phase, u.RollbackRestoreFailed, rc.TargetImage)
		}
	})
}

func backups(t *testing.T, c client.Client) int {
	t.Helper()
	l := &forgejov1.ForgejoBackupList{}
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

func restores(t *testing.T, c client.Client) int {
	t.Helper()
	l := &forgejov1.ForgejoRestoreList{}
	if err := c.List(context.Background(), l); err != nil {
		t.Fatal(err)
	}
	return len(l.Items)
}

// An upgrade does not start while the Secret of SSH host keys the pods serve is unusable: the
// new image's pods could not start, and the cold backup would stop Forgejo for nothing.
func TestUpgradeWaitsForSSHHostKeys(t *testing.T) {
	f := fixture(nil)
	c := newClient(t)
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image,
		SSHHostKeysUnavailable: true}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading)
	if f.Status.Upgrade != nil || rc.Upgrading || rc.TargetImage != imgA || cond == nil || cond.Reason != "WaitingForSSHHostKeys" {
		t.Fatalf("upgrade %+v, upgrading %v, target %s, condition %+v", f.Status.Upgrade, rc.Upgrading, rc.TargetImage, cond)
	}
	if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade == nil || !inFlight(f.Status.Upgrade.Phase) {
		t.Fatalf("once the keys are back the upgrade starts: %+v", f.Status.Upgrade)
	}
}

// An upgrade does not start while the bucket of spec.objectStorage is unusable: the new
// version could not start, and its rollback would restore the database for nothing.
func TestUpgradeWaitsForObjectStorage(t *testing.T) {
	f := fixture(nil)
	c := newClient(t)
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image,
		ObjectStorageUnavailable: true}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading)
	if f.Status.Upgrade != nil || rc.Upgrading || rc.TargetImage != imgA || cond == nil || cond.Reason != "WaitingForObjectStorage" {
		t.Fatalf("upgrade %+v, upgrading %v, target %s, condition %+v", f.Status.Upgrade, rc.Upgrading, rc.TargetImage, cond)
	}
	if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade == nil || !inFlight(f.Status.Upgrade.Phase) {
		t.Fatalf("once the bucket is usable the upgrade starts: %+v", f.Status.Upgrade)
	}
}
