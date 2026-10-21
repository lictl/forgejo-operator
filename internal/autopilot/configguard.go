package autopilot

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	// lkgSoak is how long every pod must have been Ready before a config counts as good.
	lkgSoak = time.Minute
	// notReadyTimeout marks a pod running a new config as failed when it never becomes Ready.
	notReadyTimeout = 5 * time.Minute
	// crashRestarts is the restart count at which a container counts as crash-looping.
	crashRestarts = 3
)

func configRollbackEnabled(f *forgejov1.Forgejo) bool {
	return forgejo.SelfHealingEnabled(f) && forgejo.BoolOr(f.Spec.AutoPilot.SelfHealing.ConfigRollback, true)
}

// ReconcileConfigGuard may switch rc.ConfigSecretName/rc.ConfigHash to the last known good
// config when the desired config was rejected by self-healing.
func ReconcileConfigGuard(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	ap := f.Status.AutoPilot
	if ap == nil || ap.RejectedConfigHash == "" || rc.DesiredConfigHash == "" {
		clearDegraded(rc, ReasonConfigRolledBack)
		return forgejo.StepResult{}, nil
	}
	if !configRollbackEnabled(f) || ap.RejectedConfigHash != rc.DesiredConfigHash {
		// The rendered app.ini changed (or rollback was turned off): try the new config.
		rc.Event(corev1.EventTypeNormal, "ConfigRetry", "AutoPilot",
			"app.ini changed from rejected %s to %s; deploying it", short(ap.RejectedConfigHash), short(rc.DesiredConfigHash))
		ap.RejectedConfigHash = ""
		clearDegraded(rc, ReasonConfigRolledBack)
		return forgejo.StepResult{}, nil
	}

	lkg := &corev1.Secret{}
	err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.LKGConfigSecretName(f)}, lkg)
	if err != nil && !apierrors.IsNotFound(err) {
		return forgejo.StepResult{}, err
	}
	if err != nil || ap.LastKnownGoodConfigHash == "" || lkg.Annotations[forgejo.AnnotationConfigHash] != ap.LastKnownGoodConfigHash {
		rc.Event(corev1.EventTypeWarning, "ConfigRollbackUnavailable", "AutoPilot",
			"app.ini %s was rejected but no last known good config is stored; deploying it anyway", short(rc.DesiredConfigHash))
		ap.RejectedConfigHash = ""
		clearDegraded(rc, ReasonConfigRolledBack)
		return forgejo.StepResult{}, nil
	}

	rc.ConfigSecretName = lkg.Name
	rc.ConfigHash = ap.LastKnownGoodConfigHash
	setDegraded(rc, ReasonConfigRolledBack, fmt.Sprintf(
		"app.ini %s made Forgejo fail; running last known good %s until the configuration changes",
		short(rc.DesiredConfigHash), short(ap.LastKnownGoodConfigHash)))
	return forgejo.StepResult{}, nil
}

// recordLastKnownGood copies the deployed app.ini to "<name>-config-lkg" once it has been
// fully rolled out, every pod is Ready for lkgSoak and Forgejo reports healthy.
//
// When the operator cannot reach Forgejo at all (unreachable, see trackUnreachable), the
// kubelet's readiness probe, which calls the same /api/healthz, stands in for the
// operator's: the pods must still have been Ready for lkgSoak. Without that, no LKG would
// ever be recorded in such clusters, which also disables config rollback and the pod-level
// hung check.
func recordLastKnownGood(ctx context.Context, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, pods []corev1.Pod, unreachable bool) error {
	f := rc.Instance
	if rc.ConfigHash == "" || rc.ConfigHash != rc.DesiredConfigHash || rc.Upgrading || rc.Maintenance {
		return nil
	}
	healthy := f.Status.Health != nil && f.Status.Health.Healthy
	if (!healthy && !unreachable) || !fullyRolledOut(dep, rc.ConfigHash) {
		return nil
	}
	// Every replica must be listed, so that the soak check below cannot pass vacuously on
	// a pod cache that lags behind the Deployment status.
	if len(pods) < int(*dep.Spec.Replicas) {
		return nil
	}
	for i := range pods {
		if pods[i].Annotations[forgejo.AnnotationConfigHash] != rc.ConfigHash || readyFor(&pods[i]) < lkgSoak {
			return nil
		}
	}

	ap := autoPilotStatus(rc)
	lkg := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.LKGConfigSecretName(f), Namespace: f.Namespace}}
	if ap.LastKnownGoodConfigHash == rc.ConfigHash {
		err := rc.Client.Get(ctx, client.ObjectKeyFromObject(lkg), lkg)
		if err == nil && lkg.Annotations[forgejo.AnnotationConfigHash] == rc.ConfigHash {
			return nil
		}
		if client.IgnoreNotFound(err) != nil {
			return err
		}
	}

	src := &corev1.Secret{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, src); err != nil {
		return client.IgnoreNotFound(err)
	}
	if h, ok := src.Annotations[forgejo.AnnotationConfigHash]; ok && h != rc.ConfigHash {
		return nil // stale cache read; try again on the next reconcile
	}
	if _, err := rc.CreateOrUpdate(ctx, lkg, true, func() error {
		lkg.Labels = forgejo.Labels(f, forgejo.ComponentServer)
		if lkg.Annotations == nil {
			lkg.Annotations = map[string]string{}
		}
		lkg.Annotations[forgejo.AnnotationConfigHash] = rc.ConfigHash
		lkg.Type = corev1.SecretTypeOpaque
		lkg.Data = src.Data
		return nil
	}); err != nil {
		return err
	}
	ap.LastKnownGoodConfigHash = rc.ConfigHash
	rc.Event(corev1.EventTypeNormal, "LastKnownGoodConfig", "AutoPilot", "app.ini %s ran healthy; stored as last known good", short(rc.ConfigHash))
	return nil
}

// rollbackFailingConfig rejects the desired app.ini when pods running it crash-loop or never
// become Ready, while the last known good config is available. Image changes are owned by
// the upgrade step, so nothing happens while an upgrade or restore is in progress.
func rollbackFailingConfig(ctx context.Context, rc *forgejo.ReconcileContext, st *instanceState, pods []corev1.Pod) error {
	f := rc.Instance
	ap := f.Status.AutoPilot
	desired := rc.DesiredConfigHash
	if !configRollbackEnabled(f) || rc.Upgrading || rc.Maintenance || !rc.DatabaseReady || ap == nil ||
		ap.LastKnownGoodConfigHash == "" || desired == "" || desired == ap.LastKnownGoodConfigHash ||
		rc.ConfigHash != desired || ap.RejectedConfigHash == desired {
		return nil
	}
	var failure string
	var failed *corev1.Pod
	for i := range pods {
		if pods[i].Annotations[forgejo.AnnotationConfigHash] != desired {
			continue
		}
		if failure = podFailure(&pods[i]); failure != "" {
			failed = &pods[i]
			break
		}
	}
	if failed == nil {
		return nil
	}
	lkg := &corev1.Secret{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.LKGConfigSecretName(f)}, lkg); err != nil {
		return client.IgnoreNotFound(err)
	}
	// The status patch may not be visible to the next reconcile yet; don't record twice.
	if !st.allow("reject/"+desired, 5*time.Minute) {
		return nil
	}
	ap.RejectedConfigHash = desired
	msg := fmt.Sprintf("app.ini %s: pod %s %s; rolled back to last known good %s", short(desired), failed.Name, failure, short(ap.LastKnownGoodConfigHash))
	rc.RecordAutoPilotAction(ActionConfigRolledBack, msg)
	setDegraded(rc, ReasonConfigRolledBack, msg)
	rc.Requeue(time.Second)
	return nil
}

// podFailure describes why a pod counts as failed by its configuration, or returns "".
// OOM kills are left to vertical scaling.
func podFailure(p *corev1.Pod) string {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if oomKilled(cs) {
			return ""
		}
	}
	for _, cs := range statuses {
		// CrashLoopBackOff already shows after the first exit; require a repeated failure.
		backoff := cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff" && cs.RestartCount >= 2
		if backoff || cs.RestartCount >= crashRestarts {
			msg := fmt.Sprintf("container %s is crash-looping (%d restarts", cs.Name, cs.RestartCount)
			if t := cs.LastTerminationState.Terminated; t != nil {
				msg += fmt.Sprintf(", last exit code %d", t.ExitCode)
			}
			return msg + ")"
		}
	}
	// Measured from the start of the main container, so slow init containers don't count.
	cs := containerStatus(p, serverContainerName)
	if p.Status.Phase == corev1.PodRunning && cs != nil && cs.State.Running != nil && !cs.Ready &&
		time.Since(cs.State.Running.StartedAt.Time) > notReadyTimeout {
		if c := podReadyCondition(p); c != nil && c.Status != corev1.ConditionTrue && time.Since(c.LastTransitionTime.Time) > notReadyTimeout {
			return fmt.Sprintf("has not been Ready for %s", notReadyTimeout)
		}
	}
	return ""
}

func oomKilled(cs corev1.ContainerStatus) bool {
	if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
		return true
	}
	t := cs.State.Terminated
	return t != nil && t.Reason == "OOMKilled"
}

func podReadyCondition(p *corev1.Pod) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == corev1.PodReady {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// readyFor returns how long the pod has been Ready (0 when it is not).
func readyFor(p *corev1.Pod) time.Duration {
	c := podReadyCondition(p)
	if c == nil || c.Status != corev1.ConditionTrue {
		return 0
	}
	return time.Since(c.LastTransitionTime.Time)
}

func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}
