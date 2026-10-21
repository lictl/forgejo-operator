package autopilot

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	restartCooldown            = 10 * time.Minute
	hungPodGracePeriod         = 5
	defaultFailureThreshold    = 5
	defaultHealthCheckInterval = 30 * time.Second
)

// restartUnhealthy remediates a hung Forgejo: the process is alive (the tcpSocket liveness
// probe passes because the kernel still accepts connections) but /api/healthz fails. It only
// acts while the database is ready, no upgrade/restore runs and the pods are not
// crash-looping (crash loops are config rollback's or the upgrade's business).
//
//   - instance level: status.health.consecutiveFailures >= failureThreshold while every
//     server container is running → rolling restart through the restarted-at annotation;
//   - pod level: a started container whose pod stayed NotReady for failureThreshold ×
//     healthCheckInterval while running the last known good config → that pod is deleted
//     with a short grace period.
func restartUnhealthy(ctx context.Context, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, pods []corev1.Pod) error {
	f := rc.Instance
	if rc.Upgrading || rc.Maintenance || !rc.DatabaseReady || dep == nil || len(pods) == 0 ||
		inCooldown(f, ActionPodRestarted, restartCooldown) {
		return nil
	}
	sh := f.Spec.AutoPilot.SelfHealing
	threshold := sh.FailureThreshold
	if threshold <= 0 {
		threshold = defaultFailureThreshold
	}

	if h := f.Status.Health; h != nil && h.ConsecutiveFailures >= threshold && allRunning(pods) {
		patch := client.MergeFrom(dep.DeepCopy())
		if dep.Spec.Template.Annotations == nil {
			dep.Spec.Template.Annotations = map[string]string{}
		}
		dep.Spec.Template.Annotations[forgejo.AnnotationRestartedAt] = time.Now().UTC().Format(time.RFC3339)
		if err := rc.Client.Patch(ctx, dep, patch); err != nil {
			return err
		}
		rc.RecordAutoPilotAction(ActionPodRestarted, fmt.Sprintf(
			"health check failed %d consecutive times (%s) while the pods kept running; restarting Deployment %s",
			h.ConsecutiveFailures, h.Message, dep.Name))
		return nil
	}

	lkg := ""
	if ap := f.Status.AutoPilot; ap != nil {
		lkg = ap.LastKnownGoodConfigHash
	}
	hungAfter := time.Duration(threshold) * forgejo.DurationOr(sh.HealthCheckInterval, defaultHealthCheckInterval)
	for i := range pods {
		p := &pods[i]
		if lkg == "" || p.Annotations[forgejo.AnnotationConfigHash] != lkg || !hung(p, hungAfter) {
			continue
		}
		// A hung process will not shut down gracefully, and until it is killed it keeps
		// file locks (e.g. the LevelDB queue of a non-HA instance) the replacement needs.
		if err := rc.Client.Delete(ctx, p, client.Preconditions{UID: &p.UID}, client.GracePeriodSeconds(hungPodGracePeriod)); client.IgnoreNotFound(err) != nil {
			return err
		}
		rc.RecordAutoPilotAction(ActionPodRestarted, fmt.Sprintf(
			"pod %s stopped answering /api/healthz for more than %s while its container kept running; deleted it", p.Name, hungAfter))
		return nil
	}
	return nil
}

// allRunning reports whether every server container is running and none is crash-looping.
func allRunning(pods []corev1.Pod) bool {
	for i := range pods {
		cs := containerStatus(&pods[i], serverContainerName)
		if cs == nil || cs.State.Running == nil || podFailure(&pods[i]) != "" {
			return false
		}
	}
	return true
}

// hung reports whether the server container passed its startup probe and is still running
// but the pod has been NotReady for longer than d.
func hung(p *corev1.Pod, d time.Duration) bool {
	cs := containerStatus(p, serverContainerName)
	if p.Status.Phase != corev1.PodRunning || cs == nil || cs.State.Running == nil || cs.Started == nil || !*cs.Started || cs.Ready {
		return false
	}
	if time.Since(cs.State.Running.StartedAt.Time) < d {
		return false
	}
	c := podReadyCondition(p)
	return c != nil && c.Status != corev1.ConditionTrue && time.Since(c.LastTransitionTime.Time) > d
}

func containerStatus(p *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}
