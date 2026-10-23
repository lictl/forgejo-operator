package backup_test

// Uploads of completed backups and restores of remote backups, driven through the real
// ForgejoBackup and ForgejoRestore reconcilers against fake clients and a fake S3 store.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// fbKey is the backup key Secret of instance "fb".
const fbKey = "fb-backup-key"

// remoteFixture is instance "fb" with spec.backup.remote on a fake store, its identity
// Secrets, the credentials and the backup key.
func remoteFixture(t *testing.T) (*backup.FakeS3, *forgejov1.Forgejo, []client.Object) {
	t.Helper()
	store := backup.NewFakeS3(t)
	f := instance("")
	f.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{S3: store.FakeConnection("team")}
	fakeSecrets := backup.FakeSecrets(ns, fbKey)
	objs := make([]client.Object, 0, 3+len(fakeSecrets))
	objs = append(objs, f,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fb-secrets", Namespace: ns}, Data: map[string][]byte{"secret-key": []byte("sk")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fb-admin", Namespace: ns}, Data: map[string][]byte{"password": []byte("pw")}},
	)
	for _, s := range fakeSecrets {
		objs = append(objs, s)
	}
	return store, f, objs
}

// uploadPending is a completed backup whose upload has not started.
func uploadPending(name string) *forgejov1.ForgejoBackup {
	b := runningBackup(name)
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-time.Minute)}
	b.Status.ForgejoImage = "codeberg.org/forgejo/forgejo:16"
	path := backup.BackupRemotePath(b)
	b.Status.Remote = &forgejov1.RemoteBackupStatus{Phase: forgejov1.RemoteBackupPending, Path: path,
		Location: "s3://" + backup.FakeBucket + "/team/" + path + "/"}
	return b
}

// A backup that completes on the backup PVC is queued for upload; the upload does not hold
// up its completion.
func TestCompletedBackupIsQueuedForUpload(t *testing.T) {
	s := scheme(t)
	_, _, objs := remoteFixture(t)
	b := runningBackup("q1")
	job := finishedJob(t, s, b.Status.JobName, b, batchv1.JobComplete)
	pod := jobPod(job.Name, backup.ContainerArchive, 0, `{"databaseSizeBytes":1,"dataSizeBytes":2,"warnings":0}`)
	c := newFakeClient(s, append(objs, b, job, pod)...)

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseCompleted || got.Status.Remote == nil || got.Status.Remote.Phase != forgejov1.RemoteBackupPending ||
		got.Status.Remote.Path != "regression/fb/q1-q1uid" || got.Status.Remote.Location != "s3://backups/team/regression/fb/q1-q1uid/" {
		t.Fatalf("status %+v / remote %+v", got.Status, got.Status.Remote)
	}
}

func TestUploadRunsVerifiesAndReleasesItsJob(t *testing.T) {
	s := scheme(t)
	ctx := context.Background()
	_, _, objs := remoteFixture(t)
	b := uploadPending("u1")
	c := newFakeClient(s, append(objs, b)...)
	before := counterValue(t, forgejo.BackupUploadTotal.WithLabelValues(ns, "fb", "Manual", "succeeded"))

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if r := got.Status.Remote; r.Phase != forgejov1.RemoteBackupUploading || r.KeyFingerprint == "" || r.KeySecret == nil ||
		r.KeySecret.Name != fbKey || r.S3 == nil || r.S3.Bucket != backup.FakeBucket {
		t.Fatalf("remote %+v", got.Status.Remote)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: backup.UploadJobName(b)}, job); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(job, got) || job.Spec.Template.Spec.Containers[0].Name != backup.ContainerUpload {
		t.Errorf("job %+v", job.Spec.Template.Spec.Containers)
	}
	head := ""
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "MANIFEST_HEAD" {
			head = e.Value
		}
	}
	if !strings.Contains(head, `"backup":"u1"`) || !strings.Contains(head, `"secretKeyFingerprint":"`) || strings.Contains(head, `"sk"`) {
		t.Errorf("manifest head %s", head)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: backup.IdentitySecretName(b)}, sec); err != nil {
		t.Fatal(err)
	}
	id := &backup.Identity{}
	if err := json.Unmarshal(sec.Data[backup.IdentityFile], id); err != nil || string(id.Secret(backup.IdentityRoleSecrets).Data["secret-key"]) != "sk" ||
		!metav1.IsControlledBy(sec, got) || sec.Labels[forgejo.LabelManagedBy] != forgejo.ManagedBy {
		t.Errorf("identity Secret %+v (%v)", sec.ObjectMeta, err)
	}

	// The Job succeeds.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	job.Status.Succeeded = 1
	if err := c.Status().Update(ctx, job); err != nil {
		t.Fatal(err)
	}
	result := `{"files":[{"name":"db.dump","sizeBytes":5,"sha256":"` + strings.Repeat("a", 64) + `"}],"verified":true}`
	if err := c.Create(ctx, jobPod(job.Name, backup.ContainerUpload, 0, result)); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, c, b.Name)
	reconcileBackup(t, c, b.Name)

	got = getBackup(t, c, b.Name)
	if r := got.Status.Remote; r.Phase != forgejov1.RemoteBackupUploaded || !r.Verified || len(r.Files) != 1 || r.CompletionTime == nil {
		t.Fatalf("remote %+v", r)
	}
	if jobExists(t, c, job.Name) {
		t.Error("upload Job kept")
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: backup.IdentitySecretName(b)}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("identity Secret kept (%v)", err)
	}
	if after := counterValue(t, forgejo.BackupUploadTotal.WithLabelValues(ns, "fb", "Manual", "succeeded")); after != before+1 {
		t.Errorf("uploads_total{succeeded} %v -> %v", before, after)
	}
}

// uploading is a completed backup whose upload Job runs, writing to store.
func uploading(t *testing.T, store *backup.FakeS3, name string) *forgejov1.ForgejoBackup {
	t.Helper()
	b := uploadPending(name)
	conn := store.FakeConnection("team")
	b.Status.Remote.Phase = forgejov1.RemoteBackupUploading
	b.Status.Remote.JobName = backup.UploadJobName(b)
	b.Status.Remote.StartTime = &metav1.Time{Time: time.Now().Add(-5 * time.Minute)}
	b.Status.Remote.S3 = &conn
	b.Status.Remote.KeySecret = &forgejov1.BackupKeySecretRef{Name: fbKey, Key: forgejo.BackupKeySecretKey}
	return b
}

// A failed upload keeps the backup Completed (its copy on the PVC is fine), is counted, and
// removes what it left in the bucket: objects without a manifest in the backup's own directory.
func TestFailedUploadKeepsTheBackupAndPurgesTheBucket(t *testing.T) {
	s := scheme(t)
	store, f, objs := remoteFixture(t)
	b := uploading(t, store, "u2")
	job := finishedJob(t, s, b.Status.Remote.JobName, b, batchv1.JobFailed)
	pod := jobPod(job.Name, backup.ContainerUpload, 1, "uploading db.dump to s3://backups/team/regression/fb/u2/ failed: AccessDenied")
	dir := "team/" + b.Status.Remote.Path + "/"
	store.Put(dir+"db.dump.bin", []byte("partial"), time.Now())
	// A neighbour of the same name prefix is not touched.
	store.Put("team/regression/fb/u2-other/db.dump.bin", []byte("x"), time.Now())
	// The spec now points elsewhere: the upload is checked where it went.
	f.Spec.Backup.Remote.S3.Prefix = "moved"
	c := newFakeClient(s, append(objs, b, job, pod)...)
	before := counterValue(t, forgejo.BackupUploadTotal.WithLabelValues(ns, "fb", "Manual", "failed"))

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if got.Status.Phase != forgejov1.BackupPhaseCompleted || got.Status.Remote.Phase != forgejov1.RemoteBackupFailed ||
		!strings.Contains(got.Status.Remote.Message, "AccessDenied") || !strings.Contains(got.Status.Remote.Message, "removed the 1 objects") {
		t.Fatalf("status %s, remote %+v", got.Status.Phase, got.Status.Remote)
	}
	if after := counterValue(t, forgejo.BackupUploadTotal.WithLabelValues(ns, "fb", "Manual", "failed")); after != before+1 {
		t.Errorf("uploads_total{failed} %v -> %v", before, after)
	}
	if keys := store.Keys(dir); len(keys) != 0 {
		t.Errorf("objects of the failed upload kept: %v", keys)
	}
	if keys := store.Keys("team/regression/fb/u2-other/"); len(keys) != 1 {
		t.Errorf("another directory was touched: %v", keys)
	}
	if jobExists(t, c, job.Name) {
		t.Error("failed upload Job kept")
	}
}

// The bucket is only looked at once the upload's pods have stopped: a pod of a vanished Job
// may still write.
func TestFailedUploadWaitsForItsPods(t *testing.T) {
	s := scheme(t)
	store, _, objs := remoteFixture(t)
	b := uploading(t, store, "u4")
	pod := jobPod(b.Status.Remote.JobName, backup.ContainerUpload, 0, "")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	store.Put("team/"+b.Status.Remote.Path+"/db.dump.bin", []byte("partial"), time.Now())
	c := newFakeClient(s, append(objs, b, pod)...)

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if got.Status.Remote.Phase != forgejov1.RemoteBackupUploading || !strings.Contains(got.Status.Remote.Message, "waiting for its pod to stop") {
		t.Fatalf("remote %+v", got.Status.Remote)
	}
	if len(store.Keys("team/"+b.Status.Remote.Path+"/")) != 1 {
		t.Error("objects removed while a pod may still write")
	}
}

// An upload Job that vanished after it uploaded everything (its manifest is in the bucket)
// is recorded as uploaded, never purged; a manifest another upload wrote is left alone.
func TestVanishedUploadJobIsCheckedAgainstTheBucket(t *testing.T) {
	s := scheme(t)
	store, _, objs := remoteFixture(t)
	files := map[string][]byte{"db.dump": []byte("PGDMP"), "data.tar.gz": []byte("tar"), "identity.json": backup.FakeIdentity(t, "sk")}

	b := uploading(t, store, "u5")
	store.PutBackup(t, "team/", b.Status.Remote.Path, backup.RemoteManifest{Backup: "u5", BackupUID: string(b.UID), Namespace: ns,
		Forgejo: "fb", ForgejoUID: "fb-uid", Reason: forgejov1.BackupReasonManual, IncludesData: true}, files, time.Now())
	c := newFakeClient(s, append(objs, b)...)
	reconcileBackup(t, c, b.Name)
	got := getBackup(t, c, b.Name)
	if r := got.Status.Remote; r.Phase != forgejov1.RemoteBackupUploaded || !r.Verified || len(r.Files) != 3 ||
		!strings.Contains(r.Message, "disappeared") {
		t.Fatalf("own complete upload: %+v", r)
	}
	if len(store.Keys("team/"+b.Status.Remote.Path+"/")) != 4 {
		t.Error("a complete upload was deleted")
	}

	other := uploading(t, store, "u6")
	store.PutBackup(t, "team/", other.Status.Remote.Path, backup.RemoteManifest{Backup: "u6", BackupUID: "someone-else", Namespace: ns,
		Forgejo: "fb", ForgejoUID: "other-cluster", Reason: forgejov1.BackupReasonManual, IncludesData: true}, files, time.Now())
	c = newFakeClient(s, append(objs, other)...)
	reconcileBackup(t, c, other.Name)
	got = getBackup(t, c, other.Name)
	if r := got.Status.Remote; r.Phase != forgejov1.RemoteBackupFailed || !strings.Contains(r.Message, "did not write") {
		t.Fatalf("foreign manifest: %+v", r)
	}
	if len(store.Keys("team/"+other.Status.Remote.Path+"/")) != 4 {
		t.Error("another upload's backup was deleted")
	}
}

// Without a usable key nothing is uploaded, and the reason says why.
func TestUploadFailsWithoutAKey(t *testing.T) {
	s := scheme(t)
	_, _, objs := remoteFixture(t)
	var kept []client.Object
	for _, o := range objs {
		if o.GetName() != fbKey {
			kept = append(kept, o)
		}
	}
	b := uploadPending("u3")
	c := newFakeClient(s, append(kept, b)...)

	reconcileBackup(t, c, b.Name)

	got := getBackup(t, c, b.Name)
	if got.Status.Remote.Phase != forgejov1.RemoteBackupFailed || !strings.Contains(got.Status.Remote.Message, `"fb-backup-key" not found`) {
		t.Fatalf("remote %+v", got.Status.Remote)
	}
	if jobExists(t, c, backup.UploadJobName(b)) {
		t.Error("an upload Job was created without a key")
	}
}

// A restore of a remote backup needs no ForgejoBackup: it reads the manifest from the bucket,
// and its Job downloads and checks the files before the usual steps.
func TestRestoreOfARemoteBackup(t *testing.T) {
	s := scheme(t)
	ctx := context.Background()
	store, f, objs := remoteFixture(t)
	f.Status.Backup.ActiveRestore = "r1"
	store.PutBackup(t, "team/", "prod/old/b1", backup.RemoteManifest{Backup: "b1", Namespace: "prod", Forgejo: "old",
		Reason: forgejov1.BackupReasonManual, ForgejoImage: "codeberg.org/forgejo/forgejo:16", IncludesData: true,
		SecretKeyFingerprint: "different"},
		map[string][]byte{"db.dump": []byte("PGDMP"), "data.tar.gz": []byte("tar"), "identity.json": backup.FakeIdentity(t, "x")}, time.Now())
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: "r1-uid"}}
	rs.Spec.ForgejoRef.Name = "fb"
	rs.Spec.Remote = &forgejov1.RemoteBackupSource{S3: store.FakeConnection("team"), Path: "prod/old/b1"}
	rs.Spec.RestoreDatabase = func() *bool { v := false; return &v }() // no database in the fake environment
	c := newFakeClient(s, append(objs, rs)...)

	got := reconcileRestore(t, c, s, rs.Name, 2)
	if got.Status.Phase != forgejov1.RestorePhaseScalingDown || got.Status.Remote == nil || got.Status.Remote.BackupName != "b1" ||
		!got.Status.Remote.IncludesData || len(got.Status.Remote.Files) != 3 {
		t.Fatalf("status %+v", got.Status)
	}
	got = reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhaseRestoring {
		t.Fatalf("phase %s: %s", got.Status.Phase, got.Status.Message)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: backup.RestoreJobName(rs)}, job); err != nil {
		t.Fatal(err)
	}
	init := job.Spec.Template.Spec.InitContainers
	if len(init) < 2 || init[0].Name != backup.ContainerDownload || job.Spec.Template.Spec.Containers[0].Name != backup.ContainerRestoreData {
		t.Errorf("steps %v", job.Spec.Template.Spec.InitContainers)
	}

	// A wrong key fails the restore before maintenance mode.
	bad := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r2", Namespace: ns, UID: "r2-uid"}}
	bad.Spec.ForgejoRef.Name = "fb"
	bad.Spec.Remote = &forgejov1.RemoteBackupSource{S3: store.FakeConnection("team"), Path: "prod/old/b1",
		KeySecret: &forgejov1.BackupKeySecretRef{Name: "wrong", Key: "key"}}
	wrong := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "wrong", Namespace: ns}, Data: map[string][]byte{"key": []byte("a-different-backup-key")}}
	c2 := newFakeClient(s, append(objs, bad, wrong)...)
	got = reconcileRestore(t, c2, s, bad.Name, 2)
	if got.Status.Phase != forgejov1.RestorePhaseFailed || !strings.Contains(got.Status.Message, "wrong key") {
		t.Errorf("wrong key: %s %s", got.Status.Phase, got.Status.Message)
	}
	// A path without a complete backup fails too.
	missing := bad.DeepCopy()
	missing.Name, missing.UID, missing.ResourceVersion = "r3", "r3-uid", ""
	missing.Spec.Remote.KeySecret = nil
	missing.Spec.Remote.Path = "prod/old/nothing"
	c3 := newFakeClient(s, append(objs, missing)...)
	if got := reconcileRestore(t, c3, s, missing.Name, 2); got.Status.Phase != forgejov1.RestorePhaseFailed ||
		!strings.Contains(got.Status.Message, "no complete backup") {
		t.Errorf("missing: %s %s", got.Status.Phase, got.Status.Message)
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
