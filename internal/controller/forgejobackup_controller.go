/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	// dependencyTimeout bounds how long a backup or restore waits for a database that is not
	// ready before it fails.
	dependencyTimeout = 2 * time.Minute
	// jobAppearGrace covers the cache lagging behind a Job that was just created.
	jobAppearGrace = time.Minute
	// cleanupTimeout stops a ForgejoBackup deletion from blocking forever on an unschedulable
	// or failing cleanup Job.
	cleanupTimeout = 10 * time.Minute
	// podStartTimeout fails a backup or restore whose Job cannot start a pod.
	podStartTimeout = 10 * time.Minute
	// conditionReady is set on ForgejoBackup and ForgejoRestore.
	conditionReady = "Ready"
)

// ForgejoBackupReconciler reconciles a ForgejoBackup object
type ForgejoBackupReconciler struct {
	client.Client
	// APIReader reads straight from the API server for decisions a stale cache must not make
	// (whether a Job really disappeared, whether the owning Forgejo is gone). nil falls back
	// to Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups/status,verbs=patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile runs a backup Job for each ForgejoBackup and removes the backup files when the
// object is deleted. Status is patched with optimistic locking: a pass that started from a
// stale copy (e.g. still Running after the backup was recorded and its Job deleted) must not
// overwrite what a newer pass recorded, so a conflict just retries.
func (r *ForgejoBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	res, err := r.reconcile(ctx, req)
	return settle(ctx, res, err)
}

// settle maps the outcome of a ForgejoBackup or ForgejoRestore pass to the controller's
// result. A conflict retries a second later. A create refused because the namespace is being
// deleted drops the request without an error or a requeue: every later create would fail the
// same way, and the namespace's deletion removes the object (see forgejo.IsNamespaceTerminating).
func settle(ctx context.Context, res ctrl.Result, err error) (ctrl.Result, error) {
	switch {
	case apierrors.IsConflict(err):
		return ctrl.Result{RequeueAfter: time.Second}, nil
	case forgejo.IsNamespaceTerminating(err):
		logf.FromContext(ctx).V(1).Info("Dropped reconcile because the namespace is terminating", "error", err.Error())
		return ctrl.Result{}, nil
	}
	return res, err
}

func (r *ForgejoBackupReconciler) reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	b := &forgejov1.ForgejoBackup{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !b.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, b)
	}
	if updated, err := r.ensureMetadata(ctx, b); err != nil || updated {
		return ctrl.Result{}, err
	}

	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}, f); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		f = nil
	}
	if f != nil && f.DeletionTimestamp.IsZero() {
		// retainPolicy may have changed since the backup was created; see backup.ReconcileOwnership.
		if err := r.syncOwnership(ctx, f, b); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return ctrl.Result{}, err
		}
	}

	if !backup.IsBackupActive(b) {
		if f != nil && f.DeletionTimestamp.IsZero() && b.Status.Phase == forgejov1.BackupPhaseCompleted && b.Status.CompletionTime != nil {
			// Re-exported after an operator restart even when the Forgejo pipeline is blocked.
			backup.RecordLastSuccess(b.Namespace, f.Name, b.Status.CompletionTime.Time)
		}
		if err := r.releaseJobs(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
		if b.Status.Phase == forgejov1.BackupPhaseCompleted {
			return r.reconcileUpload(ctx, b, f)
		}
		return r.removeFailedFiles(ctx, b)
	}
	if b.Status.Phase == forgejov1.BackupPhaseRunning {
		return r.track(ctx, b, f)
	}
	return r.start(ctx, b, f)
}

// releaseJobs deletes the finished Jobs of a finished backup: its backup Job, whose outcome
// status records (phase, message, sizes, times), and the cleanup Job of a failed backup once
// the FilesRemoved condition records its outcome. Their pods would otherwise keep the data
// and backup PVCs from being deleted with the instance (see backup.RemoveJob).
func (r *ForgejoBackupReconciler) releaseJobs(ctx context.Context, b *forgejov1.ForgejoBackup) error {
	if err := backup.DeleteFinishedJob(ctx, r.Client, b, b.Status.JobName); err != nil {
		return fmt.Errorf("deleting finished backup job: %w", err)
	}
	if meta.FindStatusCondition(b.Status.Conditions, backup.ConditionFilesRemoved) != nil {
		if err := backup.DeleteFinishedJob(ctx, r.Client, b, backup.CleanupJobName(b)); err != nil {
			return fmt.Errorf("deleting finished cleanup job: %w", err)
		}
	}
	return nil
}

// patchStatus writes b's status changes relative to orig, failing with a conflict when b is
// not the latest version (see Reconcile).
func (r *ForgejoBackupReconciler) patchStatus(ctx context.Context, b, orig *forgejov1.ForgejoBackup) error {
	return r.Status().Patch(ctx, b, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{}))
}

// syncOwnership applies the instance's current retain policy to the backup's owner
// references and to the backup PVC (see backup.ReconcileOwnership).
func (r *ForgejoBackupReconciler) syncOwnership(ctx context.Context, f *forgejov1.Forgejo, b *forgejov1.ForgejoBackup) error {
	if _, err := backup.SyncBackupOwnership(ctx, r.Client, r.Scheme, f, b); err != nil {
		return err
	}
	_, err := backup.SyncBackupPVCOwnership(ctx, r.Client, r.Scheme, f)
	return err
}

// reader returns the live API reader (the cache in tests that do not set one).
func (r *ForgejoBackupReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *ForgejoBackupReconciler) ensureMetadata(ctx context.Context, b *forgejov1.ForgejoBackup) (bool, error) {
	want := map[string]string{
		forgejo.LabelForgejo:      b.Spec.ForgejoRef.Name,
		forgejo.LabelBackupReason: string(backup.Reason(b)),
	}
	changed := controllerutil.AddFinalizer(b, forgejo.FinalizerName)
	for k, v := range want {
		if b.Labels[k] != v {
			if b.Labels == nil {
				b.Labels = map[string]string{}
			}
			b.Labels[k] = v
			changed = true
		}
	}
	if !changed {
		return false, nil
	}
	return true, r.Update(ctx, b)
}

func (r *ForgejoBackupReconciler) start(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) (ctrl.Result, error) {
	if f == nil {
		return ctrl.Result{}, r.fail(ctx, b, "InstanceNotFound", fmt.Sprintf("Forgejo %q not found", b.Spec.ForgejoRef.Name))
	}

	restores, err := backup.ListRestores(ctx, r.Client, f.Namespace, f.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	// The cold backup of a PostgreSQL major upgrade in flight does not wait for restores that
	// have not started (phase "" or Pending): they wait for the upgrade (see the restore
	// controller's begin), which waits for this backup.
	ownDatabaseUpgradeBackup := forgejo.DatabaseUpgradeInFlight(f) && forgejo.DatabaseUpgrade(f).BackupName == b.Name
	for i := range restores {
		rs := &restores[i]
		if ownDatabaseUpgradeBackup && (rs.Status.Phase == "" || rs.Status.Phase == forgejov1.RestorePhasePending) {
			continue
		}
		if backup.BlocksBackups(rs) {
			return r.wait(ctx, b, fmt.Sprintf("Waiting for ForgejoRestore %s to finish", rs.Name), 10*time.Second)
		}
	}

	if backup.Bootstrapping(f) {
		// Nothing to back up before spec.restoreFrom has restored the instance.
		return r.wait(ctx, b, fmt.Sprintf("Waiting for Forgejo %q to be restored from spec.restoreFrom", f.Name), 30*time.Second)
	}
	if u := forgejo.DatabaseUpgrade(f); forgejo.DatabaseUpgradeInFlight(f) && u.BackupName != b.Name {
		// Only the upgrade's own cold backup runs meanwhile; the database is stopped or being
		// moved to another major.
		return r.wait(ctx, b, fmt.Sprintf("Waiting for the PostgreSQL major upgrade of Forgejo %q to %s to finish (phase %s)",
			f.Name, u.ToImage, u.Phase), 30*time.Second)
	}
	if !backup.DatabaseReady(f) {
		return r.waitForDependency(ctx, b, "DatabaseNotReady", fmt.Sprintf("Database of Forgejo %q is not ready", f.Name))
	}
	db, err := forgejo.ResolveDB(ctx, r.Client, f)
	if err != nil {
		return r.waitForDependency(ctx, b, "DatabaseNotReady", err.Error())
	}
	warn := func(reason, msg string) { r.eventOn(f, corev1.EventTypeWarning, reason, "ResizeVolume", "%s", msg) }
	pvc, err := backup.EnsureBackupPVC(ctx, r.Client, r.Scheme, f, warn)
	if forgejo.IsNamespaceTerminating(err) {
		return ctrl.Result{}, err // not a storage problem to wait for (see settle)
	}
	if err != nil {
		return r.waitForDependency(ctx, b, "BackupStorageUnavailable", err.Error())
	}

	includeData := backup.IncludeData(b, f)
	// Also a database-only Job: the first one decides where a node-local backup volume lands.
	affinity, err := backup.Placement(ctx, r.Client, f, includeData, pvc)
	if err != nil {
		var conflict *backup.VolumeConflictError
		if errors.As(err, &conflict) {
			return ctrl.Result{}, r.fail(ctx, b, "BackupVolumeConflict", "Cannot run the backup: "+conflict.Error())
		}
		return ctrl.Result{}, err
	}
	image := backup.CurrentImage(f)
	// pg_dump of the running server's major (the old one during a PostgreSQL major upgrade), so
	// that the dump can be restored into a server of that major.
	pgImage := forgejo.RunningPostgresImage(f)
	now := metav1.Now()
	job, err := backup.BackupJob(backup.JobInput{
		Scheme:        r.Scheme,
		Instance:      f,
		InstanceName:  f.Name,
		ForgejoImage:  image,
		PostgresImage: pgImage,
		DB:            db,
		BackupPVC:     pvc,
		Affinity:      affinity,
	}, b, includeData, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := backup.CreateJob(ctx, r.Client, b, job); err != nil {
		var conflict *backup.JobConflictError
		if !errors.As(err, &conflict) {
			return ctrl.Result{}, fmt.Errorf("creating backup job: %w", err)
		}
		if conflict.Transient {
			return r.wait(ctx, b, fmt.Sprintf("Waiting for the old Job %s to be deleted", conflict.Job), 5*time.Second)
		}
		return ctrl.Result{}, r.fail(ctx, b, "JobNameConflict",
			fmt.Sprintf("Cannot run the backup: %v; delete that Job or use a different ForgejoBackup name", conflict))
	}

	orig := b.DeepCopy()
	b.Status.Phase = forgejov1.BackupPhaseRunning
	b.Status.StartTime = &now
	b.Status.JobName = job.Name
	b.Status.PVCName = pvc
	b.Status.Path = "/" + b.Name
	b.Status.ForgejoImage = image
	b.Status.PostgresImage = pgImage
	b.Status.IncludesData = includeData
	b.Status.Message = fmt.Sprintf("Backup job %s is running", job.Name)
	setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Running", b.Status.Message)
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return ctrl.Result{}, err
	}
	r.event(b, corev1.EventTypeNormal, "BackupStarted", "Backup", "Started job %s writing to %s:%s", job.Name, pvc, b.Status.Path)
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

func (r *ForgejoBackupReconciler) track(ctx context.Context, b *forgejov1.ForgejoBackup, f *forgejov1.Forgejo) (ctrl.Result, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.JobName}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if b.Status.StartTime != nil && time.Since(b.Status.StartTime.Time) < jobAppearGrace {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// The cache may still lag behind; only the API server can tell that the Job is gone.
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.JobName}, job); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.fail(ctx, b, "JobMissing", fmt.Sprintf("Backup job %s disappeared", b.Status.JobName))
		}
	}
	// Never read the state of a Job this backup does not control: a foreign Job that
	// succeeded would otherwise report a backup that wrote nothing.
	if err := backup.CheckOwned(job, b); err != nil {
		return ctrl.Result{}, r.fail(ctx, b, "JobNameConflict", fmt.Sprintf("Backup job %s was replaced: %v", b.Status.JobName, err))
	}
	pods, err := backup.JobPods(ctx, r.Client, job)
	if err != nil {
		return ctrl.Result{}, err
	}

	state, jobMsg := backup.StateOf(job)
	switch state {
	case backup.JobFailed:
		msg := backup.FailureMessage(pods)
		if msg == "" {
			msg = jobMsg
		}
		if err := r.fail(ctx, b, "JobFailed", "Backup job failed: "+msg); err != nil {
			return ctrl.Result{}, err
		}
		// The message above keeps the failing container's output; the Job is not needed any
		// more. removeFailedFiles waits for its pods to stop before it removes the files.
		return ctrl.Result{}, backup.RemoveJob(ctx, r.Client, job)
	case backup.JobRunning:
		if stalled := backup.Stalled(job, pods, podStartTimeout); stalled != "" {
			if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.fail(ctx, b, "JobStalled", "Backup job did not start: "+stalled)
		}
		msg := backup.PendingMessage(pods)
		if msg == "" {
			msg = fmt.Sprintf("Backup job %s is running", job.Name)
		}
		if msg != b.Status.Message {
			orig := b.DeepCopy()
			b.Status.Message = msg
			if err := r.patchStatus(ctx, b, orig); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	res, err := backup.ParseBackupResult(pods)
	if err != nil {
		logf.FromContext(ctx).Error(err, "backup succeeded but its sizes are unknown", "job", job.Name)
	}
	done := metav1.Now()
	if job.Status.CompletionTime != nil {
		done = *job.Status.CompletionTime
	}
	orig := b.DeepCopy()
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.CompletionTime = &done
	b.Status.DatabaseSizeBytes = res.DatabaseSizeBytes
	b.Status.DataSizeBytes = res.DataSizeBytes
	b.Status.Message = fmt.Sprintf("Backup completed (database %d bytes, data %d bytes)", res.DatabaseSizeBytes, res.DataSizeBytes)
	if res.Warnings > 0 {
		b.Status.Message += fmt.Sprintf("; %d file(s) changed or vanished while archiving", res.Warnings)
	}
	setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionTrue, "Completed", b.Status.Message)
	if backup.RemoteEnabled(f) && f.DeletionTimestamp.IsZero() {
		// The upload starts on the next pass (reconcileUpload); the backup is complete on the
		// backup PVC now, so restores and upgrades need not wait for it.
		path := backup.BackupRemotePath(b)
		b.Status.Remote = &forgejov1.RemoteBackupStatus{Phase: forgejov1.RemoteBackupPending, Path: path,
			Location: backup.RemoteLocation(&f.Spec.Backup.Remote.S3, path), Message: "Waiting to be uploaded"}
	}
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return ctrl.Result{}, err
	}

	ns, inst := b.Namespace, b.Spec.ForgejoRef.Name
	forgejo.BackupTotal.WithLabelValues(ns, inst, string(backup.Reason(b)), "succeeded").Inc()
	backup.RecordLastSuccess(ns, inst, done.Time)
	forgejo.BackupSize.WithLabelValues(ns, inst, "database").Set(float64(res.DatabaseSizeBytes))
	forgejo.BackupSize.WithLabelValues(ns, inst, "data").Set(float64(res.DataSizeBytes))
	if b.Status.StartTime != nil {
		forgejo.BackupDuration.WithLabelValues(ns, inst).Set(done.Sub(b.Status.StartTime.Time).Seconds())
	}
	r.event(b, corev1.EventTypeNormal, "BackupCompleted", "Backup", "%s", b.Status.Message)
	if f != nil {
		r.eventOn(f, corev1.EventTypeNormal, "BackupCompleted", "Backup", "ForgejoBackup %s completed", b.Name)
	}
	// Status now holds everything the Job reported; its pod would only keep the data and
	// backup PVCs from being deleted.
	if err := backup.RemoveJob(ctx, r.Client, job); err != nil {
		return ctrl.Result{}, err
	}
	if b.Status.Remote != nil {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// waitForDependency keeps the backup Pending until dependencyTimeout, then fails it.
func (r *ForgejoBackupReconciler) waitForDependency(ctx context.Context, b *forgejov1.ForgejoBackup, reason, msg string) (ctrl.Result, error) {
	if time.Since(b.CreationTimestamp.Time) > dependencyTimeout {
		return ctrl.Result{}, r.fail(ctx, b, reason, msg)
	}
	return r.wait(ctx, b, msg, 10*time.Second)
}

func (r *ForgejoBackupReconciler) wait(ctx context.Context, b *forgejov1.ForgejoBackup, msg string, after time.Duration) (ctrl.Result, error) {
	if b.Status.Phase != forgejov1.BackupPhasePending || b.Status.Message != msg {
		orig := b.DeepCopy()
		b.Status.Phase = forgejov1.BackupPhasePending
		b.Status.Message = msg
		setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Pending", msg)
		if err := r.patchStatus(ctx, b, orig); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *ForgejoBackupReconciler) fail(ctx context.Context, b *forgejov1.ForgejoBackup, reason, msg string) error {
	orig := b.DeepCopy()
	now := metav1.Now()
	b.Status.Phase = forgejov1.BackupPhaseFailed
	b.Status.CompletionTime = &now
	b.Status.Message = msg
	setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, reason, msg)
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return err
	}
	ns, inst := b.Namespace, b.Spec.ForgejoRef.Name
	forgejo.BackupTotal.WithLabelValues(ns, inst, string(backup.Reason(b)), "failed").Inc()
	if b.Status.StartTime != nil {
		forgejo.BackupDuration.WithLabelValues(ns, inst).Set(now.Sub(b.Status.StartTime.Time).Seconds())
	}
	r.event(b, corev1.EventTypeWarning, "BackupFailed", "Backup", "%s", msg)
	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: inst}, f); err == nil {
		r.eventOn(f, corev1.EventTypeWarning, "BackupFailed", "Backup", "ForgejoBackup %s failed: %s", b.Name, msg)
	}
	return nil
}

// finalize deletes the backup files with a cleanup Job before releasing the object. It gives
// up (with a warning event) when the PVC is gone or the cleanup cannot finish in time, and
// right away when the namespace is being deleted (with the backup volume in it).
func (r *ForgejoBackupReconciler) finalize(ctx context.Context, b *forgejov1.ForgejoBackup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, forgejo.FinalizerName) {
		return ctrl.Result{}, nil
	}
	done, err := r.cleanup(ctx, b)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	controllerutil.RemoveFinalizer(b, forgejo.FinalizerName)
	if err := r.Update(ctx, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// Garbage collection removes the cleanup Job with the object; deleting a finished one
	// right away releases the backup PVC sooner (it may be waiting to be deleted with the
	// instance). Nothing depends on this, so a failure is only logged.
	if err := backup.DeleteFinishedJob(ctx, r.Client, b, backup.CleanupJobName(b)); err != nil {
		logf.FromContext(ctx).Info("could not delete the finished cleanup job; garbage collection will", "error", err.Error())
	}
	return ctrl.Result{}, nil
}

func (r *ForgejoBackupReconciler) cleanup(ctx context.Context, b *forgejov1.ForgejoBackup) (bool, error) {
	// FilesRemoved: the files of this failed backup were already removed.
	if b.Status.PVCName == "" || backup.FilesRemoved(b) {
		return true, nil
	}
	if name, err := backup.RestoreUsing(ctx, r.Client, b); err != nil || name != "" {
		return false, err
	}
	if time.Since(b.DeletionTimestamp.Time) > cleanupTimeout {
		r.event(b, corev1.EventTypeWarning, "CleanupTimedOut", "Delete",
			"Gave up removing %s:%s after %s; delete the directory manually", b.Status.PVCName, b.Status.Path, cleanupTimeout)
		return true, nil
	}

	// Stop a still-running backup first so it cannot write after the cleanup, and an upload,
	// which would read files that are going away.
	for _, name := range []string{b.Status.JobName, uploadJobName(b)} {
		if name == "" {
			continue
		}
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: name}, job)
		if err == nil && metav1.IsControlledBy(job, b) {
			if state, _ := backup.StateOf(job); state == backup.JobRunning {
				if job.DeletionTimestamp.IsZero() {
					if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); client.IgnoreNotFound(err) != nil {
						return false, err
					}
				}
				return false, nil
			}
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.PVCName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return true, nil
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: backup.CleanupJobName(b)}, job)
	if apierrors.IsNotFound(err) {
		if kept, err := r.instanceGoneButRetained(ctx, b, pvc); err != nil {
			return false, err
		} else if kept {
			r.event(b, corev1.EventTypeNormal, "BackupFilesKept", "Delete",
				"Forgejo %s is gone and its backup volume is retained; kept %s:%s", b.Spec.ForgejoRef.Name, b.Status.PVCName, b.Status.Path)
			return true, nil
		}
		err = r.createCleanupJob(ctx, b)
		if forgejo.IsNamespaceTerminating(err) {
			// No Job can be created in a namespace that is being deleted, and the backup
			// volume is in that namespace too, so it goes with it (the files remain only when
			// its PersistentVolume is retained). Waiting for cleanupTimeout would only hold up
			// the namespace deletion; no event either, the API server would reject it.
			logf.FromContext(ctx).Info("Released ForgejoBackup without removing its files because the namespace is terminating",
				"pvc", b.Status.PVCName, "path", b.Status.Path)
			return true, nil
		}
	} else if err == nil {
		err = backup.CheckOwned(job, b)
	}
	if err != nil {
		var conflict *backup.JobConflictError
		if !errors.As(err, &conflict) {
			return false, err
		}
		if conflict.Transient {
			return false, nil
		}
		r.event(b, corev1.EventTypeWarning, "CleanupFailed", "Delete", "Could not remove %s:%s: %v", b.Status.PVCName, b.Status.Path, conflict)
		return true, nil
	}
	if job.Name == "" {
		return false, nil // just created
	}
	switch state, msg := backup.StateOf(job); state {
	case backup.JobSucceeded:
		r.event(b, corev1.EventTypeNormal, "BackupDeleted", "Delete", "Removed %s:%s", b.Status.PVCName, b.Status.Path)
		return true, nil
	case backup.JobFailed:
		r.event(b, corev1.EventTypeWarning, "CleanupFailed", "Delete", "Could not remove %s:%s: %s", b.Status.PVCName, b.Status.Path, msg)
		return true, nil
	}
	return false, nil
}

// instanceGoneButRetained reports whether the backup is being deleted only because the Forgejo
// that owned it is gone (garbage collection) while the backup volume outlives it: the
// instance's retainPolicy was switched to Retain after the backup was created and nothing has
// removed the owner reference yet. The files then stay. An explicit delete (the owner is
// alive, or the backup has no owner reference) and a consistent Delete policy (the backup
// volume is garbage-collected anyway) still remove them. The owner is read live: a stale
// cache must not decide that backups are kept or destroyed.
func (r *ForgejoBackupReconciler) instanceGoneButRetained(ctx context.Context, b *forgejov1.ForgejoBackup, pvc *corev1.PersistentVolumeClaim) (bool, error) {
	ref := backup.ForgejoOwner(b)
	if ref == nil {
		return false, nil
	}
	f := &forgejov1.Forgejo{}
	err := r.reader().Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: ref.Name}, f)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, err
	case f.UID != ref.UID: // replaced by a new instance of the same name
	case f.DeletionTimestamp.IsZero():
		return false, nil
	case !forgejo.RetainData(f):
		return false, nil
	}
	return !backup.ControlledByUID(pvc, ref.UID), nil
}

func (r *ForgejoBackupReconciler) createCleanupJob(ctx context.Context, b *forgejov1.ForgejoBackup) error {
	in := backup.JobInput{
		Scheme:       r.Scheme,
		InstanceName: b.Spec.ForgejoRef.Name,
		ForgejoImage: b.Status.ForgejoImage,
		BackupPVC:    b.Status.PVCName,
	}
	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}, f); err == nil {
		in.Instance = f
		in.ForgejoImage = backup.CurrentImage(f)
		// A bound backup volume constrains the pod by itself; before its first bind, keep it
		// next to the data volume.
		if in.Affinity, err = backup.Placement(ctx, r.Client, f, false, b.Status.PVCName); err != nil {
			return err
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	if in.ForgejoImage == "" {
		in.ForgejoImage = forgejov1.DefaultImage
	}
	job, err := backup.CleanupJob(in, b)
	if err != nil {
		return err
	}
	if err := backup.CreateJob(ctx, r.Client, b, job); err != nil {
		var conflict *backup.JobConflictError
		if errors.As(err, &conflict) {
			return err
		}
		return fmt.Errorf("creating cleanup job: %w", err)
	}
	return nil
}

// removeFailedFiles deletes the directory of a failed backup with the cleanup Job, once
// nothing of the backup Job can still write: whatever a failed run left behind (a partial
// archive on a full volume, for example) cannot be restored and would only take space from
// later backups. The object stays so its status remains visible. The FilesRemoved condition
// records how the cleanup went, after which its Job is deleted (see trackFileRemoval);
// deleting the backup later finishes right away when its files are known to be gone.
func (r *ForgejoBackupReconciler) removeFailedFiles(ctx context.Context, b *forgejov1.ForgejoBackup) (ctrl.Result, error) {
	if b.Status.Phase != forgejov1.BackupPhaseFailed || b.Status.PVCName == "" ||
		meta.FindStatusCondition(b.Status.Conditions, backup.ConditionFilesRemoved) != nil {
		return ctrl.Result{}, nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: backup.CleanupJobName(b)}, job); err == nil {
		var conflict *backup.JobConflictError
		if errors.As(backup.CheckOwned(job, b), &conflict) {
			if conflict.Transient {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil // wait for the old Job to go away
			}
			return ctrl.Result{}, nil // someone else's Job (handled on deletion)
		}
		return r.trackFileRemoval(ctx, b, job)
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if b.Status.JobName != "" {
		bj := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.JobName}, bj)
		if err == nil && metav1.IsControlledBy(bj, b) {
			if state, _ := backup.StateOf(bj); state == backup.JobRunning {
				return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		pods, err := backup.JobPodsByName(ctx, r.Client, b.Namespace, b.Status.JobName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if backup.UnfinishedPods(pods) > 0 {
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.PVCName}, pvc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := r.createCleanupJob(ctx, b); err != nil {
		var conflict *backup.JobConflictError
		if errors.As(err, &conflict) {
			if conflict.Transient {
				return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
			}
			logf.FromContext(ctx).Info("not removing the files of the failed backup", "reason", conflict.Error())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	r.event(b, corev1.EventTypeNormal, "RemovingFiles", "Cleanup", "Removing what the failed backup left in %s:%s", b.Status.PVCName, b.Status.Path)
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// trackFileRemoval follows the cleanup Job of a failed backup. Once it has finished (or can
// no longer matter) the outcome goes into the FilesRemoved condition and the Job is deleted,
// so that its pod does not keep the backup PVC from being deleted.
func (r *ForgejoBackupReconciler) trackFileRemoval(ctx context.Context, b *forgejov1.ForgejoBackup, job *batchv1.Job) (ctrl.Result, error) {
	where := b.Status.PVCName + ":" + b.Status.Path
	cond := metav1.Condition{Type: backup.ConditionFilesRemoved, ObservedGeneration: b.Generation,
		Status: metav1.ConditionTrue, Reason: "Removed", Message: "Removed " + where}
	state, jobMsg := backup.StateOf(job)
	switch state {
	case backup.JobFailed:
		pods, err := backup.JobPods(ctx, r.Client, job)
		if err != nil {
			return ctrl.Result{}, err
		}
		if msg := backup.FailureMessage(pods); msg != "" {
			jobMsg = msg
		}
		cond.Status, cond.Reason = metav1.ConditionFalse, "CleanupFailed"
		cond.Message = fmt.Sprintf("Could not remove %s: %s; deleting the ForgejoBackup tries again", where, jobMsg)
	case backup.JobRunning:
		pvc := &corev1.PersistentVolumeClaim{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.PVCName}, pvc)
		switch {
		case err != nil && !apierrors.IsNotFound(err):
			return ctrl.Result{}, err
		case err != nil || !pvc.DeletionTimestamp.IsZero():
			// The volume, and with it the files, is going away; a pod that waits for it would
			// only keep it from being deleted.
			cond.Reason, cond.Message = "VolumeDeleted", fmt.Sprintf("Backup volume %s is deleted", b.Status.PVCName)
		default:
			pods, err := backup.JobPods(ctx, r.Client, job)
			if err != nil {
				return ctrl.Result{}, err
			}
			stalled := backup.Stalled(job, pods, podStartTimeout)
			if stalled == "" {
				return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
			}
			cond.Status, cond.Reason = metav1.ConditionFalse, "CleanupStalled"
			cond.Message = fmt.Sprintf("Could not remove %s: %s; deleting the ForgejoBackup tries again", where, stalled)
		}
	}
	orig := b.DeepCopy()
	meta.SetStatusCondition(&b.Status.Conditions, cond)
	if err := r.patchStatus(ctx, b, orig); err != nil {
		return ctrl.Result{}, err
	}
	if cond.Status == metav1.ConditionTrue {
		r.event(b, corev1.EventTypeNormal, "FilesRemoved", "Cleanup", "%s", cond.Message)
	} else {
		r.event(b, corev1.EventTypeWarning, cond.Reason, "Cleanup", "%s", cond.Message)
	}
	return ctrl.Result{}, backup.RemoveJob(ctx, r.Client, job)
}

func (r *ForgejoBackupReconciler) event(b *forgejov1.ForgejoBackup, eventType, reason, action, note string, args ...any) {
	r.eventOn(b, eventType, reason, action, note, args...)
}

func (r *ForgejoBackupReconciler) eventOn(obj runtime.Object, eventType, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, action, note, args...)
	}
}

func setCondition(conds *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               conditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForgejoBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&forgejov1.ForgejoBackup{}).
		Owns(&batchv1.Job{}).
		// A spec change (retainPolicy) re-applies ownership to every backup of the instance,
		// independently of how far the Forgejo pipeline gets.
		Watches(&forgejov1.Forgejo{}, handler.EnqueueRequestsFromMapFunc(r.mapForgejoToBackups),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("forgejobackup").
		Complete(r)
}

// mapForgejoToBackups enqueues every backup of a Forgejo.
func (r *ForgejoBackupReconciler) mapForgejoToBackups(ctx context.Context, obj client.Object) []reconcile.Request {
	backups, err := backup.ListBackups(ctx, r.Client, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(backups))
	for i := range backups {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&backups[i])})
	}
	return reqs
}
