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

	appsv1 "k8s.io/api/apps/v1"
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
	// scaleDownTimeout fails a restore when Forgejo does not enter maintenance mode in time
	// (e.g. the Forgejo reconcile is blocked earlier in its pipeline).
	scaleDownTimeout = 15 * time.Minute
	// scaleUpTimeout bounds how long a finished restore waits for Forgejo to become available.
	scaleUpTimeout = 15 * time.Minute
)

// ForgejoRestoreReconciler reconciles a ForgejoRestore object
type ForgejoRestoreReconciler struct {
	client.Client
	// APIReader reads straight from the API server; a restore is only failed for a missing
	// Job after a live read. nil falls back to Client.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
}

// reader returns the live API reader (the cache in tests that do not set one).
func (r *ForgejoRestoreReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorestores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorestores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorestores/finalizers,verbs=update
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

// Reconcile drives a restore through Pending → ScalingDown → Restoring → ScalingUp →
// Completed. The Forgejo pipeline's maintenance step keeps the instance scaled to zero while
// the restore is in ScalingDown or Restoring, and after a failure that had already changed
// the database or the data volume (DataModified condition) until that is acknowledged.
func (r *ForgejoRestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	rs := &forgejov1.ForgejoRestore{}
	if err := r.Get(ctx, req.NamespacedName, rs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rs.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, rs)
	}
	if !backup.IsRestoreActive(rs) {
		return ctrl.Result{}, nil
	}
	// The finalizer is in place before any Job exists: deleting the restore must not end
	// maintenance while a restore pod may still be writing.
	changed := controllerutil.AddFinalizer(rs, forgejo.FinalizerName)
	if rs.Labels[forgejo.LabelForgejo] != rs.Spec.ForgejoRef.Name {
		if rs.Labels == nil {
			rs.Labels = map[string]string{}
		}
		rs.Labels[forgejo.LabelForgejo] = rs.Spec.ForgejoRef.Name
		changed = true
	}
	if changed {
		return ctrl.Result{}, r.Update(ctx, rs)
	}

	f := &forgejov1.Forgejo{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.ForgejoRef.Name}, f); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if rs.Status.Phase == forgejov1.RestorePhaseScalingUp {
			return ctrl.Result{}, r.complete(ctx, rs, nil, "Restore completed; the Forgejo instance no longer exists")
		}
		return ctrl.Result{}, r.fail(ctx, rs, nil, "InstanceNotFound", fmt.Sprintf("Forgejo %q not found", rs.Spec.ForgejoRef.Name))
	}

	switch rs.Status.Phase {
	case forgejov1.RestorePhaseScalingDown:
		return r.scaleDown(ctx, rs, f)
	case forgejov1.RestorePhaseRestoring:
		return r.track(ctx, rs, f)
	case forgejov1.RestorePhaseScalingUp:
		return r.scaleUp(ctx, rs, f)
	default:
		return r.validate(ctx, rs, f)
	}
}

// what resolves which parts are restored.
func what(rs *forgejov1.ForgejoRestore, b *forgejov1.ForgejoBackup) (db, data bool) {
	return forgejo.BoolOr(rs.Spec.RestoreDatabase, true),
		forgejo.BoolOr(rs.Spec.RestoreData, true) && b.Status.IncludesData
}

func (r *ForgejoRestoreReconciler) getBackup(ctx context.Context, rs *forgejov1.ForgejoRestore) (*forgejov1.ForgejoBackup, error) {
	b := &forgejov1.ForgejoBackup{}
	err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.BackupName}, b)
	return b, err
}

func (r *ForgejoRestoreReconciler) validate(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (ctrl.Result, error) {
	b, err := r.getBackup(ctx, rs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.fail(ctx, rs, f, "BackupNotFound", fmt.Sprintf("ForgejoBackup %q not found", rs.Spec.BackupName))
		}
		return ctrl.Result{}, err
	}
	switch {
	case b.Spec.ForgejoRef.Name != f.Name:
		return ctrl.Result{}, r.fail(ctx, rs, f, "BackupMismatch",
			fmt.Sprintf("ForgejoBackup %q belongs to Forgejo %q, not %q", b.Name, b.Spec.ForgejoRef.Name, f.Name))
	case b.Status.Phase != forgejov1.BackupPhaseCompleted:
		return ctrl.Result{}, r.fail(ctx, rs, f, "BackupNotCompleted",
			fmt.Sprintf("ForgejoBackup %q is %q, not Completed", b.Name, b.Status.Phase))
	case !b.DeletionTimestamp.IsZero():
		return ctrl.Result{}, r.fail(ctx, rs, f, "BackupDeleted", fmt.Sprintf("ForgejoBackup %q is being deleted", b.Name))
	}
	restoreDB, restoreData := what(rs, b)
	if !restoreDB && !restoreData {
		return ctrl.Result{}, r.fail(ctx, rs, f, "NothingToRestore",
			"Neither the database nor data are selected (or the backup has no data archive)")
	}

	restores, err := backup.ListRestores(ctx, r.Client, f.Namespace, f.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range restores {
		if o := &restores[i]; o.Name != rs.Name && backup.IsRestoreActive(o) && o.Status.Phase != "" && o.Status.Phase != forgejov1.RestorePhasePending {
			return r.wait(ctx, rs, fmt.Sprintf("Waiting for ForgejoRestore %s to finish", o.Name))
		}
	}

	// Refuse a Job name taken by something else before maintenance mode is turned on.
	existing := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: backup.RestoreJobName(rs)}, existing); err == nil {
		if res, done, err := r.jobConflict(ctx, rs, f, backup.CheckOwned(existing, rs)); done {
			return res, err
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	if from, ok := forgejo.ImageVersion(b.Status.ForgejoImage); ok {
		if to, ok := forgejo.ImageVersion(backup.CurrentImage(f)); ok && from.Compare(to) > 0 {
			r.eventOn(rs, corev1.EventTypeWarning, "NewerBackup", "Restore",
				"Backup was taken with %s but %s is deployed; Forgejo cannot start on a database migrated by a newer version",
				b.Status.ForgejoImage, backup.CurrentImage(f))
		}
	}

	orig := rs.DeepCopy()
	now := metav1.Now()
	rs.Status.Phase = forgejov1.RestorePhaseScalingDown
	rs.Status.StartTime = &now
	rs.Status.Message = "Waiting for Forgejo to scale down"
	setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionFalse, "ScalingDown", rs.Status.Message)
	if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	r.eventOn(rs, corev1.EventTypeNormal, "RestoreStarted", "Restore", "Restoring backup %s (database: %t, data: %t)", b.Name, restoreDB, restoreData)
	r.eventOn(f, corev1.EventTypeNormal, "RestoreStarted", "Restore", "ForgejoRestore %s is restoring backup %s", rs.Name, b.Name)
	return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
}

// scaleDown waits for maintenance mode to take effect, then starts the restore Job.
func (r *ForgejoRestoreReconciler) scaleDown(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (ctrl.Result, error) {
	waitDown := func(msg string) (ctrl.Result, error) {
		if rs.Status.StartTime != nil && time.Since(rs.Status.StartTime.Time) > scaleDownTimeout {
			return ctrl.Result{}, r.fail(ctx, rs, f, "ScaleDownTimeout", fmt.Sprintf("Timed out after %s: %s", scaleDownTimeout, msg))
		}
		return r.wait(ctx, rs, msg)
	}
	if f.Status.Backup == nil || f.Status.Backup.ActiveRestore != rs.Name {
		return waitDown("Waiting for Forgejo to enter maintenance mode")
	}
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err == nil {
		if dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 || dep.Status.Replicas != 0 {
			return waitDown("Waiting for Forgejo to scale down")
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	if n, err := backup.ServerPodsRunning(ctx, r.Client, f); err != nil {
		return ctrl.Result{}, err
	} else if n > 0 {
		return waitDown(fmt.Sprintf("Waiting for %d Forgejo pod(s) to terminate", n))
	}
	backups, err := backup.ListBackups(ctx, r.Client, f.Namespace, f.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range backups {
		if backups[i].Status.Phase == forgejov1.BackupPhaseRunning {
			return r.wait(ctx, rs, fmt.Sprintf("Waiting for ForgejoBackup %s to finish", backups[i].Name))
		}
	}
	// Two restores must never write to the instance at once, e.g. the pods of an earlier,
	// failed or deleted restore that are still shutting down.
	if busy, err := r.restoreActivity(ctx, rs, f); err != nil {
		return ctrl.Result{}, err
	} else if busy != "" {
		return waitDown("Waiting for " + busy + " to stop")
	}

	b, err := r.getBackup(ctx, rs)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.fail(ctx, rs, f, "BackupNotFound", fmt.Sprintf("ForgejoBackup %q not found", rs.Spec.BackupName))
		}
		return ctrl.Result{}, err
	}
	restoreDB, restoreData := what(rs, b)
	in := backup.JobInput{
		Scheme:        r.Scheme,
		Instance:      f,
		InstanceName:  f.Name,
		ForgejoImage:  backup.CurrentImage(f),
		PostgresImage: forgejo.PostgresImage(f),
	}
	if restoreDB {
		if in.DB, err = forgejo.ResolveDB(ctx, r.Client, f); err != nil {
			return ctrl.Result{}, r.fail(ctx, rs, f, "DatabaseNotReady", err.Error())
		}
	}
	// A database-only restore also mounts the data volume, to reset the admin sync marker.
	if in.Affinity, err = backup.Placement(ctx, r.Client, f, restoreData || restoreDB, b.Status.PVCName); err != nil {
		var conflict *backup.VolumeConflictError
		if errors.As(err, &conflict) {
			return ctrl.Result{}, r.fail(ctx, rs, f, "BackupVolumeConflict", "Cannot run the restore (nothing was changed): "+conflict.Error())
		}
		return ctrl.Result{}, err
	}
	job, err := backup.RestoreJob(in, rs, b, restoreDB, restoreData)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := backup.CreateJob(ctx, r.Client, rs, job); err != nil {
		if res, done, cerr := r.jobConflict(ctx, rs, f, err); done {
			return res, cerr
		}
		return ctrl.Result{}, fmt.Errorf("creating restore job: %w", err)
	}

	orig := rs.DeepCopy()
	rs.Status.Phase = forgejov1.RestorePhaseRestoring
	rs.Status.JobName = job.Name
	if rs.Status.JobStartTime == nil {
		now := metav1.Now()
		rs.Status.JobStartTime = &now
	}
	rs.Status.Message = fmt.Sprintf("Restore job %s is running", job.Name)
	setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionFalse, "Restoring", rs.Status.Message)
	if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	r.eventOn(rs, corev1.EventTypeNormal, "Restoring", "Restore", "Started job %s", job.Name)
	return ctrl.Result{RequeueAfter: time.Minute}, nil
}

// restoreActivity names a restore Job or pod of the instance other than rs's own that may
// still be running, or returns "".
func (r *ForgejoRestoreReconciler) restoreActivity(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (string, error) {
	sel := client.MatchingLabels{forgejo.LabelInstance: f.Name, forgejo.LabelComponent: forgejo.ComponentRestore}
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs, client.InNamespace(f.Namespace), sel); err != nil {
		return "", err
	}
	var ownJob types.UID
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if metav1.IsControlledBy(j, rs) {
			ownJob = j.UID
			continue
		}
		if state, _ := backup.StateOf(j); state == backup.JobRunning {
			owner := "no restore"
			if ref := metav1.GetControllerOf(j); ref != nil {
				owner = ref.Kind + " " + ref.Name
			}
			return fmt.Sprintf("restore Job %s (%s)", j.Name, owner), nil
		}
	}
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(f.Namespace), sel); err != nil {
		return "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		// Pods of rs's own Job (it may exist after a restart) are fine; a pod of an earlier
		// Job of the same name, whose Job is already gone, is not.
		if ref := metav1.GetControllerOf(p); ownJob != "" && ref != nil && ref.UID == ownJob {
			continue
		}
		if !backup.PodFinished(p) {
			return "restore pod " + p.Name, nil
		}
	}
	return "", nil
}

// jobConflict handles a *backup.JobConflictError from err: it waits while the Job is going
// away and fails the restore otherwise. done is false when err is no conflict.
func (r *ForgejoRestoreReconciler) jobConflict(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo, err error) (ctrl.Result, bool, error) {
	var conflict *backup.JobConflictError
	if !errors.As(err, &conflict) {
		return ctrl.Result{}, false, nil
	}
	if conflict.Transient {
		res, err := r.wait(ctx, rs, fmt.Sprintf("Waiting for the old Job %s to be deleted", conflict.Job))
		return res, true, err
	}
	return ctrl.Result{}, true, r.fail(ctx, rs, f, "JobNameConflict",
		fmt.Sprintf("Cannot run the restore: %v. Nothing was changed; delete that Job or use a different ForgejoRestore name", conflict))
}

func (r *ForgejoRestoreReconciler) track(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (ctrl.Result, error) {
	recorded := backup.RecordedModification(rs)
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Status.JobName}, job); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		since := rs.Status.JobStartTime
		if since == nil {
			since = rs.Status.StartTime
		}
		if since != nil && time.Since(since.Time) < jobAppearGrace {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		// The cache may still lag behind; only the API server can tell that the Job is gone.
		if err := r.reader().Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Status.JobName}, job); err != nil {
			if !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, r.failModified(ctx, rs, f, "JobMissing", fmt.Sprintf("Restore job %s disappeared", rs.Status.JobName), recorded, "")
		}
	}
	// Never read the state of a Job this restore does not control: a foreign Job that
	// succeeded would otherwise report a restore that never ran.
	if err := backup.CheckOwned(job, rs); err != nil {
		return ctrl.Result{}, r.failModified(ctx, rs, f, "JobNameConflict",
			fmt.Sprintf("Restore job %s was replaced: %v", rs.Status.JobName, err), recorded, "")
	}
	pods, err := backup.JobPods(ctx, r.Client, job)
	if err != nil {
		return ctrl.Result{}, err
	}
	observed, why := backup.RestoreModification(pods)
	switch state, jobMsg := backup.StateOf(job); state {
	case backup.JobFailed:
		msg := backup.FailureMessage(pods)
		if msg == "" {
			msg = jobMsg
		}
		// The pods have the final word unless one may still be running or has vanished;
		// what was already recorded as committed stays committed.
		mod := observed
		if !backup.PodsSettled(job, pods) || recorded == backup.Modified {
			mod = max(mod, recorded)
		}
		return ctrl.Result{}, r.failModified(ctx, rs, f, "JobFailed", "Restore job failed: "+msg, mod, why)
	case backup.JobRunning:
		mod := max(observed, recorded)
		if stalled := backup.Stalled(job, pods, podStartTimeout); stalled != "" {
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
			if mod == backup.Unmodified {
				return ctrl.Result{}, r.failModified(ctx, rs, f, "JobStalled", "Restore job did not start (nothing was changed): "+stalled, mod, "")
			}
			// An earlier pod got past verification, so the newest pod being stuck does not
			// mean nothing was changed.
			return ctrl.Result{}, r.failModified(ctx, rs, f, "JobStalled", "Restore job stalled after an earlier attempt had started restoring: "+stalled, mod, why)
		}
		msg := backup.PendingMessage(pods)
		if msg == "" {
			msg = fmt.Sprintf("Restore job %s is running", job.Name)
		}
		if msg != rs.Status.Message || mod > recorded {
			orig := rs.DeepCopy()
			rs.Status.Message = msg
			if mod > recorded {
				meta.SetStatusCondition(&rs.Status.Conditions, backup.ModificationCondition(mod, rs.Generation, why))
			}
			if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	orig := rs.DeepCopy()
	rs.Status.Phase = forgejov1.RestorePhaseScalingUp
	rs.Status.Message = "Backup restored; waiting for Forgejo to become available"
	setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionFalse, "ScalingUp", rs.Status.Message)
	meta.SetStatusCondition(&rs.Status.Conditions, backup.ModificationCondition(backup.Modified, rs.Generation, "The backup was restored"))
	if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	r.eventOn(rs, corev1.EventTypeNormal, "Restored", "Restore", "Backup %s restored", rs.Spec.BackupName)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// scaleUp completes the restore once the Deployment serves again (or immediately when the
// instance has no Deployment or wants zero replicas).
func (r *ForgejoRestoreReconciler) scaleUp(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (ctrl.Result, error) {
	if f.Status.Backup != nil && f.Status.Backup.ActiveRestore != "" && f.Status.Backup.ActiveRestore != rs.Name {
		return ctrl.Result{}, r.complete(ctx, rs, f, fmt.Sprintf("Backup restored; ForgejoRestore %s keeps Forgejo in maintenance", f.Status.Backup.ActiveRestore))
	}
	// spec.replicas does not stop an instance whose HPA owns the replica count; the HPA
	// keeps at least its minimum.
	desired := forgejo.DesiredReplicas(f)
	if forgejo.HorizontalAutoscalingEnabled(f) {
		desired = forgejo.HPAMinReplicas(f)
	}
	dep := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep)
	if apierrors.IsNotFound(err) || desired == 0 {
		return ctrl.Result{}, r.complete(ctx, rs, f, "Backup restored")
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if dep.Spec.Replicas != nil && *dep.Spec.Replicas > 0 &&
		dep.Status.ObservedGeneration >= dep.Generation && dep.Status.AvailableReplicas > 0 {
		return ctrl.Result{}, r.complete(ctx, rs, f, "Backup restored and Forgejo is available")
	}

	since := time.Now()
	if rs.Status.StartTime != nil {
		since = rs.Status.StartTime.Time
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Status.JobName}, job); err == nil &&
		metav1.IsControlledBy(job, rs) && job.Status.CompletionTime != nil {
		since = job.Status.CompletionTime.Time
	}
	if time.Since(since) > scaleUpTimeout {
		r.eventOn(rs, corev1.EventTypeWarning, "ScaleUpTimeout", "Restore", "Forgejo did not become available within %s after the restore", scaleUpTimeout)
		return ctrl.Result{}, r.complete(ctx, rs, f, fmt.Sprintf("Backup restored, but Forgejo was not available %s later", scaleUpTimeout))
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func (r *ForgejoRestoreReconciler) wait(ctx context.Context, rs *forgejov1.ForgejoRestore, msg string) (ctrl.Result, error) {
	if rs.Status.Message != msg || rs.Status.Phase == "" {
		orig := rs.DeepCopy()
		if rs.Status.Phase == "" {
			rs.Status.Phase = forgejov1.RestorePhasePending
		}
		rs.Status.Message = msg
		setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionFalse, string(rs.Status.Phase), msg)
		if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

func (r *ForgejoRestoreReconciler) complete(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo, msg string) error {
	orig := rs.DeepCopy()
	now := metav1.Now()
	rs.Status.Phase = forgejov1.RestorePhaseCompleted
	rs.Status.CompletionTime = &now
	rs.Status.Message = msg
	setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionTrue, "Completed", msg)
	if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
		return err
	}
	forgejo.RestoreTotal.WithLabelValues(rs.Namespace, rs.Spec.ForgejoRef.Name, "succeeded").Inc()
	r.eventOn(rs, corev1.EventTypeNormal, "RestoreCompleted", "Restore", "%s", msg)
	if f != nil {
		r.eventOn(f, corev1.EventTypeNormal, "RestoreCompleted", "Restore", "ForgejoRestore %s completed: %s", rs.Name, msg)
	}
	return nil
}

// failModified fails a restore whose Job had started and records in the DataModified
// condition how far it got. A restore that (maybe) changed the instance keeps it in
// maintenance until it is released.
func (r *ForgejoRestoreReconciler) failModified(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo,
	reason, msg string, mod backup.Modification, why string) error {
	switch {
	case mod != backup.Unmodified:
		meta.SetStatusCondition(&rs.Status.Conditions, backup.ModificationCondition(mod, rs.Generation, why))
		if why != "" {
			why = " (" + why + ")"
		}
		msg += fmt.Sprintf(". The database or the data volume may already have been changed%s, so Forgejo stays scaled down: "+
			"fix the cause and run a new ForgejoRestore, or set annotation %s=true on this ForgejoRestore (or delete it) to start Forgejo as it is",
			why, backup.AnnotationReleaseMaintenance)
	case meta.FindStatusCondition(rs.Status.Conditions, backup.ConditionDataModified) != nil:
		meta.SetStatusCondition(&rs.Status.Conditions, backup.ModificationCondition(mod, rs.Generation, "The restore job failed before it changed anything"))
	}
	return r.fail(ctx, rs, f, reason, msg)
}

func (r *ForgejoRestoreReconciler) fail(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo, reason, msg string) error {
	orig := rs.DeepCopy()
	now := metav1.Now()
	rs.Status.Phase = forgejov1.RestorePhaseFailed
	rs.Status.CompletionTime = &now
	rs.Status.Message = msg
	setCondition(&rs.Status.Conditions, rs.Generation, metav1.ConditionFalse, reason, msg)
	if err := r.Status().Patch(ctx, rs, client.MergeFrom(orig)); err != nil {
		return err
	}
	forgejo.RestoreTotal.WithLabelValues(rs.Namespace, rs.Spec.ForgejoRef.Name, "failed").Inc()
	r.eventOn(rs, corev1.EventTypeWarning, "RestoreFailed", "Restore", "%s", msg)
	if f != nil {
		r.eventOn(f, corev1.EventTypeWarning, "RestoreFailed", "Restore", "ForgejoRestore %s failed: %s", rs.Name, msg)
	}
	return nil
}

func (r *ForgejoRestoreReconciler) eventOn(obj runtime.Object, eventType, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(obj, nil, eventType, reason, action, note, args...)
	}
}

// finalize stops the restore Job and keeps the object (and with it maintenance mode, while
// it is ScalingDown or Restoring) until none of the Job's pods can still write: pods only go
// away once their containers were killed. The Forgejo instance is not needed for this.
func (r *ForgejoRestoreReconciler) finalize(ctx context.Context, rs *forgejov1.ForgejoRestore) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(rs, forgejo.FinalizerName) {
		return ctrl.Result{}, nil
	}
	names := []string{backup.RestoreJobName(rs)}
	if rs.Status.JobName != "" && rs.Status.JobName != names[0] {
		names = append(names, rs.Status.JobName)
	}
	running := 0
	for _, name := range names {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: name}, job)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return ctrl.Result{}, err
		case !metav1.IsControlledBy(job, rs):
			continue // not ours: neither stop it nor wait for it
		case job.DeletionTimestamp.IsZero():
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); client.IgnoreNotFound(err) != nil {
				return ctrl.Result{}, err
			}
		}
		pods, err := backup.JobPodsByName(ctx, r.Client, rs.Namespace, name)
		if err != nil {
			return ctrl.Result{}, err
		}
		for i := range pods {
			if ref := metav1.GetControllerOf(&pods[i]); ref != nil && ref.Kind == "Job" && !backup.PodFinished(&pods[i]) {
				running++
			}
		}
	}
	if running > 0 {
		logf.FromContext(ctx).Info("waiting for restore pods to terminate before releasing the ForgejoRestore", "pods", running)
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if rs.Status.Phase == forgejov1.RestorePhaseRestoring && backup.RecordedModification(rs) != backup.Unmodified {
		f := &forgejov1.Forgejo{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.ForgejoRef.Name}, f); err == nil {
			r.eventOn(f, corev1.EventTypeWarning, "RestoreAborted", "Restore",
				"ForgejoRestore %s was deleted while restoring; the database or the data volume may be partially restored", rs.Name)
		}
	}
	controllerutil.RemoveFinalizer(rs, forgejo.FinalizerName)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, rs))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForgejoRestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&forgejov1.ForgejoRestore{}).
		Owns(&batchv1.Job{}).
		// Container transitions inside a restore pod (verify done, database committed, data
		// step started) do not always change the Job; watch the pods to record DataModified
		// promptly.
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(r.mapPodToRestore),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
				return o.GetLabels()[forgejo.LabelComponent] == forgejo.ComponentRestore
			}))).
		Named("forgejorestore").
		Complete(r)
}

// mapPodToRestore maps a restore Job's pod to the ForgejoRestore controlling that Job.
func (r *ForgejoRestoreReconciler) mapPodToRestore(ctx context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[batchv1.JobNameLabel]
	if name == "" {
		return nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, job); err != nil {
		return nil
	}
	ref := metav1.GetControllerOf(job)
	if ref == nil || ref.Kind != "ForgejoRestore" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: job.Namespace, Name: ref.Name}}}
}
