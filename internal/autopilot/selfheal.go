package autopilot

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	restartCooldown            = 10 * time.Minute
	hungPodGracePeriod         = 5
	defaultFailureThreshold    = 5
	defaultHealthCheckInterval = 30 * time.Second
	// unreachableAfter is how long the operator's /api/healthz probes must have gone
	// unanswered by Forgejo, while pods stayed Ready, before this is put down to the network
	// path between the operator and the Service. The kubelet's readiness probe calls the same
	// endpoint (3 × 10s, 5s timeout), so a Forgejo that really stopped answering is NotReady
	// well within this time.
	unreachableAfter     = time.Minute
	unreachableWarnEvery = time.Hour
	// healthReasonUnreachable is the Healthy condition reason the health step sets for a
	// probe that got no answer from Forgejo (observability.ReasonUnreachable).
	healthReasonUnreachable = "Unreachable"
)

func failureThreshold(f *forgejov1.Forgejo) int32 {
	if t := f.Spec.AutoPilot.SelfHealing.FailureThreshold; t > 0 {
		return t
	}
	return defaultFailureThreshold
}

func healthCheckInterval(f *forgejov1.Forgejo) time.Duration {
	return forgejo.DurationOr(f.Spec.AutoPilot.SelfHealing.HealthCheckInterval, defaultHealthCheckInterval)
}

// forgejoAnswered reports whether the latest health probe got a /api/healthz response from
// Forgejo itself, with its list of checks. A transport error (timeout, refused or reset
// connection, DNS failure) or a reply that is not Forgejo's healthz document (e.g. from a
// mesh sidecar) leaves status.health.checks empty.
func forgejoAnswered(h *forgejov1.HealthStatus) bool {
	return h != nil && len(h.Checks) > 0
}

// probeUnanswered reports whether the latest health probe got no answer from Forgejo: the
// health step reported it as Unreachable, or counted it as a failure without Forgejo's list
// of checks (a timeout, for example).
func probeUnanswered(f *forgejov1.Forgejo) bool {
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionHealthy); c != nil &&
		c.Status != metav1.ConditionTrue && c.Reason == healthReasonUnreachable {
		return true
	}
	h := f.Status.Health
	return h != nil && !h.Healthy && h.ConsecutiveFailures > 0 && !forgejoAnswered(h)
}

// trackUnreachable records since when the operator's probes have gone unanswered while
// pods are Ready, and reports whether that has lasted unreachableAfter. Forgejo is then
// serving (the kubelet's readiness probe on the same /api/healthz passes); the operator
// just cannot reach it: a default-deny NetworkPolicy, a service mesh with strict mTLS, or
// an operator running outside the cluster.
func trackUnreachable(st *instanceState, f *forgejov1.Forgejo, pods []corev1.Pod) bool {
	if !probeUnanswered(f) || readyPods(pods) == 0 {
		st.unansweredSince = time.Time{}
		return false
	}
	if st.unansweredSince.IsZero() {
		st.unansweredSince = time.Now()
	}
	return time.Since(st.unansweredSince) >= unreachableAfter
}

// reportProbeUnreachable sets Degraded with reason HealthProbeUnreachable (unless Degraded
// is already held by another reason) and emits an hourly Warning while the operator cannot
// reach Forgejo. Restarting would not help, since Forgejo is serving, so restartUnhealthy
// never acts on unanswered probes.
func reportProbeUnreachable(rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod, unreachable bool) {
	f := rc.Instance
	if !unreachable {
		clearDegraded(rc, ReasonHealthProbeUnreachable)
		return
	}
	msg := fmt.Sprintf("the operator cannot reach Forgejo at %s although %d pod(s) pass their readiness probe on /api/healthz (%s); "+
		"allow ingress from the operator to the Forgejo pods' HTTP port (NetworkPolicy, service mesh). "+
		"Until then health-based restarts are suspended and the last known good config is recorded from pod readiness",
		forgejo.InternalHTTPURL(f), readyPods(pods), f.Status.Health.Message)
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason == ReasonHealthProbeUnreachable {
		setDegraded(rc, ReasonHealthProbeUnreachable, msg)
	}
	warnEvery(rc, st, "health-probe-unreachable", unreachableWarnEvery, ReasonHealthProbeUnreachable, "%s", msg)
}

// restartUnhealthy remediates a hung Forgejo: the process is alive (the tcpSocket liveness
// probe passes because the kernel still accepts connections) but /api/healthz fails. It only
// acts while the database is ready, no upgrade/restore runs and the pods are not
// crash-looping (crash loops are config rollback's or the upgrade's business).
//
//   - instance level: status.health.consecutiveFailures >= failureThreshold, the latest
//     failing probe was answered by Forgejo itself (it reported failing checks) and every
//     server container is running → rolling restart through the restarted-at annotation.
//     Probes that Forgejo did not answer (timeouts, refused connections) never restart: the
//     kubelet probes the same endpoint, so a Forgejo that really hangs goes NotReady, which
//     resets the streak and leaves it to the pod-level check; failures that persist while
//     pods stay Ready mean the operator cannot reach Forgejo (see trackUnreachable);
//   - pod level: a started container whose pod stayed NotReady for failureThreshold ×
//     healthCheckInterval while running the last known good config → that pod is deleted
//     with a short grace period.
func restartUnhealthy(ctx context.Context, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, pods []corev1.Pod) error {
	f := rc.Instance
	if rc.Upgrading || rc.Maintenance || !rc.DatabaseReady || dep == nil || len(pods) == 0 ||
		inCooldown(f, ActionPodRestarted, restartCooldown) {
		return nil
	}
	threshold := failureThreshold(f)

	if h := f.Status.Health; h != nil && h.ConsecutiveFailures >= threshold && forgejoAnswered(h) && allRunning(pods) {
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
	hungAfter := time.Duration(threshold) * healthCheckInterval(f)
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

// readyPods counts the pods whose Ready condition is True.
func readyPods(pods []corev1.Pod) int {
	n := 0
	for i := range pods {
		if c := podReadyCondition(&pods[i]); c != nil && c.Status == corev1.ConditionTrue {
			n++
		}
	}
	return n
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
