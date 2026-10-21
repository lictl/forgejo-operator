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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

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
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=core,resources=persistentvolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile runs a backup Job for each ForgejoBackup and removes the backup files when the
// object is deleted.
func (r *ForgejoBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
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
	if !backup.IsBackupActive(b) {
		return ctrl.Result{}, nil
	}

	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}, f); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		f = nil
	}
	if b.Status.Phase == forgejov1.BackupPhaseRunning {
		return r.track(ctx, b, f)
	}
	return r.start(ctx, b, f)
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
	for i := range restores {
		if backup.BlocksBackups(&restores[i]) {
			return r.wait(ctx, b, fmt.Sprintf("Waiting for ForgejoRestore %s to finish", restores[i].Name), 10*time.Second)
		}
	}

	if !backup.DatabaseReady(f) {
		return r.waitForDependency(ctx, b, "DatabaseNotReady", fmt.Sprintf("Database of Forgejo %q is not ready", f.Name))
	}
	db, err := forgejo.ResolveDB(ctx, r.Client, f)
	if err != nil {
		return r.waitForDependency(ctx, b, "DatabaseNotReady", err.Error())
	}
	pvc, err := backup.EnsureBackupPVC(ctx, r.Client, r.Scheme, f)
	if err != nil {
		return r.waitForDependency(ctx, b, "BackupStorageUnavailable", err.Error())
	}

	includeData := backup.IncludeData(b, f)
	var affinity *corev1.Affinity
	if includeData {
		if affinity, err = backup.Placement(ctx, r.Client, f); err != nil {
			return ctrl.Result{}, err
		}
	}
	image := backup.CurrentImage(f)
	now := metav1.Now()
	job, err := backup.BackupJob(backup.JobInput{
		Scheme:        r.Scheme,
		Instance:      f,
		InstanceName:  f.Name,
		ForgejoImage:  image,
		PostgresImage: forgejo.PostgresImage(f),
		DB:            db,
		BackupPVC:     pvc,
		Affinity:      affinity,
	}, b, includeData, now)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, fmt.Errorf("creating backup job: %w", err)
	}

	orig := b.DeepCopy()
	b.Status.Phase = forgejov1.BackupPhaseRunning
	b.Status.StartTime = &now
	b.Status.JobName = job.Name
	b.Status.PVCName = pvc
	b.Status.Path = "/" + b.Name
	b.Status.ForgejoImage = image
	b.Status.IncludesData = includeData
	b.Status.Message = fmt.Sprintf("Backup job %s is running", job.Name)
	setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Running", b.Status.Message)
	if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
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
		return ctrl.Result{}, r.fail(ctx, b, "JobMissing", fmt.Sprintf("Backup job %s disappeared", b.Status.JobName))
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
		return ctrl.Result{}, r.fail(ctx, b, "JobFailed", "Backup job failed: "+msg)
	case backup.JobRunning:
		if stalled := backup.Stalled(job, pods, podStartTimeout); stalled != "" {
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
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
			if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
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
	if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}

	ns, inst := b.Namespace, b.Spec.ForgejoRef.Name
	forgejo.BackupTotal.WithLabelValues(ns, inst, string(backup.Reason(b)), "succeeded").Inc()
	forgejo.BackupLastSuccess.WithLabelValues(ns, inst).Set(float64(done.Unix()))
	forgejo.BackupSize.WithLabelValues(ns, inst, "database").Set(float64(res.DatabaseSizeBytes))
	forgejo.BackupSize.WithLabelValues(ns, inst, "data").Set(float64(res.DataSizeBytes))
	if b.Status.StartTime != nil {
		forgejo.BackupDuration.WithLabelValues(ns, inst).Set(done.Sub(b.Status.StartTime.Time).Seconds())
	}
	r.event(b, corev1.EventTypeNormal, "BackupCompleted", "Backup", "%s", b.Status.Message)
	if f != nil {
		r.eventOn(f, corev1.EventTypeNormal, "BackupCompleted", "Backup", "ForgejoBackup %s completed", b.Name)
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
		if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
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
	if err := r.Status().Patch(ctx, b, client.MergeFrom(orig)); err != nil {
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
// up (with a warning event) when the PVC is gone or the cleanup cannot finish in time.
func (r *ForgejoBackupReconciler) finalize(ctx context.Context, b *forgejov1.ForgejoBackup) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(b, forgejo.FinalizerName) {
		return ctrl.Result{}, nil
	}
	done, err := r.cleanup(ctx, b)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, err
	}
	controllerutil.RemoveFinalizer(b, forgejo.FinalizerName)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, b))
}

func (r *ForgejoBackupReconciler) cleanup(ctx context.Context, b *forgejov1.ForgejoBackup) (bool, error) {
	if b.Status.PVCName == "" {
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

	// Stop a still-running backup first so it cannot write after the cleanup.
	if b.Status.JobName != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: b.Namespace, Name: b.Status.JobName}, job)
		if err == nil {
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
		return false, r.createCleanupJob(ctx, b)
	}
	if err != nil {
		return false, err
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
		// Keep every user of the RWO backup volume on the node that holds the data volume.
		if in.Affinity, err = backup.Placement(ctx, r.Client, f); err != nil {
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
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating cleanup job: %w", err)
	}
	return nil
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
		Named("forgejobackup").
		Complete(r)
}
