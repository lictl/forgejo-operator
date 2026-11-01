package autopilot

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

func verticalInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.Autoscaling.Vertical = &forgejov1.VerticalAutoscalingSpec{Enabled: true}
	f.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi"), corev1.ResourceCPU: resource.MustParse("100m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
	}
	return f
}

func memRes(req, lim string) *corev1.ResourceRequirements {
	return &corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(req), corev1.ResourceCPU: resource.MustParse("100m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(lim)},
	}
}

func TestPlanVerticalRebasesOnSpecChange(t *testing.T) {
	ctx := context.Background()
	f := verticalInstance()
	f.Status.AutoPilot = &forgejov1.AutoPilotStatus{AppliedResources: memRes("384Mi", "384Mi")}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep)

	// Applied resources from before the annotation existed are adopted, not dropped.
	planVertical(ctx, env.rc)
	if env.rc.ResourcesOverride == nil || f.Status.AutoPilot.AppliedResources == nil {
		t.Fatal("applied resources dropped without a recorded base")
	}
	if err := recordVerticalBase(ctx, env.rc, dep, false); err != nil {
		t.Fatal(err)
	}
	if dep.Annotations[annotationVerticalBase] != specResourcesHash(f) {
		t.Fatal("base not recorded")
	}

	// spec.resources changes: the spec takes effect.
	f.Spec.Resources.Requests[corev1.ResourceCPU] = resource.MustParse("500m")
	env.rc.ResourcesOverride = nil
	planVertical(ctx, env.rc)
	if env.rc.ResourcesOverride != nil || f.Status.AutoPilot.AppliedResources != nil {
		t.Fatal("applied resources kept after spec.resources changed")
	}
	if !hasEvent(env.events(), "Normal VerticalRebased") {
		t.Fatal("no VerticalRebased event")
	}
}

func TestPlanVerticalClampsToBounds(t *testing.T) {
	f := verticalInstance()
	f.Spec.Autoscaling.Vertical.MaxAllowed = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	f.Status.AutoPilot = &forgejov1.AutoPilotStatus{AppliedResources: memRes("2Gi", "3Gi")}
	env := newTestEnv(t, f)
	planVertical(context.Background(), env.rc)
	got := env.rc.ResourcesOverride
	if got == nil || !got.Limits.Memory().Equal(resource.MustParse("1Gi")) || !got.Requests.Memory().Equal(resource.MustParse("1Gi")) {
		t.Fatalf("not clamped to maxAllowed: %s", describeResources(got))
	}
}

func oomPod(f *forgejov1.Forgejo, res corev1.ResourceRequirements) *corev1.Pod {
	p := readyPod(f, "fj-oom", testHash, time.Minute)
	p.Spec.Containers = []corev1.Container{{Name: serverContainerName, Resources: res}}
	p.Status.ContainerStatuses[0].RestartCount = 1
	p.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Reason: reasonOOMKilled, ExitCode: 137, FinishedAt: metav1.NewTime(time.Now().Add(-time.Second)),
	}}
	return p
}

func TestOOMNeverLowersALimitAboveMaxAllowed(t *testing.T) {
	stubNodeCapacity(t, 1, 64<<30)
	f := verticalInstance()
	f.Spec.Resources = *memRes("4Gi", "4Gi")
	f.Spec.Autoscaling.Vertical.MaxAllowed = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}
	env := newTestEnv(t, f)
	pods := []corev1.Pod{*oomPod(f, f.Spec.Resources)}
	seen, scaled := handleOOM(context.Background(), env.rc, stateFor(f), pods)
	if !seen || scaled || (f.Status.AutoPilot != nil && f.Status.AutoPilot.AppliedResources != nil) {
		t.Fatalf("seen=%v scaled=%v applied=%+v", seen, scaled, f.Status.AutoPilot)
	}
	if !hasEvent(env.events(), "Warning OOMAtMaxAllowed") {
		t.Fatal("no OOMAtMaxAllowed warning")
	}
}

func TestOOMGrowthCappedByNodeCapacity(t *testing.T) {
	stubNodeCapacity(t, 1, 1<<30)
	f := verticalInstance()
	f.Status.AutoPilot = &forgejov1.AutoPilotStatus{AppliedResources: memRes("800Mi", "800Mi")}
	env := newTestEnv(t, f)
	env.rc.ResourcesOverride = f.Status.AutoPilot.AppliedResources.DeepCopy()
	pods := []corev1.Pod{*oomPod(f, *f.Status.AutoPilot.AppliedResources)}
	if _, scaled := handleOOM(context.Background(), env.rc, stateFor(f), pods); !scaled {
		t.Fatal("not scaled")
	}
	got := f.Status.AutoPilot.AppliedResources
	if !got.Limits.Memory().Equal(resource.MustParse("1Gi")) || got.Requests.Memory().Value() != (1<<30)*90/100&^(1<<20-1) {
		t.Fatalf("growth not capped by node capacity: %s", describeResources(got))
	}
}

func TestUnschedulableResourcesAreReverted(t *testing.T) {
	f := verticalInstance()
	f.Status.AutoPilot = &forgejov1.AutoPilotStatus{
		AppliedResources: memRes("8Gi", "8Gi"),
		RecentActions:    []forgejov1.AutoPilotAction{{Type: ActionResourcesScaled, Time: metav1.NewTime(time.Now().Add(-5 * time.Minute))}},
	}
	env := newTestEnv(t, f)
	env.rc.ResourcesOverride = f.Status.AutoPilot.AppliedResources.DeepCopy()
	st := stateFor(f)
	st.prevApplied, st.prevKnown = memRes("512Mi", "512Mi"), true
	p := readyPod(f, "fj-pending", testHash, 0)
	p.Spec.Containers = []corev1.Container{{Name: serverContainerName, Resources: *memRes("8Gi", "8Gi")}}
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
		Message: "0/1 nodes are available: 1 Insufficient memory.", LastTransitionTime: metav1.NewTime(time.Now().Add(-3 * time.Minute)),
	}}}
	if !revertUnschedulable(env.rc, st, []corev1.Pod{*p}) {
		t.Fatal("not reverted")
	}
	if got := f.Status.AutoPilot.AppliedResources; got == nil || !got.Requests.Memory().Equal(resource.MustParse("512Mi")) {
		t.Fatalf("applied = %s, want the previous 512Mi", describeResources(got))
	}
	if st.memRequestCap != 512<<20 {
		t.Fatalf("memRequestCap = %d", st.memRequestCap)
	}
}

func TestRightSizingLeavesHPAResourcesAlone(t *testing.T) {
	f := verticalInstance()
	f.Spec.HighAvailability.Enabled = true
	f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: 2, MaxReplicas: 4}
	env := newTestEnv(t, f)
	st := stateFor(f)
	for range minSamples {
		st.samples = append(st.samples, usageSample{at: time.Now(), cpu: 900, memory: 600 << 20})
	}
	if !applyRecommendation(env.rc, st, false) {
		t.Fatal("memory not right-sized")
	}
	ap := f.Status.AutoPilot
	if !ap.AppliedResources.Requests.Cpu().Equal(resource.MustParse("100m")) {
		t.Fatalf("CPU request changed under the HPA: %s", describeResources(ap.AppliedResources))
	}
	if ap.AppliedResources.Requests.Memory().Cmp(resource.MustParse("800Mi")) < 0 {
		t.Fatalf("memory not raised: %s", describeResources(ap.AppliedResources))
	}
	if ap.RecommendedResources.Requests.Cpu().MilliValue() != 900*100/defaultTargetUtil {
		t.Fatalf("recommendation not published: %s", describeResources(ap.RecommendedResources))
	}
	if !hasEvent(env.events(), "Normal VerticalLimitedByHPA") {
		t.Fatal("no VerticalLimitedByHPA event")
	}
}
