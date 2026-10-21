package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// JobState summarizes a Job.
type JobState int

const (
	JobRunning JobState = iota
	JobSucceeded
	JobFailed
)

// StateOf returns whether the Job finished and how.
func StateOf(job *batchv1.Job) (JobState, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return JobSucceeded, ""
		case batchv1.JobFailed:
			return JobFailed, strings.TrimSpace(c.Reason + ": " + c.Message)
		}
	}
	return JobRunning, ""
}

// JobPods returns the Job's pods, newest first.
func JobPods(ctx context.Context, c client.Client, job *batchv1.Job) ([]corev1.Pod, error) {
	return JobPodsByName(ctx, c, job.Namespace, job.Name)
}

// JobPodsByName returns the pods of the Job with the given name, newest first. It also finds
// pods whose Job is already gone.
func JobPodsByName(ctx context.Context, c client.Client, namespace, name string) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{batchv1.JobNameLabel: name}); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	return pods.Items, nil
}

// PodFinished reports whether a pod can no longer run anything: it reached a terminal phase
// and no container is reported running (a pod marked Failed by pod GC on a lost node may still
// report the containers it last ran).
func PodFinished(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
		return false
	}
	for _, cs := range containerStatuses(p) {
		if cs.State.Running != nil {
			return false
		}
	}
	return true
}

// UnfinishedPods counts the pods that may still be running a container.
func UnfinishedPods(pods []corev1.Pod) int {
	n := 0
	for i := range pods {
		if !PodFinished(&pods[i]) {
			n++
		}
	}
	return n
}

// PodsSettled reports whether pods have the final word about what a finished Job did: every
// pod is finished and none is missing from the Job's counts (pods force-deleted from a lost
// node are counted by the Job but gone from the API).
func PodsSettled(job *batchv1.Job, pods []corev1.Pod) bool {
	return UnfinishedPods(pods) == 0 && int32(len(pods)) >= job.Status.Failed+job.Status.Succeeded
}

func containerStatuses(p *corev1.Pod) []corev1.ContainerStatus {
	return append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
}

// RestoreModification inspects every pod of a restore Job, not only the newest, and reports
// how far the restore may have changed the instance:
//   - Modified when a pod's restore-database step exited 0 (its transaction committed) or a
//     pod's restore-data step started (it wipes the volume before extracting);
//   - MaybeModified when a pod passed verification and its destructive step has not failed
//     cleanly yet (a failing restore-database step rolls its transaction back);
//   - Unmodified otherwise.
func RestoreModification(pods []corev1.Pod) (Modification, string) {
	mod, why := Unmodified, ""
	raise := func(m Modification, msg string) {
		if m > mod {
			mod, why = m, msg
		}
	}
	for i := range pods {
		p := &pods[i]
		state := map[string]corev1.ContainerState{}
		for _, cs := range containerStatuses(p) {
			state[cs.Name] = cs.State
		}
		if s, ok := state[ContainerRestoreData]; ok && (s.Running != nil || s.Terminated != nil) {
			raise(Modified, fmt.Sprintf("pod %s started replacing the data volume", p.Name))
			continue
		}
		db, hasDB := state[ContainerRestoreDB]
		if hasDB && db.Terminated != nil && db.Terminated.ExitCode == 0 {
			raise(Modified, fmt.Sprintf("pod %s restored the database", p.Name))
			continue
		}
		v := state[ContainerVerify]
		if v.Terminated == nil || v.Terminated.ExitCode != 0 {
			continue
		}
		if hasDB && db.Terminated != nil {
			continue // the database step failed, so its transaction was rolled back
		}
		raise(MaybeModified, fmt.Sprintf("pod %s passed verification and started restoring", p.Name))
	}
	return mod, why
}

// JobConflictError reports a Job with the wanted name that the object does not control.
type JobConflictError struct {
	Job string
	// Owner is "Kind name" of the Job's controller, or "" when it has none.
	Owner string
	// Transient: the Job is being deleted, or it belongs to a deleted object of the same kind
	// and name and garbage collection will remove it; retry later.
	Transient bool
}

func (e *JobConflictError) Error() string {
	owner := "has no controller"
	if e.Owner != "" {
		owner = "belongs to " + e.Owner
	}
	return fmt.Sprintf("Job %s already exists and %s", e.Job, owner)
}

// CreateJob creates a Job for owner. An existing Job of the same name is accepted only when
// owner controls it (compared by UID), e.g. after a restart between creating the Job and
// recording it; otherwise it returns a *JobConflictError, so a Job of another object is never
// mistaken for this object's own.
func CreateJob(ctx context.Context, c client.Client, owner client.Object, job *batchv1.Job) error {
	err := c.Create(ctx, job)
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(job), existing); err != nil {
		return fmt.Errorf("job %s exists but cannot be read yet: %w", job.Name, err)
	}
	return CheckOwned(existing, owner)
}

// RemoveJob deletes job with background propagation, so that garbage collection also removes
// its pods. A finished pod still names the PVCs it mounted, and the PVC protection controller
// keeps a deleted claim (Terminating) until such pods are gone; so once a controller has
// recorded what a Job did, the Job and its pods must go. The UID precondition makes sure a
// newer Job of the same name is never deleted instead.
func RemoveJob(ctx context.Context, c client.Client, job *batchv1.Job) error {
	opts := []client.DeleteOption{client.PropagationPolicy(metav1.DeletePropagationBackground)}
	if job.UID != "" {
		opts = append(opts, client.Preconditions{UID: &job.UID})
	}
	err := c.Delete(ctx, job, opts...)
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil // gone already, or replaced by a Job that is not this one
	}
	return err
}

// DeleteFinishedJob removes the Job of that name in owner's namespace (see RemoveJob) when
// owner controls it and it has finished. A missing Job, a running one, one being deleted and
// a Job of another object are left alone. It is cheap (one cache read) and idempotent, so
// controllers call it on every reconcile of a finished ForgejoBackup or ForgejoRestore; that
// also removes Jobs an earlier operator version kept.
func DeleteFinishedJob(ctx context.Context, c client.Client, owner client.Object, name string) error {
	if name == "" {
		return nil
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, job); err != nil {
		return client.IgnoreNotFound(err)
	}
	if !job.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(job, owner) {
		return nil
	}
	if state, _ := StateOf(job); state == JobRunning {
		return nil
	}
	return RemoveJob(ctx, c, job)
}

// CheckOwned returns nil when owner is the Job's controller, otherwise a *JobConflictError.
func CheckOwned(job *batchv1.Job, owner client.Object) error {
	if metav1.IsControlledBy(job, owner) {
		return nil
	}
	e := &JobConflictError{Job: job.Name, Transient: !job.DeletionTimestamp.IsZero()}
	if ref := metav1.GetControllerOf(job); ref != nil {
		e.Owner = ref.Kind + " " + ref.Name
		if ref.Name == owner.GetName() && ref.Kind == kindOf(owner) {
			e.Transient = true // left behind by a deleted object of the same name
		}
	}
	return e
}

func kindOf(obj client.Object) string {
	switch obj.(type) {
	case *forgejov1.ForgejoBackup:
		return "ForgejoBackup"
	case *forgejov1.ForgejoRestore:
		return "ForgejoRestore"
	}
	return obj.GetObjectKind().GroupVersionKind().Kind
}

// ParseBackupResult reads the archive container's termination message from the succeeded pod.
func ParseBackupResult(pods []corev1.Pod) (BackupResult, error) {
	var res BackupResult
	for _, p := range pods {
		if p.Status.Phase != corev1.PodSucceeded {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != ContainerArchive || cs.State.Terminated == nil {
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

// FailureMessage explains why the newest pod failed (the failing container's termination
// message, which falls back to its log tail).
func FailureMessage(pods []corev1.Pod) string {
	for _, p := range pods {
		for _, cs := range containerStatuses(&p) {
			t := cs.State.Terminated
			if t == nil || t.ExitCode == 0 {
				continue
			}
			msg := strings.TrimSpace(t.Message)
			if msg == "" {
				msg = t.Reason
			}
			return truncate(fmt.Sprintf("container %s exited with %d: %s", cs.Name, t.ExitCode, msg), 1024)
		}
	}
	return ""
}

// PendingMessage explains why the newest pod is not making progress (unschedulable, image
// pull errors, missing secrets), or "" when it is.
func PendingMessage(pods []corev1.Pod) string {
	if len(pods) == 0 {
		return ""
	}
	p := pods[0]
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			return truncate("pod "+p.Name+" cannot be scheduled: "+c.Message, 1024)
		}
	}
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "PodInitializing" && w.Reason != "ContainerCreating" {
			return truncate(fmt.Sprintf("container %s is waiting: %s %s", cs.Name, w.Reason, w.Message), 1024)
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Stalled reports why the Job's newest pod has not started any container within timeout
// (unschedulable, image pull errors, pods rejected by admission), or "" when it is progressing.
func Stalled(job *batchv1.Job, pods []corev1.Pod, timeout time.Duration) string {
	if len(pods) == 0 {
		if time.Since(job.CreationTimestamp.Time) > timeout {
			return fmt.Sprintf("job %s created no pod within %s; see its events", job.Name, timeout)
		}
		return ""
	}
	p := pods[0]
	if time.Since(p.CreationTimestamp.Time) < timeout {
		return ""
	}
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return ""
		}
	}
	msg := PendingMessage(pods)
	if msg == "" {
		msg = "pod " + p.Name + " did not start"
	}
	return fmt.Sprintf("%s (for more than %s)", msg, timeout)
}

// RestoreUsing returns an unfinished restore that reads the given backup, or "".
func RestoreUsing(ctx context.Context, c client.Client, b *forgejov1.ForgejoBackup) (string, error) {
	restores, err := ListRestores(ctx, c, b.Namespace, b.Spec.ForgejoRef.Name)
	if err != nil {
		return "", err
	}
	for i := range restores {
		r := &restores[i]
		if r.Spec.BackupName == b.Name && BlocksBackups(r) {
			return r.Name, nil
		}
	}
	return "", nil
}
