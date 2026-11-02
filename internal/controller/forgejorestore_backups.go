package controller

import (
	"context"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// backupActivity checks live execution before a restore may replace the database or data.
// Terminal backup status does not prove its pods stopped: a missing Job can fail a backup
// while its orphaned pods still read the database or archive the data volume.
func (r *ForgejoRestoreReconciler) backupActivity(ctx context.Context, rs *forgejov1.ForgejoRestore, f *forgejov1.Forgejo) (string, error) {
	backups, err := backup.ListBackups(ctx, r.reader(), f.Namespace, f.Name)
	if err != nil {
		return "", err
	}
	ownName := backup.PreRestoreBackupName(rs)
	ownActive := false
	for i := range backups {
		b := &backups[i]
		if b.Name == ownName && backup.IsBackupActive(b) {
			// preRestoreBackup owns this wait and reports its progress. Once it reaches
			// a terminal phase, it gets the same physical quiescence check as any dump.
			ownActive = true
			continue
		}
		if backup.BackupMayBeWriting(b) {
			return "ForgejoBackup " + b.Name, nil
		}
		if b.Status.JobName != "" {
			if busy, err := r.backupJobActivity(ctx, b.Namespace, b.Status.JobName); err != nil || busy != "" {
				return busy, err
			}
		}
	}
	// A deleted ForgejoBackup can leave a Job or pod too. Inspect the instance's dump
	// workloads independently of CR status; upload and cleanup Jobs do not read live data.
	ownJob := backup.BackupJobName(&forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: ownName}})
	sel := client.MatchingLabels(forgejo.Labels(f, forgejo.ComponentBackup))
	jobs := &batchv1.JobList{}
	if err := r.reader().List(ctx, jobs, client.InNamespace(f.Namespace), sel); err != nil {
		return "", err
	}
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if ownActive && j.Name == ownJob {
			continue
		}
		if state, _ := backup.StateOf(j); state == backup.JobRunning && dumpPod(&j.Spec.Template.Spec) {
			return "backup Job " + j.Name, nil
		}
	}
	pods := &corev1.PodList{}
	if err := r.reader().List(ctx, pods, client.InNamespace(f.Namespace), sel); err != nil {
		return "", err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if ownActive && p.Labels[batchv1.JobNameLabel] == ownJob {
			continue
		}
		if dumpPod(&p.Spec) && !backup.PodFinished(p) {
			return "backup pod " + p.Name, nil
		}
	}
	return "", nil
}

func (r *ForgejoRestoreReconciler) backupJobActivity(ctx context.Context, namespace, name string) (string, error) {
	job := &batchv1.Job{}
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, job)
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	if err == nil {
		if state, _ := backup.StateOf(job); state == backup.JobRunning {
			return "backup Job " + name, nil
		}
	}
	pods, err := backup.JobPodsByName(ctx, r.reader(), namespace, name)
	if err != nil {
		return "", err
	}
	for i := range pods {
		if !backup.PodFinished(&pods[i]) {
			return "backup pod " + pods[i].Name, nil
		}
	}
	return "", nil
}

func dumpPod(spec *corev1.PodSpec) bool {
	return slices.ContainsFunc(spec.InitContainers, func(c corev1.Container) bool { return c.Name == backup.ContainerDump })
}
