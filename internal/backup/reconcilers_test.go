package backup_test

// Tests that drive the real ForgejoBackup and ForgejoRestore reconcilers (internal/controller)
// against fake clients, so they need no envtest: their fixtures, and backups and restores that
// share a name.

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

const (
	testImage16               = "codeberg.org/forgejo/forgejo:16"
	testBackupsPVC            = "fb-backups"
	testSecretsName           = "fb-secrets"
	testPasswordField         = "password"
	testReasonJobNameConflict = "JobNameConflict"
)

const ns = "reconcilers"

func instance(activeRestore string) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: ns, UID: "forgejo-uid"}}
	f.Spec.Image = testImage16
	f.Status.Backup = &forgejov1.BackupStatusSummary{ActiveRestore: activeRestore}
	return f
}

func completedBackup(name string) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "backup-uid"}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.PVCName = testBackupsPVC
	b.Status.IncludesData = true
	b.Status.ForgejoImage = testImage16
	return b
}

// runningBackup is a manual backup of instance "fb" whose Job was started 5 minutes ago.
func runningBackup(name string) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(name + "-uid"),
		Finalizers: []string{forgejo.FinalizerName},
		Labels:     map[string]string{forgejo.LabelForgejo: "fb", forgejo.LabelBackupReason: string(forgejov1.BackupReasonManual)}}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Status.Phase = forgejov1.BackupPhaseRunning
	b.Status.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
	b.Status.JobName = backup.BackupJobName(b)
	b.Status.PVCName = testBackupsPVC
	b.Status.Path = "/" + name
	b.Status.IncludesData = true
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

// fbSecrets is the identity Secret of instance "fb".
func fbSecrets() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsName, Namespace: ns},
		Data: map[string][]byte{"secret-key": []byte("sk")}}
}

// dbAppSecret is CNPG's "fb-db-app" Secret of instance "fb".
func dbAppSecret(password string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fb-db-app", Namespace: ns},
		Data: map[string][]byte{"username": []byte("forgejo"), testPasswordField: []byte(password)}}
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

func newFakeClient(s *runtime.Scheme, objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&forgejov1.ForgejoBackup{}, &forgejov1.ForgejoRestore{}, &batchv1.Job{}).
		WithObjects(objs...).Build()
}

func reconcileBackup(t *testing.T, c client.Client, name string) {
	t.Helper()
	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: backup.FakeScheme(t)}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func reconcileRestore(t *testing.T, c client.Client, s *runtime.Scheme, name string, times int) *forgejov1.ForgejoRestore {
	t.Helper()
	r := &controller.ForgejoRestoreReconciler{Client: c, Scheme: s}
	for range times {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	return getRestore(t, c, name)
}

func getBackup(t *testing.T, c client.Client, name string) *forgejov1.ForgejoBackup {
	t.Helper()
	b := &forgejov1.ForgejoBackup{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func getRestore(t *testing.T, c client.Client, name string) *forgejov1.ForgejoRestore {
	t.Helper()
	r := &forgejov1.ForgejoRestore{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func jobExists(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &batchv1.Job{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
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
	s := backup.FakeScheme(t)
	b := completedBackup("pre-migration")
	rs := newRestore("pre-migration", forgejov1.RestorePhaseScalingDown)
	backupJob := finishedJob(t, s, backup.BackupJobName(b), b, batchv1.JobComplete)
	c := newFakeClient(s, instance(rs.Name), b, rs, backupJob)

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
// being tracked, both before and after the instance was scaled down. A restore that already
// tracks a Job it does not control (e.g. one started by an operator version with the shared
// "-job" suffix) must not report the foreign Job's success.
func TestRestoreRefusesForeignJob(t *testing.T) {
	s := backup.FakeScheme(t)
	for _, tc := range []struct {
		name    string
		phase   forgejov1.RestorePhase
		tracked string // the Job the restore tracks already
	}{
		{name: "before scale-down"},
		{name: "after scale-down", phase: forgejov1.RestorePhaseScalingDown},
		{name: "tracking a Job of an older operator", phase: forgejov1.RestorePhaseRestoring, tracked: "x-job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := completedBackup("x")
			rs := newRestore("x", tc.phase)
			job := backup.RestoreJobName(rs)
			if tc.tracked != "" {
				rs.Status.JobName, job = tc.tracked, tc.tracked
			}
			c := newFakeClient(s, instance(rs.Name), b, rs, finishedJob(t, s, job, b, batchv1.JobComplete))

			got := reconcileRestore(t, c, s, rs.Name, 3)
			if got.Status.Phase != forgejov1.RestorePhaseFailed || reason(got.Status.Conditions) != testReasonJobNameConflict {
				t.Fatalf("phase %s reason %s (%s), want Failed/JobNameConflict", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
			}
			if got.Status.JobName != tc.tracked {
				t.Errorf("restore recorded the foreign Job %s", got.Status.JobName)
			}
		})
	}
}

// The reverse case: a backup whose Job name is taken by a restore's completed Job fails
// instead of being marked Completed without any files.
func TestBackupRefusesForeignJob(t *testing.T) {
	s := backup.FakeScheme(t)
	rs := newRestore("y", forgejov1.RestorePhaseCompleted)
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "y", Namespace: ns, UID: "backup-y"}}
	b.Spec.ForgejoRef.Name = "fb"
	c := newFakeClient(s, instance(""), rs, b, dbAppSecret("x"), finishedJob(t, s, backup.BackupJobName(b), rs, batchv1.JobComplete))

	for range 3 {
		reconcileBackup(t, c, b.Name)
	}
	if got := getBackup(t, c, b.Name); got.Status.Phase != forgejov1.BackupPhaseFailed || reason(got.Status.Conditions) != testReasonJobNameConflict {
		t.Fatalf("phase %s reason %s (%s), want Failed/JobNameConflict", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
	}
}
