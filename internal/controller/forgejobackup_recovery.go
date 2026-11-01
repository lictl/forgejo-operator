package controller

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
)

// recoverStartedJobs runs before start-only checks and deletion. Full execution metadata
// is journaled in status before Job creation. A Pending execution with a JobName may already
// have written data, including when its Job disappeared. Resume normal tracking with those
// original coordinates; never create a replacement or infer them from the current instance.
func (r *ForgejoBackupReconciler) recoverStartedJobs(ctx context.Context, b *forgejov1.ForgejoBackup) (bool, error) {
	if (b.Status.Phase == "" || b.Status.Phase == forgejov1.BackupPhasePending) && b.Status.JobName != "" {
		if err := r.checkPreparedJob(ctx, b, b.Status.JobName); err != nil {
			return r.recoverJobConflict(ctx, b, err, false)
		}
		orig := b.DeepCopy()
		b.Status.Phase = forgejov1.BackupPhaseRunning
		b.Status.Message = fmt.Sprintf("Tracking prepared backup job %s", b.Status.JobName)
		setCondition(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Running", b.Status.Message)
		return true, r.patchStatus(ctx, b, orig)
	}
	if remote := b.Status.Remote; remote != nil && (remote.Phase == "" || remote.Phase == forgejov1.RemoteBackupPending) && remote.JobName != "" {
		if err := r.checkPreparedJob(ctx, b, remote.JobName); err != nil {
			return r.recoverJobConflict(ctx, b, err, true)
		}
		orig := b.DeepCopy()
		remote.Phase = forgejov1.RemoteBackupUploading
		remote.Message = fmt.Sprintf("Tracking prepared upload job %s", remote.JobName)
		return true, r.patchStatus(ctx, b, orig)
	}
	return false, nil
}

// A collision found during recovery does not establish whether our Job previously ran.
// Preserve its journal, and retry transient conflicts just as CreateJob does.
func (r *ForgejoBackupReconciler) recoverJobConflict(ctx context.Context, b *forgejov1.ForgejoBackup, err error, upload bool) (bool, error) {
	var conflict *backup.JobConflictError
	if !errors.As(err, &conflict) {
		return false, err
	}
	if conflict.Transient {
		return true, nil
	}
	if upload {
		return true, r.uploadFailed(ctx, b, "JobNameConflict", "Not uploaded: "+conflict.Error())
	}
	return true, r.fail(ctx, b, "JobNameConflict", "Cannot run the backup: "+conflict.Error())
}

func (r *ForgejoBackupReconciler) checkPreparedJob(ctx context.Context, b *forgejov1.ForgejoBackup, name string) error {
	job := &batchv1.Job{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, job); err != nil {
		return client.IgnoreNotFound(err)
	}
	return backup.CheckOwned(job, b)
}

// jobStopped confirms through live reads that neither the Job nor any orphaned pod can
// still use the backup files or identity Secret. Deletion requests stop only an owned Job;
// file cleanup always waits for its pods, including pods hidden by a lagging cache.
func (r *ForgejoBackupReconciler) jobStopped(ctx context.Context, b *forgejov1.ForgejoBackup, name string, stop bool) (bool, error) {
	if name == "" {
		return true, nil
	}
	job := &batchv1.Job{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: name}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, err
	}
	if err == nil {
		if !metav1.IsControlledBy(job, b) {
			return false, fmt.Errorf("job %s belongs to another object; leaving backup files and identity untouched", name)
		}
		if state, _ := backup.StateOf(job); state == backup.JobRunning {
			if stop && job.DeletionTimestamp.IsZero() {
				err = r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground), client.Preconditions{UID: &job.UID})
				return false, client.IgnoreNotFound(err)
			}
			return false, nil
		}
	}
	pods, err := backup.JobPodsByName(ctx, r.reader(), b.Namespace, name)
	return err == nil && backup.UnfinishedPods(pods) == 0, err
}
