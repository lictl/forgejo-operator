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
	pods := &corev1.PodList{}
	if err := c.List(ctx, pods, client.InNamespace(job.Namespace),
		client.MatchingLabels{batchv1.JobNameLabel: job.Name}); err != nil {
		return nil, err
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	return pods.Items, nil
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
		statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
		for _, cs := range statuses {
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
