package backup

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/rclone"
	"github.com/lictl/forgejo-operator/internal/s3"
)

const (
	testImage17           = "codeberg.org/forgejo/forgejo:17"
	testBackupKeySecret   = "fj-backup-key"
	testOwnSecret         = "mine"
	testOldName           = "old"
	testSecretsSecret     = "fj-secrets"
	testSSHHostKeysSecret = "fj-ssh-host-keys"
	testRemotePath        = "dr/old/b1"
	testRemoteLocation    = "s3://backups/team/dr/old/b1/"
	testDataPVC           = "fj-data"
	testScheduledBackup   = "fj-scheduled-1"
)

const remoteNS = "dr"

func remoteInstance(store *FakeS3) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: remoteNS, UID: "fj-uid"}}
	f.Spec.Image = testImage17
	conn := forgejov1.S3Connection{Endpoint: "http://garage:3900", Bucket: FakeBucket, CredentialsSecret: forgejov1.S3CredentialsSecretRef{Name: testCredsSecret}}
	if store != nil {
		conn = store.FakeConnection("team")
	}
	f.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{S3: conn}
	return f
}

func secretObjects(secrets []*corev1.Secret) []client.Object {
	out := make([]client.Object, 0, len(secrets))
	for _, s := range secrets {
		out = append(out, s)
	}
	return out
}

func TestRemoteNamesAndKeys(t *testing.T) {
	f := remoteInstance(nil)
	if got := BackupKeyRef(f); got.Name != testBackupKeySecret || got.Key != testKeyField {
		t.Errorf("default key: %+v", got)
	}
	f.Spec.Backup.Remote.Encryption.KeySecret = &forgejov1.BackupKeySecretRef{Name: testOwnSecret}
	if got := BackupKeyRef(f); got.Name != testOwnSecret || got.Key != testKeyField {
		t.Errorf("own key: %+v", got)
	}
	src := &forgejov1.RemoteBackupSource{Path: "a/b/c"}
	if got := SourceKeyRef(f, src); got.Name != testOwnSecret {
		t.Errorf("source without a key uses the instance's: %+v", got)
	}
	src.KeySecret = &forgejov1.BackupKeySecretRef{Name: testOldName, Key: "k"}
	if got := SourceKeyRef(f, src); got.Name != testOldName || got.Key != "k" {
		t.Errorf("source key: %+v", got)
	}
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "prod", UID: "0F8E2C4A-91B2-4c3d-8e5f-0123456789ab"}}
	b.Spec.ForgejoRef.Name = "git"
	// Each ForgejoBackup has a directory of its own: a backup of the same name created again,
	// or taken by an instance of the same namespace and name elsewhere, gets another one.
	if got := BackupRemotePath(b); got != "prod/git/nightly-0f8e2c4a" {
		t.Errorf("path %q", got)
	}
	again := b.DeepCopy()
	again.UID = "7d1e0b3a-0000-4000-8000-000000000000"
	if BackupRemotePath(again) == BackupRemotePath(b) {
		t.Error("two ForgejoBackups of the same name share a remote directory")
	}
	c := &forgejov1.S3Connection{Bucket: "bk", Prefix: "/forgejo/"}
	if got := RemoteLocation(c, "prod/git/nightly"); got != "s3://bk/forgejo/prod/git/nightly/" {
		t.Errorf("location %q", got)
	}
	if RcloneImage(f) != forgejov1.DefaultRcloneImage {
		t.Errorf("image %q", RcloneImage(f))
	}
}

// The upload Job completes the head the operator renders; the result is the manifest the
// operator reads back.
func TestManifestHeadCompletesToValidManifest(t *testing.T) {
	head, err := ManifestHead(RemoteManifest{Backup: "b1", Namespace: "ns", Forgejo: "fj", Reason: forgejov1.BackupReasonScheduled,
		ForgejoImage: testImage17, IncludesData: true, CompletedAt: "2026-10-22T03:00:00Z", KeyFingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(head, "}") || strings.Contains(head, "files") {
		t.Fatalf("head %s", head)
	}
	sum := strings.Repeat("ab", 32)
	files := `{"name":"db.dump","sizeBytes":1,"sha256":"` + sum + `"},{"name":"data.tar.gz","sizeBytes":2,"sha256":"` + sum +
		`"},{"name":"identity.json","sizeBytes":3,"sha256":"` + sum + `"}`
	full := head + `,"uploadedAt":"2026-10-22T03:01:00Z","files":[` + files + `],"local":{"formatVersion": 1, "data": {"included": true}}}`
	m, err := ParseRemoteManifest([]byte(full))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	if m.Reason != forgejov1.BackupReasonScheduled || m.File("data.tar.gz").SizeBytes != 2 || m.CompletedTime().IsZero() || len(m.Local) == 0 {
		t.Errorf("parsed %+v", m)
	}
	// A broken local manifest does not make the backup unreadable.
	broken := head + `,"uploadedAt":"x","files":[` + files + `],"local":{"data": oops}}`
	if m, err := ParseRemoteManifest([]byte(broken)); err != nil || m.Validate() != nil {
		t.Errorf("broken local manifest: %v", err)
	}
	// Without the data archive's checksum, a manifest that says it has one is refused.
	noData := strings.Replace(full, `{"name":"data.tar.gz","sizeBytes":2,"sha256":"`+sum+`"},`, "", 1)
	if m, _ := ParseRemoteManifest([]byte(noData)); m.Validate() == nil {
		t.Error("a manifest without the data archive's checksum validated")
	}
	if m, _ := ParseRemoteManifest([]byte(`{"formatVersion":2,"kind":"ForgejoRemoteBackup"}`)); m.Validate() == nil {
		t.Error("an unknown format version validated")
	}
}

func TestStoreReadsAndDeletesRemoteBackups(t *testing.T) {
	store := NewFakeS3(t)
	f := remoteInstance(store)
	secrets := FakeSecrets(remoteNS, "fj-backup-key")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secretObjects(secrets)...).Build()
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)
	store.PutBackup(t, "team/", "dr/fj/b1", RemoteManifest{Backup: "b1", Namespace: "dr", Forgejo: "fj", Reason: forgejov1.BackupReasonManual},
		map[string][]byte{DBDumpFile: []byte("PGDMP..."), IdentityFile: FakeIdentity(t, "the-secret-key")}, at)
	store.Put("team/dr/fj/partial/db.dump.bin", []byte("x"), at)

	st, err := OpenStore(ctx, c, remoteNS, &f.Spec.Backup.Remote.S3, BackupKeyRef(f))
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.ReadManifest(ctx, "dr/fj/b1")
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.ReadIdentity(ctx, "dr/fj/b1", m)
	if err != nil {
		t.Fatal(err)
	}
	if string(id.Secret(IdentityRoleSecrets).Data["secret-key"]) != "the-secret-key" {
		t.Errorf("identity %+v", id)
	}
	if _, err := st.ReadManifest(ctx, "dr/fj/partial"); err == nil || !s3.IsNotFound(err) || TransientRemoteError(err) {
		t.Errorf("incomplete backup: %v", err)
	}
	dirs, err := st.ListDirs(ctx, "dr/fj")
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 || !dirs["b1"].Complete || dirs["partial"].Complete || len(dirs["b1"].Keys) != 3 {
		t.Errorf("dirs %+v", dirs)
	}

	// A tampered identity does not match its checksum.
	good := store.Objects["team/dr/fj/b1/identity.json.bin"]
	cipher, _ := rclone.NewCipher(FakeKey)
	other, _ := cipher.Encrypt(FakeIdentity(t, "another-key"), strings.NewReader(strings.Repeat("n", 24)))
	store.Put("team/dr/fj/b1/identity.json.bin", other, at)
	if _, err := st.ReadIdentity(ctx, "dr/fj/b1", m); err == nil || !IsConfigError(err) {
		t.Errorf("tampered identity: %v", err)
	}
	store.Put("team/dr/fj/b1/identity.json.bin", good, at)

	// Another key cannot decrypt the backup.
	secrets[1].Data["key"] = []byte("some-other-long-key")
	c2 := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(secretObjects(secrets)...).Build()
	wrong, err := OpenStore(ctx, c2, remoteNS, &f.Spec.Backup.Remote.S3, BackupKeyRef(f))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrong.ReadManifest(ctx, "dr/fj/b1"); !errors.Is(err, rclone.ErrAuthentication) || TransientRemoteError(err) {
		t.Errorf("wrong key: %v", err)
	}

	store.Down = true
	if _, err := st.ReadManifest(ctx, "dr/fj/b1"); !TransientRemoteError(err) {
		t.Errorf("an unavailable store is transient: %v", err)
	}
	store.Down = false

	if err := st.DeleteDir(ctx, "dr/fj/b1"); err != nil {
		t.Fatal(err)
	}
	if len(store.Keys("team/dr/fj/b1/")) != 0 || store.Deleted[0] != "team/dr/fj/b1/manifest.json.bin" {
		t.Errorf("deleted %v, left %v; the manifest goes first", store.Deleted, store.Keys("team/dr/fj/b1/"))
	}
}

func TestOpenStoreReportsMissingSecretsAndBadKeys(t *testing.T) {
	f := remoteInstance(nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		key  []byte
		objs bool
		want string
	}{
		"no secrets":        {want: `Secret "creds" not found`},
		"short key":         {objs: true, key: []byte("short"), want: "shorter than 16"},
		"newline after key": {objs: true, key: []byte(FakeKey + "\n"), want: "white space"},
	} {
		t.Run(name, func(t *testing.T) {
			var objs []client.Object
			if tc.objs {
				secrets := FakeSecrets(remoteNS, "fj-backup-key")
				secrets[1].Data["key"] = tc.key
				objs = secretObjects(secrets)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
			_, err := OpenStore(ctx, c, remoteNS, &f.Spec.Backup.Remote.S3, BackupKeyRef(f))
			if err == nil || !IsConfigError(err) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("got %v, want a configuration error containing %q", err, tc.want)
			}
			if err != nil && strings.Contains(err.Error(), FakeKey) {
				t.Error("the error reveals the key")
			}
		})
	}
}

// remoteJobFixture is an instance with spec.backup.remote (prefix "team", a private CA), a
// completed backup of it, and the Job input.
func remoteJobFixture(t *testing.T) (*forgejov1.Forgejo, *forgejov1.ForgejoBackup, RemoteTarget, JobInput) {
	t.Helper()
	f := remoteInstance(nil)
	f.Spec.Backup.Remote.S3.TLS = &forgejov1.S3TLSSpec{CASecret: &forgejov1.SecretKeyRef{Name: "ca"}}
	f.Spec.Backup.Remote.S3.Prefix = "team"
	b := completedBackup(t, "b1")
	target := RemoteTarget{Conn: &f.Spec.Backup.Remote.S3, Key: BackupKeyRef(f), Image: RcloneImage(f), Path: BackupRemotePath(b)}
	in := JobInput{Scheme: testScheme(t), Instance: f, InstanceName: f.Name, ForgejoImage: f.Spec.Image, PostgresImage: forgejov1.DefaultPostgresImage,
		DB: &forgejo.DBConnection{Host: "db", Port: 5432, Name: "forgejo", SecretName: "fj-db-app", UsernameKey: "username", PasswordKey: testPasswordField}}
	return f, b, target, in
}

func envOf(c corev1.Container) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	return env
}

// Credentials and the key reach the upload Job only through Secret references, never as values.
func TestUploadJobKeepsSecretsOutOfThePodSpec(t *testing.T) {
	_, b, target, in := remoteJobFixture(t)
	up, err := UploadJob(in, b, target, IdentitySecretName(b), `{"formatVersion":1`)
	if err != nil {
		t.Fatal(err)
	}
	pod := up.Spec.Template.Spec
	if len(pod.Containers) != 1 || pod.Containers[0].Name != ContainerUpload || pod.Containers[0].Image != forgejov1.DefaultRcloneImage {
		t.Fatalf("containers %+v", pod.Containers)
	}
	env := envOf(pod.Containers[0])
	for name, secret := range map[string]string{envAWSAccessKeyID: testCredsSecret, envAWSSecretAccessKey: testCredsSecret, "BACKUP_KEY": testBackupKeySecret} {
		e := env[name]
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != secret {
			t.Errorf("%s is not a reference to Secret %s: %+v", name, secret, e)
		}
	}
	for name, want := range map[string]string{
		"RCLONE_CONFIG_ENC_REMOTE":              "s3:" + FakeBucket + "/team",
		"RCLONE_CONFIG_ENC_FILENAME_ENCRYPTION": "off",
		"RCLONE_CONFIG_S3_ENV_AUTH":             valueTrue,
		"RCLONE_CONFIG_S3_REGION":               "us-east-1",
		"REMOTE_PATH":                           "regression/fb/b1",
		"RCLONE_CA_CERT":                        remoteCAMount + "/ca.crt",
	} {
		if env[name].Value != want {
			t.Errorf("%s = %q, want %q", name, env[name].Value, want)
		}
	}
	if _, ok := env["RCLONE_CONFIG_ENC_PASSWORD"]; ok {
		t.Error("the crypt password must not be in the pod spec")
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("volume %s is writable", v.Name)
		}
	}
	if !strings.Contains(uploadScript, `printf '%s' "$BACKUP_KEY" | rclone obscure -`) || strings.Contains(uploadScript, "obscure \"$BACKUP_KEY") {
		t.Error("the key must reach rclone through stdin")
	}
}

// A remote restore downloads and checks the files into an emptyDir it asks room for, then runs
// the usual steps; it mounts no backup PVC.
func TestRemoteRestoreJobDownloadsFirst(t *testing.T) {
	f, _, target, in := remoteJobFixture(t)
	r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns}}
	files := []forgejov1.RemoteBackupFile{{Name: DBDumpFile, SizeBytes: 100 << 20, SHA256: "d"}, {Name: DataArchiveFile, SizeBytes: 900 << 20, SHA256: "a"}}
	job, err := RemoteRestoreJob(in, r, RemoteSource{RemoteTarget: target, Files: files}, true, true)
	if err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	if len(pod.InitContainers) < 3 || pod.InitContainers[0].Name != ContainerDownload || pod.InitContainers[1].Name != ContainerVerify ||
		pod.Containers[0].Name != ContainerRestoreData {
		t.Fatalf("steps: %v / %v", names(pod.InitContainers), names(pod.Containers))
	}
	dl := pod.InitContainers[0]
	env := envOf(dl)
	if env["DB_SHA256"].Value != "d" || env["DATA_SHA256"].Value != "a" || env["BACKUP_DIR"].Value != downloadMount {
		t.Errorf("download env %v", env)
	}
	need := dl.Resources.Requests[corev1.ResourceEphemeralStorage]
	if need.Cmp(resource.MustParse("1000Mi")) <= 0 {
		t.Errorf("ephemeral storage request %s does not cover the download", need.String())
	}
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName != DataClaimName(f) {
			t.Errorf("a remote restore mounts claim %s", v.PersistentVolumeClaim.ClaimName)
		}
	}
	if _, err := RemoteRestoreJob(in, r, RemoteSource{RemoteTarget: target, Files: files[:1]}, true, true); err == nil {
		t.Error("a restore of data without the archive's checksum was built")
	}
}

func names(cs []corev1.Container) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}

func completedBackup(t *testing.T, name string) *forgejov1.ForgejoBackup {
	t.Helper()
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.PVCName = testBackupsPVC
	b.Status.IncludesData = true
	return b
}

const ns = "regression"

func TestBuildIdentity(t *testing.T) {
	f := remoteInstance(nil)
	f.Annotations = map[string]string{corev1.LastAppliedConfigAnnotation: "{...}", "team": "a"}
	objs := []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsSecret, Namespace: remoteNS}, Data: map[string][]byte{testSecretKeyField: []byte("k")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fj-admin", Namespace: remoteNS}, Data: map[string][]byte{testPasswordField: []byte("pw")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSSHHostKeysSecret, Namespace: remoteNS,
			Annotations: map[string]string{forgejo.AnnotationSSHHostKeysSource: keySourceGenerated, "other": "x"}},
			Data: map[string][]byte{testHostKeyFile: []byte("key")}},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	id, err := BuildIdentity(context.Background(), c, f)
	if err != nil {
		t.Fatal(err)
	}
	if len(id.Secrets) != 3 || id.Secret(IdentityRoleSSHHostKeys).Annotations["other"] != "" ||
		id.Secret(IdentityRoleSSHHostKeys).Annotations[forgejo.AnnotationSSHHostKeysSource] != keySourceGenerated {
		t.Errorf("secrets %+v", id.Secrets)
	}
	if _, ok := id.Instance.Metadata.Annotations[corev1.LastAppliedConfigAnnotation]; ok || id.Instance.Metadata.Annotations["team"] != "a" {
		t.Errorf("annotations %v", id.Instance.Metadata.Annotations)
	}
	data, _ := json.Marshal(id)
	back := &Identity{}
	if err := json.Unmarshal(data, back); err != nil || string(back.Secret(IdentityRoleSecrets).Data["secret-key"]) != "k" {
		t.Errorf("round trip: %v", err)
	}
	// The secrets are required; a missing admin Secret is an error too.
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs[0]).Build()
	if _, err := BuildIdentity(context.Background(), c, f); err == nil {
		t.Error("an identity without the admin Secret was built")
	}
}

// restoreFromInstance is a new instance created from the backup at "dr/old/b1".
func restoreFromInstance(store *FakeS3) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: remoteNS, UID: "fj-uid"}}
	f.Spec.Image = testImage17
	f.Spec.RestoreFrom = &forgejov1.RestoreFromSpec{Remote: forgejov1.RemoteBackupSource{S3: store.FakeConnection("team"), Path: testRemotePath}}
	return f
}

func putOldBackup(t *testing.T, store *FakeS3) {
	t.Helper()
	store.PutBackup(t, "team/", "dr/old/b1", RemoteManifest{Backup: "b1", Namespace: "dr", Forgejo: testOldName, Reason: forgejov1.BackupReasonScheduled,
		ForgejoImage: testImage17},
		map[string][]byte{DBDumpFile: []byte("PGDMP"), IdentityFile: FakeIdentity(t, "old-secret-key")}, time.Now())
}

func TestReconcileRestoreIdentity(t *testing.T) {
	store := NewFakeS3(t)
	putOldBackup(t, store)
	s := testScheme(t)
	ctx := context.Background()
	f := restoreFromInstance(store)

	// Without the key Secret the step waits, and the secrets step after it does not run.
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(FakeSecrets(remoteNS, "unrelated")[0]).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	res, err := ReconcileRestoreIdentity(ctx, rc)
	if err != nil || !res.Stop {
		t.Fatalf("missing key: %+v %v", res, err)
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRestoredFromBackup); cond == nil ||
		cond.Status != metav1.ConditionFalse || !strings.Contains(cond.Message, `"fj-backup-key" not found`) {
		t.Errorf("condition %+v", cond)
	}

	c = fake.NewClientBuilder().WithScheme(s).WithObjects(secretObjects(FakeSecrets(remoteNS, "fj-backup-key"))...).Build()
	rc = &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	if res, err := ReconcileRestoreIdentity(ctx, rc); err != nil || res.Stop {
		t.Fatalf("restore: %+v %v", res, err)
	}
	st := f.Status.RestoredFrom
	if st.Phase != forgejov1.RestoredFromRestoringData || st.IdentityRestoredTime == nil || st.BackupName != "b1" ||
		st.Location != testRemoteLocation {
		t.Errorf("status %+v", st)
	}
	for name, key := range map[string]string{testSecretsSecret: testSecretKeyField, "fj-admin": testPasswordField, testSSHHostKeysSecret: testHostKeyFile} {
		sec := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: remoteNS, Name: name}, sec); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(sec.Data[key]) == 0 || len(sec.OwnerReferences) != 0 || sec.Labels[forgejo.LabelManagedBy] != forgejo.ManagedBy {
			t.Errorf("%s: %+v", name, sec)
		}
	}
	keys := &corev1.Secret{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: remoteNS, Name: testSSHHostKeysSecret}, keys)
	if keys.Annotations[forgejo.AnnotationSSHHostKeysSource] != keySourceGenerated {
		t.Errorf("host keys annotations %v", keys.Annotations)
	}
	// Done once: later passes do not read the backup again.
	store.Down = true
	if res, err := ReconcileRestoreIdentity(ctx, rc); err != nil || res.Stop {
		t.Errorf("second pass: %+v %v", res, err)
	}
}

// An unreachable or failing bucket is read again only after a backoff (30 s doubling to 5 min),
// so a hung endpoint does not hold the Forgejo controller's worker on every pass.
func TestRestoreIdentityBacksOffTransientFailures(t *testing.T) {
	store := NewFakeS3(t)
	putOldBackup(t, store)
	s := testScheme(t)
	ctx := context.Background()
	f := restoreFromInstance(store)
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	t.Cleanup(func() { ForgetRemote(f.Namespace, f.Name) })
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secretObjects(FakeSecrets(remoteNS, "fj-backup-key"))...).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}

	store.Down = true
	res, err := ReconcileRestoreIdentity(ctx, rc)
	if err != nil || !res.Stop || res.RequeueAfter != bootstrapRetry {
		t.Fatalf("first failure: %+v %v", res, err)
	}
	msg, reads := f.Status.RestoredFrom.Message, store.RequestCount()
	if reads == 0 || !strings.Contains(msg, "Cannot read the backup") {
		t.Fatalf("reads %d, message %q", reads, msg)
	}
	// Within the backoff nothing is read, and the step reports the failure unchanged.
	res, err = ReconcileRestoreIdentity(ctx, rc)
	if err != nil || !res.Stop || res.RequeueAfter <= 0 || res.RequeueAfter > bootstrapRetry ||
		store.RequestCount() != reads || f.Status.RestoredFrom.Message != msg {
		t.Fatalf("within the backoff: %+v %v, %d reads, %q", res, err, store.RequestCount()-reads, f.Status.RestoredFrom.Message)
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRestoredFromBackup); cond == nil ||
		cond.Reason != "RemoteBackupUnavailable" || cond.Message != msg {
		t.Errorf("condition %+v", cond)
	}
	// Once it runs out the bucket is read again, and the backoff doubles up to 5 min.
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, identityRetryMax, identityRetryMax}
	for _, backoff := range want {
		expireIdentityBackoff(t, key)
		res, err = ReconcileRestoreIdentity(ctx, rc)
		if err != nil || !res.Stop || res.RequeueAfter != backoff || store.RequestCount() == reads {
			t.Fatalf("after the backoff: %+v %v, want requeue %s", res, err, backoff)
		}
		reads = store.RequestCount()
	}
	// Another source is read at once.
	f.Spec.RestoreFrom.Remote.Path = "dr/old/b2"
	if res, err = ReconcileRestoreIdentity(ctx, rc); err != nil || !res.Stop || res.RequeueAfter != bootstrapRetry || store.RequestCount() == reads {
		t.Fatalf("changed source: %+v %v", res, err)
	}
	// A success forgets the failure.
	f.Spec.RestoreFrom.Remote.Path = testRemotePath
	store.Down = false
	if res, err = ReconcileRestoreIdentity(ctx, rc); err != nil || res.Stop {
		t.Fatalf("restore: %+v %v (%s)", res, err, f.Status.RestoredFrom.Message)
	}
	if _, ok := identityReadFailures.Load(key); ok {
		t.Error("the failure is still recorded after a success")
	}

	checkConfigurationErrorIsNotBackedOff(t, store, key)
}

// expireIdentityBackoff lets the recorded identity read failure of key retry at once.
func expireIdentityBackoff(t *testing.T, key types.NamespacedName) {
	t.Helper()
	v, ok := identityReadFailures.Load(key)
	if !ok {
		t.Fatal("no failure recorded")
	}
	v.(*identityReadFailure).retryAt = time.Now().Add(-time.Second)
}

// Failures that do not go away by themselves (here a missing key Secret) are not backed off.
func checkConfigurationErrorIsNotBackedOff(t *testing.T, store *FakeS3, key types.NamespacedName) {
	t.Helper()
	s := testScheme(t)
	f := restoreFromInstance(store)
	rc := &forgejo.ReconcileContext{Client: fake.NewClientBuilder().WithScheme(s).Build(), Scheme: s, Instance: f}
	if res, err := ReconcileRestoreIdentity(context.Background(), rc); err != nil || !res.Stop {
		t.Fatalf("missing key: %+v %v", res, err)
	}
	if _, ok := identityReadFailures.Load(key); ok {
		t.Error("a configuration error was backed off")
	}
}

func TestRestoreIdentityNeverOverwritesSecrets(t *testing.T) {
	store := NewFakeS3(t)
	putOldBackup(t, store)
	s := testScheme(t)
	ctx := context.Background()
	f := restoreFromInstance(store)
	same := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsSecret, Namespace: remoteNS},
		Data: map[string][]byte{testSecretKeyField: []byte("old-secret-key"), "internal-token": []byte("tok"), "extra": []byte("x")}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(secretObjects(FakeSecrets(remoteNS, "fj-backup-key")), same)...).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	if res, err := ReconcileRestoreIdentity(ctx, rc); err != nil || res.Stop {
		t.Fatalf("existing Secret with the backup's values: %+v %v (%s)", res, err, f.Status.RestoredFrom.Message)
	}

	f = restoreFromInstance(store)
	other := same.DeepCopy()
	other.ResourceVersion = ""
	other.Data["secret-key"] = []byte("a new key")
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(append(secretObjects(FakeSecrets(remoteNS, "fj-backup-key")), other)...).Build()
	rc = &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	res, err := ReconcileRestoreIdentity(ctx, rc)
	if err != nil || !res.Stop || !strings.Contains(f.Status.RestoredFrom.Message, "fj-secrets exists with other values") {
		t.Fatalf("conflict: %+v %v %+v", res, err, f.Status.RestoredFrom)
	}
	got := &corev1.Secret{}
	_ = c.Get(ctx, types.NamespacedName{Namespace: remoteNS, Name: testSecretsSecret}, got)
	if string(got.Data["secret-key"]) != "a new key" {
		t.Error("the existing Secret was overwritten")
	}
}

func TestMaintenanceHoldsUntilBootstrapRestored(t *testing.T) {
	store := NewFakeS3(t)
	s := testScheme(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		phase   forgejov1.RestorePhase
		done    bool
		marked  bool
		hold    bool
		held    bool // only a person ends the hold
		failed  string
		message string
	}{
		{name: "no restore yet", hold: true, message: "before Forgejo starts"},
		{name: "restore pending", phase: forgejov1.RestorePhasePending, hold: true},
		// Forgejo starts only once the restore is recorded on <name>-secrets (by restore-from).
		{name: "restored, not yet recorded", phase: forgejov1.RestorePhaseScalingUp, hold: true, message: "recorded on Secret fj-secrets"},
		{name: "restored and recorded", phase: forgejov1.RestorePhaseScalingUp, marked: true},
		{name: "restore failed", phase: forgejov1.RestorePhaseFailed, hold: true, held: true, message: "Delete it to try again"},
		{name: "target not empty", failed: "spec.restoreFrom was not restored: TargetNotEmpty", hold: true, held: true, message: "TargetNotEmpty"},
		{name: "completed long ago", done: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := restoreFromInstance(store)
			if tc.done {
				f.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: forgejov1.RestoredFromCompleted}
			}
			if tc.failed != "" {
				f.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: forgejov1.RestoredFromFailed, Message: tc.failed}
			}
			var objs []client.Object
			if tc.marked {
				objs = append(objs, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsSecret, Namespace: remoteNS,
					Annotations: map[string]string{forgejo.AnnotationRestoredFrom: testRemoteLocation}}})
			}
			if tc.phase != "" {
				r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: BootstrapRestoreName(f), Namespace: remoteNS}}
				r.Spec.ForgejoRef.Name = f.Name
				r.Status.Phase = tc.phase
				objs = append(objs, r)
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
			rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
			if _, err := ReconcileMaintenance(ctx, rc); err != nil {
				t.Fatal(err)
			}
			if rc.Maintenance != tc.hold || rc.MaintenanceHeld != tc.held || !strings.Contains(rc.MaintenanceReason, tc.message) {
				t.Errorf("maintenance %t held %t (%q), want %t held %t", rc.Maintenance, rc.MaintenanceHeld, rc.MaintenanceReason, tc.hold, tc.held)
			}
		})
	}
}

func TestReconcileBootstrapCreatesAndFollowsTheRestore(t *testing.T) {
	store := NewFakeS3(t)
	s := testScheme(t)
	ctx := context.Background()
	f := restoreFromInstance(store)
	f.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: forgejov1.RestoredFromRestoringData, IdentityRestoredTime: &metav1.Time{Time: time.Now()},
		Location: testRemoteLocation, BackupName: "b1"}
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsSecret, Namespace: remoteNS}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, secrets).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}

	// The database is not ready yet: no restore.
	if _, err := ReconcileBootstrap(ctx, rc); err != nil {
		t.Fatal(err)
	}
	rs := &forgejov1.ForgejoRestore{}
	key := types.NamespacedName{Namespace: remoteNS, Name: "fj-restore-from"}
	if err := c.Get(ctx, key, rs); err == nil {
		t.Fatal("restore created before the database was ready")
	}
	rc.DatabaseReady = true
	if _, err := ReconcileBootstrap(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, key, rs); err != nil {
		t.Fatal(err)
	}
	if rs.Spec.Remote == nil || rs.Spec.Remote.Path != testRemotePath || rs.Spec.Remote.KeySecret == nil || rs.Spec.Remote.KeySecret.Name != testBackupKeySecret ||
		!metav1.IsControlledBy(rs, f) || rs.Spec.BackupName != "" {
		t.Errorf("restore %+v", rs)
	}
	rs.Status.Phase, rs.Status.Message = forgejov1.RestorePhaseFailed, "boom"
	if err := c.Update(ctx, rs); err != nil {
		t.Fatal(err)
	}
	_, _ = ReconcileBootstrap(ctx, rc)
	if f.Status.RestoredFrom.Phase != forgejov1.RestoredFromFailed || !strings.Contains(f.Status.RestoredFrom.Message, "boom") {
		t.Errorf("failed: %+v", f.Status.RestoredFrom)
	}
	// Restored: recorded on <name>-secrets before Forgejo starts, and complete.
	rs.Status.Phase, rs.Status.Message = forgejov1.RestorePhaseScalingUp, "Waiting for Forgejo"
	if err := c.Update(ctx, rs); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileBootstrap(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.RestoredFrom.Phase != forgejov1.RestoredFromCompleted || f.Status.RestoredFrom.CompletionTime == nil ||
		!meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionRestoredFromBackup) || Bootstrapping(f) {
		t.Errorf("completed: %+v", f.Status.RestoredFrom)
	}
	_ = c.Get(ctx, types.NamespacedName{Namespace: remoteNS, Name: testSecretsSecret}, secrets)
	if secrets.Annotations[forgejo.AnnotationRestoredFrom] != testRemoteLocation {
		t.Errorf("restore not recorded on the Secret: %v", secrets.Annotations)
	}
}

// spec.restoreFrom never restores over data the instance (or a previous one of the same name)
// has: not after its status is lost, not after its ForgejoRestore is deleted once it restored,
// and not into a database or data volume older than the Forgejo object.
func TestBootstrapNeverRestoresOverLiveData(t *testing.T) {
	store := NewFakeS3(t)
	s := testScheme(t)
	ctx := context.Background()
	where := testRemoteLocation
	created := metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
	marked := func(loc string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSecretsSecret, Namespace: remoteNS,
			Annotations: map[string]string{forgejo.AnnotationRestoredFrom: loc}}}
	}
	restoring := func() *forgejov1.Forgejo {
		f := restoreFromInstance(store)
		f.CreationTimestamp = created
		f.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: forgejov1.RestoredFromRestoringData,
			IdentityRestoredTime: &metav1.Time{Time: time.Now()}, Location: where, BackupName: "b1"}
		return f
	}
	restoreCreated := func(t *testing.T, c client.Client) bool {
		t.Helper()
		err := c.Get(ctx, types.NamespacedName{Namespace: remoteNS, Name: "fj-restore-from"}, &forgejov1.ForgejoRestore{})
		return err == nil
	}

	t.Run("status lost", func(t *testing.T) {
		f := restoreFromInstance(store) // re-created with retainPolicy Retain: no status at all
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(marked(where)).Build()
		rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
		if _, err := ReconcileMaintenance(ctx, rc); err != nil {
			t.Fatal(err)
		}
		if rc.Maintenance || Bootstrapping(f) || !meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionRestoredFromBackup) {
			t.Fatalf("maintenance %t (%s), status %+v", rc.Maintenance, rc.MaintenanceReason, f.Status.RestoredFrom)
		}
		store.Down = true // nothing is read from the backup again
		defer func() { store.Down = false }()
		if res, err := ReconcileRestoreIdentity(ctx, rc); err != nil || res.Stop {
			t.Fatalf("identity: %+v %v", res, err)
		}
		rc.DatabaseReady = true
		if _, err := ReconcileBootstrap(ctx, rc); err != nil || restoreCreated(t, c) {
			t.Fatalf("restore created again (%v)", err)
		}
	})

	t.Run("restore deleted after it restored", func(t *testing.T) {
		f := restoring()
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(marked(where)).Build()
		rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, DatabaseReady: true}
		if _, err := ReconcileBootstrap(ctx, rc); err != nil || restoreCreated(t, c) || f.Status.RestoredFrom.Phase != forgejov1.RestoredFromCompleted {
			t.Fatalf("restore created again (%v): %+v", err, f.Status.RestoredFrom)
		}
	})

	for _, tc := range []struct {
		name      string
		objs      []client.Object
		overwrite bool
		want      string
	}{
		{name: "restored from another backup", objs: []client.Object{marked("s3://backups/team/dr/old/b0/")}, want: "was restored from s3://backups/team/dr/old/b0/"},
		{name: "data volume older than the instance", objs: []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: testDataPVC, Namespace: remoteNS, CreationTimestamp: metav1.NewTime(created.Add(-time.Hour))}}}, want: "PVC fj-data"},
		{name: "overwrite allowed", objs: []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: testDataPVC, Namespace: remoteNS, CreationTimestamp: metav1.NewTime(created.Add(-time.Hour))}}}, overwrite: true},
		{name: "fresh volume", objs: []client.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: testDataPVC, Namespace: remoteNS, CreationTimestamp: metav1.NewTime(created.Add(time.Second))}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := restoring()
			if tc.overwrite {
				f.Annotations = map[string]string{forgejo.AnnotationRestoreFromOverwrite: valueTrue}
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(tc.objs, f)...).Build()
			rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, DatabaseReady: true}
			if _, err := ReconcileBootstrap(ctx, rc); err != nil {
				t.Fatal(err)
			}
			cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionRestoredFromBackup)
			if tc.want == "" {
				if !restoreCreated(t, c) {
					t.Fatalf("no restore: %+v", f.Status.RestoredFrom)
				}
				return
			}
			if restoreCreated(t, c) || cond == nil || cond.Reason != "TargetNotEmpty" || !strings.Contains(cond.Message, tc.want) ||
				f.Status.RestoredFrom.Phase != forgejov1.RestoredFromFailed {
				t.Fatalf("condition %+v, status %+v", cond, f.Status.RestoredFrom)
			}
			// Forgejo stays down meanwhile, and the reason is the one maintenance reports.
			rc2 := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
			if _, err := ReconcileMaintenance(ctx, rc2); err != nil || !rc2.Maintenance || !strings.Contains(rc2.MaintenanceReason, "TargetNotEmpty") &&
				!strings.Contains(rc2.MaintenanceReason, tc.want) {
				t.Fatalf("maintenance %t %q (%v)", rc2.Maintenance, rc2.MaintenanceReason, err)
			}
		})
	}
}

// Remote retention protects what restores read whatever the endpoint is called (bucket and
// prefix decide), and the spec.restoreFrom source of every bootstrapping instance in the
// cluster, also before its ForgejoRestore exists.
func TestProtectedRemoteDirs(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := remoteInstance(nil) // endpoint http://garage:3900, prefix ""
	source := func(endpoint, prefix, path string) forgejov1.RemoteBackupSource {
		c := f.Spec.Backup.Remote.S3
		c.Endpoint, c.Prefix = endpoint, prefix
		return forgejov1.RemoteBackupSource{S3: c, Path: path}
	}
	restore := func(name string, src forgejov1.RemoteBackupSource, phase forgejov1.RestorePhase) *forgejov1.ForgejoRestore {
		r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "elsewhere"}}
		r.Spec.Remote = &src
		r.Status.Phase = phase
		return r
	}
	bootstrapping := func(namespace, name string, src forgejov1.RemoteBackupSource, phase forgejov1.RestoredFromPhase) *forgejov1.Forgejo {
		o := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		o.Spec.RestoreFrom = &forgejov1.RestoreFromSpec{Remote: src}
		if phase != "" {
			o.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: phase}
		}
		return o
	}
	objs := []client.Object{
		restore("case", source("http://GARAGE:3900/", "", "dr/fj/b-case"), forgejov1.RestorePhaseRestoring),
		restore("port", source("http://garage.dr.svc:3900", "", "dr/fj/b-port/"), forgejov1.RestorePhaseScalingDown),
		restore("default-port", source("http://garage", "", "dr/fj/b-default-port"), forgejov1.RestorePhasePending),
		restore("done", source("http://garage:3900", "", "dr/fj/b-done"), forgejov1.RestorePhaseCompleted),
		restore("other-prefix", source("http://garage:3900", "team", "dr/fj/b-prefix"), forgejov1.RestorePhaseRestoring),
		restore("other-instance", source("http://garage:3900", "", "dr/other/b-other"), forgejov1.RestorePhaseRestoring),
		// A DR instance in another namespace that restores its identity or waits for its
		// database: no ForgejoRestore yet.
		bootstrapping("recovery", "fj", source("https://Garage.dr.svc.cluster.local:443", "", "dr/fj/b-dr"), forgejov1.RestoredFromRestoringIdentity),
		bootstrapping("waiting", "fj", source("http://garage:3900", "", "dr/fj/b-dr-new"), ""),
		bootstrapping("finished", "fj", source("http://garage:3900", "", "dr/fj/b-dr-done"), forgejov1.RestoredFromCompleted),
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	got, err := protectedRemoteDirs(ctx, rc, nil)
	if err != nil {
		t.Fatal(err)
	}
	dirs := make([]string, 0, len(got))
	for d := range got {
		dirs = append(dirs, d)
	}
	slices.Sort(dirs)
	want := []string{"b-case", "b-default-port", "b-dr", "b-dr-new", "b-port"}
	if !slices.Equal(dirs, want) {
		t.Errorf("protected %v, want %v", dirs, want)
	}
}

func TestRemoteRetentionPrunesOnlyScheduledBackups(t *testing.T) {
	store := NewFakeS3(t)
	s := testScheme(t)
	ctx := context.Background()
	f := remoteInstance(store)
	f.Spec.Backup.Enabled, f.Spec.Backup.Retention = true, 2
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 0, 0, 0, time.UTC) }
	files := map[string][]byte{DBDumpFile: []byte("PGDMP"), IdentityFile: FakeIdentity(t, "k")}
	putBy := func(uid, name string, reason forgejov1.BackupReason, d int) {
		store.PutBackup(t, "team/", "dr/fj/"+name, RemoteManifest{Backup: name, Namespace: remoteNS, Forgejo: "fj", ForgejoUID: uid,
			Reason: reason, CompletedAt: day(d).Format(time.RFC3339)}, files, day(d))
	}
	put := func(name string, reason forgejov1.BackupReason, d int) { putBy("fj-uid", name, reason, d) }
	put("fj-scheduled-1", forgejov1.BackupReasonScheduled, 1)
	put("fj-scheduled-2", forgejov1.BackupReasonScheduled, 2)
	put("fj-scheduled-3", forgejov1.BackupReasonScheduled, 3)
	put("fj-scheduled-4", forgejov1.BackupReasonScheduled, 4)
	put("manual-old", forgejov1.BackupReasonManual, 1)
	put("pre-upgrade", forgejov1.BackupReasonPreUpgrade, 1)
	// Read by a running restore: kept although beyond retention.
	put("fj-scheduled-0", forgejov1.BackupReasonScheduled, 0)
	// Uploads that never completed: an old one goes, a recent one may still be running.
	store.Put("team/dr/fj/broken/db.dump.bin", []byte("x"), time.Now().Add(-48*time.Hour))
	store.Put("team/dr/fj/uploading/db.dump.bin", []byte("x"), time.Now().Add(-time.Hour))
	// Another instance's backups are not this instance's to prune.
	store.PutBackup(t, "team/", "dr/other/fj-scheduled-1", RemoteManifest{Backup: testScheduledBackup, Namespace: remoteNS, Forgejo: "other",
		Reason: forgejov1.BackupReasonScheduled}, files, day(1))
	// Neither are those of an instance of the same namespace and name in another cluster (a
	// disaster recovery drill), however old, ...
	putBy("drill-uid", "drill-1", forgejov1.BackupReasonScheduled, 1)
	// ... nor backups without an instance UID.
	putBy("", "anonymous-1", forgejov1.BackupReasonScheduled, 1)
	// The instance spec.restoreFrom recreated: its scheduled backups up to the restored one
	// (taken before the disaster) are this instance's now, later ones are not.
	f.Status.RestoredFrom = &forgejov1.RestoredFromStatus{Phase: forgejov1.RestoredFromCompleted, InstanceUID: "lost-uid",
		BackupCompletionTime: &metav1.Time{Time: day(2)}}
	putBy("lost-uid", "lost-1", forgejov1.BackupReasonScheduled, 1)
	putBy("lost-uid", "lost-5", forgejov1.BackupReasonScheduled, 5)

	restore := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "elsewhere"}}
	restore.Spec.Remote = &forgejov1.RemoteBackupSource{S3: store.FakeConnection("team/"), Path: "dr/fj/fj-scheduled-0"}
	restore.Status.Phase = forgejov1.RestorePhaseRestoring
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(append(secretObjects(FakeSecrets(remoteNS, "fj-backup-key")), restore)...).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}

	if err := runRemoteRetention(ctx, rc, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, k := range store.Keys("team/dr/") {
		if strings.HasSuffix(k, "/manifest.json.bin") || strings.HasSuffix(k, "/db.dump.bin") &&
			(strings.Contains(k, "/uploading/") || strings.Contains(k, "/broken/")) {
			left = append(left, strings.TrimPrefix(k, "team/dr/"))
		}
	}
	want := []string{"fj/anonymous-1/manifest.json.bin", "fj/drill-1/manifest.json.bin", "fj/fj-scheduled-0/manifest.json.bin",
		"fj/fj-scheduled-3/manifest.json.bin", "fj/fj-scheduled-4/manifest.json.bin", "fj/lost-5/manifest.json.bin",
		"fj/manual-old/manifest.json.bin", "fj/pre-upgrade/manifest.json.bin", "fj/uploading/db.dump.bin", "other/fj-scheduled-1/manifest.json.bin"}
	if strings.Join(left, " ") != strings.Join(want, " ") {
		t.Errorf("left\n %v\nwant\n %v", left, want)
	}

	// pruneRemote runs again only after a new upload or an hour.
	f.Status.Backup = &forgejov1.BackupStatusSummary{}
	pruneRemote(ctx, rc, nil, time.Now())
	at := f.Status.Backup.Remote.LastRetentionTime
	if at == nil {
		t.Fatal("no retention time recorded")
	}
	store.Down = true
	pruneRemote(ctx, rc, nil, time.Now().Add(time.Minute))
	if f.Status.Backup.Remote.Message != "" {
		t.Errorf("retention ran again without a new upload: %q", f.Status.Backup.Remote.Message)
	}
	pruneRemote(ctx, rc, nil, time.Now().Add(2*time.Hour))
	if !strings.Contains(f.Status.Backup.Remote.Message, "Remote retention failed") {
		t.Errorf("a failed run is not reported: %q", f.Status.Backup.Remote.Message)
	}
	forgetRemoteRetention(f.Namespace, f.Name)
}

func TestBackupHealthReportsUploads(t *testing.T) {
	sched, _ := cron.ParseStandard("0 3 * * *")
	done := time.Now().Add(-time.Hour)
	f := remoteInstance(nil)
	for _, tc := range []struct {
		phase  forgejov1.RemoteBackupPhase
		status metav1.ConditionStatus
		reason string
		inMsg  string
	}{
		{forgejov1.RemoteBackupUploaded, metav1.ConditionTrue, testReasonBackupSucceeded, "uploaded to s3://"},
		{forgejov1.RemoteBackupUploading, metav1.ConditionTrue, testReasonBackupSucceeded, "uploading to s3://"},
		{forgejov1.RemoteBackupFailed, metav1.ConditionFalse, "UploadFailed", "but its upload to s3://backups/x/ failed: denied"},
	} {
		b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: testScheduledBackup}}
		b.Spec.Reason = forgejov1.BackupReasonScheduled
		b.Status.Phase = forgejov1.BackupPhaseCompleted
		b.Status.CompletionTime = &metav1.Time{Time: done}
		b.Status.Remote = &forgejov1.RemoteBackupStatus{Phase: tc.phase, Location: "s3://backups/x/", Message: "denied"}
		f.Status.Conditions = nil
		rc := &forgejo.ReconcileContext{Instance: f}
		setHealth(rc, sched, []*forgejov1.ForgejoBackup{b}, time.Now(), nil, keyProblem{})
		c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBackupHealthy)
		if c == nil || c.Status != tc.status || c.Reason != tc.reason || !strings.Contains(c.Message, tc.inMsg) {
			t.Errorf("%s: %+v", tc.phase, c)
		}
	}
}

// A generated key nobody acknowledged to have copied out of the cluster, or a missing key,
// is reported persistently: in status.backup.remote.message and as BackupHealthy=False, which
// only a failed backup overrides.
func TestBackupKeyProblemsAreReported(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	sched, _ := cron.ParseStandard("0 3 * * *")
	creds := FakeSecrets(remoteNS, "fj-backup-key")
	generated := creds[1].DeepCopy()
	generated.Annotations = map[string]string{forgejo.AnnotationBackupKey: keySourceGenerated}
	exported := generated.DeepCopy()
	exported.Annotations[forgejo.AnnotationBackupKeyExported] = valueTrue
	own := creds[1].DeepCopy() // created by the user, e.g. restored from the offline copy

	ok := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: testScheduledBackup}}
	ok.Spec.Reason = forgejov1.BackupReasonScheduled
	ok.Status.Phase = forgejov1.BackupPhaseCompleted
	ok.Status.CompletionTime = &metav1.Time{Time: time.Now().Add(-time.Hour)}
	failed := ok.DeepCopy()
	failed.Status.Phase, failed.Status.Message = forgejov1.BackupPhaseFailed, "disk full"

	for _, tc := range []struct {
		name        string
		key         *corev1.Secret
		fingerprint string // known before
		latest      *forgejov1.ForgejoBackup
		reason      string
		inMsg       string
	}{
		{name: "generated, not exported", key: generated, latest: ok, reason: "BackupKeyNotExported", inMsg: "backup-key-exported=true"},
		{name: "generated and exported", key: exported, latest: ok, reason: testReasonBackupSucceeded},
		{name: "the user's own key", key: own, latest: ok, reason: testReasonBackupSucceeded},
		{name: "missing, fingerprint known", fingerprint: "0123456789abcdef", latest: ok, reason: "BackupKeyMissing",
			inMsg: "backup-key-lost=0123456789abcdef"},
		{name: "a failed backup says more", key: generated, latest: failed, reason: "BackupFailed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := remoteInstance(nil)
			f.Spec.Backup.Enabled = true
			if tc.fingerprint != "" {
				f.Status.Backup = &forgejov1.BackupStatusSummary{Remote: &forgejov1.RemoteBackupSummary{KeySecretName: testBackupKeySecret,
					KeyFingerprint: tc.fingerprint}}
			}
			objs := []client.Object{creds[0]}
			if tc.key != nil {
				objs = append(objs, tc.key.DeepCopy())
			}
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
			rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
			key := summarizeRemote(ctx, rc, nil)
			setHealth(rc, sched, []*forgejov1.ForgejoBackup{tc.latest}, time.Now(), nil, key)
			cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBackupHealthy)
			if cond == nil || cond.Reason != tc.reason || !strings.Contains(cond.Message, tc.inMsg) {
				t.Fatalf("BackupHealthy %+v", cond)
			}
			r := f.Status.Backup.Remote
			if tc.inMsg != "" && !strings.Contains(r.Message, tc.inMsg) {
				t.Errorf("status.backup.remote.message %q", r.Message)
			}
			if tc.fingerprint != "" && r.KeyFingerprint != tc.fingerprint {
				t.Errorf("the fingerprint of the missing key was dropped: %q", r.KeyFingerprint)
			}
		})
	}
	forgetRemoteRetention(remoteNS, "fj")
}
