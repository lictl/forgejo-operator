package backup

// Remote backups (spec.backup.remote): every backup is uploaded, encrypted with rclone crypt,
// to "s3://<bucket>/<prefix>/<namespace>/<name>/<backup>/" after it was written to the backup
// PVC. The Jobs do the heavy lifting with a stock rclone (internal/backup/remotejobs.go); the
// operator itself reads the small files of a remote backup (the manifest and the identity
// bundle) with its own S3 client and rclone-compatible decryption, which is what the disaster
// recovery bootstrap (spec.restoreFrom) and remote retention need.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/rclone"
	"github.com/lictl/forgejo-operator/internal/s3"
)

// Files of a remote backup, by their names before encryption (the objects end in ".bin").
const (
	// IdentityFile holds the instance's identity: its Secrets and its spec (see Identity).
	IdentityFile = "identity.json"
	// RemoteManifestFile describes the backup and the checksums of its files. It is uploaded
	// last: a directory without it is not a (complete) backup.
	RemoteManifestFile = "manifest.json"

	remoteFormatVersion = 1
	remoteManifestKind  = "ForgejoRemoteBackup"
	manifestLimit       = 1 << 20
	identityLimit       = 8 << 20
	// remoteListLimit bounds the objects listed below one instance's remote directory.
	remoteListLimit = 100000
)

// Roles of the Secrets in an identity bundle; a restore maps them to the Secrets of the new
// instance.
const (
	IdentityRoleSecrets     = "secrets"
	IdentityRoleAdmin       = "admin"
	IdentityRoleSSHHostKeys = "ssh-host-keys"
)

// RemoteEnabled reports whether backups of f are uploaded (spec.backup.remote).
func RemoteEnabled(f *forgejov1.Forgejo) bool { return f != nil && f.Spec.Backup.Remote != nil }

// BackupKeyRef is the Secret key holding the instance's backup encryption key:
// spec.backup.remote.encryption.keySecret, else the generated "<name>-backup-key".
func BackupKeyRef(f *forgejov1.Forgejo) forgejov1.BackupKeySecretRef {
	if r := f.Spec.Backup.Remote; r != nil && r.Encryption.KeySecret != nil {
		ref := *r.Encryption.KeySecret
		if ref.Key == "" {
			ref.Key = forgejo.BackupKeySecretKey
		}
		return ref
	}
	return forgejov1.BackupKeySecretRef{Name: forgejo.BackupKeySecretName(f), Key: forgejo.BackupKeySecretKey}
}

// SourceKeyRef is the key a remote backup source is decrypted with: its own keySecret, else
// the instance's backup key.
func SourceKeyRef(f *forgejov1.Forgejo, src *forgejov1.RemoteBackupSource) forgejov1.BackupKeySecretRef {
	if src.KeySecret != nil {
		ref := *src.KeySecret
		if ref.Key == "" {
			ref.Key = forgejo.BackupKeySecretKey
		}
		return ref
	}
	return BackupKeyRef(f)
}

// RcloneImage is the image of the Jobs that upload and download remote backups.
func RcloneImage(f *forgejov1.Forgejo) string {
	if r := f.Spec.Backup.Remote; r != nil && r.Image != "" {
		return r.Image
	}
	return forgejov1.DefaultRcloneImage
}

// InstanceRemotePath is the directory of an instance's remote backups below the prefix.
func InstanceRemotePath(namespace, name string) string { return namespace + "/" + name }

// remoteUIDLength is how much of a ForgejoBackup's UID its remote directory's name carries.
const remoteUIDLength = 8

// RemoteBackupDirName is the directory of a backup in its instance's remote directory:
// "<backup>-<the first 8 characters of its UID>". Every ForgejoBackup gets a directory of its
// own: a backup created again under an earlier name, or one of the same name taken by an
// instance of the same namespace and name in another cluster (a disaster recovery drill, the
// original cluster after a recovery), never writes into, or cleans up, another's upload.
func RemoteBackupDirName(name string, uid types.UID) string {
	u := strings.ReplaceAll(string(uid), "-", "")
	if len(u) > remoteUIDLength {
		u = u[:remoteUIDLength]
	}
	if u == "" {
		return name
	}
	return name + "-" + strings.ToLower(u)
}

// BackupRemotePath is the directory of a backup below the prefix:
// "<namespace>/<name>/<backup>-<uid>" (see RemoteBackupDirName).
func BackupRemotePath(b *forgejov1.ForgejoBackup) string {
	return InstanceRemotePath(b.Namespace, b.Spec.ForgejoRef.Name) + "/" + RemoteBackupDirName(b.Name, b.UID)
}

// RemoteLocation is the URL of a directory below the prefix, for status and messages.
func RemoteLocation(c *forgejov1.S3Connection, path string) string {
	return "s3://" + c.Bucket + "/" + forgejo.S3Prefix(c) + strings.Trim(path, "/") + "/"
}

// RemoteManifest is manifest.json of a remote backup. The upload Job writes it from the head
// the operator renders (ManifestHead) and the files it uploaded.
type RemoteManifest struct {
	FormatVersion int    `json:"formatVersion"`
	Kind          string `json:"kind"`
	Backup        string `json:"backup"`
	Namespace     string `json:"namespace"`
	Forgejo       string `json:"forgejo"`
	// BackupUID and ForgejoUID are the UIDs of the ForgejoBackup and of its instance: what tells
	// the backups of different instances of the same namespace and name apart (remote
	// retention prunes only its own instance's), and an upload's own manifest from another.
	BackupUID     string                 `json:"backupUID,omitempty"`
	ForgejoUID    string                 `json:"forgejoUID,omitempty"`
	Reason        forgejov1.BackupReason `json:"reason"`
	ForgejoImage  string                 `json:"forgejoImage"`
	PostgresImage string                 `json:"postgresImage,omitempty"`
	IncludesData  bool                   `json:"includesData"`
	// CompletedAt is when the backup was completed on the backup PVC (RFC 3339).
	CompletedAt string `json:"completedAt,omitempty"`
	// KeyFingerprint identifies the encryption key (rclone.Cipher.Fingerprint).
	KeyFingerprint string `json:"keyFingerprint"`
	// SecretKeyFingerprint identifies the instance's SECRET_KEY (see SecretKeyFingerprint).
	SecretKeyFingerprint string `json:"secretKeyFingerprint,omitempty"`
	// UploadedAt and Files are added by the upload Job.
	UploadedAt string                       `json:"uploadedAt,omitempty"`
	Files      []forgejov1.RemoteBackupFile `json:"files,omitempty"`
	// Local is the manifest.json of the backup PVC (sizes, excluded paths, object storage).
	Local json.RawMessage `json:"local,omitempty"`
}

// File returns the entry of the named file, or nil.
func (m *RemoteManifest) File(name string) *forgejov1.RemoteBackupFile {
	for i := range m.Files {
		if m.Files[i].Name == name {
			return &m.Files[i]
		}
	}
	return nil
}

// Validate checks that m describes a complete backup the operator can restore.
func (m *RemoteManifest) Validate() error {
	switch {
	case m.Kind != remoteManifestKind:
		return fmt.Errorf("%s is not the manifest of a Forgejo backup (kind %q)", RemoteManifestFile, m.Kind)
	case m.FormatVersion != remoteFormatVersion:
		return fmt.Errorf("unsupported backup format version %d (this operator reads version %d)", m.FormatVersion, remoteFormatVersion)
	}
	need := []string{DBDumpFile, IdentityFile}
	if m.IncludesData {
		need = append(need, DataArchiveFile)
	}
	for _, name := range need {
		f := m.File(name)
		if f == nil || len(f.SHA256) != sha256.Size*2 {
			return fmt.Errorf("the manifest lists no checksum for %s", name)
		}
	}
	return nil
}

// ParseRemoteManifest parses manifest.json. The upload Job embeds the local manifest as the
// last field ("local"); should that be malformed, the rest of the manifest still counts.
func ParseRemoteManifest(data []byte) (*RemoteManifest, error) {
	m := &RemoteManifest{}
	err := json.Unmarshal(data, m)
	if err == nil {
		return m, nil
	}
	if i := bytes.LastIndex(data, []byte(`,"local":`)); i > 0 {
		m = &RemoteManifest{}
		if json.Unmarshal(append(bytes.Clone(data[:i]), '}'), m) == nil {
			return m, nil
		}
	}
	return nil, err
}

// CompletedTime parses CompletedAt (zero when absent or malformed).
func (m *RemoteManifest) CompletedTime() time.Time {
	t, _ := time.Parse(time.RFC3339, m.CompletedAt)
	return t
}

// ManifestHead renders the fields of manifest.json the operator knows, as a JSON object
// without its closing brace; the upload Job appends uploadedAt, files and local.
func ManifestHead(m RemoteManifest) (string, error) {
	m.FormatVersion, m.Kind = remoteFormatVersion, remoteManifestKind
	m.UploadedAt, m.Files, m.Local = "", nil, nil
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(b), "}"), nil
}

// Identity is identity.json of a remote backup: what makes a restored instance the same
// instance besides its database and data volume. Forgejo derives the keys of TOTP secrets,
// Actions secrets, mirror and webhook credentials from SECRET_KEY; the admin Secret keeps the
// bootstrap admin's password; the SSH host keys are what clients pinned.
type Identity struct {
	FormatVersion int `json:"formatVersion"`
	// Instance is the Forgejo object as it was (metadata and spec, no status), for reference.
	Instance *IdentityInstance `json:"instance,omitempty"`
	Secrets  []IdentitySecret  `json:"secrets"`
}

// IdentityInstance is the sanitised Forgejo object of an identity bundle.
type IdentityInstance struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Metadata   IdentityMeta          `json:"metadata"`
	Spec       forgejov1.ForgejoSpec `json:"spec"`
}

// IdentityMeta is the metadata kept of an object.
type IdentityMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// IdentitySecret is one Secret of an identity bundle.
type IdentitySecret struct {
	// Role is IdentityRoleSecrets, IdentityRoleAdmin or IdentityRoleSSHHostKeys.
	Role        string            `json:"role"`
	Name        string            `json:"name"`
	Type        corev1.SecretType `json:"type,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Data        map[string][]byte `json:"data"`
}

// Secret returns the Secret of role, or nil.
func (id *Identity) Secret(role string) *IdentitySecret {
	for i := range id.Secrets {
		if id.Secrets[i].Role == role {
			return &id.Secrets[i]
		}
	}
	return nil
}

// keptAnnotations are the annotations of identity Secrets that carry meaning for the operator.
var keptAnnotations = []string{forgejo.AnnotationSSHHostKeysSource}

// BuildIdentity collects the identity of f: Secret "<name>-secrets", the admin Secret, the
// Secret of SSH host keys (when the operator serves them and it exists) and the sanitised
// object.
func BuildIdentity(ctx context.Context, c client.Reader, f *forgejov1.Forgejo) (*Identity, error) {
	id := &Identity{FormatVersion: remoteFormatVersion, Instance: sanitisedInstance(f)}
	add := func(role, name string, required bool) error {
		sec := &corev1.Secret{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) && !required {
				return nil
			}
			return fmt.Errorf("reading Secret %s: %w", name, err)
		}
		is := IdentitySecret{Role: role, Name: name, Type: sec.Type, Data: sec.Data}
		for _, k := range keptAnnotations {
			if v, ok := sec.Annotations[k]; ok {
				if is.Annotations == nil {
					is.Annotations = map[string]string{}
				}
				is.Annotations[k] = v
			}
		}
		id.Secrets = append(id.Secrets, is)
		return nil
	}
	if err := add(IdentityRoleSecrets, forgejo.SecretsName(f), true); err != nil {
		return nil, err
	}
	admin := forgejo.AdminSecretName(f)
	if f.Spec.Admin.ExistingSecret != "" {
		admin = f.Spec.Admin.ExistingSecret
	}
	if err := add(IdentityRoleAdmin, admin, true); err != nil {
		return nil, err
	}
	if forgejo.SSHHostKeysManaged(f) {
		if err := add(IdentityRoleSSHHostKeys, forgejo.SSHHostKeysSecret(f), false); err != nil {
			return nil, err
		}
	}
	return id, nil
}

func sanitisedInstance(f *forgejov1.Forgejo) *IdentityInstance {
	ann := map[string]string{}
	for k, v := range f.Annotations {
		if k != corev1.LastAppliedConfigAnnotation {
			ann[k] = v
		}
	}
	if len(ann) == 0 {
		ann = nil
	}
	return &IdentityInstance{
		APIVersion: forgejov1.GroupVersion.String(),
		Kind:       "Forgejo",
		Metadata:   IdentityMeta{Name: f.Name, Namespace: f.Namespace, Labels: f.Labels, Annotations: ann},
		Spec:       *f.Spec.DeepCopy(),
	}
}

// SecretKeyFingerprint identifies the SECRET_KEY of "<name>-secrets" (its data) without
// revealing it, so that a restore can tell whether a backup was taken by an instance with the
// same identity. 16 hex digits.
func SecretKeyFingerprint(data map[string][]byte) string {
	key := data[forgejo.SecretKeySecretKey]
	if len(key) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("forgejo-operator secret-key fingerprint v1\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ConfigError is a problem with the Secrets or the spec of a remote location, which waiting
// does not fix.
type ConfigError struct{ msg string }

func (e *ConfigError) Error() string { return e.msg }

func configErrorf(format string, args ...any) error {
	return &ConfigError{msg: fmt.Sprintf(format, args...)}
}

// IsConfigError reports whether err is a *ConfigError.
func IsConfigError(err error) bool {
	var ce *ConfigError
	return errors.As(err, &ce)
}

// ReadSecretValue reads one value of a Secret in namespace. A missing Secret or key is a
// *ConfigError.
func ReadSecretValue(ctx context.Context, c client.Reader, namespace, name, key string) ([]byte, error) {
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, configErrorf("Secret %q not found", name)
		}
		return nil, fmt.Errorf("reading Secret %q: %w", name, err)
	}
	v, ok := sec.Data[key]
	if !ok || len(v) == 0 {
		return nil, configErrorf("Secret %q has no key %q", name, key)
	}
	return v, nil
}

// ReadBackupKey reads and checks the encryption key that ref names.
func ReadBackupKey(ctx context.Context, c client.Reader, namespace string, ref forgejov1.BackupKeySecretRef) (*rclone.Cipher, error) {
	v, err := ReadSecretValue(ctx, c, namespace, ref.Name, ref.Key)
	if err != nil {
		return nil, fmt.Errorf("backup key: %w", err)
	}
	if err := rclone.ValidateKey(string(v)); err != nil {
		return nil, configErrorf("backup key in key %q of Secret %q: %v", ref.Key, ref.Name, err)
	}
	return rclone.NewCipher(string(v))
}

// Store reads, lists and deletes remote backups with the operator's own S3 client.
type Store struct {
	conn   *forgejov1.S3Connection
	bucket s3.Bucket
	cipher *rclone.Cipher
}

// OpenStore resolves the Secrets of a remote location (credentials, CA and key) in namespace.
// Short endpoint host names resolve as in a pod of namespace.
func OpenStore(ctx context.Context, c client.Reader, namespace string, conn *forgejov1.S3Connection, key forgejov1.BackupKeySecretRef) (*Store, error) {
	ep, err := forgejo.ParseS3Endpoint(conn.Endpoint)
	if err != nil {
		return nil, configErrorf("%v", err)
	}
	id, err := ReadSecretValue(ctx, c, namespace, conn.CredentialsSecret.Name, forgejo.S3AccessKeyIDKey(conn))
	if err != nil {
		return nil, fmt.Errorf("S3 credentials: %w", err)
	}
	secret, err := ReadSecretValue(ctx, c, namespace, conn.CredentialsSecret.Name, forgejo.S3SecretAccessKeyKey(conn))
	if err != nil {
		return nil, fmt.Errorf("S3 credentials: %w", err)
	}
	var roots *x509.CertPool
	if ref := forgejo.S3CASecret(conn); ref != nil {
		ca, err := ReadSecretValue(ctx, c, namespace, ref.Name, ref.Key)
		if err != nil {
			return nil, fmt.Errorf("CA certificate: %w", err)
		}
		if roots, err = x509.SystemCertPool(); err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(ca) {
			return nil, configErrorf("CA certificate: key %q of Secret %q holds no PEM certificate", ref.Key, ref.Name)
		}
	}
	cipher, err := ReadBackupKey(ctx, c, namespace, key)
	if err != nil {
		return nil, err
	}
	scheme := "http"
	if ep.Secure {
		scheme = "https"
	}
	return &Store{conn: conn, cipher: cipher, bucket: s3.Bucket{
		Scheme: scheme, Host: ep.Host, Name: conn.Bucket, Region: forgejo.S3Region(conn),
		VirtualHost: forgejo.S3VirtualHostStyle(conn), AccessKeyID: string(id), SecretAccessKey: string(secret),
		RootCAs: roots, InsecureSkipVerify: forgejo.S3InsecureSkipVerify(conn),
		DialContext: forgejo.NamespaceDialer(namespace),
	}}, nil
}

// KeyFingerprint identifies the store's encryption key.
func (s *Store) KeyFingerprint() string { return s.cipher.Fingerprint() }

// Location is the URL of path.
func (s *Store) Location(path string) string { return RemoteLocation(s.conn, path) }

func (s *Store) objectKey(path, file string) string {
	return forgejo.S3Prefix(s.conn) + strings.Trim(path, "/") + "/" + file + rclone.EncryptedSuffix
}

// ReadFile downloads and decrypts one file of the backup at path.
func (s *Store) ReadFile(ctx context.Context, path, file string, limit int64) ([]byte, error) {
	data, err := s3.GetObject(ctx, s.bucket, s.objectKey(path, file), limit)
	if err != nil {
		return nil, err
	}
	plain, err := s.cipher.Decrypt(data)
	if err != nil {
		return nil, fmt.Errorf("%s of %s: %w (fingerprint of the key used: %s)", file, s.Location(path), err, s.cipher.Fingerprint())
	}
	return plain, nil
}

// ReadManifest reads and checks manifest.json of the backup at path. A missing manifest means
// that there is no complete backup at path.
func (s *Store) ReadManifest(ctx context.Context, path string) (*RemoteManifest, error) {
	data, err := s.ReadFile(ctx, path, RemoteManifestFile, manifestLimit)
	if err != nil {
		if s3.IsNotFound(err) {
			return nil, fmt.Errorf("no complete backup at %s (%s%s not found): %w", s.Location(path), RemoteManifestFile, rclone.EncryptedSuffix, err)
		}
		return nil, err
	}
	m, err := ParseRemoteManifest(data)
	if err != nil {
		return nil, configErrorf("%s of %s: %v", RemoteManifestFile, s.Location(path), err)
	}
	if err := m.Validate(); err != nil {
		return nil, configErrorf("%s: %v", s.Location(path), err)
	}
	return m, nil
}

// ReadIdentity reads identity.json of the backup at path and checks it against the manifest.
func (s *Store) ReadIdentity(ctx context.Context, path string, m *RemoteManifest) (*Identity, error) {
	data, err := s.ReadFile(ctx, path, IdentityFile, identityLimit)
	if err != nil {
		return nil, err
	}
	want := m.File(IdentityFile)
	sum := sha256.Sum256(data)
	if want == nil || hex.EncodeToString(sum[:]) != want.SHA256 {
		return nil, configErrorf("%s of %s does not match the checksum in its manifest", IdentityFile, s.Location(path))
	}
	id := &Identity{}
	if err := json.Unmarshal(data, id); err != nil {
		return nil, configErrorf("%s of %s: %v", IdentityFile, s.Location(path), err)
	}
	if id.FormatVersion != remoteFormatVersion || id.Secret(IdentityRoleSecrets) == nil {
		return nil, configErrorf("%s of %s has no Secret %q", IdentityFile, s.Location(path), IdentityRoleSecrets)
	}
	return id, nil
}

// RemoteDir is one directory below an instance's remote path, e.g. one backup.
type RemoteDir struct {
	// Name is the directory's name (the backup's name).
	Name string
	// Complete is true when it has a manifest.
	Complete bool
	// LastModified is the newest modification of its objects.
	LastModified time.Time
	// Keys are its objects.
	Keys []string
}

// ListDirs lists the directories below path (e.g. the backups of an instance), by name.
func (s *Store) ListDirs(ctx context.Context, path string) (map[string]*RemoteDir, error) {
	base := forgejo.S3Prefix(s.conn) + strings.Trim(path, "/") + "/"
	objs, err := s3.ListObjects(ctx, s.bucket, base, remoteListLimit)
	if err != nil {
		return nil, err
	}
	dirs := map[string]*RemoteDir{}
	for _, o := range objs {
		rest := strings.TrimPrefix(o.Key, base)
		name, file, ok := strings.Cut(rest, "/")
		if !ok || name == "" {
			continue
		}
		d := dirs[name]
		if d == nil {
			d = &RemoteDir{Name: name}
			dirs[name] = d
		}
		d.Keys = append(d.Keys, o.Key)
		if o.LastModified.After(d.LastModified) {
			d.LastModified = o.LastModified
		}
		if file == RemoteManifestFile+rclone.EncryptedSuffix {
			d.Complete = true
		}
	}
	return dirs, nil
}

// DeleteDir removes every object below path, the manifest first, so that an interrupted
// deletion never leaves something that looks like a complete backup.
func (s *Store) DeleteDir(ctx context.Context, path string) error {
	objs, err := s.List(ctx, path)
	if err != nil {
		return err
	}
	manifest := s.objectKey(path, RemoteManifestFile)
	keys := []string{}
	for _, o := range objs {
		if o.Key == manifest {
			keys = append([]string{o.Key}, keys...)
		} else {
			keys = append(keys, o.Key)
		}
	}
	return s.Delete(ctx, keys)
}

// List lists the objects below path.
func (s *Store) List(ctx context.Context, path string) ([]s3.Object, error) {
	return s3.ListObjects(ctx, s.bucket, forgejo.S3Prefix(s.conn)+strings.Trim(path, "/")+"/", remoteListLimit)
}

// HasManifest reports whether objs (of path) include the manifest of a complete backup.
func (s *Store) HasManifest(path string, objs []s3.Object) bool {
	manifest := s.objectKey(path, RemoteManifestFile)
	for _, o := range objs {
		if o.Key == manifest {
			return true
		}
	}
	return false
}

// Delete removes the objects keys, in order.
func (s *Store) Delete(ctx context.Context, keys []string) error {
	for _, k := range keys {
		if err := s3.DeleteObject(ctx, s.bucket, k); err != nil {
			return err
		}
	}
	return nil
}

// TransientRemoteError reports whether reading a remote backup failed for a reason that may go
// away by itself (the endpoint is unreachable or answers 5xx, the API server failed), as
// opposed to a wrong key, missing Secrets or objects, refused access or a TLS problem.
func TransientRemoteError(err error) bool {
	var se *s3.Error
	switch {
	case err == nil, IsConfigError(err), errors.Is(err, rclone.ErrAuthentication), errors.Is(err, rclone.ErrNotEncrypted):
		return false
	case errors.As(err, &se):
		return se.Reason == s3.ReasonUnreachable || se.Status >= 500
	}
	return true
}
