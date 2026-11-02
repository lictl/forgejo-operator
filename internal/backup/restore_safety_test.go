package backup_test

// Restores that would lose data or identity are refused before anything is changed, a restore
// waits while it could leave Forgejo unable to start again or another restore may still write,
// and an in-place restore backs up what it replaces first. Driven through the real reconcilers
// against fake clients.

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// otherIdentity is the SECRET_KEY fingerprint of another instance.
var otherIdentity = forgejo.SecretKeyFingerprint(map[string][]byte{forgejo.SecretKeySecretKey: []byte("another instance")})

const (
	testImageNewer = "codeberg.org/forgejo/forgejo:17"
	// testProdPath is a remote backup of instance prod/old.
	testProdPath = "prod/old/b1"
)

// unstartedRestore is restore r1 of backup b1 that has not started, the database too.
func unstartedRestore() *forgejov1.ForgejoRestore {
	rs := newRestore("r1", "")
	rs.UID = "3f2a9c1e-0000-4000-8000-0000000000r1"
	rs.Spec.BackupName = "b1"
	rs.Spec.RestoreDatabase = nil
	rs.Status.StartTime = nil
	return rs
}

// restoreOutcome reconciles a restore that has not started (finalizer, then validation) in a
// fake client holding copies of objs, and returns it and the reason of its Ready condition.
func restoreOutcome(t *testing.T, objs ...client.Object) (*forgejov1.ForgejoRestore, string) {
	t.Helper()
	s := backup.FakeScheme(t)
	var name string
	copies := make([]client.Object, 0, len(objs))
	for _, o := range objs {
		if rs, ok := o.(*forgejov1.ForgejoRestore); ok {
			name = rs.Name
		}
		copies = append(copies, o.DeepCopyObject().(client.Object))
	}
	got := reconcileRestore(t, newFakeClient(s, copies...), s, name, 2)
	return got, reason(got.Status.Conditions)
}

// A restore that would lose data or identity fails before maintenance mode starts:
//   - the database of a backup taken with another SECRET_KEY, unless the restore allows it: its
//     two-factor secrets, Actions secrets and mirror credentials would not decrypt. A backup
//     that records no fingerprint, and a data-only restore, are not checked. A remote backup's
//     manifest records the fingerprint; the message points at spec.restoreFrom, which recreates
//     that instance with its identity;
//   - the database of a backup of a newer Forgejo: the deployed version would not start on it.
//     The message says to upgrade first. The data volume alone does not depend on the version;
//   - a dump of a newer PostgreSQL major than the server's;
//   - a data archive larger than the data volume, which cannot be extracted onto it: the
//     restore fails before it empties the volume.
func TestRestoreRefusesToLoseDataOrIdentity(t *testing.T) {
	foreign := completedBackup("b1")
	foreign.Status.SecretKeyFingerprint = otherIdentity
	own := completedBackup("b1")
	own.Status.SecretKeyFingerprint = forgejo.SecretKeyFingerprint(fbSecrets().Data)
	newer := completedBackup("b1")
	newer.Status.ForgejoImage = testImageNewer
	upgraded := instance("")
	upgraded.Status.Image = testImageNewer
	pg17 := instance("")
	pg17.Status.Database = &forgejov1.DatabaseStatus{Type: forgejov1.DatabaseTypeCNPG, Image: testPG17, MajorVersion: 17}
	pg18Dump := completedBackup("b1")
	pg18Dump.Status.PostgresImage = testPG18
	dataPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fb-data", Namespace: ns}}
	dataPVC.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}
	dataPVC.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	big := completedBackup("b1")
	big.Status.DataSizeBytes = 2 << 30
	fits := completedBackup("b1")
	fits.Status.DataSizeBytes = 512 << 20
	allowed := unstartedRestore()
	allowed.Spec.AllowIdentityMismatch = true
	dataOnly := unstartedRestore()
	dataOnly.Spec.RestoreDatabase = ptr.To(false)
	dbOnly := unstartedRestore()
	dbOnly.Spec.RestoreData = ptr.To(false)

	store, _, remote := remoteFixture(t)
	store.PutBackup(t, "team/", testProdPath, backup.RemoteManifest{Backup: "b1", Namespace: "prod", Forgejo: "old",
		Reason: forgejov1.BackupReasonManual, ForgejoImage: testImage16, SecretKeyFingerprint: otherIdentity},
		map[string][]byte{backup.DBDumpFile: []byte("PGDMP"), backup.IdentityFile: backup.FakeIdentity(t, "x")}, time.Now())
	fromRemote := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: "r1-uid"}}
	fromRemote.Spec.ForgejoRef.Name = "fb"
	fromRemote.Spec.Remote = &forgejov1.RemoteBackupSource{S3: store.FakeConnection("team"), Path: testProdPath}
	fromRemoteAllowed := fromRemote.DeepCopy()
	fromRemoteAllowed.Spec.AllowIdentityMismatch = true

	for _, tc := range []struct {
		name   string
		objs   []client.Object
		reason string   // "" when the restore goes ahead
		msg    []string // in the message of a refusal
	}{
		{name: "another identity", objs: []client.Object{instance(""), fbSecrets(), foreign, unstartedRestore()},
			reason: "IdentityMismatch", msg: []string{"allowIdentityMismatch", otherIdentity}},
		{name: "another identity, allowed", objs: []client.Object{instance(""), fbSecrets(), foreign, allowed}},
		{name: "another identity, data only", objs: []client.Object{instance(""), fbSecrets(), foreign, dataOnly}},
		{name: "same identity", objs: []client.Object{instance(""), fbSecrets(), own, unstartedRestore()}},
		{name: "identity not recorded", objs: []client.Object{instance(""), fbSecrets(), completedBackup("b1"), unstartedRestore()}},
		{name: "remote backup of another identity", objs: append(slices.Clip(remote), fromRemote),
			reason: "IdentityMismatch", msg: []string{"prod/old", "spec.restoreFrom"}},
		{name: "remote backup of another identity, allowed", objs: append(slices.Clip(remote), fromRemoteAllowed)},
		{name: "newer Forgejo", objs: []client.Object{instance(""), newer, unstartedRestore()},
			reason: "NewerBackup", msg: []string{"set spec.image of Forgejo fb to " + testImageNewer}},
		{name: "newer Forgejo, data only", objs: []client.Object{instance(""), newer, dataOnly}},
		{name: "newer Forgejo, instance upgraded", objs: []client.Object{upgraded, newer, unstartedRestore()}},
		{name: "dump of a newer PostgreSQL", objs: []client.Object{pg17, pg18Dump, unstartedRestore()}, reason: "NewerDump"},
		{name: "archive larger than the data volume", objs: []client.Object{instance(""), dataPVC, big, unstartedRestore()},
			reason: "DataVolumeTooSmall", msg: []string{"1Gi"}},
		{name: "larger archive, database only", objs: []client.Object{instance(""), dataPVC, big, dbOnly}},
		{name: "archive fits", objs: []client.Object{instance(""), dataPVC, fits, unstartedRestore()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := restoreOutcome(t, tc.objs...)
			if tc.reason == "" {
				if got.Status.Phase != forgejov1.RestorePhaseScalingDown {
					t.Fatalf("%s/%s (%s), want ScalingDown", got.Status.Phase, why, got.Status.Message)
				}
				return
			}
			if got.Status.Phase != forgejov1.RestorePhaseFailed || why != tc.reason {
				t.Fatalf("%s/%s (%s), want Failed/%s", got.Status.Phase, why, got.Status.Message, tc.reason)
			}
			for _, want := range tc.msg {
				if !strings.Contains(got.Status.Message, want) {
					t.Errorf("message %q lacks %q", got.Status.Message, want)
				}
			}
			if got.Status.StartTime != nil {
				t.Errorf("maintenance started for a refused restore")
			}
		})
	}
}

// A restore waits while a PostgreSQL major upgrade runs, and does not scale Forgejo down while
// a Secret its pods need is missing or unusable (the SSH host keys, those of
// spec.objectStorage): the Forgejo pipeline stops before the workload step then (no
// scale-down), and Forgejo could not start again afterwards. It waits as Pending, which is no
// maintenance. An unreachable bucket does not hold it.
func TestRestoreWaitsBeforeScalingDown(t *testing.T) {
	s := backup.FakeScheme(t)
	storageReady := func(f *forgejov1.Forgejo, reason, msg string) {
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionObjectStorageReady,
			Status: metav1.ConditionFalse, Reason: reason, Message: msg})
	}
	for _, tc := range []struct {
		name     string
		instance func() *forgejov1.Forgejo
		release  func(*forgejov1.Forgejo)
		want     string
	}{{
		name:     "PostgreSQL major upgrade",
		instance: func() *forgejov1.Forgejo { return upgradingInstance(forgejov1.DatabaseUpgradeUpgrading) },
		release:  func(f *forgejov1.Forgejo) { f.Status.Database.Upgrade.Phase = forgejov1.DatabaseUpgradeSucceeded },
		want:     "Waiting for the PostgreSQL major upgrade to " + testPG18 + " to finish (phase Upgrading)",
	}, {
		name: "SSH host keys",
		instance: func() *forgejov1.Forgejo {
			f := instance("")
			f.Status.SSH = &forgejov1.SSHStatus{HostKeysSecretName: "fb-ssh-host-keys", Message: "Secret fb-ssh-host-keys with the SSH host keys the pods serve is missing"}
			return f
		},
		release: func(f *forgejov1.Forgejo) { f.Status.SSH.Message = "" },
		want:    "Waiting for the SSH host keys: Secret fb-ssh-host-keys with the SSH host keys the pods serve is missing",
	}, {
		name: "object storage Secrets",
		instance: func() *forgejov1.Forgejo {
			f := instance("")
			f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{}
			storageReady(f, "CredentialsUnavailable", `secret "s3" not found`)
			return f
		},
		release: func(f *forgejov1.Forgejo) { storageReady(f, "UnreachableFromOperator", "dial tcp: i/o timeout") },
		want:    `Waiting for spec.objectStorage: secret "s3" not found`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newRestore("r1", "")
			rs.Finalizers = []string{forgejo.FinalizerName}
			rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
			rs.Spec.BackupName = "b1"
			f := tc.instance()
			c := newFakeClient(s, f, completedBackup("b1"), rs)

			got := reconcileRestore(t, c, s, rs.Name, 1)
			if got.Status.Phase != forgejov1.RestorePhasePending || got.Status.Message != tc.want {
				t.Fatalf("phase %s (%s), want Pending: %s", got.Status.Phase, got.Status.Message, tc.want)
			}
			if active, _ := backup.MaintenanceRestore([]forgejov1.ForgejoRestore{*got}); active != nil {
				t.Fatal("a waiting restore puts the instance into maintenance")
			}
			tc.release(f)
			if err := c.Update(context.Background(), f); err != nil {
				t.Fatal(err)
			}
			if got = reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
				t.Fatalf("phase %s (%s), want ScalingDown once released", got.Status.Phase, got.Status.Message)
			}
		})
	}
}

// A restore does not start its Job while a pod of an earlier restore of the instance may
// still be writing.
func TestRestoreWaitsForOtherRestorePods(t *testing.T) {
	s := backup.FakeScheme(t)
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
	if got := getRestore(t, c, rs.Name); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
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

// scalingDown is restore r1 of backup b1 (data only) into instance "fb", which has run and
// is in maintenance for it with no pod left.
func scalingDown(t *testing.T) (*forgejov1.Forgejo, *forgejov1.ForgejoRestore) {
	t.Helper()
	f := instance("r1")
	f.Status.Image = testImage16
	rs := newRestore("r1", forgejov1.RestorePhaseScalingDown)
	rs.UID = "3f2a9c1e-5b7d-4c11-9e2f-000000000001"
	rs.Spec.BackupName = "b1"
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	return f, rs
}

// An in-place restore of an instance that has run first takes a backup of what it replaces,
// with Forgejo stopped; that backup does not wait for the restore (which waits for it), and
// the restore Job starts only once it has completed.
func TestRestoreTakesAPreRestoreBackupFirst(t *testing.T) {
	ctx := context.Background()
	s := backup.FakeScheme(t)
	f, rs := scalingDown(t)
	waiting := newRestore("r2", forgejov1.RestorePhasePending)
	waiting.UID = "r2-uid"
	c := newFakeClient(s, f, fbSecrets(), dbAppSecret("pw"), completedBackup("b1"), rs, waiting)

	name := backup.PreRestoreBackupName(rs)
	if name != "r1-pre-restore-3f2a9c1e" {
		t.Fatalf("name %q", name)
	}
	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseScalingDown || got.Status.PreRestoreBackupName != name ||
		!strings.HasPrefix(got.Status.Message, "Taking pre-restore backup "+name) || jobExists(t, c, backup.RestoreJobName(rs)) {
		t.Fatalf("restore: %s (%s), backup %q", got.Status.Phase, got.Status.Message, got.Status.PreRestoreBackupName)
	}
	pre := getBackup(t, c, name)
	if backup.Reason(pre) != forgejov1.BackupReasonPreRestore || pre.Spec.IncludeData == nil || !*pre.Spec.IncludeData ||
		pre.Labels[forgejo.LabelBackupReason] != string(forgejov1.BackupReasonPreRestore) || len(pre.OwnerReferences) != 0 {
		t.Fatalf("pre-restore backup %+v", pre)
	}

	// The backup runs while the restore holds Forgejo stopped, and records the identity.
	reconcileBackup(t, c, name) // finalizer and labels
	reconcileBackup(t, c, name)
	pre = getBackup(t, c, name)
	if pre.Status.Phase != forgejov1.BackupPhaseRunning || pre.Status.SecretKeyFingerprint != forgejo.SecretKeyFingerprint(fbSecrets().Data) {
		t.Fatalf("pre-restore backup: %s (%s), identity %q", pre.Status.Phase, pre.Status.Message, pre.Status.SecretKeyFingerprint)
	}
	if got = reconcileRestore(t, c, s, rs.Name, 1); !strings.HasPrefix(got.Status.Message, "Taking pre-restore backup "+name) {
		t.Fatalf("restore while the backup runs: %s", got.Status.Message)
	}

	pre.Status.Phase = forgejov1.BackupPhaseCompleted
	if err := c.Status().Update(ctx, pre); err != nil {
		t.Fatal(err)
	}
	got = reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseScalingDown || jobExists(t, c, backup.RestoreJobName(rs)) {
		t.Fatal("completed backup status bypassed its still-running Job")
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pre.Status.JobName}, job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}

	got = reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseRestoring || !jobExists(t, c, backup.RestoreJobName(rs)) {
		t.Fatalf("after the backup: %s (%s)", got.Status.Phase, got.Status.Message)
	}

	// Under retainPolicy Delete the backup belongs to the instance, like a pre-upgrade backup.
	f, rs = scalingDown(t)
	f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete
	c = newFakeClient(s, f, completedBackup("b1"), rs)
	reconcileRestore(t, c, s, rs.Name, 1)
	if refs := getBackup(t, c, name).OwnerReferences; len(refs) != 1 || refs[0].UID != f.UID {
		t.Errorf("owner references %v", refs)
	}
}

// A pre-restore backup that fails fails the restore, which has changed nothing: Forgejo
// starts again. spec.preRestoreBackup false, an instance that has never run and a Broken one
// take none.
func TestPreRestoreBackupFailureAndOptOut(t *testing.T) {
	s := backup.FakeScheme(t)
	f, rs := scalingDown(t)
	failed := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: backup.PreRestoreBackupName(rs), Namespace: ns}}
	failed.Spec.ForgejoRef.Name = "fb"
	failed.Spec.Reason = forgejov1.BackupReasonPreRestore
	failed.Status.Phase = forgejov1.BackupPhaseFailed
	failed.Status.Message = "Backup job failed: disk full"
	c := newFakeClient(s, f, completedBackup("b1"), rs, failed)
	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseFailed || reason(got.Status.Conditions) != "PreRestoreBackupFailed" ||
		!strings.Contains(got.Status.Message, "disk full") || !strings.Contains(got.Status.Message, "spec.preRestoreBackup: false") {
		t.Fatalf("%s/%s (%s)", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
	}
	if meta.FindStatusCondition(got.Status.Conditions, backup.ConditionDataModified) != nil || backup.HoldsMaintenance(got) ||
		jobExists(t, c, backup.RestoreJobName(rs)) {
		t.Errorf("a restore that changed nothing holds the instance: %+v", got.Status.Conditions)
	}

	f, rs = scalingDown(t)
	rs.Spec.PreRestoreBackup = ptr.To(false)
	neverRan, rs2 := scalingDown(t)
	neverRan.Status.Image = ""
	broken, rs3 := scalingDown(t)
	meta.SetStatusCondition(&broken.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionBroken, Status: metav1.ConditionTrue,
		Reason: forgejo.BrokenDatabaseRestorePending, Message: "empty database"})
	for name, objs := range map[string][]client.Object{
		"opted out": {f, completedBackup("b1"), rs},
		"never ran": {neverRan, completedBackup("b1"), rs2},
		"broken":    {broken, completedBackup("b1"), rs3},
	} {
		c := newFakeClient(s, objs...)
		if got := reconcileRestore(t, c, s, "r1", 1); got.Status.Phase != forgejov1.RestorePhaseRestoring || got.Status.PreRestoreBackupName != "" {
			t.Errorf("%s: %s (%s)", name, got.Status.Phase, got.Status.Message)
		}
		if list := (&forgejov1.ForgejoBackupList{}); c.List(context.Background(), list) != nil || len(list.Items) != 1 {
			t.Errorf("%s: backups %d", name, len(list.Items))
		}
	}
}

// A pre-restore backup that was started is followed to its end even when none would be taken
// now (the instance became Broken while it ran): the restore Job must not write what the backup
// still reads. A recorded one that is gone fails the restore instead of being taken again.
func TestStartedPreRestoreBackupIsAwaited(t *testing.T) {
	ctx := context.Background()
	s := backup.FakeScheme(t)
	f, rs := scalingDown(t)
	c := newFakeClient(s, f, fbSecrets(), dbAppSecret("pw"), completedBackup("b1"), rs)
	name := backup.PreRestoreBackupName(rs)
	if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.PreRestoreBackupName != name {
		t.Fatalf("restore: %s (%s)", got.Status.Phase, got.Status.Message)
	}
	live := &forgejov1.Forgejo{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(f), live); err != nil {
		t.Fatal(err)
	}
	meta.SetStatusCondition(&live.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionBroken, Status: metav1.ConditionTrue,
		Reason: forgejo.BrokenDatabaseRestorePending, Message: "empty database"})
	if err := c.Update(ctx, live); err != nil { // no status subresource for Forgejo in this fake client
		t.Fatal(err)
	}
	pre := getBackup(t, c, name)
	pre.Status.Phase = forgejov1.BackupPhaseRunning
	if err := c.Status().Update(ctx, pre); err != nil {
		t.Fatal(err)
	}
	if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingDown ||
		jobExists(t, c, backup.RestoreJobName(rs)) || !strings.HasPrefix(got.Status.Message, "Taking pre-restore backup "+name) {
		t.Fatalf("restore while the backup runs on a Broken instance: %s (%s)", got.Status.Phase, got.Status.Message)
	}

	if err := c.Delete(ctx, getBackup(t, c, name)); err != nil {
		t.Fatal(err)
	}
	pre = &forgejov1.ForgejoBackup{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, pre); err == nil && len(pre.Finalizers) > 0 {
		pre.Finalizers = nil
		if err := c.Update(ctx, pre); err != nil {
			t.Fatal(err)
		}
	}
	if got := reconcileRestore(t, c, s, rs.Name, 1); got.Status.Phase != forgejov1.RestorePhaseFailed ||
		reason(got.Status.Conditions) != "PreRestoreBackupFailed" || !strings.Contains(got.Status.Message, "was deleted") {
		t.Fatalf("recorded backup gone: %s/%s (%s)", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
	}
}

// After the pre-restore backup, the scale-down timeout counts from its completion: a backup that
// outlasted the timeout does not fail the restore, a scale-down that stays stuck after it does.
func TestScaleDownTimeoutAfterThePreRestoreBackup(t *testing.T) {
	s := backup.FakeScheme(t)
	for _, tc := range []struct {
		name   string
		backup func(*forgejov1.ForgejoBackup)
		failed bool
	}{
		{"backup running", func(b *forgejov1.ForgejoBackup) { b.Status.Phase = forgejov1.BackupPhaseRunning }, false},
		{"backup just completed", func(b *forgejov1.ForgejoBackup) {
			b.Status.Phase, b.Status.CompletionTime = forgejov1.BackupPhaseCompleted, &metav1.Time{Time: time.Now().Add(-time.Minute)}
		}, false},
		{"stuck long after the backup", func(b *forgejov1.ForgejoBackup) {
			b.Status.Phase, b.Status.CompletionTime = forgejov1.BackupPhaseCompleted, &metav1.Time{Time: time.Now().Add(-20 * time.Minute)}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, rs := scalingDown(t)
			f.Status.Backup.ActiveRestore = "" // not in maintenance: the scale-down waits
			rs.Status.StartTime = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
			rs.Status.PreRestoreBackupName = backup.PreRestoreBackupName(rs)
			pre := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: rs.Status.PreRestoreBackupName, Namespace: ns}}
			pre.Spec.ForgejoRef.Name, pre.Spec.Reason = "fb", forgejov1.BackupReasonPreRestore
			tc.backup(pre)
			got := reconcileRestore(t, newFakeClient(s, f, completedBackup("b1"), rs, pre), s, rs.Name, 1)
			if failed := got.Status.Phase == forgejov1.RestorePhaseFailed && reason(got.Status.Conditions) == "ScaleDownTimeout"; failed != tc.failed {
				t.Fatalf("%s/%s (%s)", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
			}
		})
	}
}

// spec.replicas: 0 does not mean "no pods" for an instance whose HPA owns the replica count:
// the restore waits for Forgejo to become available instead of completing right away.
func TestRestoreScaleUpWaitsForHPAManagedInstance(t *testing.T) {
	s := backup.FakeScheme(t)
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
