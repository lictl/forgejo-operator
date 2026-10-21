package backup

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	volBackups = "backups"
	volData    = "data"
	volTmp     = "tmp"

	// Container names. The main container of a backup Job reports its result through the
	// termination message.
	ContainerDump        = "dump-database"
	ContainerArchive     = "archive"
	ContainerVerify      = "verify"
	ContainerRestoreDB   = "restore-database"
	ContainerRestoreData = "restore-data"
	ContainerCleanup     = "cleanup"

	jobBackoffLimit   = 1
	jobActiveDeadline = 24 * time.Hour
)

// BackupResult is the JSON the archive container writes to its termination message.
type BackupResult struct {
	DatabaseSizeBytes int64 `json:"databaseSizeBytes"`
	DataSizeBytes     int64 `json:"dataSizeBytes"`
	Warnings          int64 `json:"warnings"`
}

// JobInput collects what every job builder needs.
type JobInput struct {
	Scheme *runtime.Scheme
	// Instance may be nil for cleanup jobs of orphaned backups.
	Instance     *forgejov1.Forgejo
	InstanceName string
	// ForgejoImage provides busybox tar/gzip and the uid 1000 "git" user.
	ForgejoImage  string
	PostgresImage string
	DB            *forgejo.DBConnection
	BackupPVC     string
	Affinity      *corev1.Affinity
}

// BackupJob dumps the database (init container, PostgreSQL image) and archives the data
// volume (Forgejo image) into "<pvc>/<backup>/".
func BackupJob(in JobInput, b *forgejov1.ForgejoBackup, includeData bool, startedAt metav1.Time) (*batchv1.Job, error) {
	dir := BackupDir(b.Name)
	env := []corev1.EnvVar{
		{Name: "BACKUP_NAME", Value: b.Name},
		{Name: "BACKUP_DIR", Value: dir},
	}
	mounts := []corev1.VolumeMount{
		{Name: volBackups, MountPath: BackupMountPath},
		{Name: volTmp, MountPath: "/tmp"},
	}
	excludes, _ := json.Marshal(DataExcludes)

	dump := container(ContainerDump, in.PostgresImage, dumpScript, mounts,
		append(append([]corev1.EnvVar{}, env...), dbEnv(in.DB)...))
	archiveEnv := append(append([]corev1.EnvVar{}, env...),
		corev1.EnvVar{Name: "INCLUDE_DATA", Value: strconv.FormatBool(includeData)},
		corev1.EnvVar{Name: "NAMESPACE", Value: b.Namespace},
		corev1.EnvVar{Name: "FORGEJO_NAME", Value: in.InstanceName},
		corev1.EnvVar{Name: "REASON", Value: string(Reason(b))},
		corev1.EnvVar{Name: "FORGEJO_IMAGE", Value: in.ForgejoImage},
		corev1.EnvVar{Name: "POSTGRES_IMAGE", Value: in.PostgresImage},
		corev1.EnvVar{Name: "DATABASE_NAME", Value: in.DB.Name},
		corev1.EnvVar{Name: "STARTED_AT", Value: startedAt.UTC().Format(time.RFC3339)},
		corev1.EnvVar{Name: "DATA_EXCLUDES_JSON", Value: string(excludes)},
	)
	archiveMounts := mounts
	volumes := []corev1.Volume{backupVolume(in.BackupPVC, false), tmpVolume()}
	if includeData {
		archiveMounts = append(append([]corev1.VolumeMount{}, mounts...),
			corev1.VolumeMount{Name: volData, MountPath: "/data", ReadOnly: true})
		volumes = append(volumes, dataVolume(in.Instance, true))
	}
	archive := container(ContainerArchive, in.ForgejoImage, archiveScript(), archiveMounts, archiveEnv)

	job := newJob(in, b.Namespace, BackupJobName(b), forgejo.ComponentBackup, in.Affinity, volumes,
		[]corev1.Container{dump}, archive)
	return job, controllerutil.SetControllerReference(b, job, in.Scheme)
}

// CleanupJob removes "<pvc>/<backup>/" when a ForgejoBackup is deleted.
func CleanupJob(in JobInput, b *forgejov1.ForgejoBackup) (*batchv1.Job, error) {
	c := container(ContainerCleanup, in.ForgejoImage, cleanupScript,
		[]corev1.VolumeMount{{Name: volBackups, MountPath: BackupMountPath}},
		[]corev1.EnvVar{{Name: "BACKUP_DIR", Value: BackupDir(b.Name)}})
	job := newJob(in, b.Namespace, CleanupJobName(b), forgejo.ComponentBackup, in.Affinity,
		[]corev1.Volume{backupVolume(in.BackupPVC, false)}, nil, c)
	return job, controllerutil.SetControllerReference(b, job, in.Scheme)
}

// RestoreJob verifies the backup files, then restores the database and/or the data volume.
// The data step runs as root (CHOWN/FOWNER/DAC_OVERRIDE only) so it can replace files of any
// owner and hand everything back to uid/gid 1000.
func RestoreJob(in JobInput, r *forgejov1.ForgejoRestore, b *forgejov1.ForgejoBackup, restoreDB, restoreData bool) (*batchv1.Job, error) {
	env := []corev1.EnvVar{
		{Name: "BACKUP_DIR", Value: BackupDir(b.Name)},
		{Name: "RESTORE_DATABASE", Value: strconv.FormatBool(restoreDB)},
		{Name: "RESTORE_DATA", Value: strconv.FormatBool(restoreData)},
	}
	roBackups := []corev1.VolumeMount{
		{Name: volBackups, MountPath: BackupMountPath, ReadOnly: true},
		{Name: volTmp, MountPath: "/tmp"},
	}
	volumes := []corev1.Volume{backupVolume(b.Status.PVCName, true), tmpVolume()}

	steps := []corev1.Container{container(ContainerVerify, in.ForgejoImage, verifyScript, roBackups, env)}
	if restoreDB {
		steps = append(steps, container(ContainerRestoreDB, in.PostgresImage, restoreDBScript, roBackups,
			append(append([]corev1.EnvVar{}, env...), dbEnv(in.DB)...)))
	}
	if restoreData {
		volumes = append(volumes, dataVolume(in.Instance, false))
		uid := forgejo.LayoutForImage(in.ForgejoImage).UID
		gid := forgejo.LayoutForImage(in.ForgejoImage).GID
		c := container(ContainerRestoreData, in.ForgejoImage, restoreDataScript,
			append(append([]corev1.VolumeMount{}, roBackups...), corev1.VolumeMount{Name: volData, MountPath: "/data"}),
			append(append([]corev1.EnvVar{}, env...), corev1.EnvVar{Name: "OWNER", Value: fmt.Sprintf("%d:%d", uid, gid)}))
		c.SecurityContext.RunAsUser = ptr.To[int64](0)
		c.SecurityContext.RunAsGroup = ptr.To[int64](0)
		c.SecurityContext.RunAsNonRoot = ptr.To(false)
		// FSETID keeps setgid directory bits (set by fsGroup) when tar restores modes.
		c.SecurityContext.Capabilities.Add = []corev1.Capability{"CHOWN", "FOWNER", "DAC_OVERRIDE", "FSETID"}
		steps = append(steps, c)
	}

	last := len(steps) - 1
	job := newJob(in, r.Namespace, RestoreJobName(r), forgejo.ComponentRestore, in.Affinity, volumes, steps[:last], steps[last])
	// The restore job must not change ownership of the data volume behind the data step's back.
	job.Spec.Template.Spec.SecurityContext.FSGroup = nil
	job.Spec.Template.Spec.SecurityContext.FSGroupChangePolicy = nil
	return job, controllerutil.SetControllerReference(r, job, in.Scheme)
}

func newJob(in JobInput, namespace, name, component string, affinity *corev1.Affinity, volumes []corev1.Volume,
	initContainers []corev1.Container, main corev1.Container) *batchv1.Job {
	labelOwner := in.Instance
	if labelOwner == nil {
		labelOwner = &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: in.InstanceName}}
	}
	labels := forgejo.Labels(labelOwner, component)
	layout := forgejo.LayoutForImage(in.ForgejoImage)

	spec := corev1.PodSpec{
		RestartPolicy:  corev1.RestartPolicyNever,
		InitContainers: initContainers,
		Containers:     []corev1.Container{main},
		Volumes:        volumes,
		Affinity:       affinity,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser:           ptr.To(layout.UID),
			RunAsGroup:          ptr.To(layout.GID),
			RunAsNonRoot:        ptr.To(true),
			FSGroup:             ptr.To(layout.GID),
			FSGroupChangePolicy: ptr.To(corev1.FSGroupChangeOnRootMismatch),
			SeccompProfile:      &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		AutomountServiceAccountToken: ptr.To(false),
	}
	if f := in.Instance; f != nil {
		spec.ImagePullSecrets = f.Spec.ImagePullSecrets
		spec.Tolerations = f.Spec.PodTemplate.Tolerations
		spec.NodeSelector = f.Spec.PodTemplate.NodeSelector
		spec.PriorityClassName = f.Spec.PodTemplate.PriorityClassName
		if f.Spec.ImagePullPolicy != "" {
			for i := range spec.InitContainers {
				spec.InitContainers[i].ImagePullPolicy = f.Spec.ImagePullPolicy
			}
			spec.Containers[0].ImagePullPolicy = f.Spec.ImagePullPolicy
		}
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          ptr.To[int32](jobBackoffLimit),
			ActiveDeadlineSeconds: ptr.To(int64(jobActiveDeadline.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       spec,
			},
		},
	}
}

func container(name, image, script string, mounts []corev1.VolumeMount, env []corev1.EnvVar) corev1.Container {
	return corev1.Container{
		Name:                     name,
		Image:                    image,
		Command:                  []string{"/bin/sh", "-c", script},
		Env:                      append(append([]corev1.EnvVar{}, env...), corev1.EnvVar{Name: "HOME", Value: "/tmp"}),
		VolumeMounts:             mounts,
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
	}
}

// dbEnv configures libpq; credentials come from the database Secret, never from the spec.
func dbEnv(db *forgejo.DBConnection) []corev1.EnvVar {
	secretRef := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: db.SecretName},
			Key:                  key,
		}}
	}
	return []corev1.EnvVar{
		{Name: "PGHOST", Value: db.Host},
		{Name: "PGPORT", Value: strconv.Itoa(int(db.Port))},
		{Name: "PGDATABASE", Value: db.Name},
		{Name: "PGSSLMODE", Value: db.SSLMode},
		{Name: "PGCONNECT_TIMEOUT", Value: "15"},
		{Name: "PGAPPNAME", Value: "forgejo-operator"},
		{Name: "PGUSER", ValueFrom: secretRef(db.UsernameKey)},
		{Name: "PGPASSWORD", ValueFrom: secretRef(db.PasswordKey)},
	}
}

func backupVolume(claim string, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: volBackups, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim, ReadOnly: readOnly},
	}}
}

func dataVolume(f *forgejov1.Forgejo, readOnly bool) corev1.Volume {
	return corev1.Volume{Name: volData, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: DataClaimName(f), ReadOnly: readOnly},
	}}
}

func tmpVolume() corev1.Volume {
	return corev1.Volume{Name: volTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}
}

// shellFail writes its message to the termination log so the controller can surface it.
const shellFail = `fail() { echo "$*" >&2; printf '%s' "$*" | head -c 3000 > /dev/termination-log; exit 1; }
`

// dumpScript runs in the PostgreSQL image. The dump is a single transactionally consistent
// snapshot; it is written under a temporary name so a partial file is never mistaken for a dump.
const dumpScript = `set -eu
` + shellFail + `umask 077
mkdir -p "$BACKUP_DIR"
rm -f "$BACKUP_DIR/db.dump.partial"
pg_dump --version > "$BACKUP_DIR/.pg_dump_version"
pg_dump --format=custom --lock-wait-timeout=300s --file="$BACKUP_DIR/db.dump.partial" || fail "pg_dump of database $PGDATABASE failed"
mv "$BACKUP_DIR/db.dump.partial" "$BACKUP_DIR/db.dump"
echo "database dump written: $(stat -c %s "$BACKUP_DIR/db.dump") bytes"
`

// benignTarErrors are what tar reports when files change or vanish while a live volume is
// archived (busybox and GNU wording). tar exits 1 for them; any other message is fatal.
const benignTarErrors = `No such file or directory|file changed as we read it|File removed before we read it|socket ignored|error exit delayed from previous errors`

func archiveScript() string {
	var excludes strings.Builder
	for _, e := range DataExcludes {
		excludes.WriteString(" --exclude ./" + e)
	}
	return `set -eu
` + shellFail + `umask 077
dump="$BACKUP_DIR/db.dump"
[ -s "$dump" ] || fail "database dump $dump is missing or empty"
db_size=$(stat -c %s "$dump")
data_size=0
warnings=0
data_file=""
if [ "$INCLUDE_DATA" = true ]; then
  out="$BACKUP_DIR/data.tar.gz"
  rm -f "$out" "$out.partial"
  set +e
  { tar -C /data -cf -` + excludes.String() + ` . 2>/tmp/tar.err; echo $? > /tmp/tar.rc; } | gzip -6 > "$out.partial"
  gzip_rc=$?
  set -e
  tar_rc=$(cat /tmp/tar.rc)
  cat /tmp/tar.err >&2
  [ "$gzip_rc" -eq 0 ] || fail "gzip failed with exit code $gzip_rc (backup volume full?)"
  if [ "$tar_rc" -ne 0 ]; then
    bad=$(grep -vE '` + benignTarErrors + `' /tmp/tar.err || true)
    if [ "$tar_rc" -ne 1 ] || [ -n "$bad" ] || [ ! -s /tmp/tar.err ]; then
      fail "tar failed with exit code $tar_rc: $bad"
    fi
    warnings=$(grep -cv 'error exit delayed' /tmp/tar.err || true)
    echo "tar: $warnings file(s) changed or vanished while archiving; continuing" >&2
  fi
  tar -tzf "$out.partial" > /dev/null || fail "verification of the data archive failed"
  mv "$out.partial" "$out"
  data_size=$(stat -c %s "$out")
  data_file="data.tar.gz"
  echo "data archive written: $data_size bytes"
fi
pg_dump_version=$(cat "$BACKUP_DIR/.pg_dump_version" 2>/dev/null | tr -d '"\\' || true)
rm -f "$BACKUP_DIR/.pg_dump_version"
cat > "$BACKUP_DIR/manifest.json.partial" <<EOF
{
  "formatVersion": 1,
  "backup": "$BACKUP_NAME",
  "namespace": "$NAMESPACE",
  "forgejo": "$FORGEJO_NAME",
  "reason": "$REASON",
  "forgejoImage": "$FORGEJO_IMAGE",
  "postgresImage": "$POSTGRES_IMAGE",
  "startedAt": "$STARTED_AT",
  "completedAt": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "database": {"file": "db.dump", "format": "pg_dump-custom", "name": "$DATABASE_NAME", "pgDumpVersion": "$pg_dump_version", "sizeBytes": $db_size},
  "data": {"included": $INCLUDE_DATA, "file": "$data_file", "sizeBytes": $data_size, "excludes": $DATA_EXCLUDES_JSON, "warnings": $warnings}
}
EOF
mv "$BACKUP_DIR/manifest.json.partial" "$BACKUP_DIR/manifest.json"
printf '{"databaseSizeBytes":%s,"dataSizeBytes":%s,"warnings":%s}' "$db_size" "$data_size" "$warnings" > /dev/termination-log
`
}

const cleanupScript = `set -eu
rm -rf "$BACKUP_DIR"
echo "removed $BACKUP_DIR"
`

// verifyScript checks the backup files before anything is modified.
const verifyScript = `set -eu
` + shellFail + `if [ "$RESTORE_DATABASE" = true ]; then
  [ -s "$BACKUP_DIR/db.dump" ] || fail "$BACKUP_DIR/db.dump is missing or empty"
  [ "$(head -c 5 "$BACKUP_DIR/db.dump")" = PGDMP ] || fail "$BACKUP_DIR/db.dump is not a pg_dump custom-format archive"
fi
if [ "$RESTORE_DATA" = true ]; then
  [ -s "$BACKUP_DIR/data.tar.gz" ] || fail "$BACKUP_DIR/data.tar.gz is missing or empty"
  tar -tzf "$BACKUP_DIR/data.tar.gz" > /dev/null || fail "$BACKUP_DIR/data.tar.gz is corrupt"
fi
echo "backup files verified"
`

// restoreDBScript replaces the database with the dump in one transaction. When the app user
// may drop and recreate schema "public" (it owns the database, as with CNPG), the schema is
// recreated so objects created after the backup (e.g. by a newer Forgejo's migrations) are
// gone too. Otherwise only the objects contained in the dump are dropped (--clean). COMMIT is
// only sent when pg_restore succeeded, so a failure leaves the database untouched.
const restoreDBScript = `set -eu
` + shellFail + `dump="$BACKUP_DIR/db.dump"
pg_restore --list "$dump" > /dev/null || fail "cannot read $dump"
mode=$(psql -X -At -v ON_ERROR_STOP=1 -c "SELECT CASE
  WHEN NOT has_database_privilege(current_database(), 'CREATE') THEN 'clean'
  WHEN NOT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'public') THEN 'create'
  WHEN (SELECT pg_has_role(current_user, nspowner, 'USAGE') FROM pg_namespace WHERE nspname = 'public') THEN 'recreate'
  ELSE 'clean' END") || fail "cannot connect to database $PGDATABASE on $PGHOST"
case "$mode" in
  recreate) prelude='DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION CURRENT_USER;'; clean='' ;;
  create) prelude='CREATE SCHEMA public AUTHORIZATION CURRENT_USER;'; clean='' ;;
  *) echo "warning: $PGUSER may not recreate schema public; only objects contained in the backup are replaced" >&2
     prelude=''; clean='--clean --if-exists' ;;
esac
echo "restoring database $PGDATABASE (mode: $mode)"
{
  printf 'SET client_min_messages = warning;\nBEGIN;\n%s\n' "$prelude"
  if pg_restore --no-owner --no-acl $clean --file=- "$dump"; then
    echo 'COMMIT;'
  else
    echo 1 > /tmp/pg_restore.failed
  fi
} | psql -X -q -v ON_ERROR_STOP=1 --file=- > /dev/null || fail "applying the dump to $PGDATABASE failed; the database was left unchanged"
[ ! -e /tmp/pg_restore.failed ] || fail "pg_restore could not read $dump; the database was left unchanged"
echo "database restored"
`

// restoreDataScript replaces the data volume's contents (except lost+found) with the archive.
const restoreDataScript = `set -eu
` + shellFail + `[ "$RESTORE_DATA" = true ] || exit 0
archive="$BACKUP_DIR/data.tar.gz"
find /data -mindepth 1 -maxdepth 1 ! -name lost+found -exec rm -rf {} +
tar -xzf "$archive" -C /data || fail "extracting $archive failed"
chown "$OWNER" /data
find /data -mindepth 1 -maxdepth 1 ! -name lost+found -exec chown -R -h "$OWNER" {} +
echo "data volume restored"
`
