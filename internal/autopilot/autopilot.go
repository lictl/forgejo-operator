// Package autopilot implements Level V capabilities: HA dependencies (Valkey), horizontal
// and vertical scaling, auto-tuning, storage auto-expansion and self-healing.
//
// Pipeline steps (see internal/controller/forgejo_controller.go for the order):
//   - ReconcileHA          Valkey with Sentinel for the shared cache and queues; rc.ValkeyURL
//   - ReportValkey         the Sentinels' view of Valkey: ValkeyReady, roles, failovers
//   - ReconcileTuning      rc.Tuning, rc.TuningEnv, rc.ResourcesOverride, rc.HPAManaged
//   - ReconcileConfigGuard switches to the last-known-good app.ini after a rejected config
//   - ReconcileScaling     HPA and PodDisruptionBudget
//   - ReconcileStorage     volume measurement, StorageHealthy and PVC auto-expansion; has no
//     prerequisites, so a stopped database step (e.g. a full CNPG volume) cannot starve it
//   - Reconcile            post-rollout actions: LKG bookkeeping, config rollback,
//     health-based restarts, OOM and usage-based right-sizing
package autopilot

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Action types recorded in status.autoPilot.recentActions.
const (
	ActionConfigRolledBack = "ConfigRolledBack"
	ActionPodRestarted     = "PodRestarted"
	ActionResourcesScaled  = "ResourcesScaled"
	ActionStorageExpanded  = "StorageExpanded"
)

// Degraded/StorageHealthy reasons owned by this package.
const (
	ReasonHARequiresSharedStorage     = "HARequiresSharedStorage"
	ReasonHorizontalScalingRequiresHA = "HorizontalScalingRequiresHA"
	ReasonConfigRolledBack            = "ConfigRolledBack"
	ReasonExpansionUnsupported        = "ExpansionUnsupported"
	ReasonMaxSizeReached              = "MaxSizeReached"
	ReasonHealthProbeUnreachable      = "HealthProbeUnreachable"
	ReasonHPAMissingResourceRequests  = "HPAMissingResourceRequests"
	ReasonAutoExpandDisabled          = "AutoExpandDisabled"
	ReasonExpansionStalled            = "ExpansionStalled"
	ReasonDatabaseClusterMissing      = "DatabaseClusterMissing"
)

// serverContainerName is the Forgejo container in the server pods.
const serverContainerName = "forgejo"

// Reconcile runs post-rollout autonomous actions: vertical right-sizing, self-healing and
// last-known-good bookkeeping. Storage is handled by ReconcileStorage, its own step.
func Reconcile(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()

	dep, err := serverDeployment(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	cron, err := cronRunner(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	// The server pods and the cron runner's: both run the same app.ini (hash), image and
	// resources, so a config that crash-loops, a hung pod or an OOM kill counts in either.
	pods, err := forgejoPods(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	webPods := podsOf(pods, forgejo.ComponentServer)

	var errs []error
	// The operator probes Forgejo through the Service, which routes to the server pods only.
	unreachable := trackUnreachable(st, f, webPods)
	// The LKG is recorded before Valkey cleanup looks at it: a non-HA config that just
	// became last known good lets Valkey go in the same pass.
	errs = append(errs, recordLastKnownGood(ctx, rc, dep, cron, pods, unreachable))
	errs = append(errs, cleanupValkey(ctx, rc, st, dep, pods))
	reportProbeUnreachable(rc, st, webPods, unreachable)
	if forgejo.SelfHealingEnabled(f) {
		errs = append(errs, rollbackFailingConfig(ctx, rc, st, pods))
		errs = append(errs, restartUnhealthy(ctx, rc, dep, pods))
	}
	if forgejo.VerticalAutoscalingEnabled(f) && !rc.Maintenance && !replacementsHeld(rc) {
		reverted := revertUnschedulable(rc, st, pods)
		oomSeen, oomScaled := false, false
		if !reverted {
			oomSeen, oomScaled = handleOOM(ctx, rc, st, pods)
		}
		rsScaled := rightSize(ctx, rc, st, pods, oomSeen || reverted)
		errs = append(errs, recordVerticalBase(ctx, rc, dep, reverted || oomScaled || rsScaled))
	}
	return forgejo.StepResult{}, errors.Join(errs...)
}

// serverDeployment returns the Forgejo Deployment, or nil when it does not exist (yet).
func serverDeployment(ctx context.Context, rc *forgejo.ReconcileContext) (*appsv1.Deployment, error) {
	f := rc.Instance
	dep := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return dep, nil
}

// cronRunner returns the cron runner StatefulSet the instance controls while
// highAvailability is enabled, or nil.
func cronRunner(ctx context.Context, rc *forgejo.ReconcileContext) (*appsv1.StatefulSet, error) {
	if !forgejo.CronRunnerEnabled(rc.Instance) {
		return nil, nil
	}
	return forgejo.CronRunner(ctx, rc.Client, rc.Instance)
}

// workloadHeld reports whether a migration owns the workload: an upgrade in progress, a
// failed upgrade whose new image is still deployed and unverified, or a restored database
// waiting for its first pod. The workload then runs a single pod that must not be rolled,
// restarted or joined by others, so autonomous actions that would change the pod template
// or the replica count wait.
func workloadHeld(rc *forgejo.ReconcileContext) bool {
	return rc.Upgrading || rc.RestoreMigrating || forgejo.UpgradeHoldsWorkload(rc.Instance)
}

// replacementsHeld reports whether autonomous actions that replace pods (restarts, resizes,
// config rollbacks) must wait: a migration owns the workload, the Secret of SSH host keys
// the pods serve is missing or unusable (the workload step leaves the Deployment as it is
// meanwhile), or the bucket of spec.objectStorage is not usable (Forgejo does not start
// without it), so that a replacement pod could not start. The HPA is left alone: pods it
// adds just do not start, and the running ones keep serving.
func replacementsHeld(rc *forgejo.ReconcileContext) bool {
	return workloadHeld(rc) || rc.SSHHostKeysUnavailable || rc.ObjectStorageUnavailable
}

// serverPods lists the Forgejo server pods of the instance that are neither terminating nor
// terminated. Terminal pods (phase Failed or Succeeded, e.g. evicted under node pressure or
// stopped by a graceful node shutdown) stay in the API until pod GC, while their ReplicaSet
// has already replaced them; they would otherwise block LKG bookkeeping and self-healing
// forever. With restartPolicy Always, a bad config shows as a crash loop in phase Running,
// never as a terminal phase, so config rollback loses nothing.
func serverPods(ctx context.Context, rc *forgejo.ReconcileContext) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.SelectorLabels(rc.Instance))); err != nil {
		return nil, err
	}
	return livePods(list.Items), nil
}

// forgejoPods lists the server pods and the cron runner's pods of the instance that are
// neither terminating nor terminated (see serverPods).
func forgejoPods(ctx context.Context, rc *forgejo.ReconcileContext) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabelsSelector{Selector: forgejo.ForgejoPodSelector(rc.Instance)}); err != nil {
		return nil, err
	}
	return livePods(list.Items), nil
}

// podsOf returns the pods of one component (server or cron).
func podsOf(pods []corev1.Pod, component string) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		if pods[i].Labels[forgejo.LabelComponent] == component {
			out = append(out, pods[i])
		}
	}
	return out
}

func livePods(items []corev1.Pod) []corev1.Pod {
	pods := items[:0]
	for _, p := range items {
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		pods = append(pods, p)
	}
	return pods
}

// statefulSetRolledOut reports whether every replica of sts runs the current revision with
// the given config hash and is available.
func statefulSetRolledOut(sts *appsv1.StatefulSet, configHash string) bool {
	if sts == nil || sts.Spec.Replicas == nil || *sts.Spec.Replicas == 0 {
		return false
	}
	want := *sts.Spec.Replicas
	s := sts.Status
	return s.ObservedGeneration >= sts.Generation && s.UpdateRevision != "" && s.CurrentRevision == s.UpdateRevision &&
		s.UpdatedReplicas == want && s.Replicas == want && s.AvailableReplicas == want &&
		sts.Spec.Template.Annotations[forgejo.AnnotationConfigHash] == configHash
}

// fullyRolledOut reports whether every replica of dep runs the current template with the
// given config hash and is available.
func fullyRolledOut(dep *appsv1.Deployment, configHash string) bool {
	if dep == nil || dep.Spec.Replicas == nil || *dep.Spec.Replicas == 0 {
		return false
	}
	want := *dep.Spec.Replicas
	s := dep.Status
	return s.ObservedGeneration >= dep.Generation &&
		s.UpdatedReplicas == want && s.Replicas == want && s.AvailableReplicas == want &&
		dep.Spec.Template.Annotations[forgejo.AnnotationConfigHash] == configHash
}

func autoPilotStatus(rc *forgejo.ReconcileContext) *forgejov1.AutoPilotStatus {
	if rc.Instance.Status.AutoPilot == nil {
		rc.Instance.Status.AutoPilot = &forgejov1.AutoPilotStatus{}
	}
	return rc.Instance.Status.AutoPilot
}

// lastAction returns the time of the newest recorded action of the given type.
func lastAction(f *forgejov1.Forgejo, actionType string) time.Time {
	if f.Status.AutoPilot == nil {
		return time.Time{}
	}
	for _, a := range f.Status.AutoPilot.RecentActions {
		if a.Type == actionType {
			return a.Time.Time
		}
	}
	return time.Time{}
}

func inCooldown(f *forgejov1.Forgejo, actionType string, d time.Duration) bool {
	t := lastAction(f, actionType)
	return !t.IsZero() && time.Since(t) < d
}

func setDegraded(rc *forgejo.ReconcileContext, reason, message string) {
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, reason, message)
}

// clearDegraded resets Degraded only when it was set for one of the given reasons, so
// that conditions owned by other steps are left alone.
func clearDegraded(rc *forgejo.ReconcileContext, reasons ...string) {
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
	if c != nil && c.Status == metav1.ConditionTrue && slices.Contains(reasons, c.Reason) {
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}

// warnInterval is how often a persisting problem is repeated as a Warning event.
const warnInterval = time.Hour

// warnEvery emits a Warning event at most once per warnInterval for the given key.
func warnEvery(rc *forgejo.ReconcileContext, st *instanceState, key, reason, note string, args ...any) {
	if st.allow(key, warnInterval) {
		rc.Event(corev1.EventTypeWarning, reason, "AutoPilot", note, args...)
	}
}

// --- in-memory per-instance state ---

// instanceState holds what the operator only needs to remember while it runs: usage
// samples, measurement throttling and event rate limits. Losing it on restart is harmless.
type instanceState struct {
	mu       sync.Mutex
	touched  time.Time
	samples  []usageSample
	lastSeen map[string]time.Time // pod -> timestamp of the last PodMetrics sample used
	limits   map[string]time.Time // rate-limit key -> last time allowed

	lastStorageCheck time.Time
	measure          *measureState        // volume measurements, updated in the background
	resizeSeen       map[string]time.Time // claim -> when a pending resize was first seen
	resizeRestarted  map[string]time.Time // claim -> last restart for a file system resize

	// prevApplied is status.autoPilot.appliedResources before the last ResourcesScaled of
	// this process (nil: spec.resources), used to revert a change that left a pod
	// unschedulable; prevKnown says whether it was recorded.
	prevApplied *corev1.ResourceRequirements
	prevKnown   bool
	// memRequestCap stops OOM growth of the memory request after a larger one could not be
	// scheduled; zero when unset.
	memRequestCap int64

	unansweredSince time.Time // start of the current run of unanswered health probes

	valkeyMaster string               // the Valkey primary last seen, to notice failovers
	valkeyGroup  map[string]types.UID // the Valkey pods (name -> UID) when valkeyMaster was seen
	sentinels    sentinelAnswers      // the last answers of the Sentinels
}

func (s *instanceState) allow(key string, every time.Duration) bool {
	if t, ok := s.limits[key]; ok && time.Since(t) < every {
		return false
	}
	s.limits[key] = time.Now()
	return true
}

var (
	statesMu sync.Mutex
	states   = map[types.UID]*instanceState{}
)

func stateFor(f *forgejov1.Forgejo) *instanceState {
	statesMu.Lock()
	defer statesMu.Unlock()
	now := time.Now()
	for uid, s := range states {
		if now.Sub(s.touched) > 24*time.Hour {
			delete(states, uid)
		}
	}
	s, ok := states[f.UID]
	if !ok {
		s = &instanceState{
			lastSeen:        map[string]time.Time{},
			limits:          map[string]time.Time{},
			measure:         newMeasureState(),
			resizeSeen:      map[string]time.Time{},
			resizeRestarted: map[string]time.Time{},
		}
		states[f.UID] = s
	}
	s.touched = now
	return s
}

func logger(ctx context.Context) logr.Logger {
	return logf.FromContext(ctx).WithName("autopilot")
}
