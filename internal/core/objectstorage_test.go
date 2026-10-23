package core

import (
	"context"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/s3"
)

const (
	img17          = "codeberg.org/forgejo/forgejo:17"
	testAccessKey  = "GKa1b2c3d4e5f6a7b8c9d0e1f2"
	testSecretKey  = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	testS3Secret   = "s3-credentials"
	testS3Endpoint = "http://garage.s3-lab.svc.cluster.local:3900"
)

func objectStorageInstance(image string) *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.Image = image
	f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{S3: forgejov1.S3Connection{
		Endpoint: testS3Endpoint, Bucket: "forgejo", Region: "garage",
		CredentialsSecret: forgejov1.S3CredentialsSecretRef{Name: testS3Secret},
	}}
	return f
}

func s3CredentialsSecret(f *forgejov1.Forgejo, access, secret string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testS3Secret, Namespace: f.Namespace},
		Data: map[string][]byte{"AWS_ACCESS_KEY_ID": []byte(access), "AWS_SECRET_ACCESS_KEY": []byte(secret)}}
}

// renderInstanceConfig runs the config step for f and returns app.ini.
func renderInstanceConfig(t *testing.T, f *forgejov1.Forgejo) string {
	t.Helper()
	ctx := context.Background()
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data: map[string][]byte{forgejo.SecretKeyInternalToken: []byte("tok")}}
	rc := testContext(t, f, secrets)
	rc.Recorder = events.NewFakeRecorder(10)
	rc.TargetImage = forgejo.Image(f)
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.DB = &forgejo.DBConnection{Host: "db", Port: 5432, Name: "forgejo", User: "forgejo", Password: "pw", SSLMode: "disable"}
	if _, err := ReconcileConfig(ctx, rc); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, sec); err != nil {
		t.Fatal(err)
	}
	return string(sec.Data[forgejo.ConfigSecretKey])
}

// Forgejo 15 and 17 read [storage] the same way (modules/setting/storage.go is identical), so
// both get the same app.ini: every subsystem inherits [storage], none has its own target, the
// access key is not in it, and [lfs] PATH (ignored with a bucket) is left out.
func TestObjectStorageConfigPerMajor(t *testing.T) {
	const want = `MINIO_BUCKET = forgejo
MINIO_BUCKET_LOOKUP = auto
MINIO_ENDPOINT = garage.s3-lab.svc.cluster.local:3900
MINIO_INSECURE_SKIP_VERIFY = false
MINIO_LOCATION = garage
MINIO_USE_SSL = false
SERVE_DIRECT = false
STORAGE_TYPE = minio
`
	var first string
	for _, image := range []string{img15, img17, "codeberg.org/forgejo/forgejo:17-rootless"} {
		ini := renderInstanceConfig(t, objectStorageInstance(image))
		if got := section(ini, "storage"); got != want {
			t.Errorf("%s: [storage]\n%s\nwant\n%s", image, got, want)
		}
		for _, s := range forgejo.StorageSubsystems {
			if strings.Contains(section(ini, s.Section), "STORAGE_TYPE") {
				t.Errorf("%s: [%s] must inherit [storage]:\n%s", image, s.Section, ini)
			}
		}
		if strings.Contains(ini, "\n[lfs]\n") || strings.Contains(ini, "MINIO_ACCESS_KEY_ID") || strings.Contains(ini, "MINIO_SECRET_ACCESS_KEY") {
			t.Errorf("%s: no [lfs] PATH and no access key expected:\n%s", image, ini)
		}
		body := section(ini, "storage")
		if first == "" {
			first = body
		} else if body != first {
			t.Errorf("%s: [storage] differs between majors", image)
		}
	}

	// Without spec.objectStorage nothing changes: no [storage], LFS on the volume.
	ini := renderInstanceConfig(t, testInstance())
	if strings.Contains(ini, "[storage") || section(ini, "lfs") != "PATH = /data/git/lfs\n" {
		t.Errorf("without objectStorage:\n%s", ini)
	}
}

func TestObjectStorageConfigOptions(t *testing.T) {
	f := objectStorageInstance(img17)
	os := f.Spec.ObjectStorage
	os.S3.Endpoint = "https://s3.example.com"
	os.S3.Region = ""
	os.S3.PathStyle = ptr.To(true)
	os.S3.Prefix = "team-a/forgejo/"
	os.S3.TLS = &forgejov1.S3TLSSpec{InsecureSkipVerify: true}
	os.ServeDirect = true
	os.Subsystems = forgejov1.ObjectStorageSubsystems{LFS: ptr.To(false), Avatars: ptr.To(false), ActionsLogs: ptr.To(false),
		ActionsArtifacts: ptr.To(false), Attachments: ptr.To(true)}
	f.Spec.Config = map[string]map[string]string{"storage": {"MINIO_CHECKSUM_ALGORITHM": "md5"}}
	ini := renderInstanceConfig(t, f)

	st := section(ini, "storage")
	for _, line := range []string{"MINIO_ENDPOINT = s3.example.com\n", "MINIO_USE_SSL = true\n", "MINIO_INSECURE_SKIP_VERIFY = true\n",
		"MINIO_LOCATION = us-east-1\n", "MINIO_BUCKET_LOOKUP = path\n", "MINIO_BASE_PATH = team-a/forgejo/\n", "SERVE_DIRECT = true\n",
		"MINIO_CHECKSUM_ALGORITHM = md5\n"} {
		if !strings.Contains(st, line) {
			t.Errorf("[storage] lacks %q:\n%s", line, st)
		}
	}
	// Subsystems kept on the volume become their own (local) target; the others inherit.
	for sec, want := range map[string]string{
		"lfs":                 "PATH = /data/git/lfs\nSTORAGE_TYPE = local\n",
		"avatar":              "STORAGE_TYPE = local\n",
		"storage.actions_log": "STORAGE_TYPE = local\n",
		"actions.artifacts":   "STORAGE_TYPE = local\n",
		"attachment":          "",
		"repo-avatar":         "",
		"packages":            "",
	} {
		if got := section(ini, sec); got != want {
			t.Errorf("[%s] = %q, want %q", sec, got, want)
		}
	}

	os.S3.PathStyle = ptr.To(false)
	if st := section(renderInstanceConfig(t, f), "storage"); !strings.Contains(st, "MINIO_BUCKET_LOOKUP = dns\n") {
		t.Errorf("pathStyle false:\n%s", st)
	}
}

// envOf maps a container's environment by name.
func envOf(c corev1.Container) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	return env
}

// s3Containers are the bootstrap and forgejo containers, which both get the bucket.
func s3Containers(spec corev1.PodSpec) []corev1.Container {
	return []corev1.Container{spec.InitContainers[len(spec.InitContainers)-1], spec.Containers[0]}
}

// secretRefKey is the Secret key an env var references, or "" when it has a literal value.
func secretRefKey(e corev1.EnvVar) string {
	if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil || e.ValueFrom.SecretKeyRef.Name != testS3Secret {
		return ""
	}
	return e.ValueFrom.SecretKeyRef.Key
}

func TestObjectStoragePodSpecCredentials(t *testing.T) {
	f := objectStorageInstance(img17)
	rc := &forgejo.ReconcileContext{Instance: f, TargetImage: img17, Layout: forgejo.LayoutForImage(img17), ConfigSecretName: "fj-config"}
	for _, c := range s3Containers(podSpec(rc)) {
		env := envOf(c)
		if secretRefKey(env["AWS_ACCESS_KEY_ID"]) != "AWS_ACCESS_KEY_ID" || secretRefKey(env["AWS_SECRET_ACCESS_KEY"]) != "AWS_SECRET_ACCESS_KEY" {
			t.Fatalf("%s: the access key must come from the Secret: %+v", c.Name, c.Env)
		}
		if _, ok := env["SSL_CERT_DIR"]; ok {
			t.Errorf("%s: SSL_CERT_DIR without a CA", c.Name)
		}
	}

	// Without object storage the pod is unchanged.
	f.Spec.ObjectStorage = nil
	for _, c := range podSpec(rc).Containers {
		for _, e := range c.Env {
			if strings.HasPrefix(e.Name, "AWS_") || e.Name == "SSL_CERT_DIR" {
				t.Fatalf("without objectStorage: %s", e.Name)
			}
		}
	}
}

// A CA certificate is mounted into both containers and added to the trust store.
func TestObjectStoragePodSpecCA(t *testing.T) {
	f := objectStorageInstance(img17)
	f.Spec.ObjectStorage.S3.Endpoint = "https://s3.example.com"
	f.Spec.ObjectStorage.S3.CredentialsSecret.AccessKeyIDKey = "id"
	f.Spec.ObjectStorage.S3.TLS = &forgejov1.S3TLSSpec{CASecret: &forgejov1.SecretKeyRef{Name: "s3-ca"}}
	rc := &forgejo.ReconcileContext{Instance: f, TargetImage: img17, Layout: forgejo.LayoutForImage(img17), ConfigSecretName: "fj-config"}
	spec := podSpec(rc)
	i := slices.IndexFunc(spec.Volumes, func(v corev1.Volume) bool { return v.Name == objectStorageCAVolume })
	if i < 0 {
		t.Fatal("no CA volume")
	}
	if v := spec.Volumes[i].Secret; v.SecretName != "s3-ca" || len(v.Items) != 1 || v.Items[0].Key != "ca.crt" || v.Items[0].Path != objectStorageCAFile {
		t.Fatalf("CA volume: %+v", v)
	}
	want := corev1.VolumeMount{Name: objectStorageCAVolume, MountPath: "/etc/forgejo-s3-ca", ReadOnly: true}
	for _, c := range s3Containers(spec) {
		env := envOf(c)
		if !slices.Contains(c.VolumeMounts, want) || env["SSL_CERT_DIR"].Value != "/etc/ssl/certs:/etc/forgejo-s3-ca" ||
			secretRefKey(env["AWS_ACCESS_KEY_ID"]) != "id" {
			t.Fatalf("%s: mounts %v env %v", c.Name, c.VolumeMounts, env)
		}
	}
}

// osStepEnv runs the object-storage step against a stubbed bucket check.
type osStepEnv struct {
	f      *forgejov1.Forgejo
	rc     *forgejo.ReconcileContext
	rec    *events.FakeRecorder
	calls  []s3.Bucket
	result s3.Result
}

func newOSStepEnv(t *testing.T, objs ...client.Object) *osStepEnv {
	t.Helper()
	e := &osStepEnv{f: objectStorageInstance(img17), rec: events.NewFakeRecorder(20),
		result: s3.Result{OK: true, Reason: s3.ReasonReachable, Message: "Bucket \"forgejo\" is reachable"}}
	e.rc = testContext(t, e.f, objs...)
	e.rc.Recorder = e.rec
	old := checkBucket
	checkBucket = func(_ context.Context, b s3.Bucket) s3.Result { e.calls = append(e.calls, b); return e.result }
	t.Cleanup(func() { checkBucket = old; ForgetObjectStorage(e.f.Namespace, e.f.Name) })
	return e
}

// run runs the step and checks that neither the condition nor an event shows the credentials.
func (e *osStepEnv) run(t *testing.T) forgejo.StepResult {
	t.Helper()
	res, err := ReconcileObjectStorage(context.Background(), e.rc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.f.Status.Conditions {
		if strings.Contains(c.Message, testSecretKey) || strings.Contains(c.Message, testAccessKey) {
			t.Fatalf("condition %s leaks credentials: %s", c.Type, c.Message)
		}
	}
	for _, ev := range drainEvents(e.rec) {
		if strings.Contains(ev, testSecretKey) || strings.Contains(ev, testAccessKey) {
			t.Fatalf("event leaks credentials: %s", ev)
		}
	}
	return res
}

func (e *osStepEnv) wantCondition(t *testing.T, status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(e.f.Status.Conditions, forgejov1.ConditionObjectStorageReady)
	if c == nil || c.Status != status || c.Reason != reason {
		t.Fatalf("ObjectStorageReady = %+v, want %s/%s", c, status, reason)
	}
	return c
}

// Without usable credentials the steps that need it wait, and nothing is checked.
func TestReconcileObjectStorageMissingCredentials(t *testing.T) {
	e := newOSStepEnv(t)
	if res := e.run(t); !res.Stop || e.rc.ObjectStorage != nil || !e.rc.ObjectStorageUnavailable || len(e.calls) != 0 {
		t.Fatalf("missing Secret: %+v %+v %d", res, e.rc.ObjectStorage, len(e.calls))
	}
	e.wantCondition(t, metav1.ConditionFalse, reasonCredentialsUnavailable)

	// Turning object storage off removes the condition.
	e.f.Spec.ObjectStorage = nil
	e.rc.ObjectStorageUnavailable = false
	if res := e.run(t); res.Stop || e.rc.ObjectStorageUnavailable ||
		meta.FindStatusCondition(e.f.Status.Conditions, forgejov1.ConditionObjectStorageReady) != nil {
		t.Fatalf("disabled: %+v %+v", res, e.f.Status.Conditions)
	}
}

func TestReconcileObjectStorageCheck(t *testing.T) {
	ctx := context.Background()
	f := objectStorageInstance(img17)
	creds := s3CredentialsSecret(f, testAccessKey, testSecretKey)
	e := newOSStepEnv(t, creds)

	if res := e.run(t); res.Stop || e.rc.ObjectStorage == nil || e.rc.ObjectStorage.Digest == "" || e.rc.ObjectStorageUnavailable || len(e.calls) != 1 {
		t.Fatalf("reachable: %+v %+v %d", res, e.rc.ObjectStorage, len(e.calls))
	}
	b := e.calls[0]
	if b.Scheme != "http" || b.Host != "garage.s3-lab.svc.cluster.local:3900" || b.Name != "forgejo" || b.Region != "garage" ||
		b.VirtualHost || b.AccessKeyID != testAccessKey || b.SecretAccessKey != testSecretKey || b.DialContext == nil {
		t.Fatalf("checked %+v", b)
	}
	e.wantCondition(t, metav1.ConditionTrue, s3.ReasonReachable)
	digest1 := e.rc.ObjectStorage.Digest

	// Within the interval and with the same inputs, the bucket is not checked again.
	if e.run(t); len(e.calls) != 1 {
		t.Fatalf("cached: %d checks", len(e.calls))
	}

	// Rotated credentials: a new digest (the pods roll) and a new check, which fails. A bucket
	// the operator cannot reach is reported, but neither stops the pipeline nor holds what
	// replaces pods: Forgejo pods may reach it (NetworkPolicy, operator outside the cluster).
	creds.Data["AWS_SECRET_ACCESS_KEY"] = []byte("rotated-" + testSecretKey)
	if err := e.rc.Client.Update(ctx, creds); err != nil {
		t.Fatal(err)
	}
	e.result = s3.Result{Reason: s3.ReasonUnreachable, Message: "garage.s3-lab.svc.cluster.local:3900: i/o timeout"}
	if res := e.run(t); res.Stop || len(e.calls) != 2 || e.rc.ObjectStorage.Digest == digest1 || e.rc.ObjectStorageUnavailable {
		t.Fatalf("rotated: %+v %d %s unavailable=%t", res, len(e.calls), e.rc.ObjectStorage.Digest, e.rc.ObjectStorageUnavailable)
	}
	if c := e.wantCondition(t, metav1.ConditionFalse, reasonUnreachableFromOperator); !strings.Contains(c.Message, "i/o timeout") {
		t.Fatalf("unreachable: %s", c.Message)
	}

	// An answer that stops Forgejo too holds replacements, without stopping the pipeline.
	e.result = s3.Result{Reason: s3.ReasonDenied, Message: "Listing bucket \"forgejo\" was refused (HTTP 403 InvalidAccessKeyId)"}
	ForgetObjectStorage(f.Namespace, f.Name)
	if res := e.run(t); res.Stop || !e.rc.ObjectStorageUnavailable {
		t.Fatalf("access denied must hold replacements but not stop the pipeline: %+v %t", res, e.rc.ObjectStorageUnavailable)
	}
	if c := e.wantCondition(t, metav1.ConditionFalse, s3.ReasonDenied); !strings.Contains(c.Message, testS3Secret) {
		t.Fatalf("denied: %s", c.Message)
	}
}

// The pod template carries the digest, so that rotated credentials (read from the environment
// at start) roll the pods.
func TestReconcileWorkloadObjectStorageDigest(t *testing.T) {
	ctx := context.Background()
	f := objectStorageInstance(img17)
	rc := testContext(t, f)
	rc.TargetImage = img17
	rc.Layout = forgejo.LayoutForImage(img17)
	rc.ConfigHash, rc.ConfigSecretName = "h", "fj-config"
	rc.ObjectStorage = &forgejo.ObjectStorage{Digest: "d1"}
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	dep := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err != nil {
		t.Fatal(err)
	}
	got := dep.Spec.Template.Annotations
	if got[forgejo.AnnotationObjectStorage] != "d1" {
		t.Fatalf("annotations %v", got)
	}
}

// An instance that waits for its object storage Secrets says so in Ready.
func TestNotDeployedMessageNamesObjectStorage(t *testing.T) {
	f := objectStorageInstance(img17)
	rc := &forgejo.ReconcileContext{Instance: f}
	rc.SetCondition(forgejov1.ConditionObjectStorageReady, metav1.ConditionFalse, reasonCredentialsUnavailable, `secret "s3" not found`)
	if got := notDeployedMessage(rc); got != `Waiting for spec.objectStorage: secret "s3" not found` {
		t.Fatalf("got %q", got)
	}
	rc.SetCondition(forgejov1.ConditionObjectStorageReady, metav1.ConditionFalse, reasonUnreachableFromOperator, "x")
	if got := notDeployedMessage(rc); got != "Waiting for the Deployment to be created" {
		t.Fatalf("an unreachable bucket does not hold the Deployment: %q", got)
	}
}
