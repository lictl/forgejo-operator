package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/s3"
)

const (
	// inspectTimeout bounds the operator's look at (and cleanup of) what an upload that did not
	// succeed left in the bucket.
	inspectTimeout = 30 * time.Second
	// uploadPodStopWait is how long a pod of a failed or vanished upload Job may stay
	// terminating before the operator stops waiting for it; the bucket is then left alone,
	// because the pod may still write (e.g. on a lost node).
	uploadPodStopWait = 5 * time.Minute
)

// uploadJobName is the upload Job of b, or "" when b is not uploaded.
func uploadJobName(b *forgejov1.ForgejoBackup) string {
	if b.Status.Remote == nil || b.Status.Remote.JobName == "" {
		return ""
	}
	return b.Status.Remote.JobName
}

// reconcileUpload uploads a completed backup to spec.backup.remote of its instance (status.remote:
// Pending → Uploading → Uploaded or Failed). A failed upload never fails the backup itself: the
// copy on the backup PVC stays usable.
func (r *ForgejoBackupReconciler) reconcileUpload(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) (ctrl.Result, error) {
	st := b.Status.Remote
	if st == nil {
		return ctrl.Result{}, nil
	}
	switch st.Phase {
	case forgejov1.RemoteBackupUploaded, forgejov1.RemoteBackupFailed:
		return ctrl.Result{}, r.releaseUpload(ctx, b)
	case forgejov1.RemoteBackupUploading:
		return r.trackUpload(ctx, b, f)
	default:
		return r.startUpload(ctx, b, f)
	}
}

// releaseUpload deletes the finished upload Job and the identity Secret it read.
func (r *ForgejoBackupReconciler) releaseUpload(ctx context.Context, b *forgejov1.ForgejoBackup) error {
	if err := backup.DeleteFinishedJob(ctx, r.Client, b, uploadJobName(b)); err != nil {
		return fmt.Errorf("deleting finished upload job: %w", err)
	}
	sec := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: backup.IdentitySecretName(b)}, sec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(sec, b) || !sec.DeletionTimestamp.IsZero() {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, sec, client.Preconditions{UID: &sec.UID}))
}

func (r *ForgejoBackupReconciler) startUpload(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) (ctrl.Result, error) {
	if f == nil || !f.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.uploadFailed(ctx, b, "InstanceNotFound", fmt.Sprintf("Not uploaded: Forgejo %q is gone", b.Spec.ForgejoRef.Name))
	}
	if !backup.RemoteEnabled(f) {
		return ctrl.Result{}, r.uploadFailed(ctx, b, "RemoteDisabled", "Not uploaded: spec.backup.remote is not set any more")
	}
	conn := &f.Spec.Backup.Remote.S3
	keyRef := backup.BackupKeyRef(f)
	failConfig := func(err error) (ctrl.Result, error) {
		if backup.IsConfigError(err) {
			return ctrl.Result{}, r.uploadFailed(ctx, b, "InvalidConfiguration", "Not uploaded: "+err.Error())
		}
		return ctrl.Result{}, err
	}
	cipher, err := backup.ReadBackupKey(ctx, r.Client, b.Namespace, keyRef)
	if err != nil {
		return failConfig(err)
	}
	for _, k := range []string{forgejo.S3AccessKeyIDKey(conn), forgejo.S3SecretAccessKeyKey(conn)} {
		if _, err := backup.ReadSecretValue(ctx, r.Client, b.Namespace, conn.CredentialsSecret.Name, k); err != nil {
			return failConfig(fmt.Errorf("S3 credentials: %w", err))
		}
	}
	if ref := forgejo.S3CASecret(conn); ref != nil {
		if _, err := backup.ReadSecretValue(ctx, r.Client, b.Namespace, ref.Name, ref.Key); err != nil {
			return failConfig(fmt.Errorf("CA certificate: %w", err))
		}
	}
	id, err := backup.BuildIdentity(ctx, r.Client, f)
	if err != nil {
		return ctrl.Result{}, r.uploadFailed(ctx, b, "IdentityUnavailable", "Not uploaded: "+err.Error())
	}
	pgImage := b.Status.PostgresImage
	if pgImage == "" {
		pgImage = forgejo.RunningPostgresImage(f)
	}
	m := backup.RemoteManifest{
		Backup: b.Name, Namespace: b.Namespace, Forgejo: f.Name, BackupUID: string(b.UID), ForgejoUID: string(f.UID),
		Reason: backup.Reason(b), ForgejoImage: b.Status.ForgejoImage, PostgresImage: pgImage,
		IncludesData: b.Status.IncludesData, KeyFingerprint: cipher.Fingerprint(),
	}
	if s := id.Secret(backup.IdentityRoleSecrets); s != nil {
		m.SecretKeyFingerprint = backup.SecretKeyFingerprint(s.Data)
	}
	if b.Status.CompletionTime != nil {
		m.CompletedAt = b.Status.CompletionTime.UTC().Format(time.RFC3339)
	}
	head, err := backup.ManifestHead(m)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureIdentitySecret(ctx, b, f, id); err != nil {
		var conflict *identityConflictError
		if errors.As(err, &conflict) {
			return ctrl.Result{}, r.uploadFailed(ctx, b, "SecretNameConflict", "Not uploaded: "+err.Error())
		}
		return ctrl.Result{}, err
	}
	affinity, err := backup.Placement(ctx, r.Client, f, false, b.Status.PVCName)
	if err != nil {
		var conflict *backup.VolumeConflictError
		if errors.As(err, &conflict) {
			return ctrl.Result{}, r.uploadFailed(ctx, b, "BackupVolumeConflict", "Not uploaded: "+conflict.Error())
		}
		return ctrl.Result{}, err
	}
	job, err := backup.UploadJob(backup.JobInput{
		Scheme: r.Scheme, Instance: f, InstanceName: f.Name, ForgejoImage: backup.CurrentImage(f), Affinity: affinity,
	}, b, backup.RemoteTarget{Conn: conn, Key: keyRef, Image: backup.RcloneImage(f), Path: b.Status.Remote.Path},
		backup.IdentitySecretName(b), head)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := backup.CreateJob(ctx, r.Client, b, job); err != nil {
		var conflict *backup.JobConflictError
		if !errors.As(err, &conflict) {
			return ctrl.Result{}, fmt.Errorf("creating upload job: %w", err)
		}
		if conflict.Transient {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{}, r.uploadFailed(ctx, b, "JobNameConflict", fmt.Sprintf("Not uploaded: %v", conflict))
	}

	orig := b.DeepCopy()
	now := metav1.Now()
	st := b.Status.Remote
	st.Phase = forgejov1.RemoteBackupUploading
	st.JobName = job.Name
	st.StartTime = &now
	// What the Job writes to: an upload that does not succeed is checked there, whatever
	// spec.backup.remote says by then.
	st.S3 = conn.DeepCopy()
	st.KeySecret = &keyRef
	st.KeyFingerprint = cipher.Fingerprint()
	st.Message = fmt.Sprintf("Upload job %s is uploading to %s", job.Name, st.Location)
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return ctrl.Result{}, err
	}
	r.event(b, corev1.EventTypeNormal, "UploadStarted", "Upload", "Started job %s uploading to %s", job.Name, st.Location)
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// identityConflictError: the identity Secret's name is taken by a Secret of something else.
type identityConflictError struct{ name string }

func (e *identityConflictError) Error() string {
	return fmt.Sprintf("Secret %s exists and does not belong to this ForgejoBackup", e.name)
}

// ensureIdentitySecret hands the identity bundle to the upload Job in Secret
// "<backup>-identity", controlled by the backup (garbage-collected with it, deleted once the
// upload finished).
func (r *ForgejoBackupReconciler) ensureIdentitySecret(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo, id *backup.Identity) error {
	data, err := json.Marshal(id)
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: backup.IdentitySecretName(b), Namespace: b.Namespace}}
	err = r.Get(ctx, client.ObjectKeyFromObject(sec), sec)
	switch {
	case apierrors.IsNotFound(err):
		sec.Labels = forgejo.Labels(f, forgejo.ComponentBackup)
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = map[string][]byte{backup.IdentityFile: data}
		if err := controllerutil.SetControllerReference(b, sec, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, sec)
	case err != nil:
		return err
	case !metav1.IsControlledBy(sec, b):
		return &identityConflictError{name: sec.Name}
	}
	sec.Data = map[string][]byte{backup.IdentityFile: data}
	return r.Update(ctx, sec)
}

func (r *ForgejoBackupReconciler) trackUpload(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) (ctrl.Result, error) {
	st := b.Status.Remote
	var job *batchv1.Job
	found := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: st.JobName}, found); err == nil {
		job = found
	} else {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if st.StartTime != nil && time.Since(st.StartTime.Time) < jobAppearGrace {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: st.JobName}, found); err == nil {
			job = found
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	if job != nil {
		if err := backup.CheckOwned(job, b); err != nil {
			// Not this backup's Job: what it wrote is not this upload's to look at.
			return ctrl.Result{}, r.uploadFailed(ctx, b, "JobNameConflict", fmt.Sprintf("Upload job %s was replaced: %v", st.JobName, err))
		}
	}
	pods, err := backup.JobPodsByName(ctx, r.Client, b.Namespace, st.JobName)
	if err != nil {
		return ctrl.Result{}, err
	}
	reason, failure := "JobMissing", fmt.Sprintf("Upload job %s disappeared", st.JobName)
	if job != nil {
		state, jobMsg := backup.StateOf(job)
		switch state {
		case backup.JobSucceeded:
			res, err := backup.ParseUploadResult(pods)
			if err != nil {
				logf.FromContext(ctx).Error(err, "Upload succeeded but its result is unknown", "job", job.Name)
			}
			done := metav1.Now()
			if job.Status.CompletionTime != nil {
				done = *job.Status.CompletionTime
			}
			msg := fmt.Sprintf("Uploaded to %s (%d files", st.Location, len(res.Files))
			if res.Verified {
				msg += ", read back and verified"
			}
			if err := r.uploadSucceeded(ctx, b, f, res.Files, res.Verified, msg+")", done); err != nil {
				return ctrl.Result{}, err
			}
			if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil // releases the identity Secret
		case backup.JobFailed:
			reason, failure = "UploadFailed", "Upload failed: "+backup.FailureMessage(pods)
			if failure == "Upload failed: " {
				failure += jobMsg
			}
		default:
			if stalled := backup.Stalled(job, pods, podStartTimeout); stalled != "" {
				// Its pod never ran, so it left nothing (what a failed earlier attempt may have
				// left has no manifest, and remote retention removes it).
				if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, r.uploadFailed(ctx, b, "JobStalled", "Upload job did not start: "+stalled)
			}
			msg := backup.PendingMessage(pods)
			if msg == "" {
				msg = fmt.Sprintf("Upload job %s is uploading to %s", job.Name, st.Location)
			}
			if err := r.setUploadMessage(ctx, b, msg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	}
	return r.settleUpload(ctx, b, f, job, pods, reason, failure)
}

// setUploadMessage updates status.remote.message.
func (r *ForgejoBackupReconciler) setUploadMessage(ctx context.Context, b *forgejov1.ForgejoBackup, msg string) error {
	if b.Status.Remote.Message == msg {
		return nil
	}
	orig := b.DeepCopy()
	b.Status.Remote.Message = msg
	return r.patchStatus(ctx, b, orig)
}

// settleUpload ends an upload whose Job failed, did not start or vanished. Once its pods have
// stopped, it looks at the backup's directory (with the connection recorded when the upload
// started): this upload's own complete manifest means the Job uploaded everything and lost only
// its result, so the backup is Uploaded; objects without a manifest are what the upload left and
// are removed; a manifest the operator cannot read or that another upload wrote is left as it
// is. Nothing else is ever deleted.
func (r *ForgejoBackupReconciler) settleUpload(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo,
	job *batchv1.Job, pods []corev1.Pod, reason, failure string) (ctrl.Result, error) {
	st := b.Status.Remote
	inspect := true
	if backup.UnfinishedPods(pods) > 0 {
		if !podStuck(pods) && (st.StartTime == nil || time.Since(st.StartTime.Time) < backup.RemoteIncompleteAge) {
			msg := failure + "; waiting for its pod to stop before checking what it left in " + st.Location
			if err := r.setUploadMessage(ctx, b, msg); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		inspect = false
	}
	note := fmt.Sprintf("; what it may have left in %s is left to remote retention, because a pod of the upload did not stop", st.Location)
	if inspect {
		m, n := r.inspectUpload(ctx, b)
		if m != nil {
			done := metav1.Now()
			msg := fmt.Sprintf("Uploaded to %s (%d files, read back and verified by the upload job; its own result was lost: %s)",
				st.Location, len(m.Files), failure)
			if err := r.uploadSucceeded(ctx, b, f, m.Files, true, msg, done); err != nil {
				return ctrl.Result{}, err
			}
			if job != nil {
				return ctrl.Result{RequeueAfter: time.Second}, backup.RemoveJob(ctx, r.Client, job)
			}
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		note = n
	}
	if err := r.uploadFailed(ctx, b, reason, failure+note); err != nil {
		return ctrl.Result{}, err
	}
	if job != nil {
		return ctrl.Result{}, backup.RemoveJob(ctx, r.Client, job)
	}
	return ctrl.Result{}, nil
}

// podStuck reports whether a pod has been terminating for longer than uploadPodStopWait.
func podStuck(pods []corev1.Pod) bool {
	for i := range pods {
		if t := pods[i].DeletionTimestamp; t != nil && !backup.PodFinished(&pods[i]) && time.Since(t.Time) > uploadPodStopWait {
			return true
		}
	}
	return false
}

// inspectUpload looks at the backup's remote directory after its upload Job stopped without
// success (see settleUpload). It returns the manifest when the directory holds this upload's
// complete backup, else a note for the failure message about what it found and did.
func (r *ForgejoBackupReconciler) inspectUpload(ctx context.Context, b *forgejov1.ForgejoBackup) (*backup.RemoteManifest, string) {
	st := b.Status.Remote
	if st.S3 == nil || st.KeySecret == nil || st.Path == "" {
		return nil, ""
	}
	cctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	store, err := backup.OpenStore(cctx, r.Client, b.Namespace, st.S3, *st.KeySecret)
	var objs []s3.Object
	if err == nil {
		objs, err = store.List(cctx, st.Path)
	}
	if err != nil {
		return nil, fmt.Sprintf("; what it may have left in %s was not checked (%v): remote retention removes an incomplete upload later",
			st.Location, err)
	}
	if len(objs) == 0 {
		return nil, ""
	}
	if store.HasManifest(st.Path, objs) {
		m, err := store.ReadManifest(cctx, st.Path)
		switch {
		case err != nil:
			return nil, fmt.Sprintf("; %s holds a manifest the operator cannot read (%v): it was left as it is", st.Location, err)
		case m.BackupUID != string(b.UID):
			return nil, fmt.Sprintf("; %s holds a backup this upload did not write (ForgejoBackup UID %s): it was left as it is", st.Location, m.BackupUID)
		}
		return m, ""
	}
	keys := make([]string, 0, len(objs))
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if err := store.Delete(cctx, keys); err != nil {
		logf.FromContext(ctx).Info("Could not remove the objects of the failed upload; remote retention removes them later",
			"location", st.Location, "error", err.Error())
		return nil, fmt.Sprintf("; removing the %d objects it left in %s failed (%v): remote retention removes them later", len(keys), st.Location, err)
	}
	return nil, fmt.Sprintf("; removed the %d objects it left in %s", len(keys), st.Location)
}

// uploadSucceeded records a completed upload.
func (r *ForgejoBackupReconciler) uploadSucceeded(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo,
	files []forgejov1.RemoteBackupFile, verified bool, msg string, done metav1.Time) error {
	orig := b.DeepCopy()
	st := b.Status.Remote
	st.Phase = forgejov1.RemoteBackupUploaded
	st.CompletionTime = &done
	st.Files = files
	st.Verified = verified
	st.Message = msg
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return err
	}
	ns, inst := b.Namespace, b.Spec.ForgejoRef.Name
	forgejo.BackupUploadTotal.WithLabelValues(ns, inst, string(backup.Reason(b)), "succeeded").Inc()
	backup.RecordLastUpload(ns, inst, done.Time)
	r.event(b, corev1.EventTypeNormal, "BackupUploaded", "Upload", "%s", st.Message)
	if f != nil {
		r.eventOn(f, corev1.EventTypeNormal, "BackupUploaded", "Upload", "ForgejoBackup %s: %s", b.Name, st.Message)
	}
	return nil
}

// uploadFailed records a failed upload. It deletes nothing: settleUpload has dealt with what
// a Job that ran left in the bucket.
func (r *ForgejoBackupReconciler) uploadFailed(ctx context.Context, b *forgejov1.ForgejoBackup, reason, msg string) error {
	orig := b.DeepCopy()
	now := metav1.Now()
	st := b.Status.Remote
	st.Phase = forgejov1.RemoteBackupFailed
	st.CompletionTime = &now
	st.Message = msg
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return err
	}
	ns, inst := b.Namespace, b.Spec.ForgejoRef.Name
	forgejo.BackupUploadTotal.WithLabelValues(ns, inst, string(backup.Reason(b)), "failed").Inc()
	r.event(b, corev1.EventTypeWarning, "UploadFailed", "Upload", "%s (%s)", msg, reason)
	if inst := r.instance(ctx, b); inst != nil {
		r.eventOn(inst, corev1.EventTypeWarning, "BackupUploadFailed", "Upload", "ForgejoBackup %s was not uploaded to %s: %s", b.Name, st.Location, msg)
	}
	return nil
}

// instance reads the backup's Forgejo, or returns nil.
func (r *ForgejoBackupReconciler) instance(ctx context.Context, b *forgejov1.ForgejoBackup) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}, f); err != nil {
		return nil
	}
	return f
}
