package autopilot

import (
	"context"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// --- HPA and missing requests ---

func TestHPAWaitsForFirstRolloutAndMigrations(t *testing.T) {
	f := testInstance()
	f.Spec.HighAvailability.Enabled = true
	f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 4, TargetCPUUtilizationPercentage: 75}
	f.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	env := newTestEnv(t, f)
	ctx := context.Background()
	c := env.rc.Client

	if _, err := ReconcileTuning(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if env.rc.HPAManaged {
		t.Fatal("HPAManaged before the first rollout")
	}
	if _, err := ReconcileScaling(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if found(t, c, f, forgejo.HPAName(f), &autoscalingv2.HorizontalPodAutoscaler{}) {
		t.Fatal("HPA created before the first rollout")
	}
	if found(t, c, f, forgejo.PDBName(f), &policyv1.PodDisruptionBudget{}) || minPods(env.rc) != 1 {
		t.Fatalf("PDB over the single bootstrap pod (minPods %d)", minPods(env.rc))
	}

	f.Status.Image = "codeberg.org/forgejo/forgejo:11"
	if _, err := ReconcileScaling(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if !found(t, c, f, forgejo.HPAName(f), hpa) || ptr.Deref(hpa.Spec.MinReplicas, 0) != forgejo.HPAMinReplicas(f) {
		t.Fatalf("HPA after the first rollout: %+v", hpa.Spec)
	}
	if !found(t, c, f, forgejo.PDBName(f), &policyv1.PodDisruptionBudget{}) {
		t.Fatal("no PDB with an HPA minimum of 2")
	}

	now := metav1.Now()
	holds := map[string]func(rc *forgejo.ReconcileContext){
		"restore migrating": func(rc *forgejo.ReconcileContext) { rc.RestoreMigrating = true },
		"upgrading":         func(rc *forgejo.ReconcileContext) { rc.Upgrading = true },
		"failed upgrade holds the workload": func(rc *forgejo.ReconcileContext) {
			rc.Instance.Status.Upgrade = &forgejov1.UpgradeStatus{
				Phase: forgejov1.UpgradePhaseFailed, RolloutStartTime: &now, ToImage: testNewImage, FailedImage: testNewImage,
			}
		},
	}
	for name, hold := range holds {
		t.Run(name, func(t *testing.T) {
			env.rc.RestoreMigrating, env.rc.Upgrading, f.Status.Upgrade = false, false, nil
			if _, err := ReconcileScaling(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			if !found(t, c, f, forgejo.HPAName(f), &autoscalingv2.HorizontalPodAutoscaler{}) {
				t.Fatal("HPA not re-created")
			}
			hold(env.rc)
			if _, err := ReconcileScaling(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			if env.rc.HPAManaged || found(t, c, f, forgejo.HPAName(f), &autoscalingv2.HorizontalPodAutoscaler{}) {
				t.Fatal("HPA kept while a migration holds the workload")
			}
			if minPods(env.rc) != 1 || found(t, c, f, forgejo.PDBName(f), &policyv1.PodDisruptionBudget{}) {
				t.Fatal("PDB kept while a migration holds the workload")
			}
		})
	}
}

func TestHPAMissingCPURequestIsReported(t *testing.T) {
	f := testInstance()
	f.Status.Image = "img"
	f.Spec.HighAvailability.Enabled = true
	f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 4}
	env := newTestEnv(t, f)
	ctx := context.Background()
	if _, err := ReconcileScaling(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonHPAMissingResourceRequests {
		t.Fatalf("Degraded = %+v, want True/%s", c, ReasonHPAMissingResourceRequests)
	}
	if !hasEvent(env.events(), "Warning "+ReasonHPAMissingResourceRequests) {
		t.Fatal("no warning event")
	}
	// A CPU limit is copied into the request by the API server.
	f.Spec.Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	if _, err := ReconcileScaling(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded = %+v, want False", c)
	}
}
