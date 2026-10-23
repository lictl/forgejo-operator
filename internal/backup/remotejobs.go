package backup

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	envAWSAccessKeyID     = "AWS_ACCESS_KEY_ID"
	envAWSSecretAccessKey = "AWS_SECRET_ACCESS_KEY"
)

const (
	// ContainerUpload uploads a backup (UploadJob); ContainerDownload fetches a remote backup
	// before a restore (RemoteRestoreJob).
	ContainerUpload   = "upload"
	ContainerDownload = "download"

	volIdentity   = "identity"
	volRemoteCA   = "remote-ca"
	volDownload   = "download"
	identityMount = "/identity"
	remoteCAMount = "/etc/backup-s3-ca"
	downloadMount = "/restore"

	// rclone remotes: "s3" is the bucket, "enc" the crypt remote over "s3:<bucket>/<prefix>".
	rcloneS3Remote    = "s3"
	rcloneCryptRemote = "enc"
)

// RemoteTarget is a remote location as the Jobs reach it.
type RemoteTarget struct {
	Conn *forgejov1.S3Connection
	Key  forgejov1.BackupKeySecretRef
	// Image is the rclone image.
	Image string
	// Path is the backup's directory below the prefix ("<namespace>/<name>/<backup>").
	Path string
}

// UploadResult is the JSON the upload container writes to its termination message.
type UploadResult struct {
	Files    []forgejov1.RemoteBackupFile `json:"files"`
	Verified bool                         `json:"verified"`
}

// UploadJobName is the Job that uploads a backup.
func UploadJobName(b *forgejov1.ForgejoBackup) string { return childName(b.Name, "-upload") }

// IdentitySecretName is the Secret that hands a backup's identity bundle to its upload Job.
func IdentitySecretName(b *forgejov1.ForgejoBackup) string { return childName(b.Name, "-identity") }

// rcloneEnv configures rclone through environment variables: remote "s3" is the bucket, with
// the access key from its Secret (env_auth reads AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY), and
// remote "enc" encrypts below "s3:<bucket>/<prefix>" without encrypting file names, so that
// backups can be listed (and pruned) without the key. The key reaches the script in BACKUP_KEY
// (a Secret reference); the script obscures it for rclone through stdin, so it never shows up
// in a pod spec or a command line.
func rcloneEnv(t RemoteTarget) []corev1.EnvVar {
	c := t.Conn
	secretRef := func(name, key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key,
		}}
	}
	s3env := func(k, v string) corev1.EnvVar {
		return corev1.EnvVar{Name: "RCLONE_CONFIG_" + strings.ToUpper(rcloneS3Remote) + "_" + k, Value: v}
	}
	encEnv := func(k, v string) corev1.EnvVar {
		return corev1.EnvVar{Name: "RCLONE_CONFIG_" + strings.ToUpper(rcloneCryptRemote) + "_" + k, Value: v}
	}
	root := c.Bucket
	if p := strings.TrimSuffix(forgejo.S3Prefix(c), "/"); p != "" {
		root += "/" + p
	}
	env := []corev1.EnvVar{
		s3env("TYPE", "s3"),
		s3env("PROVIDER", "Other"),
		s3env("ENV_AUTH", "true"),
		s3env("ENDPOINT", strings.TrimSuffix(c.Endpoint, "/")),
		s3env("REGION", forgejo.S3Region(c)),
		s3env("FORCE_PATH_STYLE", strconv.FormatBool(!forgejo.S3VirtualHostStyle(c))),
		s3env("NO_CHECK_BUCKET", "true"),
		encEnv("TYPE", "crypt"),
		encEnv("REMOTE", rcloneS3Remote+":"+root),
		encEnv("FILENAME_ENCRYPTION", "off"),
		encEnv("DIRECTORY_NAME_ENCRYPTION", "false"),
		{Name: envAWSAccessKeyID, ValueFrom: secretRef(c.CredentialsSecret.Name, forgejo.S3AccessKeyIDKey(c))},
		{Name: envAWSSecretAccessKey, ValueFrom: secretRef(c.CredentialsSecret.Name, forgejo.S3SecretAccessKeyKey(c))},
		{Name: "BACKUP_KEY", ValueFrom: secretRef(t.Key.Name, t.Key.Key)},
		{Name: "REMOTE_PATH", Value: t.Path},
		{Name: "LOCATION", Value: RemoteLocation(c, t.Path)},
		// An empty config file (the script creates it): everything is in the environment.
		{Name: "RCLONE_CONFIG", Value: "/tmp/rclone.conf"},
		{Name: "RCLONE_CACHE_DIR", Value: "/tmp/rclone-cache"},
	}
	if forgejo.S3CASecret(c) != nil {
		env = append(env, corev1.EnvVar{Name: "RCLONE_CA_CERT", Value: remoteCAMount + "/ca.crt"})
	}
	if forgejo.S3InsecureSkipVerify(c) {
		env = append(env, corev1.EnvVar{Name: "RCLONE_NO_CHECK_CERTIFICATE", Value: valueTrue})
	}
	return env
}

// remoteCAVolume is the endpoint's CA certificate, when spec...tls.caSecret is set.
func remoteCAVolume(c *forgejov1.S3Connection) (*corev1.Volume, *corev1.VolumeMount) {
	ref := forgejo.S3CASecret(c)
	if ref == nil {
		return nil, nil
	}
	return &corev1.Volume{Name: volRemoteCA, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName:  ref.Name,
			Items:       []corev1.KeyToPath{{Key: ref.Key, Path: "ca.crt"}},
			DefaultMode: ptr.To[int32](0o444),
		}}},
		&corev1.VolumeMount{Name: volRemoteCA, MountPath: remoteCAMount, ReadOnly: true}
}

// UploadJob uploads the backup's files from the backup PVC, and the identity bundle from
// Secret identitySecret, to t, encrypted, then reads every file back to verify its checksum,
// and finally uploads manifest.json, which marks the backup complete. manifestHead is the
// operator's part of the manifest (ManifestHead).
func UploadJob(in JobInput, b *forgejov1.ForgejoBackup, t RemoteTarget, identitySecret, manifestHead string) (*batchv1.Job, error) {
	env := append(rcloneEnv(t),
		corev1.EnvVar{Name: envBackupDir, Value: BackupDir(b.Name)},
		corev1.EnvVar{Name: "INCLUDES_DATA", Value: strconv.FormatBool(b.Status.IncludesData)},
		corev1.EnvVar{Name: "IDENTITY_FILE", Value: identityMount + "/" + IdentityFile},
		corev1.EnvVar{Name: "MANIFEST_HEAD", Value: manifestHead},
	)
	mounts := []corev1.VolumeMount{
		{Name: volBackups, MountPath: BackupMountPath, ReadOnly: true},
		{Name: volTmp, MountPath: tmpPath},
		{Name: volIdentity, MountPath: identityMount, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		backupVolume(b.Status.PVCName, true),
		// The identity bundle is copied nowhere, but rclone's temporary files stay in memory too.
		{Name: volTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			Medium: corev1.StorageMediumMemory, SizeLimit: ptr.To(resource.MustParse("64Mi"))}}},
		{Name: volIdentity, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: identitySecret, DefaultMode: ptr.To[int32](0o440)}}},
	}
	if v, m := remoteCAVolume(t.Conn); v != nil {
		volumes = append(volumes, *v)
		mounts = append(mounts, *m)
	}
	c := container(ContainerUpload, t.Image, uploadScript, mounts, env)
	c.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	job := newJob(in, b.Namespace, UploadJobName(b), forgejo.ComponentBackup, in.Affinity, volumes, nil, c)
	return job, controllerutil.SetControllerReference(b, job, in.Scheme)
}

// RemoteSource is a remote backup a restore Job downloads.
type RemoteSource struct {
	RemoteTarget
	// Files are the backup's files with their checksums, from its manifest.
	Files []forgejov1.RemoteBackupFile
}

// RemoteRestoreJob restores a remote backup: init container "download" (rclone) fetches and
// decrypts the files the restore needs into an emptyDir and checks them against the
// checksums of the manifest; the steps of a local restore (RestoreJob) then run on that copy.
// The pod asks for the ephemeral storage the download takes.
func RemoteRestoreJob(in JobInput, r *forgejov1.ForgejoRestore, src RemoteSource, restoreDB, restoreData bool) (*batchv1.Job, error) {
	want := map[string]string{}
	var size int64
	for _, f := range src.Files {
		if (f.Name == DBDumpFile && restoreDB) || (f.Name == DataArchiveFile && restoreData) {
			want[f.Name] = f.SHA256
			size += f.SizeBytes
		}
	}
	if restoreDB && want[DBDumpFile] == "" {
		return nil, fmt.Errorf("the backup's manifest lists no %s", DBDumpFile)
	}
	if restoreData && want[DataArchiveFile] == "" {
		return nil, fmt.Errorf("the backup's manifest lists no %s", DataArchiveFile)
	}
	// Room for the files plus a margin; the emptyDir is limited to it as well.
	need := resource.NewQuantity(size+size/10+64<<20, resource.BinarySI)

	env := append(rcloneEnv(src.RemoteTarget),
		corev1.EnvVar{Name: envBackupDir, Value: downloadMount},
		corev1.EnvVar{Name: "RESTORE_DATABASE", Value: strconv.FormatBool(restoreDB)},
		corev1.EnvVar{Name: "RESTORE_DATA", Value: strconv.FormatBool(restoreData)},
		corev1.EnvVar{Name: "DB_SHA256", Value: want[DBDumpFile]},
		corev1.EnvVar{Name: "DATA_SHA256", Value: want[DataArchiveFile]},
	)
	mounts := []corev1.VolumeMount{{Name: volDownload, MountPath: downloadMount}, {Name: volTmp, MountPath: tmpPath}}
	extra := []corev1.Volume{{Name: volDownload, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: need}}}}
	if v, m := remoteCAVolume(src.Conn); v != nil {
		extra = append(extra, *v)
		mounts = append(mounts, *m)
	}
	dl := container(ContainerDownload, src.Image, downloadScript, mounts, env)
	dl.Resources.Requests[corev1.ResourceEphemeralStorage] = *need
	dl.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("128Mi")
	return restoreJob(in, r, restoreSteps{
		dir:     downloadMount,
		volumes: extra,
		mounts:  []corev1.VolumeMount{{Name: volDownload, MountPath: downloadMount, ReadOnly: true}, {Name: volTmp, MountPath: tmpPath}},
		first:   &dl,
	}, restoreDB, restoreData)
}

// uploadScript runs in the rclone image (busybox sh). Files are hashed before the upload and
// after reading them back from the bucket (decrypted), so a truncated or damaged upload, or
// one rclone could not decrypt, fails the Job. manifest.json goes last. A directory that holds
// a manifest already is never written to (each ForgejoBackup has a directory of its own, so
// that is a retried pod after its predecessor completed, or someone else's backup): the
// operator then reads that manifest and decides.
const uploadScript = `set -eu
set -o pipefail
` + shellFail + rcloneKey + `dst="enc:$REMOTE_PATH"
existing=$(rclone lsf --max-depth 1 "$dst" 2> /tmp/rclone.err) ||
  rclone_fail "cannot list $LOCATION (check the endpoint, the bucket and the credentials)"
if printf '%s\n' "$existing" | grep -x 'manifest.json' > /dev/null; then
  fail "$LOCATION already holds a complete backup (manifest.json): nothing was uploaded"
fi
entry=""
upload() {
  src=$1 name=$2
  [ -f "$src" ] || fail "$src is missing"
  sum=$(sha256sum "$src" | cut -d' ' -f1)
  size=$(stat -L -c %s "$src")
  echo "uploading $name ($size bytes) to $LOCATION"
  rclone copyto --copy-links "$src" "$dst/$name" 2> /tmp/rclone.err || rclone_fail "uploading $name to $LOCATION failed"
  got=$(rclone cat "$dst/$name" 2> /tmp/rclone.err | sha256sum | cut -d' ' -f1) || rclone_fail "reading $name back from $LOCATION failed"
  [ "$got" = "$sum" ] || fail "verification of $name failed: the uploaded copy has SHA-256 $got, the original $sum"
  entry="{\"name\":\"$name\",\"sizeBytes\":$size,\"sha256\":\"$sum\"}"
}
upload "$BACKUP_DIR/db.dump" db.dump
files="$entry"
if [ "$INCLUDES_DATA" = true ]; then
  upload "$BACKUP_DIR/data.tar.gz" data.tar.gz
  files="$files,$entry"
fi
upload "$IDENTITY_FILE" identity.json
files="$files,$entry"
local_manifest=null
if [ -s "$BACKUP_DIR/manifest.json" ]; then local_manifest=$(cat "$BACKUP_DIR/manifest.json"); fi
printf '%s,"uploadedAt":"%s","files":[%s],"local":%s}\n' "$MANIFEST_HEAD" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$files" "$local_manifest" > /tmp/manifest.json
upload /tmp/manifest.json manifest.json
echo "uploaded and verified $LOCATION"
printf '{"files":[%s],"verified":true}' "$files" > /dev/termination-log
`

// downloadScript fetches the files a restore needs and checks them against the manifest.
const downloadScript = `set -eu
` + shellFail + rcloneKey + `src="enc:$REMOTE_PATH"
rclone lsf --max-depth 1 "$src" > /dev/null 2> /tmp/rclone.err ||
  rclone_fail "cannot list $LOCATION (check the endpoint, the bucket and the credentials)"
fetch() {
  name=$1 want=$2
  echo "downloading $name from $LOCATION"
  rclone copyto "$src/$name" "$BACKUP_DIR/$name" 2> /tmp/rclone.err ||
    rclone_fail "downloading $name from $LOCATION failed (a wrong key fails to authenticate the data)"
  got=$(sha256sum "$BACKUP_DIR/$name" | cut -d' ' -f1)
  [ "$got" = "$want" ] || fail "$name from $LOCATION has SHA-256 $got, its manifest says $want"
  echo "$name: $(stat -c %s "$BACKUP_DIR/$name") bytes, SHA-256 verified"
}
if [ "$RESTORE_DATABASE" = true ]; then fetch db.dump "$DB_SHA256"; fi
if [ "$RESTORE_DATA" = true ]; then fetch data.tar.gz "$DATA_SHA256"; fi
echo "downloaded $LOCATION"
`

// rcloneKey hands the backup key to rclone: obscured (rclone's config encoding) through stdin
// into the crypt remote's password, never on a command line.
const rcloneKey = `rclone_fail() {
  cat /tmp/rclone.err >&2
  fail "$1: $(grep -E 'CRITICAL|ERROR|Failed|failed' /tmp/rclone.err | tail -n 2 | sed -E 's#^[0-9/]+ [0-9:]+ ##' | tr '\n' ' ' | cut -c1-1500)"
}
: > "$RCLONE_CONFIG"
RCLONE_CONFIG_ENC_PASSWORD=$(printf '%s' "$BACKUP_KEY" | rclone obscure -) || fail "rclone could not read the backup key"
export RCLONE_CONFIG_ENC_PASSWORD
unset BACKUP_KEY
`

// ParseUploadResult reads the upload container's termination message from the succeeded pod.
func ParseUploadResult(pods []corev1.Pod) (UploadResult, error) {
	var res UploadResult
	for _, p := range pods {
		if p.Status.Phase != corev1.PodSucceeded {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != ContainerUpload || cs.State.Terminated == nil {
				continue
			}
			if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &res); err != nil {
				return res, fmt.Errorf("parsing result of pod %s: %w", p.Name, err)
			}
			return res, nil
		}
	}
	return res, fmt.Errorf("no succeeded pod with a result found")
}
