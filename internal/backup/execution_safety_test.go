package backup_test

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/controller"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	nextForgejoImage    = "codeberg.org/forgejo/forgejo:17"
	testDumpExecution   = "dump"
	testUploadExecution = "upload"
	testRemotePrefix    = "team"
	testOldBackupUID    = "old-backup-uid"
)

func queuedBackup(name string, reason forgejov1.BackupReason) *forgejov1.ForgejoBackup {
	b := runningBackup(name)
	b.Spec.Reason = reason
	b.Labels[forgejo.LabelBackupReason] = string(reason)
	b.Status = forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhasePending}
	return b
}

// A backup already queued before an upgrade or failure must obey the same safety holds as
// the scheduler. A PreUpgrade reason alone never authorizes dumping a migrating database.
func TestQueuedBackupsRespectSafetyHolds(t *testing.T) {
	states := map[string]func(*forgejov1.Forgejo){
		"Broken": func(f *forgejov1.Forgejo) {
			f.Status.Conditions = []metav1.Condition{{Type: forgejov1.ConditionBroken, Status: metav1.ConditionTrue}}
		},
		"failed Forgejo migration": func(f *forgejov1.Forgejo) {
			now := metav1.Now()
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseFailed, ToImage: nextForgejoImage,
				FailedImage: nextForgejoImage, RolloutStartTime: &now}
		},
		"failed PostgreSQL upgrade": func(f *forgejov1.Forgejo) {
			f.Status.Database = &forgejov1.DatabaseStatus{Upgrade: &forgejov1.DatabaseUpgradeStatus{
				Phase: forgejov1.DatabaseUpgradeFailed, HoldsForgejo: true}}
		},
	}
	for _, phase := range []forgejov1.UpgradePhase{forgejov1.UpgradePhasePending, forgejov1.UpgradePhaseBackingUp,
		forgejov1.UpgradePhaseRollingOut, forgejov1.UpgradePhaseVerifying, forgejov1.UpgradePhaseRollingBack} {
		states[string(phase)] = func(f *forgejov1.Forgejo) {
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: phase, ToImage: nextForgejoImage, BackupName: "own-backup"}
		}
	}
	for name, setState := range states {
		for _, reason := range []forgejov1.BackupReason{forgejov1.BackupReasonManual, forgejov1.BackupReasonScheduled, forgejov1.BackupReasonPreUpgrade} {
			t.Run(name+"/"+string(reason), func(t *testing.T) {
				f := instance("")
				f.Status.Image = testImage16
				setState(f)
				b := queuedBackup("queued", reason)
				c := newFakeClient(backup.FakeScheme(t), f, b, dbAppSecret("pw"), fbSecrets())
				reconcileBackup(t, c, b.Name)
				if got := getBackup(t, c, b.Name); got.Status.Phase != forgejov1.BackupPhasePending {
					t.Fatalf("backup %s: %s", got.Status.Phase, got.Status.Message)
				}
				if jobExists(t, c, backup.BackupJobName(b)) {
					t.Fatal("backup Job created while the instance is unsafe")
				}
			})
		}
	}
}

func TestOnlyBackingUpOwnUpgradeMayStart(t *testing.T) {
	for _, phase := range []forgejov1.UpgradePhase{forgejov1.UpgradePhaseBackingUp, forgejov1.UpgradePhaseRollingOut} {
		t.Run(string(phase), func(t *testing.T) {
			f := instance("")
			f.Status.Image = testImage16
			b := queuedBackup("own-backup", forgejov1.BackupReasonPreUpgrade)
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: phase, BackupName: b.Name, ToImage: nextForgejoImage}
			c := newFakeClient(backup.FakeScheme(t), f, b, dbAppSecret("pw"), fbSecrets())
			reconcileBackup(t, c, b.Name)
			want := forgejov1.BackupPhasePending
			if phase == forgejov1.UpgradePhaseBackingUp {
				want = forgejov1.BackupPhaseRunning
			}
			if got := getBackup(t, c, b.Name); got.Status.Phase != want {
				t.Fatalf("backup %s, want %s: %s", got.Status.Phase, want, got.Status.Message)
			}
		})
	}
}

// A pre-restore backup cannot wait for a Forgejo upgrade paused by that very restore's
// maintenance. Failure releases the restore unchanged and gives the user the opt-out path.
func TestUnsafePreRestoreBackupFailsWithoutDeadlock(t *testing.T) {
	f, rs := scalingDown(t)
	f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseVerifying, ToImage: nextForgejoImage}
	b := queuedBackup(backup.PreRestoreBackupName(rs), forgejov1.BackupReasonPreRestore)
	s := backup.FakeScheme(t)
	c := newFakeClient(s, f, rs, completedBackup("b1"), b, dbAppSecret("pw"), fbSecrets())
	reconcileBackup(t, c, b.Name)
	if got := getBackup(t, c, b.Name); got.Status.Phase != forgejov1.BackupPhaseFailed || reason(got.Status.Conditions) != "InstanceUnsafe" {
		t.Fatalf("backup %s: %s", got.Status.Phase, got.Status.Message)
	}
	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseFailed || !strings.Contains(got.Status.Message, "spec.preRestoreBackup: false") || backup.HoldsMaintenance(got) {
		t.Fatalf("restore %s: %s", got.Status.Phase, got.Status.Message)
	}
}

func TestBackupStartUsesLiveSafetyState(t *testing.T) {
	s := backup.FakeScheme(t)
	f := instance("")
	stale := f.DeepCopy()
	f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseRollingOut}
	b := queuedBackup("cached", forgejov1.BackupReasonScheduled)
	live := newFakeClient(s, f, b, dbAppSecret("pw"), fbSecrets())
	cached := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if got, ok := obj.(*forgejov1.Forgejo); ok {
				stale.DeepCopyInto(got)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &controller.ForgejoBackupReconciler{Client: cached, APIReader: live, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
		t.Fatal(err)
	}
	if got := getBackup(t, live, b.Name); got.Status.Phase != forgejov1.BackupPhasePending || jobExists(t, live, backup.BackupJobName(b)) {
		t.Fatalf("stale cache started backup: %+v", got.Status)
	}
}

// The source is journaled before Job creation. A crash losing Running status must not
// overwrite it on retry and attribute the existing dump to a newly replaced SECRET_KEY.
func TestInterruptedBackupStartRecoversExecution(t *testing.T) {
	s := backup.FakeScheme(t)
	f := instance("")
	b := queuedBackup("interrupted", forgejov1.BackupReasonManual)
	secret := fbSecrets()
	live := newFakeClient(s, f, b, dbAppSecret("pw"), secret)
	lost := errors.New("lost running status")
	c := interceptor.NewClient(live, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if got, ok := obj.(*forgejov1.ForgejoBackup); ok && got.Status.Phase == forgejov1.BackupPhaseRunning {
				return lost
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); !errors.Is(err, lost) {
		t.Fatalf("reconcile error %v", err)
	}
	got := getBackup(t, live, b.Name)
	oldIdentity := forgejo.SecretKeyFingerprint(secret.Data)
	if got.Status.ForgejoUID != string(f.UID) || got.Status.SecretKeyFingerprint != oldIdentity || !jobExists(t, live, backup.BackupJobName(b)) {
		t.Fatalf("source was not saved before Job: %+v", got.Status)
	}
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["secret-key"] = []byte("changed")
	if err := live.Update(context.Background(), secret); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, live, b.Name)
	got = getBackup(t, live, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseRunning || got.Status.SecretKeyFingerprint != oldIdentity ||
		got.Status.JobName != backup.BackupJobName(b) || got.Status.PVCName != testBackupsPVC || got.Status.ForgejoImage != testImage16 {
		t.Fatalf("retry lost the original execution: %+v", got.Status)
	}
	// Follow the recovered Job through failure and file cleanup, not just status recovery.
	job := &batchv1.Job{}
	if err := live.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.BackupJobName(b)}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := live.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, live, b.Name)
	reconcileBackup(t, live, b.Name)
	got = getBackup(t, live, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseFailed || jobExists(t, live, job.Name) || !jobExists(t, live, backup.CleanupJobName(b)) {
		t.Fatalf("recovered failure did not release its Job and clean its files: %+v", got.Status)
	}
}

func TestUploadRejectsUnprovenOrChangedIdentity(t *testing.T) {
	for _, change := range []string{"changed-key", "instance", "missing-key", "missing-uid"} {
		t.Run(change, func(t *testing.T) {
			_, f, objs := remoteFixture(t)
			b := uploadPending("drift")
			switch change {
			case "changed-key":
				b.Status.SecretKeyFingerprint = forgejo.SecretKeyFingerprint(map[string][]byte{"secret-key": []byte("old-key")})
			case "instance":
				f.UID = "replacement"
			case "missing-key":
				b.Status.SecretKeyFingerprint = ""
			case "missing-uid":
				b.Status.ForgejoUID = ""
			}
			c := newFakeClient(backup.FakeScheme(t), append(objs, b)...)
			reconcileBackup(t, c, b.Name)
			got := getBackup(t, c, b.Name)
			if got.Status.Phase != forgejov1.BackupPhaseCompleted || got.Status.Remote.Phase != forgejov1.RemoteBackupFailed || jobExists(t, c, backup.UploadJobName(b)) {
				t.Fatalf("upload accepted an unproven identity: %+v", got.Status.Remote)
			}
		})
	}
}

func TestUploadReadsLiveIdentity(t *testing.T) {
	s := backup.FakeScheme(t)
	_, _, objs := remoteFixture(t)
	b := uploadPending("stale-identity")
	live := newFakeClient(s, append(objs, b)...)
	sec := fbSecrets()
	stale := sec.DeepCopy()
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(sec), sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["secret-key"] = []byte("changed")
	if err := live.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	cached := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if got, ok := obj.(*corev1.Secret); ok && key.Name == stale.Name {
				stale.DeepCopyInto(got)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r := &controller.ForgejoBackupReconciler{Client: cached, APIReader: live, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); err != nil {
		t.Fatal(err)
	}
	if got := getBackup(t, live, b.Name); got.Status.Remote.Phase != forgejov1.RemoteBackupFailed {
		t.Fatalf("stale key was accepted: %+v", got.Status.Remote)
	}
}

// The upload Job may already exist when a status write is lost. Its mounted identity is
// frozen, rather than overwritten with a later admin-password change on retry.
func TestUploadRetryKeepsItsIdentitySecret(t *testing.T) {
	s := backup.FakeScheme(t)
	store, f, objs := remoteFixture(t)
	b := uploadPending("retry")
	live := newFakeClient(s, append(objs, b)...)
	lost := errors.New("lost uploading status")
	c := interceptor.NewClient(live, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if got, ok := obj.(*forgejov1.ForgejoBackup); ok && got.Status.Remote != nil && got.Status.Remote.Phase == forgejov1.RemoteBackupUploading {
				return lost
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); !errors.Is(err, lost) {
		t.Fatalf("reconcile error %v", err)
	}
	identity := &corev1.Secret{}
	key := client.ObjectKey{Namespace: ns, Name: backup.IdentitySecretName(b)}
	if err := live.Get(context.Background(), key, identity); err != nil {
		t.Fatal(err)
	}
	original := string(identity.Data[backup.IdentityFile])
	if identity.Immutable == nil || !*identity.Immutable {
		t.Fatal("upload identity is mutable")
	}
	admin := &corev1.Secret{}
	if err := live.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "fb-admin"}, admin); err != nil {
		t.Fatal(err)
	}
	admin.Data[testPasswordField] = []byte("changed")
	if err := live.Update(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	sec := fbSecrets()
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(sec), sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["secret-key"] = []byte("new-identity")
	if err := live.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	if err := live.Get(context.Background(), client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Backup.Remote = nil
	if err := live.Update(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, live, b.Name)
	if err := live.Get(context.Background(), key, identity); err != nil {
		t.Fatal(err)
	}
	if string(identity.Data[backup.IdentityFile]) != original {
		t.Fatal("retry rewrote the identity being uploaded")
	}
	job := &batchv1.Job{}
	if err := live.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.UploadJobName(b)}, job); err != nil {
		t.Fatal(err)
	}
	if got := getBackup(t, live, b.Name); got.Status.Remote.Phase != forgejov1.RemoteBackupUploading ||
		got.Status.Remote.S3 == nil || got.Status.Remote.S3.Prefix != testRemotePrefix || got.Status.Remote.KeySecret == nil ||
		got.Status.Remote.KeySecret.Name != fbKey {
		t.Fatalf("retry did not resume the original upload target: %+v", got.Status.Remote)
	}
	// The recorded target also drives settlement and cleanup after failure, despite the
	// instance now having another identity and no remote configuration at all.
	dir := "team/" + b.Status.Remote.Path + "/"
	store.Put(dir+"db.dump.bin", []byte("partial"), time.Now())
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	if err := live.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, live, b.Name)
	reconcileBackup(t, live, b.Name)
	if got := getBackup(t, live, b.Name); got.Status.Remote.Phase != forgejov1.RemoteBackupFailed ||
		len(store.Keys(dir)) != 0 || jobExists(t, live, job.Name) {
		t.Fatalf("interrupted upload was not settled at its original target: %+v", got.Status.Remote)
	}
	if err := live.Get(context.Background(), key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("identity Secret was not released after settlement: %v", err)
	}
}

// Deletion may arrive before the retry that would have recovered Running/Uploading. It
// must still stop the owned Job before removing the backup files and releasing the CR.
func TestDeleteRecoversInterruptedJobStart(t *testing.T) {
	for _, upload := range []bool{false, true} {
		name := testDumpExecution
		if upload {
			name = testUploadExecution
		}
		t.Run(name, func(t *testing.T) {
			s := backup.FakeScheme(t)
			_, _, objs := remoteFixture(t)
			b := queuedBackup("delete-interrupted", forgejov1.BackupReasonManual)
			jobName := backup.BackupJobName(b)
			if upload {
				b = uploadPending(b.Name)
				jobName = backup.UploadJobName(b)
			}
			objs = append(objs, b, dbAppSecret("pw"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: testBackupsPVC, Namespace: ns}})
			live := newFakeClient(s, objs...)
			lost := errors.New("lost start status")
			c := interceptor.NewClient(live, interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					if got, ok := obj.(*forgejov1.ForgejoBackup); ok && (got.Status.Phase == forgejov1.BackupPhaseRunning ||
						got.Status.Remote != nil && got.Status.Remote.Phase == forgejov1.RemoteBackupUploading) {
						return lost
					}
					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			})
			r := &controller.ForgejoBackupReconciler{Client: c, Scheme: s}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}); !errors.Is(err, lost) {
				t.Fatalf("reconcile error %v", err)
			}
			if err := live.Delete(context.Background(), getBackup(t, live, b.Name)); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, live, b.Name) // recover original Job coordinates
			if !jobExists(t, live, jobName) {
				t.Fatal("recovery itself deleted the Job")
			}
			reconcileBackup(t, live, b.Name) // stop the writer/reader first
			if jobExists(t, live, jobName) || jobExists(t, live, backup.CleanupJobName(b)) {
				t.Fatal("file cleanup was not separated from stopping the recovered Job")
			}
			reconcileBackup(t, live, b.Name) // then remove files
			cleanup := &batchv1.Job{}
			if err := live.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: backup.CleanupJobName(b)}, cleanup); err != nil {
				t.Fatal(err)
			}
			cleanup.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
			if err := live.Status().Update(context.Background(), cleanup); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, live, b.Name)
			if err := live.Get(context.Background(), client.ObjectKeyFromObject(b), &forgejov1.ForgejoBackup{}); !apierrors.IsNotFound(err) {
				t.Fatalf("backup was not released after file cleanup: %v", err)
			}
		})
	}
}

// Jobs can disappear before the first recovery. Status must retain their source and target,
// and a stale cache must not hide the orphaned pods that still use those files or Secrets.
func TestInterruptedStartsSurviveMissingJobsAndStalePods(t *testing.T) {
	for _, upload := range []bool{false, true} {
		name := "missing-dump"
		if upload {
			name = "missing-upload"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := backup.FakeScheme(t)
			store, b, live, pod := interruptedExecutionWithOrphan(t, name, upload)
			jobName := backup.BackupJobName(b)
			if upload {
				jobName = backup.UploadJobName(b)
			}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}
			stale := interceptor.NewClient(live, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if pods, ok := list.(*corev1.PodList); ok {
						pods.Items = nil
						return nil
					}
					return c.List(ctx, list, opts...)
				},
			})
			r := &controller.ForgejoBackupReconciler{Client: stale, APIReader: live, Scheme: s}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			got := getBackup(t, live, b.Name)
			past := metav1.NewTime(time.Now().Add(-2 * time.Minute))
			if upload {
				if got.Status.Remote.S3 == nil || got.Status.Remote.S3.Prefix != testRemotePrefix || got.Status.Remote.KeySecret == nil {
					t.Fatalf("lost original upload target: %+v", got.Status.Remote)
				}
				got.Status.Remote.StartTime = &past
				store.Put("team/"+got.Status.Remote.Path+"/db.dump.bin", []byte("partial"), time.Now())
			} else {
				if got.Status.PVCName != testBackupsPVC || got.Status.Path != "/"+b.Name || got.Status.JobName != jobName {
					t.Fatalf("lost cleanup coordinates: %+v", got.Status)
				}
				got.Status.StartTime = &past
			}
			if err := live.Status().Update(ctx, got); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if jobExists(t, live, backup.CleanupJobName(b)) {
				t.Fatal("cleanup ran while a live pod still uses the backup")
			}
			identityKey := client.ObjectKey{Namespace: ns, Name: backup.IdentitySecretName(b)}
			if upload {
				if err := live.Get(ctx, identityKey, &corev1.Secret{}); err != nil {
					t.Fatal("identity was released while an upload pod still runs")
				}
				if got := getBackup(t, live, b.Name); got.Status.Remote.Phase != forgejov1.RemoteBackupUploading {
					t.Fatalf("upload was settled before its pod stopped: %+v", got.Status.Remote)
				}
			}
			if err := live.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}
			passes := 1
			if upload {
				passes = 2
			}
			for range passes {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if upload {
				if got := getBackup(t, live, b.Name); got.Status.Remote.Phase != forgejov1.RemoteBackupFailed ||
					len(store.Keys("team/"+got.Status.Remote.Path+"/")) != 0 {
					t.Fatalf("lost Job was not settled at its original target: %+v", got.Status.Remote)
				}
				if err := live.Get(ctx, identityKey, &corev1.Secret{}); !apierrors.IsNotFound(err) {
					t.Fatalf("finished upload kept identity Secret: %v", err)
				}
			} else if !jobExists(t, live, backup.CleanupJobName(b)) {
				t.Fatalf("missing dump Job lost the ability to clean its files: %+v", getBackup(t, live, b.Name).Status)
			}
		})
	}
}

// interruptedExecutionWithOrphan loses the post-create phase patch, removes the Job but
// leaves a running pod, and changes the instance identity and remote configuration.
func interruptedExecutionWithOrphan(t *testing.T, name string, upload bool) (*backup.FakeS3, *forgejov1.ForgejoBackup, client.WithWatch, *corev1.Pod) {
	t.Helper()
	ctx := context.Background()
	s := backup.FakeScheme(t)
	store, f, objs := remoteFixture(t)
	b := queuedBackup(name, forgejov1.BackupReasonManual)
	jobName := backup.BackupJobName(b)
	if upload {
		b = uploadPending(name)
		jobName = backup.UploadJobName(b)
	}
	objs = append(objs, b, dbAppSecret("pw"), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: testBackupsPVC, Namespace: ns}})
	live := newFakeClient(s, objs...)
	lost := errors.New("lost execution status")
	flaky := interceptor.NewClient(live, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if got, ok := obj.(*forgejov1.ForgejoBackup); ok && (got.Status.Phase == forgejov1.BackupPhaseRunning ||
				got.Status.Remote != nil && got.Status.Remote.Phase == forgejov1.RemoteBackupUploading) {
				return lost
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}
	r := &controller.ForgejoBackupReconciler{Client: flaky, APIReader: live, Scheme: s}
	if _, err := r.Reconcile(ctx, req); !errors.Is(err, lost) {
		t.Fatalf("reconcile error %v", err)
	}
	job := &batchv1.Job{}
	if err := live.Get(ctx, client.ObjectKey{Namespace: ns, Name: jobName}, job); err != nil {
		t.Fatal(err)
	}
	if err := live.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	container := backup.ContainerArchive
	if upload {
		container = backup.ContainerUpload
	}
	pod := jobPod(jobName, container, 0, "")
	pod.Spec = job.Spec.Template.Spec
	maps.Copy(pod.Labels, job.Spec.Template.Labels)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	if err := live.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	sec := fbSecrets()
	if err := live.Get(ctx, client.ObjectKeyFromObject(sec), sec); err != nil {
		t.Fatal(err)
	}
	sec.Data["secret-key"] = []byte("replacement-identity")
	if err := live.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if err := live.Get(ctx, client.ObjectKeyFromObject(f), f); err != nil {
		t.Fatal(err)
	}
	f.Spec.Backup.Remote = nil
	if err := live.Update(ctx, f); err != nil {
		t.Fatal(err)
	}
	return store, b, live, pod
}

func TestInterruptedStartDeletionUsesLiveJob(t *testing.T) {
	s := backup.FakeScheme(t)
	f := instance("")
	b := queuedBackup("review-cache-delete", forgejov1.BackupReasonManual)
	live := newFakeClient(s, f, b, dbAppSecret("pw"), fbSecrets())
	lost := errors.New("lost running status")
	flaky := interceptor.NewClient(live, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if got, ok := obj.(*forgejov1.ForgejoBackup); ok && got.Status.Phase == forgejov1.BackupPhaseRunning {
				return lost
			}
			return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
		},
	})
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)}
	r := &controller.ForgejoBackupReconciler{Client: flaky, APIReader: live, Scheme: s}
	if _, err := r.Reconcile(context.Background(), req); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	if err := live.Delete(context.Background(), getBackup(t, live, b.Name)); err != nil {
		t.Fatal(err)
	}
	stale := interceptor.NewClient(live, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*batchv1.Job); ok && key.Name == backup.BackupJobName(b) {
				return apierrors.NewNotFound(batchv1.Resource("jobs"), key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})
	r = &controller.ForgejoBackupReconciler{Client: stale, APIReader: live, Scheme: s}
	for range 2 {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if jobExists(t, live, backup.BackupJobName(b)) && jobExists(t, live, backup.CleanupJobName(b)) {
		t.Fatal("cleanup Job was created while recovered dump Job is still running in the live API")
	}
}

// Prepared execution is already a write intent even when the phase patch is still Pending.
func TestRestoreWaitsForPreparedBackup(t *testing.T) {
	f, rs := scalingDown(t)
	b := queuedBackup("prepared", forgejov1.BackupReasonManual)
	b.Status.JobName = backup.BackupJobName(b)
	b.Status.PVCName = testBackupsPVC
	s := backup.FakeScheme(t)
	c := newFakeClient(s, f, rs, b, completedBackup("b1"))
	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseScalingDown || !strings.Contains(got.Status.Message, "Waiting for ForgejoBackup prepared") || jobExists(t, c, backup.RestoreJobName(rs)) {
		t.Fatalf("restore ignored a prepared execution: %s (%s)", got.Status.Phase, got.Status.Message)
	}
}

func TestBackupJobCollisionRetriesAfterPredecessorIsGone(t *testing.T) {
	for _, upload := range []bool{false, true} {
		name := testDumpExecution
		if upload {
			name = testUploadExecution
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := backup.FakeScheme(t)
			_, _, objs := remoteFixture(t)
			b := queuedBackup("reuse", forgejov1.BackupReasonManual)
			jobName := backup.BackupJobName(b)
			if upload {
				b = uploadPending("reuse")
				jobName = backup.UploadJobName(b)
			}
			old := b.DeepCopy()
			old.UID = testOldBackupUID
			j := finishedJob(t, s, jobName, old, batchv1.JobComplete)
			c := newFakeClient(s, append(objs, b, j, dbAppSecret("pw"))...)
			for range 3 {
				reconcileBackup(t, c, b.Name)
				got := getBackup(t, c, b.Name)
				if upload {
					if got.Status.Remote.Phase != forgejov1.RemoteBackupPending || got.Status.Remote.JobName != "" {
						t.Fatalf("refused upload was treated as an execution: %+v", got.Status.Remote)
					}
				} else if got.Status.Phase != forgejov1.BackupPhasePending || got.Status.JobName != "" || got.Status.Path != "" {
					t.Fatalf("refused dump was treated as an execution: %+v", got.Status)
				}
			}
			if err := c.Delete(ctx, j); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, c, b.Name)
			got := getBackup(t, c, b.Name)
			if upload && got.Status.Remote.Phase != forgejov1.RemoteBackupUploading || !upload && got.Status.Phase != forgejov1.BackupPhaseRunning {
				t.Fatalf("did not start after predecessor removal: %+v", got.Status)
			}
			job := &batchv1.Job{}
			if err := c.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: jobName}, job); err != nil {
				t.Fatal(err)
			}
			if !metav1.IsControlledBy(job, b) {
				t.Fatal("retry did not create its own Job")
			}
		})
	}
}

func TestRecoveryCollisionPreservesExecutionJournal(t *testing.T) {
	for _, upload := range []bool{false, true} {
		for _, transient := range []bool{false, true} {
			name := testDumpExecution
			if upload {
				name = testUploadExecution
			}
			if transient {
				name += "-transient"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				_, b, c, pod := interruptedExecutionWithOrphan(t, name, upload)
				before := getBackup(t, c, b.Name)
				jobName := before.Status.JobName
				if upload {
					jobName = before.Status.Remote.JobName
				}
				replacement := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns, UID: "replacement-job"}}
				if transient {
					old := b.DeepCopy()
					old.UID = testOldBackupUID
					replacement = finishedJob(t, backup.FakeScheme(t), jobName, old, batchv1.JobComplete)
				}
				if err := c.Create(ctx, replacement); err != nil {
					t.Fatal(err)
				}
				reconcileBackup(t, c, b.Name)
				got := getBackup(t, c, b.Name)
				if got.Status.JobName != before.Status.JobName || got.Status.Path != before.Status.Path || got.Status.PVCName != before.Status.PVCName {
					t.Fatalf("collision erased execution journal: %+v", got.Status)
				}
				if upload && (got.Status.Remote.JobName != jobName || got.Status.Remote.S3 == nil || got.Status.Remote.S3.Prefix != testRemotePrefix) {
					t.Fatalf("collision erased upload target: %+v", got.Status.Remote)
				}
				if transient && (upload && got.Status.Remote.Phase != forgejov1.RemoteBackupPending || !upload && got.Status.Phase != forgejov1.BackupPhasePending) {
					t.Fatalf("transient recovery conflict became terminal: %+v", got.Status)
				}
				if !transient {
					res, err := (&controller.ForgejoBackupReconciler{Client: c, APIReader: c, Scheme: backup.FakeScheme(t)}).Reconcile(ctx,
						ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
					if err != nil || res.RequeueAfter < time.Minute {
						t.Fatalf("cleanup beside the foreign Job = %+v, %v; want a slow recheck without an error", res, err)
					}
				}
				if jobExists(t, c, backup.CleanupJobName(b)) {
					t.Fatal("collision allowed file cleanup beside the original pod")
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
					t.Fatal("original pod was removed")
				}
			})
		}
	}
}

func TestRestoreWaitsForBackupExecutionAfterTerminalStatus(t *testing.T) {
	for _, removedCR := range []bool{false, true} {
		name := "failed-backup"
		if removedCR {
			name = "deleted-backup"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			_, b, live, pod := interruptedExecutionWithOrphan(t, name, false)
			reconcileBackup(t, live, b.Name)
			got := getBackup(t, live, b.Name)
			past := metav1.NewTime(time.Now().Add(-5 * time.Minute))
			got.Status.StartTime = &past
			if err := live.Status().Update(ctx, got); err != nil {
				t.Fatal(err)
			}
			reconcileBackup(t, live, b.Name)
			got = getBackup(t, live, b.Name)
			if got.Status.Phase != forgejov1.BackupPhaseFailed {
				t.Fatalf("missing Job did not fail backup: %+v", got.Status)
			}
			if removedCR {
				got.Finalizers = nil
				if err := live.Update(ctx, got); err != nil {
					t.Fatal(err)
				}
				if err := live.Delete(ctx, got); err != nil {
					t.Fatal(err)
				}
			}
			f, rs := scalingDown(t)
			if err := live.Get(ctx, client.ObjectKeyFromObject(f), f); err != nil {
				t.Fatal(err)
			}
			f.Status.Backup.ActiveRestore = rs.Name
			if err := live.Update(ctx, f); err != nil {
				t.Fatal(err)
			}
			no := false
			rs.Spec.PreRestoreBackup = &no
			for _, obj := range []client.Object{rs, completedBackup("b1")} {
				if err := live.Create(ctx, obj); err != nil {
					t.Fatal(err)
				}
			}
			stale := interceptor.NewClient(live, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if pods, ok := list.(*corev1.PodList); ok {
						pods.Items = nil
						return nil
					}
					return c.List(ctx, list, opts...)
				},
			})
			r := &controller.ForgejoRestoreReconciler{Client: stale, APIReader: live, Scheme: backup.FakeScheme(t)}
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(rs)}
			for range 2 {
				if _, err := r.Reconcile(ctx, req); err != nil {
					t.Fatal(err)
				}
			}
			if jobExists(t, live, backup.RestoreJobName(rs)) {
				t.Fatal("restore started while orphan dump pod still runs")
			}
			if err := live.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if !jobExists(t, live, backup.RestoreJobName(rs)) {
				t.Fatal("historical failed backup still blocked restore after its pod stopped")
			}
		})
	}
}
