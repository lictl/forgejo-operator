package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
)

var jobsResource = batchv1.SchemeGroupVersion.WithResource("jobs").GroupResource()

func quotaRefusal() error {
	return apierrors.NewForbidden(jobsResource, "job", errors.New("exceeded quota: count/jobs.batch"))
}

// jobCreates counts the creates of Jobs that reach the fake API server and lets answer decide
// what each returns; answer gets the number of the attempt (from 1) and a func that stores
// the Job as the API server would.
func jobCreates(live client.WithWatch, attempts *int, answer func(attempt int, store func() error) error) client.Client {
	return interceptor.NewClient(live, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); !ok {
				return c.Create(ctx, obj, opts...)
			}
			*attempts++
			return answer(*attempts, func() error { return c.Create(ctx, obj, opts...) })
		},
	})
}

// uploadStartFixture is a completed backup whose upload has not started.
func uploadStartFixture(t *testing.T, name string) (*forgejov1.ForgejoBackup, client.WithWatch) {
	t.Helper()
	_, _, objs := remoteFixture(t)
	b := uploadPending(name)
	objs = append(objs, b, dbAppSecret("pw"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: testBackupsPVC, Namespace: ns}})
	return b, newFakeClient(backup.FakeScheme(t), objs...)
}

// dumpStartFixture is a queued manual backup whose dump has not started.
func dumpStartFixture(t *testing.T, name string) (*forgejov1.ForgejoBackup, client.WithWatch) {
	t.Helper()
	b := queuedBackup(name, forgejov1.BackupReasonManual)
	return b, newFakeClient(backup.FakeScheme(t), instance(""), b, dbAppSecret("pw"), fbSecrets())
}

// startFixture is the start of the dump or of the upload of a backup called name.
func startFixture(t *testing.T, upload bool, name string) (*forgejov1.ForgejoBackup, client.WithWatch) {
	if upload {
		return uploadStartFixture(t, name)
	}
	return dumpStartFixture(t, name)
}

func executionName(upload bool) string {
	if upload {
		return testUploadExecution
	}
	return testDumpExecution
}

// started reports whether the execution of b (its dump, or its upload) runs on a Job.
func started(t *testing.T, c client.Client, b *forgejov1.ForgejoBackup, upload bool) bool {
	t.Helper()
	got := getBackup(t, c, b.Name)
	if upload {
		return got.Status.Remote.Phase == forgejov1.RemoteBackupUploading && jobExists(t, c, backup.UploadJobName(b))
	}
	return got.Status.Phase == forgejov1.BackupPhaseRunning && jobExists(t, c, backup.BackupJobName(b))
}

// journaled reports whether b records the execution it was about to start.
func journaled(t *testing.T, c client.Client, b *forgejov1.ForgejoBackup, upload bool) bool {
	t.Helper()
	got := getBackup(t, c, b.Name)
	if upload {
		return got.Status.Remote.JobName != "" || got.Status.Remote.S3 != nil
	}
	return got.Status.JobName != "" || got.Status.ForgejoUID != "" || got.Status.PVCName != ""
}

type testClock struct{ at time.Time }

func (c *testClock) now() time.Time { return c.at }

func newBackupReconciler(t *testing.T, c client.Client, live client.Reader, clock *testClock) *controller.ForgejoBackupReconciler {
	t.Helper()
	return &controller.ForgejoBackupReconciler{Client: c, APIReader: live, Scheme: backup.FakeScheme(t), Now: clock.now}
}

func reconcileOnce(r *controller.ForgejoBackupReconciler, b *forgejov1.ForgejoBackup) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
}

func fastCreateRetries(t *testing.T) {
	t.Helper()
	t.Cleanup(backup.SetCreateRetryDelays(time.Millisecond, time.Millisecond, time.Millisecond))
}

// A rejection by the API server (a quota, an admission webhook, RBAC) proves that the Job was
// not stored. The journal is dropped and the start asked again, with a pause that doubles:
// status writes requeue an object at once, so without the pause a lasting refusal would be
// retried about ten times a second. Keeping the journal would end in JobMissing and a failed
// pre-upgrade or pre-restore backup.
func TestRefusedJobCreateForgetsTheJournalAndBacksOff(t *testing.T) {
	refusals := map[string]error{
		"quota":     quotaRefusal(),
		"admission": apierrors.NewInvalid(schema.GroupKind{Group: batchv1.GroupName, Kind: "Job"}, "job", nil),
	}
	for name, refusal := range refusals {
		for _, upload := range []bool{false, true} {
			t.Run(name+"/"+executionName(upload), func(t *testing.T) {
				b, live := startFixture(t, upload, "refused")
				attempts := 0
				c := jobCreates(live, &attempts, func(attempt int, store func() error) error {
					if attempt <= 2 {
						return refusal
					}
					return store()
				})
				clock := &testClock{at: time.Now()}
				r := newBackupReconciler(t, c, live, clock)

				if _, err := reconcileOnce(r, b); !errors.Is(err, refusal) {
					t.Fatalf("reconcile error %v, want the refusal", err)
				}
				if journaled(t, live, b, upload) || jobExists(t, live, backup.BackupJobName(b)) || jobExists(t, live, backup.UploadJobName(b)) {
					t.Fatal("the refused execution kept its journal, or a Job exists")
				}

				// The status writes of that pass requeue the object at once: that pass must not write
				// again, nor ask the API server before the pause is over.
				before := getBackup(t, live, b.Name).ResourceVersion
				res, err := reconcileOnce(r, b)
				if err != nil || res.RequeueAfter != 5*time.Second || attempts != 1 ||
					getBackup(t, live, b.Name).ResourceVersion != before {
					t.Fatalf("pass inside the pause = %+v, %v after %d attempts; want a 5s requeue without writes", res, err, attempts)
				}

				// A refusal that lasts doubles the pause.
				clock.at = clock.at.Add(5 * time.Second)
				if _, err := reconcileOnce(r, b); !errors.Is(err, refusal) || attempts != 2 {
					t.Fatalf("second attempt: %v after %d attempts", err, attempts)
				}
				if res, err := reconcileOnce(r, b); err != nil || res.RequeueAfter != 10*time.Second {
					t.Fatalf("second pause = %+v, %v; want 10s", res, err)
				}

				// Once the cluster accepts the Job the execution runs.
				clock.at = clock.at.Add(10 * time.Second)
				if _, err := reconcileOnce(r, b); err != nil {
					t.Fatalf("retry: %v", err)
				}
				if !started(t, live, b, upload) {
					t.Fatalf("the retry did not start the execution: %+v", getBackup(t, live, b.Name).Status)
				}
			})
		}
	}
}

// A refusal is reported, unless the namespace is going away (see
// TestBackupStartInTerminatingNamespace, which also covers the dump).
func TestRefusedJobCreateIsReported(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			b, live := startFixture(t, upload, "reported")
			attempts := 0
			c := jobCreates(live, &attempts, func(int, func() error) error { return quotaRefusal() })
			rec := events.NewFakeRecorder(10)
			r := newBackupReconciler(t, c, live, &testClock{at: time.Now()})
			r.Recorder = rec
			if _, err := reconcileOnce(r, b); err == nil {
				t.Fatal("a refused create was not reported as an error")
			}
			select {
			case e := <-rec.Events:
				if !strings.Contains(e, "Warning JobRefused") || !strings.Contains(e, "exceeded quota") {
					t.Fatalf("event %q", e)
				}
			default:
				t.Fatal("no event for the refused Job")
			}
		})
	}
}

// The upload start does not write events either into a terminating namespace.
func TestRefusedUploadInTerminatingNamespace(t *testing.T) {
	b, live := uploadStartFixture(t, "terminating")
	c := interceptor.NewClient(live, refuseCreate(&batchv1.Job{}, terminating("jobs")))
	rec := events.NewFakeRecorder(10)
	r := newBackupReconciler(t, c, live, &testClock{at: time.Now()})
	r.Recorder = rec
	if _, err := reconcileOnce(r, b); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	noEvents(t, rec)
	if journaled(t, live, b, true) {
		t.Fatal("the refused upload kept its journal")
	}
}

// An error that leaves open whether the Job was stored (a timeout, a 5xx, a dropped
// connection) is asked again within the pass: an attempt that did land is found by the next
// one, so the dump or upload still runs exactly once.
func TestUndecidedJobCreateIsRetriedWithinThePass(t *testing.T) {
	fastCreateRetries(t)
	for name, tc := range map[string]struct {
		answer   func(attempt int, store func() error) error
		attempts int
	}{
		"server error": {attempts: 3, answer: func(attempt int, store func() error) error {
			if attempt < 3 {
				return apierrors.NewServiceUnavailable("apiserver is restarting")
			}
			return store()
		}},
		"acknowledgement lost": {attempts: 2, answer: func(attempt int, store func() error) error {
			if attempt == 1 {
				if err := store(); err != nil {
					return err
				}
				return apierrors.NewTimeoutError("request did not complete", 1)
			}
			return store() // AlreadyExists: the Job of the first attempt
		}},
	} {
		for _, upload := range []bool{false, true} {
			t.Run(name+"/"+executionName(upload), func(t *testing.T) {
				b, live := startFixture(t, upload, "undecided")
				attempts := 0
				c := jobCreates(live, &attempts, tc.answer)
				if _, err := reconcileOnce(newBackupReconciler(t, c, live, &testClock{at: time.Now()}), b); err != nil {
					t.Fatalf("reconcile: %v", err)
				}
				if attempts != tc.attempts {
					t.Fatalf("%d create attempts, want %d", attempts, tc.attempts)
				}
				if !started(t, live, b, upload) {
					t.Fatalf("the execution did not start: %+v", getBackup(t, live, b.Name).Status)
				}
				jobs := &batchv1.JobList{}
				if err := live.List(context.Background(), jobs, client.InNamespace(ns)); err != nil || len(jobs.Items) != 1 {
					t.Fatalf("%d Jobs (%v), want exactly one", len(jobs.Items), err)
				}
			})
		}
	}
}

// A lagging cache shows an existing Job as absent right after AlreadyExists; the next attempt
// finds it.
func TestUndecidedJobCreateWaitsForTheCacheToShowTheJob(t *testing.T) {
	fastCreateRetries(t)
	b, live := dumpStartFixture(t, "lagging")
	attempts, lagging := 0, true
	cached := interceptor.NewClient(live, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); !ok {
				return c.Create(ctx, obj, opts...)
			}
			attempts++
			err := c.Create(ctx, obj, opts...)
			if attempts == 1 && err == nil {
				return apierrors.NewTimeoutError("request did not complete", 1)
			}
			if attempts == 3 {
				lagging = false // the cache has caught up
			}
			return err
		},
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*batchv1.Job); ok && lagging && attempts == 2 {
				return apierrors.NewNotFound(jobsResource, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	if _, err := reconcileOnce(newBackupReconciler(t, cached, live, &testClock{at: time.Now()}), b); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if attempts != 3 || !started(t, live, b, false) {
		t.Fatalf("%d attempts; the dump did not start: %+v", attempts, getBackup(t, live, b.Name).Status)
	}
}

// When the attempts end without an answer, the journal stays: the Job may yet appear, and
// recovery must not forget where it writes.
func TestUndecidedJobCreateKeepsTheJournal(t *testing.T) {
	fastCreateRetries(t)
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			b, live := startFixture(t, upload, "lost")
			attempts := 0
			lost := errors.New("connection reset by peer")
			c := jobCreates(live, &attempts, func(int, func() error) error { return lost })
			if _, err := reconcileOnce(newBackupReconciler(t, c, live, &testClock{at: time.Now()}), b); !errors.Is(err, lost) {
				t.Fatalf("reconcile error %v", err)
			}
			if attempts < 2 {
				t.Fatalf("only %d create attempt(s)", attempts)
			}
			if !journaled(t, live, b, upload) {
				t.Fatalf("the journal was not kept: %+v", getBackup(t, live, b.Name).Status)
			}
		})
	}
}

// An attempt of unknown outcome may have stored the Job, and a quota can then reject the next
// create because that Job used the last slot. Such a rejection must not drop the journal of
// a Job that exists and runs: nothing could clean up its files or release the identity.
func TestRefusalAfterAnUndecidedAttemptKeepsTheJournal(t *testing.T) {
	fastCreateRetries(t)
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			b, live := startFixture(t, upload, "landed")
			attempts := 0
			c := jobCreates(live, &attempts, func(attempt int, store func() error) error {
				if attempt == 1 {
					if err := store(); err != nil {
						return err
					}
					return apierrors.NewTimeoutError("request did not complete", 1)
				}
				return quotaRefusal()
			})
			rec := events.NewFakeRecorder(10)
			r := newBackupReconciler(t, c, live, &testClock{at: time.Now()})
			r.Recorder = rec
			if _, err := reconcileOnce(r, b); !apierrors.IsForbidden(err) {
				t.Fatalf("reconcile error %v, want the quota rejection", err)
			}
			jobName := backup.BackupJobName(b)
			if upload {
				jobName = backup.UploadJobName(b)
			}
			if !journaled(t, live, b, upload) || !jobExists(t, live, jobName) {
				t.Fatalf("the journal of the Job that landed was dropped: %+v", getBackup(t, live, b.Name).Status)
			}
			if len(rec.Events) != 0 {
				t.Fatalf("a Job that may exist was reported as refused: %q", <-rec.Events)
			}

			// Recovery tracks the Job that landed.
			reconcileBackup(t, live, b.Name)
			if !started(t, live, b, upload) {
				t.Fatalf("recovery did not track the Job: %+v", getBackup(t, live, b.Name).Status)
			}
		})
	}
}

// A predecessor's Job that is still being removed is waited for with the same pause as a
// refusal: the journal and its discard are status writes that requeue the backup at once.
func TestTransientJobConflictBacksOff(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			ctx := context.Background()
			b, live := startFixture(t, upload, "reuse")
			jobName := backup.BackupJobName(b)
			if upload {
				jobName = backup.UploadJobName(b)
			}
			old := b.DeepCopy()
			old.UID = testOldBackupUID
			predecessor := finishedJob(t, backup.FakeScheme(t), jobName, old, batchv1.JobComplete)
			if err := live.Create(ctx, predecessor); err != nil {
				t.Fatal(err)
			}
			clock := &testClock{at: time.Now()}
			r := newBackupReconciler(t, live, live, clock)
			if _, err := reconcileOnce(r, b); err != nil || journaled(t, live, b, upload) {
				t.Fatalf("first pass: %v; journal kept: %v", err, journaled(t, live, b, upload))
			}
			before := getBackup(t, live, b.Name).ResourceVersion
			res, err := reconcileOnce(r, b)
			if err != nil || res.RequeueAfter != 5*time.Second || getBackup(t, live, b.Name).ResourceVersion != before {
				t.Fatalf("pass inside the pause = %+v, %v; want a 5s requeue without writes", res, err)
			}
			if err := live.Delete(ctx, predecessor); err != nil {
				t.Fatal(err)
			}
			clock.at = clock.at.Add(5 * time.Second)
			if _, err := reconcileOnce(r, b); err != nil || !started(t, live, b, upload) {
				t.Fatalf("after the predecessor went: %v, %+v", err, getBackup(t, live, b.Name).Status)
			}
		})
	}
}

// A lagging cache can show the Job that an earlier, unanswered attempt stored as a
// predecessor's. A conflict that follows such an attempt must not drop the journal either:
// recovery judges the Job with a live read.
func TestConflictAfterAnUndecidedAttemptKeepsTheJournal(t *testing.T) {
	fastCreateRetries(t)
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			b, live := startFixture(t, upload, "stale")
			attempts := 0
			cached := interceptor.NewClient(live, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					if _, ok := obj.(*batchv1.Job); !ok {
						return c.Create(ctx, obj, opts...)
					}
					attempts++
					err := c.Create(ctx, obj, opts...)
					if attempts == 1 && err == nil {
						return apierrors.NewTimeoutError("request did not complete", 1)
					}
					return err
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					if job, ok := obj.(*batchv1.Job); ok && err == nil && attempts >= 2 {
						job.OwnerReferences = []metav1.OwnerReference{{APIVersion: forgejov1.GroupVersion.String(),
							Kind: "ForgejoBackup", Name: "someone-else", UID: "other-uid", Controller: ptr.To(true)}}
					}
					return err
				},
			})
			if _, err := reconcileOnce(newBackupReconciler(t, cached, live, &testClock{at: time.Now()}), b); err == nil {
				t.Fatal("a conflict after an unanswered attempt was not reported as an error")
			}
			if !journaled(t, live, b, upload) {
				t.Fatalf("the journal of the Job that landed was dropped: %+v", getBackup(t, live, b.Name).Status)
			}
			reconcileBackup(t, live, b.Name) // recovery reads the Job live and finds it is ours
			if !started(t, live, b, upload) {
				t.Fatalf("recovery did not track the Job: %+v", getBackup(t, live, b.Name).Status)
			}
		})
	}
}

// A Job of another object under the name of a journaled execution is reported, with a Warning
// event, once per recheck instead of failing the reconcile in a loop. Nothing is cleaned up or
// released beside it.
func TestForeignJobIsReportedNotLooped(t *testing.T) {
	for _, upload := range []bool{false, true} {
		t.Run(executionName(upload), func(t *testing.T) {
			s := backup.FakeScheme(t)
			_, _, objs := remoteFixture(t)
			b := runningBackup("foreign")
			b.Status.Phase = forgejov1.BackupPhaseFailed
			jobName := b.Status.JobName
			if upload {
				b = uploadPending("foreign")
				b.Status.Remote.Phase = forgejov1.RemoteBackupFailed
				b.Status.Remote.JobName = backup.UploadJobName(b)
				jobName = b.Status.Remote.JobName
			}
			identity := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: backup.IdentitySecretName(b), Namespace: ns}}
			if err := controllerutil.SetControllerReference(b, identity, s); err != nil {
				t.Fatal(err)
			}
			foreign := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, UID: "foreign-job"}}
			c := newFakeClient(s, append(objs, b, foreign, identity, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: testBackupsPVC, Namespace: ns}})...)
			rec := events.NewFakeRecorder(10)
			r := &controller.ForgejoBackupReconciler{Client: c, APIReader: c, Scheme: s, Recorder: rec}
			res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
			if err != nil || res.RequeueAfter < time.Minute {
				t.Fatalf("Reconcile = %+v, %v; want no error and a slow recheck", res, err)
			}
			select {
			case e := <-rec.Events:
				if !strings.Contains(e, "Warning ForeignJob") || !strings.Contains(e, jobName) {
					t.Fatalf("event %q", e)
				}
			default:
				t.Fatal("no event for the foreign Job")
			}
			if jobExists(t, c, backup.CleanupJobName(b)) {
				t.Fatal("files were cleaned up beside a Job that is not ours")
			}
			if !jobExists(t, c, jobName) {
				t.Fatal("the foreign Job was removed")
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(identity), &corev1.Secret{}); err != nil {
				t.Fatalf("the identity Secret was released beside a foreign Job: %v", err)
			}
		})
	}
}
