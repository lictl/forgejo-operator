package upgrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	// serverContainer is the main container of core's Deployment.
	serverContainer = "forgejo"
	// migrateContainer is core's init container that runs "forgejo migrate".
	migrateContainer = "bootstrap"
	// pullGrace is how long pods of the new image may fail to pull it.
	pullGrace = 2 * time.Minute
	// crashRestarts fails the upgrade once any container of a new pod restarted this often.
	crashRestarts = 3
	// backOffRestarts fails it earlier while the kubelet is backing off a container that
	// crashed repeatedly (a single crash may be a blip).
	backOffRestarts = 2

	// Reasons of waiting containers.
	waitingImagePullBackOff = "ImagePullBackOff"
	waitingErrImagePull     = "ErrImagePull"
	waitingCrashLoopBackOff = "CrashLoopBackOff"
)

func serverPods(ctx context.Context, rc *forgejo.ReconcileContext) ([]corev1.Pod, error) {
	pods := &corev1.PodList{}
	err := rc.Client.List(ctx, pods, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.SelectorLabels(rc.Instance)))
	return pods.Items, err
}

func podImage(p *corev1.Pod) string {
	for _, c := range p.Spec.Containers {
		if c.Name == serverContainer {
			return c.Image
		}
	}
	return ""
}

func statuses(p *corev1.Pod) []corev1.ContainerStatus {
	return append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
}

// newImageFailure reports why pods of the new image cannot work: an image that cannot be
// pulled for pullGrace, or a container (including the migration) that keeps crashing. Pods
// for which ignore returns true are not judged.
func newImageFailure(pods []corev1.Pod, image string, now time.Time, ignore func(*corev1.Pod) bool) string {
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || podImage(p) != image || ignore(p) {
			continue
		}
		for _, cs := range statuses(p) {
			if w := cs.State.Waiting; w != nil {
				switch w.Reason {
				case "InvalidImageName", "ErrImageNeverPull":
					return fmt.Sprintf("image %s cannot be used: %s", image, strings.TrimSpace(w.Reason+" "+w.Message))
				case waitingErrImagePull, waitingImagePullBackOff:
					if now.Sub(p.CreationTimestamp.Time) > pullGrace {
						return fmt.Sprintf("image %s could not be pulled for %s: %s", image, pullGrace, strings.TrimSpace(w.Reason+" "+w.Message))
					}
				case waitingCrashLoopBackOff:
					if cs.RestartCount >= backOffRestarts {
						return crashMessage(p, cs)
					}
				}
			}
			if cs.RestartCount >= crashRestarts {
				return crashMessage(p, cs)
			}
		}
	}
	return ""
}

// failing reports a pod with a container that is not running because it failed: it exited
// (an init container with an error), or it waits after a crash or a failed image pull.
func failing(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodFailed {
		return true
	}
	waitsOnFailure := func(cs corev1.ContainerStatus) bool {
		w := cs.State.Waiting
		return w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing"
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if t := cs.State.Terminated; (t != nil && t.ExitCode != 0) || waitsOnFailure(cs) {
			return true
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.State.Terminated != nil || waitsOnFailure(cs) {
			return true
		}
	}
	return false
}

func crashMessage(p *corev1.Pod, cs corev1.ContainerStatus) string {
	what := "container " + cs.Name
	if cs.Name == migrateContainer {
		what = "the database migration (init container " + cs.Name + ")"
	}
	msg := fmt.Sprintf("%s of pod %s crashed %d times", what, p.Name, cs.RestartCount)
	if t := cs.LastTerminationState.Terminated; t != nil {
		msg += fmt.Sprintf(", last exit code %d", t.ExitCode)
		if detail := strings.TrimSpace(t.Reason + " " + t.Message); detail != "" {
			msg += " (" + truncate(detail, 300) + ")"
		}
	}
	return msg
}

// rolloutProgress describes the newest pod of the new image; running is true once its
// migration finished and the server container started.
func rolloutProgress(pods []corev1.Pod, image string) (msg string, running bool) {
	var newest *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp == nil && podImage(p) == image &&
			(newest == nil || newest.CreationTimestamp.Before(&p.CreationTimestamp)) {
			newest = p
		}
	}
	if newest == nil {
		return fmt.Sprintf("Rolling out %s: waiting for its pod", image), false
	}
	for _, cs := range newest.Status.ContainerStatuses {
		if cs.Name == serverContainer && (cs.State.Running != nil || cs.State.Terminated != nil) {
			return fmt.Sprintf("Database migrated; waiting for %s to become ready", image), true
		}
	}
	for _, cs := range newest.Status.InitContainerStatuses {
		if cs.Name == migrateContainer && cs.State.Running != nil {
			return fmt.Sprintf("Rolling out %s: migrating the database (init container %s)", image, migrateContainer), false
		}
	}
	return fmt.Sprintf("Rolling out %s: starting pod %s", image, newest.Name), false
}

// rolledOut reports whether every replica of dep runs image and is available.
func rolledOut(dep *appsv1.Deployment, image string) bool {
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas == 0 {
		return false
	}
	want, s := *dep.Spec.Replicas, dep.Status
	return deploymentImage(dep) == image && s.ObservedGeneration >= dep.Generation &&
		s.UpdatedReplicas == want && s.Replicas == want && s.AvailableReplicas == want
}

// deploymentImage is the server image of dep's pod template.
func deploymentImage(dep *appsv1.Deployment) string {
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == serverContainer {
			return c.Image
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
