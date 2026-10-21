package backup_test

// Regression tests for ForgejoBackup/ForgejoRestore pairs that share a name. They drive the
// real reconcilers (internal/controller) against a fake client, so they need no envtest.

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
)

const ns = "regression"

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func instance(activeRestore string) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: ns, UID: "forgejo-uid"}}
	f.Spec.Image = "codeberg.org/forgejo/forgejo:16"
	f.Status.Backup = &forgejov1.BackupStatusSummary{ActiveRestore: activeRestore}
	return f
}

func completedBackup(name string) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "backup-uid"}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.PVCName = "fb-backups"
	b.Status.IncludesData = true
	b.Status.ForgejoImage = "codeberg.org/forgejo/forgejo:16"
	return b
}

func newRestore(name string, phase forgejov1.RestorePhase) *forgejov1.ForgejoRestore {
	r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "restore-uid"}}
	r.Spec.ForgejoRef.Name = "fb"
	r.Spec.BackupName = name
	r.Spec.RestoreDatabase = ptr.To(false) // no database needed by the fake environment
	r.Status.Phase = phase
	r.Status.StartTime = &metav1.Time{Time: time.Now()}
	return r
}

// completedJob is a finished Job named name controlled by owner.
func completedJob(t *testing.T, s *runtime.Scheme, name string, owner client.Object) *batchv1.Job {
	t.Helper()
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	if err := controllerutil.SetControllerReference(owner, j, s); err != nil {
		t.Fatal(err)
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	j.Status.Succeeded = 1
	return j
}

func reconcileRestore(t *testing.T, c client.Client, s *runtime.Scheme, name string, times int) *forgejov1.ForgejoRestore {
	t.Helper()
	r := &controller.ForgejoRestoreReconciler{Client: c, Scheme: s}
	for range times {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	got := &forgejov1.ForgejoRestore{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, got); err != nil {
		t.Fatal(err)
	}
	return got
}

func reason(conds []metav1.Condition) string {
	if c := meta.FindStatusCondition(conds, "Ready"); c != nil {
		return c.Reason
	}
	return ""
}

// A restore named like its backup runs its own Job instead of adopting the backup's
// completed Job (which reported a restore that never ran).
func TestRestoreNamedLikeItsBackupRunsItsOwnJob(t *testing.T) {
	s := scheme(t)
	b := completedBackup("pre-migration")
	rs := newRestore("pre-migration", forgejov1.RestorePhaseScalingDown)
	backupJob := completedJob(t, s, backup.BackupJobName(b), b)
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).
		WithObjects(instance(rs.Name), b, rs, backupJob).Build()

	got := reconcileRestore(t, c, s, rs.Name, 2) // finalizer, then scale-down check and Job
	if got.Status.Phase != forgejov1.RestorePhaseRestoring {
		t.Fatalf("phase %s (%s), want Restoring", got.Status.Phase, got.Status.Message)
	}
	if got.Status.JobName == backupJob.Name {
		t.Fatalf("restore adopted the backup's Job %s", backupJob.Name)
	}
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: got.Status.JobName}, job); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(job, got) {
		t.Fatalf("Job %s is not controlled by the restore", job.Name)
	}
	if !controllerutil.ContainsFinalizer(got, "forgejo.lucas.icu/finalizer") {
		t.Error("restore has no finalizer although its Job may write to the instance")
	}
}

// A Job of the restore's name that belongs to something else fails the restore instead of
// being tracked, both before and after the instance was scaled down.
func TestRestoreRefusesForeignJob(t *testing.T) {
	for _, phase := range []forgejov1.RestorePhase{"", forgejov1.RestorePhaseScalingDown} {
		t.Run("phase "+string(phase), func(t *testing.T) {
			s := scheme(t)
			b := completedBackup("x")
			rs := newRestore("x", phase)
			foreign := completedJob(t, s, backup.RestoreJobName(rs), b)
			c := fake.NewClientBuilder().WithScheme(s).
				WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).
				WithObjects(instance(rs.Name), b, rs, foreign).Build()

			got := reconcileRestore(t, c, s, rs.Name, 3)
			if got.Status.Phase != forgejov1.RestorePhaseFailed || reason(got.Status.Conditions) != "JobNameConflict" {
				t.Fatalf("phase %s reason %s (%s), want Failed/JobNameConflict", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
			}
			if got.Status.JobName != "" {
				t.Errorf("restore recorded the foreign Job %s", got.Status.JobName)
			}
		})
	}
}

// A restore that is already tracking a Job it does not control (e.g. one started by an
// operator version with the shared "-job" suffix) must not report the foreign Job's success.
func TestRestoreTrackingForeignJobDoesNotSucceed(t *testing.T) {
	s := scheme(t)
	b := completedBackup("x")
	rs := newRestore("x", forgejov1.RestorePhaseRestoring)
	rs.Status.JobName = "x-job"
	controllerutil.AddFinalizer(rs, "forgejo.lucas.icu/finalizer")
	rs.Labels = map[string]string{"forgejo.lucas.icu/instance": "fb"}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).
		WithObjects(instance(rs.Name), b, rs, completedJob(t, s, "x-job", b)).Build()

	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase == forgejov1.RestorePhaseScalingUp || got.Status.Phase == forgejov1.RestorePhaseCompleted {
		t.Fatalf("restore reached %s from a Job it does not control", got.Status.Phase)
	}
	if reason(got.Status.Conditions) != "JobNameConflict" {
		t.Errorf("reason %s (%s), want JobNameConflict", reason(got.Status.Conditions), got.Status.Message)
	}
}

// The reverse case: a backup whose Job name is taken by a restore's completed Job fails
// instead of being marked Completed without any files.
func TestBackupRefusesForeignJob(t *testing.T) {
	s := scheme(t)
	f := instance("")
	rs := newRestore("y", forgejov1.RestorePhaseCompleted)
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "y", Namespace: ns, UID: "backup-y"}}
	b.Spec.ForgejoRef.Name = "fb"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "fb-db-app", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("forgejo"), "password": []byte("x")},
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).
		WithObjects(f, rs, b, secret, completedJob(t, s, backup.BackupJobName(b), rs)).Build()

	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s}
	for range 3 {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "y"}}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	got := &forgejov1.ForgejoBackup{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "y"}, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != forgejov1.BackupPhaseFailed || reason(got.Status.Conditions) != "JobNameConflict" {
		t.Fatalf("phase %s reason %s (%s), want Failed/JobNameConflict", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
	}
}
