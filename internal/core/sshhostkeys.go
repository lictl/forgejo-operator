package core

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/ssh"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// SSH host keys (docs/design.md "SSH host keys").
//
// Forgejo's built-in SSH server loads the files listed in [server] SSH_SERVER_HOST_KEYS. By
// default it keeps an RSA key it generates itself on the data volume, which changes whenever the
// volume is rebuilt, so the operator keeps the keys in a Secret instead, mounted read-only:
//
//   - the operator generates ed25519, ECDSA P-256 and RSA 3072 keys into "<name>-ssh-host-keys";
//   - spec.server.ssh.hostKeys.existingSecret brings your own keys.
//
// Keys are never rotated by the operator: it only creates the Secret, never changes it, and it
// does not generate new keys while the pods serve keys from a Secret that has gone missing.
//
// Each pod serves a copy of the keys that lives as long as the pod: init container
// "ssh-host-keys" copies them from the Secret into a memory-backed emptyDir, and only when
// their contents match the digest of the keys the operator validated (the pod template's
// ssh-host-keys annotation). An edit of the Secret therefore reaches the pods only through the
// rollout of a new digest; a broken edit never reaches them, also not through a container
// restart. While the Secret is missing or unusable, new pods cannot start, so the Deployment is
// held as it is (rc.SSHHostKeysUnavailable).

// Keys of a Secret of SSH host keys (the file names "ssh-keygen -A" writes).
const (
	HostKeyEd25519 = "ssh_host_ed25519_key"
	HostKeyECDSA   = "ssh_host_ecdsa_key"
	HostKeyRSA     = "ssh_host_rsa_key"
)

// HostKeyFiles are the Secret keys the operator serves, in the order app.ini lists them.
var HostKeyFiles = []string{HostKeyEd25519, HostKeyECDSA, HostKeyRSA}

const (
	// AnnotationSSHHostKeysSource on "<name>-ssh-host-keys" records that the operator generated
	// its keys (Generated). A Secret without it was created by the user.
	AnnotationSSHHostKeysSource = "forgejo.lucas.icu/ssh-host-keys-source"

	// ReasonSSHHostKeysUnavailable is the Degraded reason while the Secret the pods serve
	// their host keys from is missing or unusable. The Deployment keeps serving the keys it
	// has, and the operator never replaces them with new ones on its own.
	ReasonSSHHostKeysUnavailable = "SSHHostKeysUnavailable"

	// sshHostKeysVolume is the Secret of host keys, mounted in the init container only.
	sshHostKeysVolume = "ssh-host-keys"
	// sshHostKeysCopyVolume is the pod's copy of the keys (emptyDir in memory), which the
	// forgejo container serves.
	sshHostKeysCopyVolume = "ssh-host-keys-copy"
	// sshHostKeysContainer is the init container that copies and checks the keys.
	sshHostKeysContainer = "ssh-host-keys"
	// hostKeysSecretDir is where the init container mounts the Secret.
	hostKeysSecretDir = "/etc/forgejo-ssh-secret"
)

// rsaHostKeyBits is the size of generated RSA host keys (a variable for faster tests).
var rsaHostKeyBits = 3072

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete

// ReconcileSSHHostKeys ensures the Secret of SSH host keys and sets rc.SSHHostKeys and
// status.ssh.
func ReconcileSSHHostKeys(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !forgejo.SSHHostKeysManaged(f) {
		f.Status.SSH = nil
		clearDegraded(rc, ReasonSSHHostKeysUnavailable)
		return forgejo.StepResult{}, nil
	}
	live, err := liveHostKeys(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}

	if hk := f.Spec.Server.SSH.HostKeys; hk != nil && hk.ExistingSecret != "" {
		sec := &corev1.Secret{}
		err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: hk.ExistingSecret}, sec)
		if client.IgnoreNotFound(err) != nil {
			return forgejo.StepResult{}, fmt.Errorf("spec.server.ssh.hostKeys.existingSecret %q: %w", hk.ExistingSecret, err)
		}
		var keys []hostKey
		if err == nil {
			keys, err = parseHostKeys(sec.Data)
		}
		if err != nil {
			return unavailable(rc, live, fmt.Sprintf("spec.server.ssh.hostKeys.existingSecret %q: %v", hk.ExistingSecret, notFoundMessage(err)))
		}
		useHostKeys(rc, hk.ExistingSecret, keys, forgejov1.SSHHostKeysExistingSecret)
		return forgejo.StepResult{}, nil
	}

	name := forgejo.SSHHostKeysSecretName(f)
	sec := &corev1.Secret{}
	err = rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, sec)
	if client.IgnoreNotFound(err) != nil {
		return forgejo.StepResult{}, fmt.Errorf("reading %s: %w", name, err)
	}
	if err == nil {
		keys, perr := parseHostKeys(sec.Data)
		if perr != nil {
			return unavailable(rc, live, fmt.Sprintf("Secret %s: %v. Fix the Secret; the operator does not replace host keys", name, perr))
		}
		if err := adoptHostKeysSecret(ctx, rc, sec); err != nil {
			return forgejo.StepResult{}, err
		}
		source := forgejov1.SSHHostKeysExistingSecret
		if sec.Annotations[AnnotationSSHHostKeysSource] == string(forgejov1.SSHHostKeysGenerated) {
			source = forgejov1.SSHHostKeysGenerated
		}
		useHostKeys(rc, name, keys, source)
		return forgejo.StepResult{}, nil
	}

	if live != nil && live.SecretName == name {
		// The pods serve keys from this Secret, which is gone. Running pods keep their copy, so
		// the Deployment is left as it is until the user restores the Secret: new keys would
		// break every client that pinned the old ones.
		return unavailable(rc, live, fmt.Sprintf("Secret %s with the SSH host keys the pods serve is missing; "+
			"restore it (backups keep a copy under ssh-host-keys/), or create it with the keys to serve. "+
			"The operator does not generate new host keys for an instance that has some", name))
	}
	return generateAndUse(ctx, rc)
}

// generateAndUse generates the instance's host keys into "<name>-ssh-host-keys".
func generateAndUse(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	data, err := generateHostKeys()
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("generating SSH host keys: %w", err)
	}
	keys, err := parseHostKeys(data)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if err := createHostKeysSecret(ctx, rc, data); err != nil {
		return forgejo.StepResult{}, err
	}
	name := forgejo.SSHHostKeysSecretName(rc.Instance)
	rc.Event(corev1.EventTypeNormal, "SSHHostKeysGenerated", "SSHHostKeys", "Generated SSH host keys in Secret %s (%s)", name, fingerprintList(keys))
	useHostKeys(rc, name, keys, forgejov1.SSHHostKeysGenerated)
	return forgejo.StepResult{}, nil
}

// useHostKeys hands the keys to the config and workload steps and reports them.
func useHostKeys(rc *forgejo.ReconcileContext, secret string, keys []hostKey, source forgejov1.SSHHostKeysSource) {
	files := make([]string, 0, len(keys))
	for _, k := range keys {
		files = append(files, k.file)
	}
	rc.SSHHostKeys = &forgejo.SSHHostKeys{SecretName: secret, Files: files, Digest: hostKeysDigest(keys)}
	rc.Instance.Status.SSH = &forgejov1.SSHStatus{
		HostKeysSecretName: secret,
		HostKeysSource:     source,
		HostKeys:           hostKeysStatus(keys),
	}
	clearDegraded(rc, ReasonSSHHostKeysUnavailable)
}

// unavailable reports a Secret of host keys that is missing or unusable. When the pods serve
// keys from a Secret, they keep them (rc.SSHHostKeys from the live Deployment), and the
// Deployment is held as it is (rc.SSHHostKeysUnavailable): a new pod could not start.
// Otherwise nothing can be deployed correctly, and the step fails.
func unavailable(rc *forgejo.ReconcileContext, live *forgejo.SSHHostKeys, msg string) (forgejo.StepResult, error) {
	if live == nil {
		rc.Instance.Status.SSH = &forgejov1.SSHStatus{Message: msg}
		return forgejo.StepResult{}, errors.New(msg)
	}
	msg += ". New pods cannot start meanwhile, so the operator leaves Deployment " + forgejo.DeploymentName(rc.Instance) +
		" as it is (no rollout, restart or resize), and upgrades and restores wait"
	setDegraded(rc, ReasonSSHHostKeysUnavailable, msg)
	warnEvery(rc, "ssh-host-keys-unavailable/"+msg, warnInterval, ReasonSSHHostKeysUnavailable, "SSHHostKeys", "%s", msg)
	rc.SSHHostKeys = live
	rc.SSHHostKeysUnavailable = true
	st := rc.Instance.Status.SSH
	if st == nil {
		st = &forgejov1.SSHStatus{}
		rc.Instance.Status.SSH = st
	}
	st.HostKeysSecretName = live.SecretName
	st.Message = msg
	return forgejo.StepResult{}, nil
}

func notFoundMessage(err error) error {
	if apierrors.IsNotFound(err) {
		return errors.New("not found")
	}
	return err
}

// adoptHostKeysSecret labels the Secret and sets its owner reference per retainPolicy, like
// "<name>-secrets". Its keys are never changed.
func adoptHostKeysSecret(ctx context.Context, rc *forgejo.ReconcileContext, sec *corev1.Secret) error {
	f := rc.Instance
	want := forgejo.Labels(f, forgejo.ComponentServer)
	owned := slices.ContainsFunc(sec.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == f.UID })
	labelled := true
	for k, v := range want {
		if sec.Labels[k] != v {
			labelled = false
		}
	}
	if labelled && owned == rc.OwnerRefsEnabled() {
		return nil
	}
	obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: sec.Name, Namespace: sec.Namespace}}
	return apply(ctx, rc, obj, rc.OwnerRefsEnabled(), func() error {
		obj.Labels = mergeMaps(obj.Labels, want)
		return nil
	})
}

func createHostKeysSecret(ctx context.Context, rc *forgejo.ReconcileContext, data map[string][]byte) error {
	f := rc.Instance
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        forgejo.SSHHostKeysSecretName(f),
			Namespace:   f.Namespace,
			Labels:      forgejo.Labels(f, forgejo.ComponentServer),
			Annotations: map[string]string{AnnotationSSHHostKeysSource: string(forgejov1.SSHHostKeysGenerated)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
	if err := setOwnership(rc, sec, rc.OwnerRefsEnabled()); err != nil {
		return err
	}
	// Create, never update: a Secret that appeared meanwhile (e.g. created by the user) wins.
	if err := rc.Client.Create(ctx, sec); err != nil {
		return fmt.Errorf("creating %s: %w", sec.Name, err)
	}
	return nil
}

// liveHostKeys returns the host keys the live Deployment serves from a Secret, or nil.
func liveHostKeys(ctx context.Context, rc *forgejo.ReconcileContext) (*forgejo.SSHHostKeys, error) {
	f := rc.Instance
	dep := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	return servedHostKeys(dep), nil
}

// servedHostKeys reads the host keys a Deployment mounts from a Secret, or nil.
func servedHostKeys(dep *appsv1.Deployment) *forgejo.SSHHostKeys {
	for _, v := range dep.Spec.Template.Spec.Volumes {
		if v.Name != sshHostKeysVolume || v.Secret == nil {
			continue
		}
		hk := &forgejo.SSHHostKeys{
			SecretName: v.Secret.SecretName,
			Digest:     dep.Spec.Template.Annotations[forgejo.AnnotationSSHHostKeys],
		}
		for _, item := range v.Secret.Items {
			hk.Files = append(hk.Files, item.Key)
		}
		return hk
	}
	return nil
}

// hostKey is one parsed host key of a Secret.
type hostKey struct {
	file string
	data []byte
	pub  ssh.PublicKey
}

// parseHostKeys reads the host keys of a Secret (HostKeyFiles; other entries are ignored).
// Forgejo serves one key per type, so two keys of a type are refused.
func parseHostKeys(data map[string][]byte) ([]hostKey, error) {
	var keys []hostKey
	seen := map[string]string{}
	for _, file := range HostKeyFiles {
		pemBytes := data[file]
		if len(bytes.TrimSpace(pemBytes)) == 0 {
			continue
		}
		raw, err := ssh.ParseRawPrivateKey(pemBytes)
		if err != nil {
			var missing *ssh.PassphraseMissingError
			if errors.As(err, &missing) {
				return nil, fmt.Errorf("%s is protected by a passphrase; Forgejo needs host keys without one", file)
			}
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		signer, err := ssh.NewSignerFromKey(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		pub := signer.PublicKey()
		if other, dup := seen[pub.Type()]; dup {
			return nil, fmt.Errorf("%s and %s are both %s keys; Forgejo serves one key of each type", other, file, pub.Type())
		}
		seen[pub.Type()] = file
		keys = append(keys, hostKey{file: file, data: pemBytes, pub: pub})
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("it holds none of the host keys %s", strings.Join(HostKeyFiles, ", "))
	}
	return keys, nil
}

func hostKeysStatus(keys []hostKey) []forgejov1.SSHHostKey {
	out := make([]forgejov1.SSHHostKey, 0, len(keys))
	for _, k := range keys {
		out = append(out, forgejov1.SSHHostKey{
			Type:        k.pub.Type(),
			Fingerprint: ssh.FingerprintSHA256(k.pub),
			PublicKey:   strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k.pub))),
		})
	}
	return out
}

// hostKeysDigest identifies a set of host keys by their file names and contents, in the order
// app.ini lists them. hostKeysScript computes the same digest from the files the pod got:
// sha256 over one "<file> <sha256 of its contents>" line per file, first 16 hex digits.
func hostKeysDigest(keys []hostKey) string {
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s %x\n", k.file, sha256.Sum256(k.data))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:16]
}

func fingerprintList(keys []hostKey) string {
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k.pub.Type()+" "+ssh.FingerprintSHA256(k.pub))
	}
	return strings.Join(parts, ", ")
}

// generateHostKeys creates ed25519, ECDSA P-256 and RSA host keys in OpenSSH format.
func generateHostKeys() (map[string][]byte, error) {
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	rk, err := rsa.GenerateKey(rand.Reader, rsaHostKeyBits)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for file, key := range map[string]crypto.PrivateKey{HostKeyEd25519: ed, HostKeyECDSA: ec, HostKeyRSA: rk} {
		if out[file], err = marshalHostKey(key); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// marshalHostKey encodes a private key in OpenSSH format, without a passphrase.
func marshalHostKey(key crypto.PrivateKey) ([]byte, error) {
	if k, ok := key.(*ed25519.PrivateKey); ok {
		key = *k
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

// --- app.ini and pod template -------------------------------------------------------------

// hostKeyPaths are the SSH_SERVER_HOST_KEYS of keys mounted from a Secret.
func hostKeyPaths(l forgejo.ImageLayout, hk *forgejo.SSHHostKeys) string {
	paths := make([]string, 0, len(hk.Files))
	for _, f := range hk.Files {
		paths = append(paths, l.SSHHostKeysDir+"/"+f)
	}
	return strings.Join(paths, ",")
}

// hostKeysVolumes are the Secret of host keys (group-readable by the Forgejo user through
// fsGroup; mounted in the init container only) and the pod's copy of the keys: an emptyDir in
// memory, so that private keys are never written to the node's disk, and which lives as long
// as the pod, so that a container restart serves the same keys.
func hostKeysVolumes(hk *forgejo.SSHHostKeys) []corev1.Volume {
	items := make([]corev1.KeyToPath, 0, len(hk.Files))
	for _, f := range hk.Files {
		items = append(items, corev1.KeyToPath{Key: f, Path: f})
	}
	return []corev1.Volume{
		{Name: sshHostKeysVolume, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: hk.SecretName, Items: items, DefaultMode: ptr.To[int32](0o440)},
		}},
		{Name: sshHostKeysCopyVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
		}},
	}
}

// hostKeysInitContainer copies the host keys into the pod's copy (hostKeysScript). It runs
// first, so that a pod that cannot serve the published keys fails before migrating anything.
func hostKeysInitContainer(rc *forgejo.ReconcileContext, resources corev1.ResourceRequirements, pullPolicy corev1.PullPolicy) corev1.Container {
	l, hk := rc.Layout, rc.SSHHostKeys
	return corev1.Container{
		Name:            sshHostKeysContainer,
		Image:           rc.TargetImage,
		ImagePullPolicy: pullPolicy,
		Command:         []string{shell, "-c", hostKeysScript(hostKeysSecretDir, l.SSHHostKeysDir)},
		Env: []corev1.EnvVar{
			{Name: "SSH_HOST_KEYS_SECRET", Value: hk.SecretName},
			{Name: "SSH_HOST_KEYS_FILES", Value: strings.Join(hk.Files, " ")},
			{Name: "SSH_HOST_KEYS_DIGEST", Value: hk.Digest},
		},
		Resources: resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: sshHostKeysVolume, MountPath: hostKeysSecretDir, ReadOnly: true},
			{Name: sshHostKeysCopyVolume, MountPath: l.SSHHostKeysDir},
		},
		SecurityContext:          nonRootSecurityContext(l),
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
	}
}

// hostKeysScript copies the files SSH_HOST_KEYS_FILES from src to dst (read-only for the
// Forgejo user) and checks them against SSH_HOST_KEYS_DIGEST (hostKeysDigest). On a mismatch
// it removes the copies and fails, so that the pod never serves keys other than the ones the
// operator validated and publishes in status.ssh.hostKeys: Forgejo would serve a random
// in-memory key instead of keys it cannot load. The copies are checked, not the Secret's
// files, which the kubelet may replace at any time.
func hostKeysScript(src, dst string) string {
	return fmt.Sprintf(`set -eu
umask 077
src=%s
dst=%s
for f in $SSH_HOST_KEYS_FILES; do
  rm -f "$dst/$f"
  cp "$src/$f" "$dst/$f"
  chmod 0400 "$dst/$f"
done
got=$(for f in $SSH_HOST_KEYS_FILES; do
  printf '%%s %%s\n' "$f" "$(sha256sum < "$dst/$f" | cut -d' ' -f1)"
done | sha256sum | cut -c1-16)
if [ "$got" != "$SSH_HOST_KEYS_DIGEST" ]; then
  for f in $SSH_HOST_KEYS_FILES; do rm -f "$dst/$f"; done
  echo "Secret $SSH_HOST_KEYS_SECRET holds other SSH host keys (digest $got) than the ones the operator checked and publishes in status.ssh.hostKeys (digest $SSH_HOST_KEYS_DIGEST); not starting with them. A Secret that was just changed is rolled out once the operator has checked it; otherwise the Forgejo resource's Degraded condition says why the operator cannot use it." >&2
  exit 1
fi
echo "SSH host keys copied from Secret $SSH_HOST_KEYS_SECRET: $SSH_HOST_KEYS_FILES (digest $got)"
`, src, dst)
}

// servingHostKeys reports whether this pass deploys host keys from a Secret.
func servingHostKeys(rc *forgejo.ReconcileContext) bool {
	return rc.SSHHostKeys != nil
}
