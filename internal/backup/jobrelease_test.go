package backup_test

// Finished backup, restore and cleanup Jobs are deleted once their outcome is in status:
// their pods would otherwise keep a kubernetes.io/pvc-protection hold on the data and backup
// PVCs, so deleting an instance with retainPolicy Delete left those claims Terminating.

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// runningBackup is a manual backup of instance "fb" whose Job was started 5 minutes ago.
func runningBackup(name string) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid"),
		Finalizers: []string{forgejo.FinalizerName},
		Labels:     map[string]string{forgejo.LabelForgejo: "fb", forgejo.LabelBackupReason: string(forgejov1.BackupReasonManual)}}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Status.Phase = forgejov1.BackupPhaseRunning
	b.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
	b.Status.JobName = backup.BackupJobName(b)
	b.Status.PVCName = "fb-backups"
	b.Status.Path = "/" + name
	b.Status.IncludesData = true
	return b
}

// finishedJob is a Job named name controlled by owner that ended with cond (Complete or
// Failed).
func finishedJob(t *testing.T, s *runtime.Scheme, name string, owner client.Object, cond batchv1.JobConditionType) *batchv1.Job {
	t.Helper()
	j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid"),
		Labels: map[string]string{forgejo.LabelInstance: "fb"}}}
	if err := controllerutil.SetControllerReference(owner, j, s); err != nil {
		t.Fatal(err)
	}
	j.Status.Conditions = []batchv1.JobCondition{{Type: cond, Status: corev1.ConditionTrue, Reason: "Test"}}
	now := metav1.Now()
	if cond == batchv1.JobComplete {
		j.Status.Succeeded, j.Status.CompletionTime = 1, &now
	} else {
		j.Status.Failed = 1
	}
	return j
}

// jobPod is a finished pod of Job job whose container name ended with exit code and message.
func jobPod(job, container string, code int32, message string) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job + "-x1", Namespace: ns, CreationTimestamp: metav1.Now(),
		Labels: map[string]string{batchv1.JobNameLabel: job}}}
	p.Status.Phase = corev1.PodSucceeded
	if code != 0 {
		p.Status.Phase = corev1.PodFailed
	}
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: container,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: message}}}}
	return p
}

func jobExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &batchv1.Job{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func getBackup(t *testing.T, c client.Client, name string) *forgejov1.ForgejoBackup {
	t.Helper()
	b := &forgejov1.ForgejoBackup{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newFakeClient(s *runtime.Scheme, objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoBackup{}, &forgejov1.ForgejoRestore{}, &batchv1.Job{}).
		WithObjects(objs...).Build()
}

// A successful backup records sizes, message and times, then deletes its Job.
func TestCompletedBackupDeletesItsJob(t *testing.T) {
	s := scheme(t)
	b := runningBackup("m1")
	job := finishedJob(t, s, b.Status.JobName, b, batchv1.JobComplete)
	pod := jobPod(job.Name, backup.ContainerArchive, 0, `{"databaseSizeBytes":123,"dataSizeBytes":456,"warnings":0}`)
	c := newFakeClient(s, instance(""), b, job, pod)

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseCompleted || got.Status.DatabaseSizeBytes != 123 || got.Status.DataSizeBytes != 456 ||
		got.Status.CompletionTime == nil || got.Status.StartTime == nil || got.Status.Message == "" {
		t.Fatalf("status not recorded before the Job went: %+v", got.Status)
	}
	if jobExists(t, c, job.Name) {
		t.Error("backup Job kept after its outcome was recorded")
	}
}

// A failed backup keeps the failing container's output in its message and deletes its Job.
// Its files are then removed by a cleanup Job, whose outcome goes into FilesRemoved before it
// is deleted too; neither a later reconcile nor deleting the backup runs another one.
func TestFailedBackupDeletesItsJobsAndRecordsFileRemoval(t *testing.T) {
	s := scheme(t)
	ctx := context.Background()
	b := runningBackup("m2")
	job := finishedJob(t, s, b.Status.JobName, b, batchv1.JobFailed)
	pod := jobPod(job.Name, backup.ContainerArchive, 1, "gzip failed with exit code 1 (backup volume full?)")
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fb-backups", Namespace: ns}}
	c := newFakeClient(s, instance(""), b, job, pod, pvc)

	reconcileBackup(t, c, b.Name)
	got := getBackup(t, c, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseFailed || reason(got.Status.Conditions) != "JobFailed" {
		t.Fatalf("phase %s reason %s, want Failed/JobFailed", got.Status.Phase, reason(got.Status.Conditions))
	}
	if want := "backup volume full"; !strings.Contains(got.Status.Message, want) {
		t.Errorf("message %q lost the container output %q", got.Status.Message, want)
	}
	if jobExists(t, c, job.Name) {
		t.Error("failed backup Job kept after its outcome was recorded")
	}

	// The failed run's pod has finished: the cleanup Job starts.
	reconcileBackup(t, c, b.Name)
	cleanup := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: backup.CleanupJobName(b)}, cleanup); err != nil {
		t.Fatalf("no cleanup Job for the failed backup: %v", err)
	}
	cleanup.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	cleanup.Status.Succeeded = 1
	if err := c.Status().Update(ctx, cleanup); err != nil {
		t.Fatal(err)
	}

	reconcileBackup(t, c, b.Name)
	got = getBackup(t, c, b.Name)
	if cond := meta.FindStatusCondition(got.Status.Conditions, backup.ConditionFilesRemoved); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("FilesRemoved = %+v, want True", cond)
	}
	if jobExists(t, c, cleanup.Name) {
		t.Error("cleanup Job kept after its outcome was recorded")
	}
	reconcileBackup(t, c, b.Name)
	if jobExists(t, c, cleanup.Name) {
		t.Error("a second cleanup Job was started for files already removed")
	}

	// Deleting the backup finishes without another cleanup Job.
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, c, b.Name)
	if jobExists(t, c, cleanup.Name) {
		t.Error("deleting the backup ran a cleanup Job for files already removed")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: b.Name}, &forgejov1.ForgejoBackup{}); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer not released (err %v)", err)
	}
}

// A finished backup removes a finished Job it still has (e.g. kept by an earlier operator
// version), but never a running Job or one it does not control.
func TestFinishedBackupReleasesOnlyItsFinishedJob(t *testing.T) {
	s := scheme(t)
	for name, tc := range map[string]struct {
		cond    batchv1.JobConditionType // "" = running
		foreign bool
		gone    bool
	}{
		"own finished Job":     {cond: batchv1.JobComplete, gone: true},
		"own running Job":      {},
		"foreign finished Job": {cond: batchv1.JobComplete, foreign: true},
	} {
		t.Run(name, func(t *testing.T) {
			b := runningBackup("m3")
			b.Status.Phase = forgejov1.BackupPhaseCompleted
			b.Status.CompletionTime = &metav1.Time{Time: time.Now()}
			var owner client.Object = b
			if tc.foreign {
				owner = newRestore("other", forgejov1.RestorePhaseCompleted)
			}
			job := finishedJob(t, s, b.Status.JobName, owner, batchv1.JobComplete)
			if tc.cond == "" {
				job.Status = batchv1.JobStatus{Active: 1}
			}
			c := newFakeClient(s, instance(""), b, job)

			reconcileBackup(t, c, b.Name)
			if exists := jobExists(t, c, job.Name); exists == tc.gone {
				t.Errorf("Job exists = %t, want %t", exists, !tc.gone)
			}
		})
	}
}

// A reconcile that starts from a stale copy of a backup (still Running, while the backup was
// already recorded as Completed and its Job deleted) must not overwrite the result with
// JobMissing, which would then remove the files of a good backup.
func TestStaleBackupCannotOverwriteRecordedResult(t *testing.T) {
	s := scheme(t)
	ctx := context.Background()
	b := runningBackup("m4")
	live := newFakeClient(s, instance(""), b)
	stale := getBackup(t, live, b.Name)

	done := getBackup(t, live, b.Name)
	done.Status.Phase = forgejov1.BackupPhaseCompleted
	done.Status.CompletionTime = &metav1.Time{Time: time.Now()}
	done.Status.Message = "Backup completed"
	if err := live.Status().Update(ctx, done); err != nil {
		t.Fatal(err)
	}

	cached := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if bk, ok := obj.(*forgejov1.ForgejoBackup); ok && key.Name == b.Name {
				stale.DeepCopyInto(bk)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &controller.ForgejoBackupReconciler{Client: cached, APIReader: live, Scheme: s}
	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: b.Name}})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Error("a conflict from a stale copy is not retried")
	}
	if got := getBackup(t, live, b.Name); got.Status.Phase != forgejov1.BackupPhaseCompleted {
		t.Errorf("stale reconcile overwrote the recorded result: %s %s", got.Status.Phase, got.Status.Message)
	}
}

// The restore Job stays while the restore is ScalingUp (its completion time starts the
// scale-up timeout) and goes once the restore is Completed; a failed Job goes as soon as the
// failure, with how far it got, is recorded.
func TestRestoreDeletesItsJobOnceFinished(t *testing.T) {
	s := scheme(t)
	prep := func(t *testing.T, cond batchv1.JobConditionType) (client.Client, *forgejov1.ForgejoRestore, *batchv1.Job) {
		rs := newRestore("r1", forgejov1.RestorePhaseRestoring)
		rs.Finalizers = []string{forgejo.FinalizerName}
		rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
		rs.Spec.BackupName = "b1"
		rs.Status.JobName = backup.RestoreJobName(rs)
		rs.Status.JobStartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
		job := finishedJob(t, s, rs.Status.JobName, rs, cond)
		code := int32(0)
		if cond == batchv1.JobFailed {
			code = 1
		}
		pod := jobPod(job.Name, backup.ContainerVerify, code, "data.tar.gz is corrupt")
		return newFakeClient(s, instance(rs.Name), completedBackup("b1"), rs, job, pod), rs, job
	}

	t.Run("success", func(t *testing.T) {
		c, rs, job := prep(t, batchv1.JobComplete)
		if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingUp {
			t.Fatalf("phase %s (%s), want ScalingUp", got.Status.Phase, got.Status.Message)
		}
		if !jobExists(t, c, job.Name) {
			t.Fatal("restore Job deleted while ScalingUp still needs its completion time")
		}
		// The instance has no Deployment: the restore completes.
		if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseCompleted {
			t.Fatalf("phase %s (%s), want Completed", got.Status.Phase, got.Status.Message)
		}
		if jobExists(t, c, job.Name) {
			t.Error("restore Job kept after the restore completed")
		}
	})

	t.Run("failure", func(t *testing.T) {
		c, rs, job := prep(t, batchv1.JobFailed)
		got := reconcileRestore(t, c, s, rs.Name, 1)
		if got.Status.Phase != forgejov1.RestorePhaseFailed || !strings.Contains(got.Status.Message, "corrupt") {
			t.Fatalf("phase %s message %q, want Failed with the container output", got.Status.Phase, got.Status.Message)
		}
		if jobExists(t, c, job.Name) {
			t.Error("failed restore Job kept after the failure was recorded")
		}
	})

	t.Run("instance being deleted while ScalingUp", func(t *testing.T) {
		c, rs, job := prep(t, batchv1.JobComplete)
		reconcileRestore(t, c, s, rs.Name, 1) // ScalingUp
		f := &forgejov1.Forgejo{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "fb"}, f); err != nil {
			t.Fatal(err)
		}
		f.Finalizers = []string{"test/hold"} // keep it around while it is being deleted
		if err := c.Update(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		if err := c.Delete(context.Background(), f); err != nil {
			t.Fatal(err)
		}
		if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseCompleted {
			t.Fatalf("phase %s (%s), want Completed", got.Status.Phase, got.Status.Message)
		}
		if jobExists(t, c, job.Name) {
			t.Error("restore Job kept although the instance is being deleted")
		}
	})
}
