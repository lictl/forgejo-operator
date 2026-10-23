package core

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testSSHHostKeysSecret  = "fj-ssh-host-keys"
	testExistingKeysSecret = "my-keys"
)

const hostKeysImage = "codeberg.org/forgejo/forgejo:15"

func init() {
	// RSA 3072 takes long to generate; the size does not matter for the tests.
	rsaHostKeyBits = 2048
}

// hostKeysEnv is a Forgejo instance with its generated secrets, ready to run the ssh-host-keys,
// config and workload steps.
type hostKeysEnv struct {
	t   *testing.T
	ctx context.Context
	rc  *forgejo.ReconcileContext
	f   *forgejov1.Forgejo
}

func newHostKeysEnv(t *testing.T, mutate func(f *forgejov1.Forgejo), objs ...client.Object) *hostKeysEnv {
	t.Helper()
	f := testInstance()
	f.Spec.Image = hostKeysImage
	f.UID = types.UID(t.Name())
	if mutate != nil {
		mutate(f)
	}
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data: map[string][]byte{forgejo.SecretKeyInternalToken: []byte("token")}}
	objs = append(objs, secrets)
	rc := testContext(t, f, objs...)
	rc.APIReader = rc.Client
	rc.TargetImage = forgejo.Image(f)
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.ConfigSecretName = forgejo.ConfigSecretName(f)
	rc.DB = &forgejo.DBConnection{Host: "db", Port: 5432, Name: "fjdb", User: "fjuser", Password: "pw", SSLMode: testSSLModeDisable}
	return &hostKeysEnv{t: t, ctx: context.Background(), rc: rc, f: f}
}

// pass runs ssh-host-keys, config and workload as the pipeline does and returns the
// Deployment and app.ini.
func (e *hostKeysEnv) pass() (*appsv1.Deployment, string) {
	e.t.Helper()
	e.rc.SSHHostKeys, e.rc.SSHHostKeysUnavailable = nil, false
	e.rc.ConfigHash = ""
	if _, err := ReconcileSSHHostKeys(e.ctx, e.rc); err != nil {
		e.t.Fatalf("ssh-host-keys: %v", err)
	}
	if _, err := ReconcileConfig(e.ctx, e.rc); err != nil {
		e.t.Fatalf("config: %v", err)
	}
	if _, err := ReconcileWorkload(e.ctx, e.rc); err != nil {
		e.t.Fatalf("workload: %v", err)
	}
	return e.deployment(), e.appINI()
}

func (e *hostKeysEnv) deployment() *appsv1.Deployment {
	e.t.Helper()
	d := &appsv1.Deployment{}
	if err := e.rc.Client.Get(e.ctx, types.NamespacedName{Namespace: e.f.Namespace, Name: forgejo.DeploymentName(e.f)}, d); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *hostKeysEnv) appINI() string {
	e.t.Helper()
	sec := &corev1.Secret{}
	if err := e.rc.Client.Get(e.ctx, types.NamespacedName{Namespace: e.f.Namespace, Name: forgejo.ConfigSecretName(e.f)}, sec); err != nil {
		e.t.Fatal(err)
	}
	return string(sec.Data[forgejo.ConfigSecretKey])
}

func (e *hostKeysEnv) secret(name string) (*corev1.Secret, error) {
	sec := &corev1.Secret{}
	err := e.rc.Client.Get(e.ctx, types.NamespacedName{Namespace: e.f.Namespace, Name: name}, sec)
	return sec, err
}

func fingerprintOf(t *testing.T, key crypto.PrivateKey) string {
	t.Helper()
	s, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(s.PublicKey())
}

func statusFingerprints(f *forgejov1.Forgejo) []string {
	if f.Status.SSH == nil {
		return nil
	}
	var out []string
	for _, k := range f.Status.SSH.HostKeys {
		out = append(out, k.Fingerprint)
	}
	return out
}

func mountsHostKeys(d *appsv1.Deployment) bool { return servedHostKeys(d) != nil }

func checkGeneratedSecret(t *testing.T, e *hostKeysEnv) (*corev1.Secret, []hostKey) {
	t.Helper()
	sec, err := e.secret(forgejo.SSHHostKeysSecretName(e.f))
	if err != nil {
		t.Fatal(err)
	}
	if sec.Annotations[AnnotationSSHHostKeysSource] != string(forgejov1.SSHHostKeysGenerated) ||
		sec.Labels[forgejo.LabelManagedBy] != forgejo.ManagedBy || len(sec.OwnerReferences) != 0 {
		t.Fatalf("Secret metadata: %+v", sec.ObjectMeta)
	}
	keys, err := parseHostKeys(sec.Data)
	if err != nil || len(keys) != 3 {
		t.Fatalf("want ed25519, ECDSA and RSA keys: %v %v", keys, err)
	}
	for _, file := range HostKeyFiles {
		if !strings.HasPrefix(string(sec.Data[file]), "-----BEGIN OPENSSH PRIVATE KEY-----") {
			t.Errorf("%s is not in OpenSSH format", file)
		}
	}
	return sec, keys
}

func checkGeneratedStatus(t *testing.T, f *forgejov1.Forgejo) {
	t.Helper()
	st := f.Status.SSH
	if st == nil || st.HostKeysSource != forgejov1.SSHHostKeysGenerated || len(st.HostKeys) != 3 ||
		st.HostKeysSecretName != forgejo.SSHHostKeysSecretName(f) {
		t.Fatalf("status.ssh: %+v", st)
	}
	wantTypes := []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSA}
	for i, k := range st.HostKeys {
		if k.Type != wantTypes[i] || !strings.HasPrefix(k.Fingerprint, "SHA256:") || !strings.HasPrefix(k.PublicKey, k.Type+" ") {
			t.Errorf("host key %d: %+v", i, k)
		}
	}
}

func TestNewInstanceGeneratesHostKeys(t *testing.T) {
	e := newHostKeysEnv(t, nil)
	d, ini := e.pass()

	sec, keys := checkGeneratedSecret(t, e)
	checkGeneratedStatus(t, e.f)

	want := "SSH_SERVER_HOST_KEYS = /etc/forgejo-ssh/ssh_host_ed25519_key,/etc/forgejo-ssh/ssh_host_ecdsa_key,/etc/forgejo-ssh/ssh_host_rsa_key"
	if !strings.Contains(section(ini, "server"), want) {
		t.Fatalf("app.ini: want %q:\n%s", want, ini)
	}
	served := servedHostKeys(d)
	if served == nil || served.SecretName != sec.Name || len(served.Files) != 3 || served.Digest != hostKeysDigest(keys) {
		t.Fatalf("Deployment does not mount the keys: %+v", served)
	}
	checkHostKeysPodLayout(t, d, served.Digest)

	// Later passes change nothing: the keys are never rotated.
	rv := d.ResourceVersion
	d2, _ := e.pass()
	sec2, _ := e.secret(sec.Name)
	if d2.ResourceVersion != rv || !equality.Semantic.DeepEqual(sec.Data, sec2.Data) {
		t.Fatal("a second pass changed the Deployment or the keys")
	}
}

// checkHostKeysPodLayout checks that the forgejo container serves the pod's copy of the keys
// (an emptyDir in memory, read-only), which the first init container fills from the Secret and
// checks against the digest; no other container mounts either volume.
func checkHostKeysPodLayout(t *testing.T, d *appsv1.Deployment, digest string) {
	t.Helper()
	spec := d.Spec.Template.Spec
	mounts := func(c corev1.Container) map[string]corev1.VolumeMount {
		out := map[string]corev1.VolumeMount{}
		for _, m := range c.VolumeMounts {
			if m.Name == sshHostKeysVolume || m.Name == sshHostKeysCopyVolume {
				out[m.Name] = m
			}
		}
		return out
	}
	main := mounts(spec.Containers[0])
	if m, ok := main[sshHostKeysCopyVolume]; len(main) != 1 || !ok || !m.ReadOnly || m.MountPath != "/etc/forgejo-ssh" {
		t.Fatalf("forgejo container mounts: %+v", main)
	}
	init := spec.InitContainers[0]
	im := mounts(init)
	if init.Name != sshHostKeysContainer || !im[sshHostKeysVolume].ReadOnly || im[sshHostKeysVolume].MountPath != hostKeysSecretDir ||
		im[sshHostKeysCopyVolume].ReadOnly || im[sshHostKeysCopyVolume].MountPath != "/etc/forgejo-ssh" {
		t.Fatalf("first init container %s mounts: %+v", init.Name, im)
	}
	env := map[string]string{}
	for _, e := range init.Env {
		env[e.Name] = e.Value
	}
	if env["SSH_HOST_KEYS_DIGEST"] != digest || d.Spec.Template.Annotations[forgejo.AnnotationSSHHostKeys] != digest {
		t.Fatalf("digest: env %q, annotation %q, want %q", env["SSH_HOST_KEYS_DIGEST"], d.Spec.Template.Annotations[forgejo.AnnotationSSHHostKeys], digest)
	}
	if sc := init.SecurityContext; sc == nil || !ptr.Deref(sc.RunAsNonRoot, false) || !ptr.Deref(sc.ReadOnlyRootFilesystem, false) {
		t.Fatalf("init container security context: %+v", sc)
	}
	for _, c := range spec.InitContainers[1:] {
		if len(mounts(c)) != 0 {
			t.Fatalf("init container %s does not need the host keys", c.Name)
		}
	}
	for _, v := range spec.Volumes {
		if v.Name == sshHostKeysCopyVolume && (v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory) {
			t.Fatalf("the copy must be an emptyDir in memory: %+v", v)
		}
	}
}

func TestHostKeysSecretFollowsRetainPolicy(t *testing.T) {
	e := newHostKeysEnv(t, func(f *forgejov1.Forgejo) { f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete })
	e.pass()
	sec, err := e.secret(forgejo.SSHHostKeysSecretName(e.f))
	if err != nil || len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != e.f.UID {
		t.Fatalf("retainPolicy Delete: want an owner reference, got %+v (%v)", sec.OwnerReferences, err)
	}
	e.f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyRetain
	e.pass()
	if sec, _ = e.secret(sec.Name); len(sec.OwnerReferences) != 0 {
		t.Fatalf("retainPolicy Retain: want no owner reference, got %+v", sec.OwnerReferences)
	}
}

// rsaKey is an RSA key, as other tools write them in PKCS#1 PEM.
func rsaKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A Secret that outlived its Forgejo (retainPolicy Retain) or that the user created before the
// instance is served as it is.
func TestExistingHostKeysSecretIsReused(t *testing.T) {
	first := newHostKeysEnv(t, nil)
	first.pass()
	kept, keys := checkGeneratedSecret(t, first)

	// The Forgejo object is recreated with the same name: a new UID, no status, no Deployment.
	kept.ResourceVersion = ""
	e := newHostKeysEnv(t, nil, kept)
	e.f.UID = types.UID(t.Name() + "-2")
	d, _ := e.pass()
	if got, _ := e.secret(kept.Name); !equality.Semantic.DeepEqual(got.Data, kept.Data) {
		t.Fatal("the retained keys were replaced")
	}
	if s := servedHostKeys(d); s == nil || s.Digest != hostKeysDigest(keys) {
		t.Fatalf("served: %+v", s)
	}
	checkGeneratedStatus(t, e.f)

	// Created by the user (no source annotation): labelled, and reported as ExistingSecret.
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	pemBytes, err := marshalHostKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testSSHHostKeysSecret, Namespace: "ns"}, Data: map[string][]byte{HostKeyEd25519: pemBytes}}
	e = newHostKeysEnv(t, nil, own)
	e.pass()
	got, _ := e.secret(own.Name)
	if got.Labels[forgejo.LabelManagedBy] != forgejo.ManagedBy || !equality.Semantic.DeepEqual(got.Data, own.Data) {
		t.Fatalf("user Secret: %+v", got.ObjectMeta)
	}
	if st := e.f.Status.SSH; st.HostKeysSource != forgejov1.SSHHostKeysExistingSecret || statusFingerprints(e.f)[0] != fingerprintOf(t, ed) {
		t.Fatalf("status: %+v", st)
	}
}

func TestMissingHostKeysSecretIsNeverRegenerated(t *testing.T) {
	e := newHostKeysEnv(t, nil)
	d, ini := e.pass()
	served := servedHostKeys(d)
	fps := statusFingerprints(e.f)
	sec, _ := e.secret(forgejo.SSHHostKeysSecretName(e.f))
	if err := e.rc.Client.Delete(e.ctx, sec); err != nil {
		t.Fatal(err)
	}
	// A spec change meanwhile would roll the pods, and new pods could not start: the Deployment
	// and the app.ini the pods read are held as they are.
	e.f.Spec.Config = map[string]map[string]string{"ui": {"DEFAULT_THEME": "forgejo-dark"}}

	for range 2 {
		d2, ini2 := e.pass()
		if !equality.Semantic.DeepEqual(served, servedHostKeys(d2)) || ini2 != ini || d2.ResourceVersion != d.ResourceVersion {
			t.Fatal("the Deployment and its app.ini must stay as they are")
		}
		if !e.rc.SSHHostKeysUnavailable {
			t.Fatal("rc.SSHHostKeysUnavailable must hold the other steps")
		}
		if c := meta.FindStatusCondition(e.f.Status.Conditions, forgejov1.ConditionDegraded); c == nil || c.Reason != ReasonSSHHostKeysUnavailable {
			t.Fatalf("want Degraded/%s, got %+v", ReasonSSHHostKeysUnavailable, c)
		}
		if _, err := e.secret(sec.Name); !apierrors.IsNotFound(err) {
			t.Fatalf("new keys must not be generated for an instance that has some: %v", err)
		}
		if got := statusFingerprints(e.f); !equality.Semantic.DeepEqual(got, fps) {
			t.Fatalf("status lost the fingerprints: %v", got)
		}
	}

	// Restoring the Secret ends it, and the held change is rolled out.
	sec.ResourceVersion = ""
	if err := e.rc.Client.Create(e.ctx, sec); err != nil {
		t.Fatal(err)
	}
	d3, ini3 := e.pass()
	if c := meta.FindStatusCondition(e.f.Status.Conditions, forgejov1.ConditionDegraded); c != nil && c.Status == metav1.ConditionTrue {
		t.Fatalf("still Degraded: %+v", c)
	}
	if !strings.Contains(ini3, "DEFAULT_THEME = forgejo-dark") || d3.ResourceVersion == d.ResourceVersion ||
		!equality.Semantic.DeepEqual(served, servedHostKeys(d3)) {
		t.Fatal("the held change must be rolled out with the same keys")
	}
}

// The init container copies the keys and refuses files other than the ones the operator
// checked: Forgejo would serve a random key instead of keys it cannot load.
func TestHostKeysScript(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}
	data, err := generateHostKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseHostKeys(data)
	if err != nil {
		t.Fatal(err)
	}
	src, dst := t.TempDir(), t.TempDir()
	write := func(file string, b []byte) {
		p := filepath.Join(src, file)
		_ = os.Remove(p)
		if err := os.WriteFile(p, b, 0o440); err != nil {
			t.Fatal(err)
		}
	}
	for file, b := range data {
		write(file, b)
	}
	digest := hostKeysDigest(keys)
	run := func() (string, error) {
		cmd := exec.Command("/bin/sh", "-c", hostKeysScript(src, dst))
		cmd.Env = append(os.Environ(), "SSH_HOST_KEYS_SECRET=fj-ssh-host-keys",
			"SSH_HOST_KEYS_FILES="+strings.Join(HostKeyFiles, " "), "SSH_HOST_KEYS_DIGEST="+digest)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	for range 2 { // a re-run init container replaces its read-only copies
		if out, err := run(); err != nil {
			t.Fatalf("matching keys: %v\n%s", err, out)
		}
		for file, b := range data {
			got, err := os.ReadFile(filepath.Join(dst, file))
			st, _ := os.Stat(filepath.Join(dst, file))
			if err != nil || !bytes.Equal(got, b) || st.Mode().Perm() != 0o400 {
				t.Fatalf("%s: copy differs or mode %v (%v)", file, st.Mode(), err)
			}
		}
	}

	// The Secret now holds a public key where the private key was (as after a broken edit).
	pub := ssh.MarshalAuthorizedKey(keys[0].pub)
	write(HostKeyEd25519, pub)
	out, err := run()
	if err == nil || !strings.Contains(out, "holds other SSH host keys") || !strings.Contains(out, digest) {
		t.Fatalf("want a refusal naming the digest, got %v:\n%s", err, out)
	}
	if left, _ := os.ReadDir(dst); len(left) != 0 {
		t.Fatalf("refused keys must not stay in the pod: %v", left)
	}
}

func TestExistingSecretHostKeys(t *testing.T) {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	pemBytes, err := marshalHostKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	own := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: testExistingKeysSecret, Namespace: "ns"}, Data: map[string][]byte{HostKeyEd25519: pemBytes, "other": []byte("x")}}
	e := newHostKeysEnv(t, func(f *forgejov1.Forgejo) {
		f.Spec.Server.SSH.HostKeys = &forgejov1.SSHHostKeysSpec{ExistingSecret: testExistingKeysSecret}
	}, own)
	d, ini := e.pass()
	if !strings.Contains(section(ini, "server"), "SSH_SERVER_HOST_KEYS = /etc/forgejo-ssh/ssh_host_ed25519_key\n") {
		t.Fatalf("app.ini:\n%s", ini)
	}
	if s := servedHostKeys(d); s == nil || s.SecretName != testExistingKeysSecret {
		t.Fatalf("served: %+v", s)
	}
	if _, err := e.secret(forgejo.SSHHostKeysSecretName(e.f)); !apierrors.IsNotFound(err) {
		t.Fatal("no operator Secret with existingSecret")
	}
	if st := e.f.Status.SSH; st.HostKeysSource != forgejov1.SSHHostKeysExistingSecret || statusFingerprints(e.f)[0] != fingerprintOf(t, ed) {
		t.Fatalf("status: %+v", st)
	}

	// A broken Secret keeps the pods on the keys they serve, Degraded.
	own.Data = map[string][]byte{HostKeyEd25519: []byte("garbage")}
	if err := e.rc.Client.Update(e.ctx, own); err != nil {
		t.Fatal(err)
	}
	d2, _ := e.pass()
	if d2.ResourceVersion != d.ResourceVersion {
		t.Fatal("a broken Secret must not change the Deployment")
	}
	if c := meta.FindStatusCondition(e.f.Status.Conditions, forgejov1.ConditionDegraded); c == nil || c.Reason != ReasonSSHHostKeysUnavailable {
		t.Fatalf("want Degraded: %+v", c)
	}

	// A missing existingSecret on a new instance fails the step.
	e2 := newHostKeysEnv(t, func(f *forgejov1.Forgejo) {
		f.Spec.Server.SSH.HostKeys = &forgejov1.SSHHostKeysSpec{ExistingSecret: "absent"}
	})
	if _, err := ReconcileSSHHostKeys(e2.ctx, e2.rc); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("want an error naming the Secret, got %v", err)
	}
}

func TestUnmanagedHostKeys(t *testing.T) {
	for name, mutate := range map[string]func(f *forgejov1.Forgejo){
		"ssh disabled": func(f *forgejov1.Forgejo) { f.Spec.Server.SSH.Enabled = ptr.To(false) },
		"spec.config": func(f *forgejov1.Forgejo) {
			f.Spec.Config = map[string]map[string]string{"SERVER": {"SSH_SERVER_HOST_KEYS": "/x"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newHostKeysEnv(t, mutate)
			d, ini := e.pass()
			if _, err := e.secret(forgejo.SSHHostKeysSecretName(e.f)); !apierrors.IsNotFound(err) {
				t.Fatal("no Secret for unmanaged host keys")
			}
			if mountsHostKeys(d) || e.f.Status.SSH != nil {
				t.Fatal("unmanaged host keys must be left alone")
			}
			if strings.Contains(ini, "/etc/forgejo-ssh") {
				t.Fatalf("app.ini:\n%s", ini)
			}
		})
	}
}

func TestParseHostKeys(t *testing.T) {
	data, err := generateHostKeys()
	if err != nil {
		t.Fatal(err)
	}
	keys, err := parseHostKeys(data)
	if err != nil || len(keys) != 3 {
		t.Fatalf("%v %v", keys, err)
	}
	// Two keys of one type: Forgejo would serve only one.
	dup := map[string][]byte{HostKeyEd25519: data[HostKeyEd25519], HostKeyECDSA: data[HostKeyEd25519]}
	if _, err := parseHostKeys(dup); err == nil || !strings.Contains(err.Error(), "one key of each type") {
		t.Fatalf("duplicate type: %v", err)
	}
	// A key with a passphrase cannot be loaded by Forgejo.
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKeyWithPassphrase(ed, "", []byte("secret"))
	if _, err := parseHostKeys(map[string][]byte{HostKeyEd25519: pem.EncodeToMemory(block)}); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("encrypted: %v", err)
	}
	if _, err := parseHostKeys(map[string][]byte{"unrelated": []byte("x")}); err == nil {
		t.Fatal("no keys must be an error")
	}
	// PEM formats as written by other tools are accepted too.
	rk := rsaKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(rk)})
	if keys, err := parseHostKeys(map[string][]byte{HostKeyRSA: pkcs1}); err != nil || ssh.FingerprintSHA256(keys[0].pub) != fingerprintOf(t, rk) {
		t.Fatalf("PKCS#1: %v", err)
	}
}

// A "<name>-ssh-host-keys" that another controller controls (a SealedSecret or ExternalSecret
// of a GitOps setup, created before the instance for a disaster recovery) is served as it is,
// but never adopted: with retainPolicy Delete a controller reference would be refused
// (AlreadyOwnedError) and block config and workload on every pass.
func TestHostKeysSecretControlledByAnotherController(t *testing.T) {
	_, ed, _ := ed25519.GenerateKey(rand.Reader)
	pemBytes, err := marshalHostKey(ed)
	if err != nil {
		t.Fatal(err)
	}
	sealed := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testSSHHostKeysSecret, Namespace: "ns",
			Labels: map[string]string{"sealedsecrets.bitnami.com/managed": valueTrue},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "bitnami.com/v1alpha1", Kind: "SealedSecret",
				Name: testSSHHostKeysSecret, UID: "sealed-uid", Controller: ptr.To(true)}}},
		Data: map[string][]byte{HostKeyEd25519: pemBytes},
	}
	e := newHostKeysEnv(t, func(f *forgejov1.Forgejo) { f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete }, sealed)
	before, err := e.secret(sealed.Name)
	if err != nil {
		t.Fatal(err)
	}
	d, ini := e.pass()
	if e.rc.SSHHostKeys == nil || e.rc.SSHHostKeys.SecretName != sealed.Name || !mountsHostKeys(d) ||
		!strings.Contains(ini, "SSH_SERVER_HOST_KEYS = /etc/forgejo-ssh/"+HostKeyEd25519) {
		t.Fatalf("keys not served: %+v\n%s", e.rc.SSHHostKeys, ini)
	}
	if st := e.f.Status.SSH; st == nil || st.HostKeysSource != forgejov1.SSHHostKeysExistingSecret || statusFingerprints(e.f)[0] != fingerprintOf(t, ed) {
		t.Fatalf("status: %+v", st)
	}
	after, _ := e.secret(sealed.Name)
	if after.ResourceVersion != before.ResourceVersion || !equality.Semantic.DeepEqual(after.Labels, before.Labels) ||
		!equality.Semantic.DeepEqual(after.OwnerReferences, before.OwnerReferences) {
		t.Fatalf("the Secret was changed: %+v", after.ObjectMeta)
	}
	// A second pass is just as quiet.
	e.pass()
	if again, _ := e.secret(sealed.Name); again.ResourceVersion != before.ResourceVersion {
		t.Fatal("the Secret was changed on the second pass")
	}
}

// SSH_SERVER_HOST_KEYS from configFrom means the user manages the host keys, as in
// spec.config: no Secret, no mount, no status.ssh, and app.ini keeps the user's value.
func TestHostKeysOverriddenThroughConfigFrom(t *testing.T) {
	userKeys := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-ssh", Namespace: "ns"},
		Data: map[string][]byte{"paths": []byte("/data/ssh/key1,/data/ssh/key2")}}
	e := newHostKeysEnv(t, func(f *forgejov1.Forgejo) {
		f.Spec.ConfigFrom = []forgejov1.ConfigValueFromSecret{{Section: "Server", Key: "SSH_SERVER_HOST_KEYS",
			SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "my-ssh"}, Key: "paths"}}}
	}, userKeys)
	e.f.Status.SSH = &forgejov1.SSHStatus{HostKeysSecretName: "stale"}
	d, ini := e.pass()
	if _, err := e.secret(forgejo.SSHHostKeysSecretName(e.f)); !apierrors.IsNotFound(err) {
		t.Fatal("generated host keys although configFrom sets SSH_SERVER_HOST_KEYS")
	}
	if mountsHostKeys(d) || e.f.Status.SSH != nil || e.rc.SSHHostKeys != nil {
		t.Fatalf("unmanaged host keys must be left alone: status %+v", e.f.Status.SSH)
	}
	if !strings.Contains(section(ini, "server"), "SSH_SERVER_HOST_KEYS = /data/ssh/key1,/data/ssh/key2\n") {
		t.Fatalf("app.ini lost the configFrom value:\n%s", ini)
	}
}
