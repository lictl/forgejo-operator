package autopilot

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

const scaleDownStabilization = 300

// hpaWanted reports whether an HPA should own the replica count. Upgrades (single replica,
// Recreate) and restores (scaled to zero) take the replica count back from the HPA.
func hpaWanted(rc *forgejo.ReconcileContext) bool {
	return forgejo.HorizontalAutoscalingEnabled(rc.Instance) && !rc.Upgrading && !rc.Maintenance
}

// ReconcileScaling ensures the HPA and PodDisruptionBudget.
func ReconcileScaling(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()

	if h := f.Spec.Autoscaling.Horizontal; h != nil && h.Enabled && !f.Spec.HighAvailability.Enabled {
		msg := "autoscaling.horizontal requires highAvailability.enabled (shared cache, sessions and queues); the HPA is not created"
		setDegraded(rc, ReasonHorizontalScalingRequiresHA, msg)
		warnEvery(rc, st, "hpa-requires-ha", time.Hour, ReasonHorizontalScalingRequiresHA, "%s", msg)
	} else {
		clearDegraded(rc, ReasonHorizontalScalingRequiresHA)
	}

	key := client.ObjectKey{Namespace: f.Namespace, Name: forgejo.HPAName(f)}
	rc.HPAManaged = hpaWanted(rc)
	if rc.HPAManaged {
		if err := ensureHPA(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	} else if _, err := deleteOwned(ctx, rc, key, &autoscalingv2.HorizontalPodAutoscaler{}); err != nil {
		return forgejo.StepResult{}, err
	}

	pdbKey := client.ObjectKey{Namespace: f.Namespace, Name: forgejo.PDBName(f)}
	if minPods(rc) > 1 {
		if err := ensurePDB(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	} else if _, err := deleteOwned(ctx, rc, pdbKey, &policyv1.PodDisruptionBudget{}); err != nil {
		return forgejo.StepResult{}, err
	}
	return forgejo.StepResult{}, nil
}

// minPods is the replica count the workload never goes below in the current state.
func minPods(rc *forgejo.ReconcileContext) int32 {
	f := rc.Instance
	switch {
	case rc.Maintenance:
		return 0
	case rc.Upgrading:
		return 1
	case rc.HPAManaged:
		h := f.Spec.Autoscaling.Horizontal
		return min(max(h.MinReplicas, 1), max(h.MaxReplicas, 1))
	default:
		return forgejo.DesiredReplicas(f)
	}
}

func ensureHPA(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	h := f.Spec.Autoscaling.Horizontal
	maxR := max(h.MaxReplicas, 1)
	minR := min(max(h.MinReplicas, 1), maxR)
	cpuTarget := h.TargetCPUUtilizationPercentage
	if cpuTarget <= 0 {
		cpuTarget = 75
	}
	metrics := []autoscalingv2.MetricSpec{resourceMetric(corev1.ResourceCPU, cpuTarget)}
	if m := h.TargetMemoryUtilizationPercentage; m != nil && *m > 0 {
		metrics = append(metrics, resourceMetric(corev1.ResourceMemory, *m))
	}

	hpa := &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: forgejo.HPAName(f), Namespace: f.Namespace}}
	op, err := rc.CreateOrUpdate(ctx, hpa, true, func() error {
		hpa.Labels = forgejo.Labels(f, forgejo.ComponentServer)
		hpa.Spec.ScaleTargetRef = autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: forgejo.DeploymentName(f)}
		hpa.Spec.MinReplicas = ptr.To(minR)
		hpa.Spec.MaxReplicas = maxR
		hpa.Spec.Metrics = metrics
		// Only the scale-down window is ours; the API server defaults the rest of behavior.
		if hpa.Spec.Behavior == nil {
			hpa.Spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
		}
		if hpa.Spec.Behavior.ScaleDown == nil {
			hpa.Spec.Behavior.ScaleDown = &autoscalingv2.HPAScalingRules{}
		}
		hpa.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = ptr.To[int32](scaleDownStabilization)
		return nil
	})
	if err == nil && op != controllerutil.OperationResultNone {
		rc.Event(corev1.EventTypeNormal, "HPAConfigured", "Scaling", "HorizontalPodAutoscaler %s: %d-%d replicas, CPU target %d%%", hpa.Name, minR, maxR, cpuTarget)
	}
	return err
}

func resourceMetric(name corev1.ResourceName, utilization int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name:   name,
			Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: ptr.To(utilization)},
		},
	}
}

func ensurePDB(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: forgejo.PDBName(f), Namespace: f.Namespace}}
	_, err := rc.CreateOrUpdate(ctx, pdb, true, func() error {
		pdb.Labels = forgejo.Labels(f, forgejo.ComponentServer)
		pdb.Spec.MinAvailable = ptr.To(intstr.FromInt32(1))
		pdb.Spec.MaxUnavailable = nil
		pdb.Spec.Selector = &metav1.LabelSelector{MatchLabels: forgejo.SelectorLabels(f)}
		// Crash-looping pods must not block node drains.
		pdb.Spec.UnhealthyPodEvictionPolicy = ptr.To(policyv1.AlwaysAllow)
		return nil
	})
	return err
}

func describeResources(r *corev1.ResourceRequirements) string {
	if r == nil {
		return "{}"
	}
	q := func(l corev1.ResourceList, n corev1.ResourceName) string {
		if v, ok := l[n]; ok {
			return v.String()
		}
		return "-"
	}
	return fmt.Sprintf("requests cpu=%s memory=%s, limits cpu=%s memory=%s",
		q(r.Requests, corev1.ResourceCPU), q(r.Requests, corev1.ResourceMemory),
		q(r.Limits, corev1.ResourceCPU), q(r.Limits, corev1.ResourceMemory))
}
