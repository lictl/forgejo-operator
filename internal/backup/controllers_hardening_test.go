package backup_test

// Hardening tests that drive the real ForgejoBackup/ForgejoRestore reconcilers against fake
// clients (no envtest).

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const testBackupsPVC = "fb-backups"

// deletedOwnedBackup is a completed scheduled backup owned by instance f that garbage
// collection is deleting.
func deletedOwnedBackup(t *testing.T, f *forgejov1.Forgejo) *forgejov1.ForgejoBackup {
	t.Helper()
	b := completedBackup("fb-scheduled-1")
	b.Spec.Reason = forgejov1.BackupReasonScheduled
	b.Status.Path = "/fb-scheduled-1"
	b.Finalizers = []string{forgejo.FinalizerName}
	if err := controllerutil.SetOwnerReference(f, b, scheme(t)); err != nil {
		t.Fatal(err)
	}
	b.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	return b
}

func reconcileBackup(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: scheme(t)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// A backup garbage-collected with its deleted instance keeps its files when the backup
// volume is retained (retainPolicy switched to Retain after the backup was created).
func TestBackupFinalizerKeepsFilesOfRetainedInstance(t *testing.T) {
	s := scheme(t)
	gone := instance("")
	b := deletedOwnedBackup(t, gone)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: ns}} // retained: no owner
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(b, pvc).WithStatusSubresource(b).Build()

	reconcileBackup(t, c, b.Name)

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: backup.CleanupJobName(b)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("a cleanup Job was created for the backup of a retained instance (err %v)", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: b.Name}, &forgejov1.ForgejoBackup{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer was not released (err %v)", err)
	}
}

// The guard does not stop the cleanup of a backup the user deletes while its instance lives,
// nor when the backup volume goes with the deleted instance anyway.
func TestBackupFinalizerStillCleansUp(t *testing.T) {
	s := scheme(t)
	for name, tc := range map[string]struct {
		alive    bool
		ownedPVC bool
	}{
		"explicit delete of an owned backup": {alive: true},
		"instance and backup volume deleted": {ownedPVC: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := instance("")
			f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete
			b := deletedOwnedBackup(t, f)
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: ns}}
			if tc.ownedPVC {
				if err := controllerutil.SetControllerReference(f, pvc, s); err != nil {
					t.Fatal(err)
				}
			}
			objs := []client.Object{b, pvc}
			if tc.alive {
				objs = append(objs, f)
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).WithStatusSubresource(b).Build()

			reconcileBackup(t, c, b.Name)

			if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: backup.CleanupJobName(b)}, &batchv1.Job{}); err != nil {
				t.Errorf("no cleanup Job: %v", err)
			}
		})
	}
}

// A restore Job missing from the cache is only reported as gone after a live read, and the
// grace period counts from when the Job was created, not from when scaling down started.
func TestRestoreJobMissingIsConfirmedLive(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	rs := newRestore("r1", forgejov1.RestorePhaseRestoring)
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	rs.Spec.BackupName = "b1"
	rs.Status.StartTime = &metav1.Time{Time: time.Now().Add(-20 * time.Minute)}
	rs.Status.JobName = backup.RestoreJobName(rs)

	// Created a moment ago: within the grace period although startTime is old.
	rs.Status.JobStartTime = &metav1.Time{Time: time.Now()}
	cached := fake.NewClientBuilder().WithScheme(s).WithObjects(instance(rs.Name), b, rs).WithStatusSubresource(rs).Build()
	r := &controller.ForgejoRestoreReconciler{Client: cached, APIReader: cached, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: rs.Name}}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, cached, rs.Name); got.Status.Phase != forgejov1.RestorePhaseRestoring {
		t.Errorf("restore failed within the grace period: %s %s", got.Status.Phase, got.Status.Message)
	}

	// Past the grace period, the cache lacks the Job but the API server has it.
	rs.Status.JobStartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: rs.Status.JobName, Namespace: ns, CreationTimestamp: metav1.Now()}}
	if err := controllerutil.SetControllerReference(rs, job, s); err != nil {
		t.Fatal(err)
	}
	cached = fake.NewClientBuilder().WithScheme(s).WithObjects(instance(rs.Name), b, rs).WithStatusSubresource(rs).Build()
	live := fake.NewClientBuilder().WithScheme(s).WithObjects(job).Build()
	r = &controller.ForgejoRestoreReconciler{Client: cached, APIReader: live, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: rs.Name}}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, cached, rs.Name); got.Status.Phase != forgejov1.RestorePhaseRestoring {
		t.Errorf("restore failed although the API server still has its Job: %s %s", got.Status.Phase, got.Status.Message)
	}

	// Gone from both: JobMissing.
	r = &controller.ForgejoRestoreReconciler{Client: cached, APIReader: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: rs.Name}}); err != nil {
		t.Fatal(err)
	}
	if got := get(t, cached, rs.Name); got.Status.Phase != forgejov1.RestorePhaseFailed || reason(got.Status.Conditions) != "JobMissing" {
		t.Errorf("phase %s reason %s, want Failed/JobMissing", got.Status.Phase, reason(got.Status.Conditions))
	}
}

func get(t *testing.T, c client.Client, name string) *forgejov1.ForgejoRestore {
	t.Helper()
	got := &forgejov1.ForgejoRestore{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

// A restore does not start its Job while a pod of an earlier restore of the instance may
// still be writing.
func TestRestoreWaitsForOtherRestorePods(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	rs := newRestore("r2", forgejov1.RestorePhaseScalingDown)
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	rs.Spec.BackupName = "b1"
	f := instance(rs.Name)
	old := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "r1-restore-abcde", Namespace: ns,
		Labels: map[string]string{forgejo.LabelInstance: "fb", forgejo.LabelComponent: forgejo.ComponentRestore, batchv1.JobNameLabel: "r1-restore"}}}
	old.Status.Phase = corev1.PodRunning
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, b, rs, old).WithStatusSubresource(rs).Build()
	reconcileRestore(t, c, s, rs.Name, 1)

	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: backup.RestoreJobName(rs)}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("restore Job created while another restore pod runs (err %v)", err)
	}
	if got := get(t, c, rs.Name); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Errorf("phase %s, want ScalingDown (waiting)", got.Status.Phase)
	}

	// Once that pod has finished, the Job starts and records when it was created.
	old.Status.Phase = corev1.PodSucceeded
	if err := c.Status().Update(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseRestoring || got.Status.JobStartTime == nil {
		t.Errorf("phase %s jobStartTime %v (%s), want Restoring with jobStartTime", got.Status.Phase, got.Status.JobStartTime, got.Status.Message)
	}
}

// spec.replicas: 0 does not mean "no pods" for an instance whose HPA owns the replica count:
// the restore waits for Forgejo to become available instead of completing right away.
func TestRestoreScaleUpWaitsForHPAManagedInstance(t *testing.T) {
	s := scheme(t)
	rs := newRestore("r1", forgejov1.RestorePhaseScalingUp)
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	f := instance("")
	f.Spec.Replicas = ptr.To[int32](0)
	f.Spec.HighAvailability.Enabled = true
	f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 4}
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: ns}}
	dep.Spec.Replicas = ptr.To[int32](0)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, rs, dep).WithStatusSubresource(rs).Build()

	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseScalingUp {
		t.Errorf("phase %s (%s), want ScalingUp until the HPA-managed instance is available", got.Status.Phase, got.Status.Message)
	}
}

// A restore does not scale Forgejo down while the Secret of SSH host keys the pods serve is
// missing or unusable: Forgejo could not start again afterwards.
func TestRestoreWaitsForSSHHostKeys(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	rs := newRestore("r1", "")
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	rs.Spec.BackupName = "b1"
	f := instance("")
	f.Status.SSH = &forgejov1.SSHStatus{HostKeysSecretName: "fb-ssh-host-keys", Message: "Secret fb-ssh-host-keys with the SSH host keys the pods serve is missing"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, b, rs).WithStatusSubresource(rs).Build()

	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhasePending || !strings.Contains(got.Status.Message, "Waiting for the SSH host keys: Secret fb-ssh-host-keys") {
		t.Fatalf("phase %s (%s), want Pending, waiting for the keys", got.Status.Phase, got.Status.Message)
	}

	f.Status.SSH.Message = ""
	if err := c.Update(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if got = reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Fatalf("phase %s (%s), want ScalingDown once the keys are back", got.Status.Phase, got.Status.Message)
	}
}

// A restore does not scale Forgejo down while the Secrets of spec.objectStorage are missing or
// unusable: the Forgejo pipeline stops before the workload step (no scale-down), and no pod
// could start again afterwards. An unreachable bucket does not hold it.
func TestRestoreWaitsForObjectStorageSecrets(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	rs := newRestore("r1", "")
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	rs.Spec.BackupName = "b1"
	f := instance("")
	f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{}
	setReady := func(status metav1.ConditionStatus, reason, msg string) {
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionObjectStorageReady,
			Status: status, Reason: reason, Message: msg})
	}
	setReady(metav1.ConditionFalse, "CredentialsUnavailable", `secret "s3" not found`)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, b, rs).WithStatusSubresource(rs).Build()

	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhasePending || got.Status.Message != `Waiting for spec.objectStorage: secret "s3" not found` {
		t.Fatalf("phase %s (%s), want Pending, waiting for the Secret", got.Status.Phase, got.Status.Message)
	}
	if active, _ := backup.MaintenanceRestore([]forgejov1.ForgejoRestore{*got}); active != nil {
		t.Fatal("a restore waiting for object storage puts the instance into maintenance")
	}

	setReady(metav1.ConditionFalse, "UnreachableFromOperator", "dial tcp: i/o timeout")
	if err := c.Update(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	if got = reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Fatalf("phase %s (%s), want ScalingDown once the Secret is back", got.Status.Phase, got.Status.Message)
	}
}
