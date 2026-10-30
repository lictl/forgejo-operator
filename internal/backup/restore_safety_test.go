package backup_test

// Restores that would lose data or identity are refused before anything is changed, and an
// in-place restore backs up what it replaces first. Driven through the real reconcilers
// against fake clients.

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testImageNewer = "codeberg.org/forgejo/forgejo:17"
	// testProdPath is a remote backup of instance prod/old.
	testProdPath = "prod/old/b1"
)

// fbSecrets is the identity Secret of instance "fb".
func fbSecrets() *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsName, Namespace: ns},
		Data: map[string][]byte{"secret-key": []byte("sk")}}
}

// restoreOf is a restore of backup b1 that has not started, the database too.
func restoreOf(name string) *forgejov1.ForgejoRestore {
	rs := newRestore(name, "")
	rs.UID = types.UID("3f2a9c1e-0000-4000-8000-" + strings.Repeat("0", 12-len(name)) + name)
	rs.Spec.BackupName = "b1"
	rs.Spec.RestoreDatabase = nil
	rs.Status.StartTime = nil
	return rs
}

// restoreOutcome reconciles a restore that has not started (finalizer, then validation) and
// returns its phase and the reason of its Ready condition.
func restoreOutcome(t *testing.T, objs ...client.Object) (*forgejov1.ForgejoRestore, string) {
	t.Helper()
	s := scheme(t)
	var name string
	for _, o := range objs {
		if rs, ok := o.(*forgejov1.ForgejoRestore); ok {
			name = rs.Name
		}
	}
	got := reconcileRestore(t, newFakeClient(s, objs...), s, name, 2)
	return got, reason(got.Status.Conditions)
}

// The database of a backup taken with another SECRET_KEY is not restored unless the restore
// allows it: its two-factor secrets, Actions secrets and mirror credentials would not decrypt.
// A backup that records no fingerprint, and a data-only restore, are not checked.
func TestRestoreRefusesAnotherIdentity(t *testing.T) {
	foreign := completedBackup("b1")
	foreign.Status.SecretKeyFingerprint = "0123456789abcdef"

	got, why := restoreOutcome(t, instance(""), fbSecrets(), foreign, restoreOf("r1"))
	if got.Status.Phase != forgejov1.RestorePhaseFailed || why != "IdentityMismatch" ||
		!strings.Contains(got.Status.Message, "allowIdentityMismatch") || !strings.Contains(got.Status.Message, "0123456789abcdef") {
		t.Fatalf("foreign identity: %s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
	if got.Status.StartTime != nil {
		t.Errorf("maintenance started for a refused restore")
	}

	allowed := restoreOf("r2")
	allowed.Spec.AllowIdentityMismatch = true
	dataOnly := restoreOf("r3")
	dataOnly.Spec.RestoreDatabase = ptr.To(false)
	own := completedBackup("b1")
	own.Status.SecretKeyFingerprint = forgejo.SecretKeyFingerprint(fbSecrets().Data)
	for _, tc := range []struct {
		name   string
		backup *forgejov1.ForgejoBackup
		rs     *forgejov1.ForgejoRestore
	}{
		{"allowed", foreign, allowed},
		{"data only", foreign, dataOnly},
		{"same identity", own, restoreOf("r4")},
		{"not recorded", completedBackup("b1"), restoreOf("r5")},
	} {
		if got, why := restoreOutcome(t, instance(""), fbSecrets(), tc.backup.DeepCopy(), tc.rs); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
			t.Errorf("%s: %s/%s (%s), want ScalingDown", tc.name, got.Status.Phase, why, got.Status.Message)
		}
	}
}

// The same holds for a remote backup, whose manifest records the fingerprint; the message
// points at spec.restoreFrom, which recreates that instance with its identity.
func TestRemoteRestoreRefusesAnotherIdentity(t *testing.T) {
	store, _, objs := remoteFixture(t)
	store.PutBackup(t, "team/", testProdPath, backup.RemoteManifest{Backup: "b1", Namespace: "prod", Forgejo: "old",
		Reason: forgejov1.BackupReasonManual, ForgejoImage: testImage16, SecretKeyFingerprint: "0123456789abcdef"},
		map[string][]byte{backup.DBDumpFile: []byte("PGDMP"), backup.IdentityFile: backup.FakeIdentity(t, "x")}, time.Now())
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: "r1-uid"}}
	rs.Spec.ForgejoRef.Name = "fb"
	rs.Spec.Remote = &forgejov1.RemoteBackupSource{S3: store.FakeConnection("team"), Path: testProdPath}

	got, why := restoreOutcome(t, append(objs, rs)...)
	if got.Status.Phase != forgejov1.RestorePhaseFailed || why != "IdentityMismatch" ||
		!strings.Contains(got.Status.Message, "prod/old") || !strings.Contains(got.Status.Message, "spec.restoreFrom") {
		t.Fatalf("%s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
	rs.Spec.AllowIdentityMismatch = true
	if got, why := restoreOutcome(t, append(objs, rs)...); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Errorf("allowed: %s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
}

// The database of a backup of a newer Forgejo is not restored: the deployed version would not
// start on it. The message says to upgrade first.
func TestRestoreRefusesABackupOfANewerForgejo(t *testing.T) {
	newer := completedBackup("b1")
	newer.Status.ForgejoImage = testImageNewer
	got, why := restoreOutcome(t, instance(""), newer, restoreOf("r1"))
	if got.Status.Phase != forgejov1.RestorePhaseFailed || why != "NewerBackup" ||
		!strings.Contains(got.Status.Message, "set spec.image of Forgejo fb to "+testImageNewer) {
		t.Fatalf("%s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
	// The data volume alone does not depend on the version.
	dataOnly := restoreOf("r2")
	dataOnly.Spec.RestoreDatabase = ptr.To(false)
	if got, why := restoreOutcome(t, instance(""), newer, dataOnly); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Errorf("data only: %s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
	// Once the instance runs the backup's version, it is restored.
	upgraded := instance("")
	upgraded.Status.Image = testImageNewer
	if got, why := restoreOutcome(t, upgraded, newer, restoreOf("r3")); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Errorf("upgraded: %s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
}

// A data archive larger than the data volume cannot be extracted onto it; the restore fails
// before it empties the volume.
func TestRestoreRefusesADataArchiveLargerThanTheVolume(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "fb-data", Namespace: ns}}
	pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("4Gi")}
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}
	big := completedBackup("b1")
	big.Status.DataSizeBytes = 2 << 30
	got, why := restoreOutcome(t, instance(""), pvc, big, restoreOf("r1"))
	if got.Status.Phase != forgejov1.RestorePhaseFailed || why != "DataVolumeTooSmall" || !strings.Contains(got.Status.Message, "1Gi") {
		t.Fatalf("%s/%s (%s)", got.Status.Phase, why, got.Status.Message)
	}
	dbOnly := restoreOf("r2")
	dbOnly.Spec.RestoreData = ptr.To(false)
	fits := completedBackup("b1")
	fits.Status.DataSizeBytes = 512 << 20
	for name, tc := range map[string]struct {
		b  *forgejov1.ForgejoBackup
		rs *forgejov1.ForgejoRestore
	}{"database only": {big, dbOnly}, "fits": {fits, restoreOf("r3")}} {
		if got, why := restoreOutcome(t, instance(""), pvc.DeepCopy(), tc.b.DeepCopy(), tc.rs); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
			t.Errorf("%s: %s/%s (%s)", name, got.Status.Phase, why, got.Status.Message)
		}
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
	s := scheme(t)
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
	s := scheme(t)
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
