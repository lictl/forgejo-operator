package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const keySourceGenerated = "Generated"

const (
	// remoteRetentionInterval is how often remote retention runs without a new upload.
	remoteRetentionInterval = time.Hour
	// RemoteIncompleteAge is how long a directory without a manifest is left alone: longer
	// than an upload Job may run (jobActiveDeadline), so it is no upload in progress.
	RemoteIncompleteAge = jobActiveDeadline + time.Hour
	// remoteRetentionTimeout bounds one run of remote retention.
	remoteRetentionTimeout = time.Minute
)

// remoteRetentionRun is the last run of remote retention for an instance.
type remoteRetentionRun struct {
	at time.Time
	// trigger is the newest uploaded scheduled backup when it ran: a new upload runs it again.
	trigger string
	err     error
}

// remoteRetentionRuns maps types.NamespacedName to *remoteRetentionRun.
var remoteRetentionRuns sync.Map

// remoteManifests caches the manifests retention read ("<location>|<fingerprint>" ->
// *RemoteManifest); a backup's manifest never changes.
var remoteManifests sync.Map

func forgetRemoteRetention(namespace, name string) {
	remoteRetentionRuns.Delete(types.NamespacedName{Namespace: namespace, Name: name})
}

// ForgetRemote drops what the operator remembers of a deleted instance's remote backups and of
// its reads of spec.restoreFrom.
func ForgetRemote(namespace, name string) {
	forgetRemoteBackups(namespace, name)
	forgetIdentityReadFailure(namespace, name)
}

// forgetRemoteBackups drops what the operator remembers of the instance's remote backups.
func forgetRemoteBackups(namespace, name string) {
	forgetRemoteRetention(namespace, name)
	ForgetLastUpload(namespace, name)
}

// keyProblem is a problem with the backup key that BackupHealthy reports (empty: none).
type keyProblem struct{ reason, message string }

// summarizeRemote reports spec.backup.remote in status.backup.remote: where backups go, the
// key, and the newest backup uploaded. It returns a problem with the key for BackupHealthy: a
// key Secret that is missing or unusable, or a generated key nobody acknowledged to have
// copied out of the cluster (forgejo.lucas.icu/backup-key-exported).
func summarizeRemote(ctx context.Context, rc *forgejo.ReconcileContext, backups []forgejov1.ForgejoBackup) keyProblem {
	f := rc.Instance
	if !RemoteEnabled(f) {
		if f.Status.Backup != nil {
			f.Status.Backup.Remote = nil
		}
		// Not ForgetRemote: the backoff of spec.restoreFrom reads has nothing to do with
		// spec.backup.remote.
		forgetRemoteBackups(f.Namespace, f.Name)
		return keyProblem{}
	}
	st := summary(f)
	if st.Remote == nil {
		st.Remote = &forgejov1.RemoteBackupSummary{}
	}
	r := st.Remote
	r.Location = RemoteLocation(&f.Spec.Backup.Remote.S3, InstanceRemotePath(f.Namespace, f.Name))
	ref := BackupKeyRef(f)
	if r.KeySecretName != ref.Name {
		r.KeyFingerprint = ""
	}
	r.KeySecretName = ref.Name
	key := backupKeyProblem(ctx, rc, ref, r)
	var problems []string
	if key.message != "" {
		problems = append(problems, key.message)
	}
	if run, ok := remoteRetentionRuns.Load(types.NamespacedName{Namespace: f.Namespace, Name: f.Name}); ok && run.(*remoteRetentionRun).err != nil {
		problems = append(problems, "Remote retention failed: "+run.(*remoteRetentionRun).err.Error())
	}
	r.Message = strings.Join(problems, "; ")

	r.LastUploadedBackupName, r.LastUploadTime = "", nil
	for i := range backups {
		b := &backups[i]
		u := b.Status.Remote
		if u == nil || u.Phase != forgejov1.RemoteBackupUploaded || u.CompletionTime == nil || b.DeletionTimestamp != nil {
			continue
		}
		if r.LastUploadTime == nil || u.CompletionTime.After(r.LastUploadTime.Time) {
			r.LastUploadedBackupName, r.LastUploadTime = b.Name, u.CompletionTime.DeepCopy()
		}
	}
	if r.LastUploadTime != nil {
		RecordLastUpload(f.Namespace, f.Name, r.LastUploadTime.Time)
	}
	return key
}

// backupKeyProblem checks the backup key ref and records its fingerprint in r. The fingerprint
// of a key whose Secret is missing or unusable is kept: it is what the backups need, and what
// keeps the operator from generating a new key in its place (see core.ensureBackupKey).
func backupKeyProblem(ctx context.Context, rc *forgejo.ReconcileContext, ref forgejov1.BackupKeySecretRef, r *forgejov1.RemoteBackupSummary) keyProblem {
	f := rc.Instance
	generated := ref.Name == forgejo.BackupKeySecretName(f) && f.Spec.Backup.Remote.Encryption.KeySecret == nil
	cipher, err := ReadBackupKey(ctx, rc.Client, f.Namespace, ref)
	if err != nil {
		sec := &corev1.Secret{}
		if gerr := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: ref.Name}, sec); apierrors.IsNotFound(gerr) && r.KeyFingerprint != "" {
			msg := fmt.Sprintf("Backups cannot be uploaded: Secret %q with the backup key (fingerprint %s) is missing; restore it from your copy",
				ref.Name, r.KeyFingerprint)
			if generated {
				msg += fmt.Sprintf(", or set the annotation %s=%s on the Forgejo to have a new key generated (the backups encrypted "+
					"with the lost key can then not be restored)", forgejo.AnnotationBackupKeyLost, r.KeyFingerprint)
			}
			return keyProblem{reason: "BackupKeyMissing", message: msg}
		}
		return keyProblem{reason: "BackupKeyUnusable", message: fmt.Sprintf("Backups cannot be uploaded: %v", err)}
	}
	r.KeyFingerprint = cipher.Fingerprint()
	if !generated {
		return keyProblem{}
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: ref.Name}, sec); err != nil {
		return keyProblem{}
	}
	if sec.Annotations[forgejo.AnnotationBackupKey] != keySourceGenerated || sec.Annotations[forgejo.AnnotationBackupKeyExported] == valueTrue {
		return keyProblem{}
	}
	return keyProblem{reason: "BackupKeyNotExported", message: fmt.Sprintf("The generated backup key in Secret %s (fingerprint %s) has "+
		"not been copied out of the cluster, and remote backups cannot be restored without it: export it "+
		"(kubectl -n %s get secret %s -o jsonpath='{.data.key}' | base64 -d), keep it offline, then annotate the Secret with %s=true",
		ref.Name, r.KeyFingerprint, f.Namespace, ref.Name, forgejo.AnnotationBackupKeyExported)}
}

// newestUploadedScheduled names the newest scheduled backup whose upload completed.
func newestUploadedScheduled(backups []forgejov1.ForgejoBackup) string {
	newest, at := "", time.Time{}
	for i := range backups {
		b := &backups[i]
		if Reason(b) != forgejov1.BackupReasonScheduled || b.Status.Remote == nil ||
			b.Status.Remote.Phase != forgejov1.RemoteBackupUploaded || b.Status.Remote.CompletionTime == nil {
			continue
		}
		if t := b.Status.Remote.CompletionTime.Time; newest == "" || t.After(at) {
			newest, at = b.Name, t
		}
	}
	return newest
}

// pruneRemote applies retention to the remote copies of scheduled backups, after each new
// upload of a scheduled backup and at least hourly. It lists the instance's remote directory,
// so it also prunes backups whose ForgejoBackup is gone (e.g. taken before the cluster was
// rebuilt). Failures are reported in status.backup.remote.message and an event, never as a
// step error: they must not hold up local backups.
func pruneRemote(ctx context.Context, rc *forgejo.ReconcileContext, backups []forgejov1.ForgejoBackup, now time.Time) {
	f := rc.Instance
	if !RemoteEnabled(f) {
		return
	}
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	trigger := newestUploadedScheduled(backups)
	if v, ok := remoteRetentionRuns.Load(key); ok {
		last := v.(*remoteRetentionRun)
		if last.trigger == trigger && now.Sub(last.at) < remoteRetentionInterval {
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, remoteRetentionTimeout)
	defer cancel()
	err := runRemoteRetention(cctx, rc, backups, now)
	var previous error
	if v, ok := remoteRetentionRuns.Load(key); ok {
		previous = v.(*remoteRetentionRun).err
	}
	remoteRetentionRuns.Store(key, &remoteRetentionRun{at: now, trigger: trigger, err: err})
	st := summary(f)
	if st.Remote == nil {
		st.Remote = &forgejov1.RemoteBackupSummary{}
	}
	st.Remote.LastRetentionTime = &metav1.Time{Time: now}
	if err != nil {
		logf.FromContext(ctx).Info("Remote retention failed", "error", err.Error())
		if previous == nil || previous.Error() != err.Error() {
			rc.Event(corev1.EventTypeWarning, "RemoteRetentionFailed", "Backup", "Remote retention of %s failed: %v", st.Remote.Location, err)
		}
		msg := "Remote retention failed: " + err.Error()
		if !strings.Contains(st.Remote.Message, msg) {
			st.Remote.Message = strings.TrimPrefix(st.Remote.Message+"; "+msg, "; ")
		}
	}
}

// runRemoteRetention keeps the newest spec.backup.retention complete scheduled backups of the
// instance in the bucket and deletes the older ones. Only this instance's backups count (the
// UID in the manifest), and those its spec.restoreFrom took over (see ownsRemoteBackup): an
// instance of the same namespace and name in another cluster, e.g. a disaster recovery drill,
// keeps its own. Manual and pre-upgrade backups are never deleted, and neither is a backup
// whose manifest cannot be read (e.g. one encrypted with another key). Directories without a
// manifest (failed uploads) are deleted once they are older than any upload can run.
func runRemoteRetention(ctx context.Context, rc *forgejo.ReconcileContext, backups []forgejov1.ForgejoBackup, now time.Time) error {
	f := rc.Instance
	conn := &f.Spec.Backup.Remote.S3
	store, err := OpenStore(ctx, rc.Client, f.Namespace, conn, BackupKeyRef(f))
	if err != nil {
		return err
	}
	base := InstanceRemotePath(f.Namespace, f.Name)
	dirs, err := store.ListDirs(ctx, base)
	if err != nil {
		return fmt.Errorf("listing %s: %w", store.Location(base), err)
	}
	protected, err := protectedRemoteDirs(ctx, rc, backups)
	if err != nil {
		return err
	}

	type candidate struct {
		name string
		at   time.Time
	}
	var scheduled []candidate
	var errs []error
	for name, d := range dirs {
		if protected[name] {
			continue
		}
		path := base + "/" + name
		if !d.Complete {
			if now.Sub(d.LastModified) > RemoteIncompleteAge {
				if err := store.DeleteDir(ctx, path); err != nil {
					errs = append(errs, fmt.Errorf("deleting the incomplete upload %s: %w", store.Location(path), err))
					continue
				}
				rc.Event(corev1.EventTypeNormal, "RemoteBackupPruned", "Backup",
					"Deleted %s: an upload that never completed (no %s)", store.Location(path), RemoteManifestFile)
			}
			continue
		}
		m, err := cachedManifest(ctx, store, path)
		if err != nil {
			errs = append(errs, fmt.Errorf("reading the manifest of %s (kept): %w", store.Location(path), err))
			continue
		}
		if m.Reason != forgejov1.BackupReasonScheduled || m.Namespace != f.Namespace || m.Forgejo != f.Name ||
			RemoteBackupDirName(m.Backup, types.UID(m.BackupUID)) != name || !ownsRemoteBackup(f, m) {
			continue
		}
		at := m.CompletedTime()
		if at.IsZero() {
			at = d.LastModified
		}
		scheduled = append(scheduled, candidate{name: name, at: at})
	}
	slices.SortFunc(scheduled, func(a, b candidate) int {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
		return strings.Compare(b.name, a.name)
	})
	keep := int(f.Spec.Backup.Retention)
	if keep < 1 {
		keep = 7
	}
	for i := keep; i < len(scheduled); i++ {
		path := base + "/" + scheduled[i].name
		if err := store.DeleteDir(ctx, path); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", store.Location(path), err))
			continue
		}
		remoteManifests.Delete(store.Location(path) + "|" + store.KeyFingerprint())
		rc.Event(corev1.EventTypeNormal, "RemoteBackupPruned", "Backup", "Deleted the remote copy of scheduled backup %s at %s (retention %d)",
			scheduled[i].name, store.Location(path), keep)
	}
	return errors.Join(errs...)
}

// ownsRemoteBackup reports whether remote retention of f may prune the backup of m: f took it,
// or f was created from a backup of the instance that took it (spec.restoreFrom) and m
// completed no later than that backup, i.e. before the disaster. Later backups of that
// instance (still running elsewhere) are not f's.
func ownsRemoteBackup(f *forgejov1.Forgejo, m *RemoteManifest) bool {
	if m.ForgejoUID == "" {
		return false
	}
	if m.ForgejoUID == string(f.UID) {
		return true
	}
	rf := f.Status.RestoredFrom
	if rf == nil || rf.Phase != forgejov1.RestoredFromCompleted || rf.InstanceUID != m.ForgejoUID || rf.BackupCompletionTime == nil {
		return false
	}
	at := m.CompletedTime()
	return !at.IsZero() && !at.After(rf.BackupCompletionTime.Time)
}

func cachedManifest(ctx context.Context, store *Store, path string) (*RemoteManifest, error) {
	key := store.Location(path) + "|" + store.KeyFingerprint()
	if v, ok := remoteManifests.Load(key); ok {
		return v.(*RemoteManifest), nil
	}
	m, err := store.ReadManifest(ctx, path)
	if err != nil {
		return nil, err
	}
	remoteManifests.Store(key, m)
	return m, nil
}

// protectedRemoteDirs are the backup directories of the instance that retention must not
// touch: uploads in progress, backups an unfinished restore of any namespace reads, and the
// spec.restoreFrom source of every Forgejo in the cluster that is being created from it (also
// before its ForgejoRestore exists: while it restores the identity and waits for the
// database).
//
// A source counts when it names the same bucket and prefix, whatever its endpoint: the same
// endpoint has many spellings (case, port, scheme, a short Service name resolved in the
// restore's namespace), and a false match only keeps a backup a little longer.
func protectedRemoteDirs(ctx context.Context, rc *forgejo.ReconcileContext, backups []forgejov1.ForgejoBackup) (map[string]bool, error) {
	f := rc.Instance
	conn := &f.Spec.Backup.Remote.S3
	base := InstanceRemotePath(f.Namespace, f.Name) + "/"
	out := map[string]bool{}
	for i := range backups {
		if r := backups[i].Status.Remote; r != nil && (r.Phase == forgejov1.RemoteBackupPending || r.Phase == forgejov1.RemoteBackupUploading) {
			out[RemoteBackupDirName(backups[i].Name, backups[i].UID)] = true
		}
	}
	sameBucket := func(c *forgejov1.S3Connection) bool {
		return c.Bucket == conn.Bucket && forgejo.S3Prefix(c) == forgejo.S3Prefix(conn)
	}
	protect := func(src *forgejov1.RemoteBackupSource) {
		if src == nil || !sameBucket(&src.S3) {
			return
		}
		if dir, ok := strings.CutPrefix(strings.Trim(src.Path, "/")+"/", base); ok && dir != "" {
			out[strings.TrimSuffix(dir, "/")] = true
		}
	}
	if Bootstrapping(f) {
		protect(&f.Spec.RestoreFrom.Remote)
	}
	instances := &forgejov1.ForgejoList{}
	if err := rc.Client.List(ctx, instances); err != nil {
		return nil, fmt.Errorf("listing instances: %w", err)
	}
	for i := range instances.Items {
		if o := &instances.Items[i]; Bootstrapping(o) {
			protect(&o.Spec.RestoreFrom.Remote)
		}
	}
	restores := &forgejov1.ForgejoRestoreList{}
	if err := rc.Client.List(ctx, restores); err != nil {
		return nil, fmt.Errorf("listing restores: %w", err)
	}
	for i := range restores.Items {
		if r := &restores.Items[i]; IsRestoreActive(r) {
			protect(r.Spec.Remote)
		}
	}
	return out, nil
}
