package autopilot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const testNewImage = "new"

func owned(t *testing.T, f *forgejov1.Forgejo, obj client.Object) client.Object {
	t.Helper()
	if err := controllerutil.SetControllerReference(f, obj, testScheme(t)); err != nil {
		t.Fatal(err)
	}
	return obj
}

func found(t *testing.T, c client.Client, f *forgejov1.Forgejo, name string, obj client.Object) bool {
	t.Helper()
	err := c.Get(context.Background(), client.ObjectKey{Namespace: f.Namespace, Name: name}, obj)
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

func condition(f *forgejov1.Forgejo, t string) *metav1.Condition {
	return meta.FindStatusCondition(f.Status.Conditions, t)
}

func hasEvent(evs []string, prefix string) bool {
	for _, e := range evs {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// --- HPA (F11, F36, F56, follow-ups 1, 2, 4) and missing requests (F40) ---

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

// --- Valkey cleanup (F02, F38) ---

func valkeyObjects(t *testing.T, f *forgejov1.Forgejo) []client.Object {
	return []client.Object{
		owned(t, f, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}),
		owned(t, f, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ValkeyName(f), Namespace: f.Namespace}}),
	}
}

func lkgSecret(f *forgejov1.Forgejo, redis bool) *corev1.Secret {
	ini := "[cache]\nADAPTER = memory\n"
	if redis {
		ini = "[cache]\nADAPTER = redis\nHOST = redis://:pw@" + forgejo.ValkeyName(f) + "." + f.Namespace + ".svc.cluster.local:6379/0\n"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.LKGConfigSecretName(f), Namespace: f.Namespace,
			Annotations: map[string]string{forgejo.AnnotationConfigHash: testOldHash}},
		Data: map[string][]byte{forgejo.ConfigSecretKey: []byte(ini)},
	}
}

func TestValkeyKeptWhileRedisConfigMayRun(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		rolledBack bool // pods run the LKG
		lkgRedis   bool
		wantKept   bool
	}{
		{"rollback to a redis LKG is active", true, true, true},
		{"desired rolled out but the LKG still uses Valkey", false, true, true},
		{"desired rolled out and recorded as LKG", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash}
			deployed := testHash
			if tc.rolledBack {
				deployed = testOldHash
				f.Status.AutoPilot.RejectedConfigHash = testHash
			}
			dep := testDeployment(f, 1)
			dep.Spec.Template.Annotations[forgejo.AnnotationConfigHash] = deployed
			objs := append(valkeyObjects(t, f), dep, lkgSecret(f, tc.lkgRedis), readyPod(f, "fj-a", deployed, time.Hour))
			env := newTestEnv(t, f, objs...)
			env.rc.ConfigHash = deployed
			pods, _ := serverPods(ctx, env.rc)
			if err := cleanupValkey(ctx, env.rc, stateFor(f), dep, pods); err != nil {
				t.Fatal(err)
			}
			kept := found(t, env.rc.Client, f, forgejo.ValkeyName(f), &appsv1.Deployment{})
			if kept != tc.wantKept {
				t.Fatalf("Valkey kept = %v, want %v (events %q)", kept, tc.wantKept, env.events())
			}
		})
	}
}

// With HA off and Valkey gone, a rejected config must not roll back to an LKG that needs it.
func TestConfigGuardDeclinesValkeyLKGWithoutValkey(t *testing.T) {
	f := testInstance()
	f.Spec.AutoPilot.SelfHealing.Enabled = ptr.To(true)
	f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash, RejectedConfigHash: testHash}
	env := newTestEnv(t, f, lkgSecret(f, true))
	env.rc.ConfigSecretName = forgejo.ConfigSecretName(f)
	if _, err := ReconcileConfigGuard(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	if env.rc.ConfigHash != testHash || env.rc.ConfigSecretName != forgejo.ConfigSecretName(f) {
		t.Fatalf("switched to the LKG: %s %s", env.rc.ConfigSecretName, env.rc.ConfigHash)
	}
	if evs := env.events(); f.Status.AutoPilot.RejectedConfigHash != "" || !hasEvent(evs, "Warning ConfigRollbackUnavailable") ||
		!strings.Contains(strings.Join(evs, "\n"), "needs Valkey") {
		t.Fatalf("rejection kept or no ConfigRollbackUnavailable event for Valkey: %q", evs)
	}
}

// A last known good app.ini is only mounted for an image of the same config profile: one
// rendered before profiles existed (with INTERNAL_TOKEN) still serves Forgejo 15-17, but not
// a Forgejo 18 image, and an 18 one (without the token) would crash-loop 17 and older.
const socketProfile = "internal-socket"

func TestConfigGuardMatchesLKGProfile(t *testing.T) {
	guard := func(image string, lkg *corev1.Secret) (testEnv, *forgejov1.Forgejo) {
		f := testInstance()
		f.Spec.AutoPilot.SelfHealing.Enabled = ptr.To(true)
		f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash, RejectedConfigHash: testHash}
		lkg.Name, lkg.Namespace = forgejo.LKGConfigSecretName(f), f.Namespace
		env := newTestEnv(t, f, lkg)
		env.rc.TargetImage, env.rc.Layout = image, forgejo.LayoutForImage(image)
		env.rc.ConfigSecretName = forgejo.ConfigSecretName(f)
		if _, err := ReconcileConfigGuard(context.Background(), env.rc); err != nil {
			t.Fatal(err)
		}
		return env, f
	}

	env, _ := guard("codeberg.org/forgejo/forgejo:17", lkgSecret(testInstance(), false))
	if env.rc.ConfigSecretName != forgejo.LKGConfigSecretName(testInstance()) || env.rc.ConfigHash != testOldHash {
		t.Fatalf("legacy LKG not used for 17: %s %s %q", env.rc.ConfigSecretName, env.rc.ConfigHash, env.events())
	}

	old := forgejo.Versions
	forgejo.Versions = old.WithVerified(18)
	t.Cleanup(func() { forgejo.Versions = old })
	env, f := guard("codeberg.org/forgejo/forgejo:18", lkgSecret(testInstance(), false))
	if env.rc.ConfigSecretName != forgejo.ConfigSecretName(f) || f.Status.AutoPilot.RejectedConfigHash != "" ||
		!strings.Contains(strings.Join(env.events(), "\n"), "rendered for another Forgejo version") {
		t.Fatalf("legacy LKG used for 18: %s", env.rc.ConfigSecretName)
	}
	socketLKG := lkgSecret(testInstance(), false)
	socketLKG.Annotations[forgejo.AnnotationConfigProfile] = socketProfile
	if env, f := guard("codeberg.org/forgejo/forgejo:17", socketLKG); env.rc.ConfigSecretName != forgejo.ConfigSecretName(f) {
		t.Fatal("an LKG without INTERNAL_TOKEN used for 17")
	}
	if env, _ := guard("codeberg.org/forgejo/forgejo:18", socketLKG); env.rc.ConfigHash != testOldHash {
		t.Fatal("matching LKG not used for 18")
	}
}

func TestLastKnownGoodRecordsConfigProfile(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
	dep := testDeployment(f, 1)
	src := configSecret(f)
	src.Annotations[forgejo.AnnotationConfigProfile] = socketProfile
	env := newTestEnv(t, f, dep, src, readyPod(f, "fj-a", testHash, time.Hour))
	pods, _ := serverPods(context.Background(), env.rc)
	if err := recordLastKnownGood(context.Background(), env.rc, dep, pods, false); err != nil {
		t.Fatal(err)
	}
	lkg := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), lkg) || lkg.Annotations[forgejo.AnnotationConfigProfile] != socketProfile {
		t.Fatalf("LKG profile not recorded: %+v", lkg.Annotations)
	}
}

func TestLastKnownGoodMarksValkeyConfig(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, configSecret(f), readyPod(f, "fj-a", testHash, time.Hour))
	env.rc.ValkeyURL = "redis://:pw@fj-valkey.ns.svc.cluster.local:6379"
	pods, _ := serverPods(context.Background(), env.rc)
	if err := recordLastKnownGood(context.Background(), env.rc, dep, pods, false); err != nil {
		t.Fatal(err)
	}
	lkg := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), lkg) || lkg.Annotations[annotationUsesValkey] != valueTrue {
		t.Fatalf("LKG not marked as using Valkey: %+v", lkg.Annotations)
	}
}

// --- config rollback detection (F42) ---

func TestPodFailureCountsOnlyCurrentFailures(t *testing.T) {
	f := testInstance()
	tenMinAgo := metav1.NewTime(time.Now().Add(-10 * time.Minute))

	// Init container failed during a database restart, then completed; the pod is Ready.
	p := readyPod(f, "fj-a", testHash, time.Minute)
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "bootstrap", RestartCount: 4,
		State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: tenMinAgo}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: tenMinAgo}},
	}}
	if got := podFailure(p, time.Time{}); got != "" {
		t.Fatalf("Ready pod with a completed init container failed: %s", got)
	}

	// Crash loop whose last exit was before the database became ready again.
	q := readyPod(f, "fj-b", testHash, time.Minute)
	q.Status.Conditions[0].Status = corev1.ConditionFalse
	q.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: serverContainerName, RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: tenMinAgo}},
	}
	if got := podFailure(q, time.Now().Add(-5*time.Minute)); got != "" {
		t.Fatalf("restarts from before the database recovered counted: %s", got)
	}
	if got := podFailure(q, time.Now().Add(-20*time.Minute)); got == "" {
		t.Fatal("crash loop after the database recovered not counted")
	}
}

// --- vertical scaling (F39, F41, F61) ---

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

func withNodeCapacity(t *testing.T, maxMem int64) {
	t.Helper()
	old := nodeCapacityFn
	nodeCapacityFn = func(context.Context) (int, int64, error) { return 1, maxMem, nil }
	t.Cleanup(func() { nodeCapacityFn = old })
}

func TestOOMNeverLowersALimitAboveMaxAllowed(t *testing.T) {
	withNodeCapacity(t, 64<<30)
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
	withNodeCapacity(t, 1<<30)
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

// --- storage (F01, F37, F35, F43, F50) ---

func dataPVC(f *forgejov1.Forgejo, request, capacity string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("standard"),
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(request)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}},
	}
}

func mountingPod(f *forgejov1.Forgejo) *corev1.Pod {
	p := readyPod(f, "fj-data", testHash, time.Hour)
	p.Spec.Volumes = []corev1.Volume{{Name: volData, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: forgejo.DataPVCName(f)}}}}
	p.Spec.Containers = []corev1.Container{{Name: serverContainerName, VolumeMounts: []corev1.VolumeMount{{Name: volData, MountPath: "/data"}}}}
	return p
}

func storageInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	f.Spec.AutoPilot.StorageAutoExpand.ThresholdPercent = 80
	return f
}

type fakeExec struct {
	mu    sync.Mutex
	calls map[string]int
	df    func() (string, error)
	du    func() (string, error)
}

func (e *fakeExec) install(t *testing.T) {
	t.Helper()
	e.calls = map[string]int{}
	old, oldWait := execFn, measureWait
	measureWait = 2 * time.Second
	execFn = func(_ context.Context, _, _, _ string, cmd []string) (string, error) {
		e.mu.Lock()
		e.calls[cmd[0]]++
		e.mu.Unlock()
		if cmd[0] == "df" {
			return e.df()
		}
		return e.du()
	}
	t.Cleanup(func() { execFn, measureWait = old, oldWait })
}

func (e *fakeExec) count(cmd string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[cmd]
}

const nodeFSDF = "Filesystem     1024-blocks      Used Available Capacity Mounted on\n/dev/vda1 409600000 100000000 309600000  25% /data\n"

// With auto-expansion off, usage is still measured and reported; nothing is expanded.
func TestStorageMeasuredWithAutoExpandDisabled(t *testing.T) {
	f := storageInstance()
	f.Spec.AutoPilot.StorageAutoExpand.Enabled = ptr.To(false)
	pvc := dataPVC(f, "1Gi", "1Gi")
	env := newTestEnv(t, f, pvc, mountingPod(f))
	(&fakeExec{df: func() (string, error) {
		return "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 943718 104858 90% /data\n", nil
	}}).install(t)
	if err := reconcileStorage(context.Background(), env.rc, stateFor(f), nil); err != nil {
		t.Fatal(err)
	}
	if f.Status.Storage == nil || len(f.Status.Storage.Volumes) != 1 || f.Status.Storage.Volumes[0].UsedPercent != 89 {
		t.Fatalf("status.storage = %+v", f.Status.Storage)
	}
	var m dto.Metric
	if err := forgejo.VolumeUsedRatio.WithLabelValues(f.Namespace, f.Name, pvc.Name).Write(&m); err != nil {
		t.Fatal(err)
	}
	if v := m.GetGauge().GetValue(); v < 0.89 || v > 0.91 {
		t.Fatalf("volume_used_ratio = %v", v)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonAutoExpandDisabled {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	got := &corev1.PersistentVolumeClaim{}
	_ = env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(pvc), got)
	if !got.Spec.Resources.Requests.Storage().Equal(resource.MustParse("1Gi")) {
		t.Fatal("PVC expanded with auto-expansion disabled")
	}
}

// A slow du never holds the reconcile; its result is reported by a later one. Failures are
// backed off instead of retried on every check.
func TestDuRunsInBackgroundAndBacksOff(t *testing.T) {
	f := storageInstance()
	env := newTestEnv(t, f, dataPVC(f, "1Gi", "1Gi"), mountingPod(f))
	release := make(chan struct{})
	var fail atomic.Bool
	fx := &fakeExec{
		df: func() (string, error) { return nodeFSDF, nil },
		du: func() (string, error) {
			<-release
			if fail.Load() {
				return "", errors.New("command terminated with exit code 137")
			}
			return "524288\t/data\n", nil
		},
	}
	fx.install(t)
	st := stateFor(f)
	ctx := context.Background()

	start := time.Now()
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > measureWait+time.Second {
		t.Fatalf("reconcile waited %s for du", d)
	}
	if c := condition(f, forgejov1.ConditionStorageHealthy); c == nil || c.Reason != "MeasurementUnavailable" {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	if env.rc.RequeueAfter == 0 {
		t.Fatal("no requeue while du runs")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for st.measure.anyRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if v := f.Status.Storage.Volumes[0]; v.UsedBytes != 512<<20 || v.UsedPercent != 50 {
		t.Fatalf("du result not reported: %+v", v)
	}

	// du fails: the next attempts back off.
	fail.Store(true)
	st.measure.mu.Lock()
	c := st.measure.claims[forgejo.DataPVCName(f)]
	c.attempt, c.duAttempt = time.Time{}, time.Time{}
	st.measure.mu.Unlock()
	st.lastStorageCheck = time.Time{}
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	for st.measure.anyRunning() && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := fx.count("sh")
	st.lastStorageCheck = time.Time{}
	st.measure.mu.Lock()
	c.attempt = time.Now().Add(-storageCheckInterval - time.Second) // the plain interval has passed
	st.measure.mu.Unlock()
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if fx.count("sh") != calls {
		t.Fatal("du retried right after a failure")
	}
	if v := f.Status.Storage.Volumes[0]; v.UsedBytes != 512<<20 {
		t.Fatalf("last good measurement not kept after a failure: %+v", v)
	}
}

func TestStalledResizeIsReportedAndFSResizeRestarts(t *testing.T) {
	f := storageInstance()
	pvc := dataPVC(f, "2Gi", "1Gi")
	long := metav1.NewTime(time.Now().Add(-40 * time.Minute))
	pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue, LastTransitionTime: long,
		Message: "Waiting for user to (re-)start a pod to finish file system resize of volume on node.",
	}}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, pvc, mountingPod(f), dep)
	(&fakeExec{df: func() (string, error) {
		return "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 943718 104858 90% /data\n", nil
	}}).install(t)
	st := stateFor(f)
	if err := reconcileStorage(context.Background(), env.rc, st, dep); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonExpansionStalled || !strings.Contains(c.Message, "FileSystemResizePending") {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	got := &appsv1.Deployment{}
	_ = env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(dep), got)
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; !ok || lastAction(f, ActionPodRestarted).IsZero() {
		t.Fatal("no restart for a pending file system resize")
	}
	// Once per pending resize.
	f.Status.AutoPilot.RecentActions = nil
	st.lastStorageCheck = time.Time{}
	if err := reconcileStorage(context.Background(), env.rc, st, got); err != nil {
		t.Fatal(err)
	}
	if !lastAction(f, ActionPodRestarted).IsZero() {
		t.Fatal("restarted twice for the same resize")
	}
}

func TestParseDF(t *testing.T) {
	used, size, err := parseDF("Filesystem     1024-blocks    Used Available Capacity Mounted on\nserver:/export with space 2048 1024 1024 50% /data\n")
	if err != nil || used != 1024*1024 || size != 2048*1024 {
		t.Fatalf("used=%d size=%d err=%v", used, size, err)
	}
	if _, _, err := parseDF("df: /data: No such file or directory\n"); err == nil {
		t.Fatal("garbage parsed")
	}
}

// Follow-up 4: a failed upgrade whose unverified image is still deployed holds the workload
// like an upgrade: no restart, no config rejection, no vertical change.
func TestFailedUpgradeHoldStopsAutonomousChanges(t *testing.T) {
	now := metav1.Now()
	crashPod := func(f *forgejov1.Forgejo) *corev1.Pod {
		p := readyPod(f, "fj-crash", testHash, time.Minute)
		p.Status.Conditions[0].Status = corev1.ConditionFalse
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name: "bootstrap", RestartCount: 5,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: now}},
		}}
		return p
	}
	for name, pod := range map[string]func(f *forgejov1.Forgejo) *corev1.Pod{
		"health restart":  func(f *forgejov1.Forgejo) *corev1.Pod { return readyPod(f, "fj-live", testHash, time.Hour) },
		"config rollback": crashPod,
		"vertical OOM":    func(f *forgejov1.Forgejo) *corev1.Pod { return oomPod(f, f.Spec.Resources) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := verticalInstance()
			f.Status.Image = "old"
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseFailed, RolloutStartTime: &now, ToImage: testNewImage, FailedImage: testNewImage}
			f.Status.Health = &forgejov1.HealthStatus{ConsecutiveFailures: 9, Checks: map[string]string{testCheckDatabase: testCheckFail}}
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash}
			meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionDatabaseReady, Status: metav1.ConditionTrue,
				Reason: "OK", LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour))})
			dep := testDeployment(f, 1)
			env := newTestEnv(t, f, dep, lkgSecret(f, false), pod(f))
			if !workloadHeld(env.rc) {
				t.Fatal("fixture does not hold the workload")
			}
			if _, err := Reconcile(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			got := &appsv1.Deployment{}
			_ = env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got)
			if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok {
				t.Fatal("restarted during the hold")
			}
			if ap := f.Status.AutoPilot; ap.RejectedConfigHash != "" || ap.AppliedResources != nil || len(ap.RecentActions) != 0 {
				t.Fatalf("autonomous change during the hold: %+v", ap)
			}
		})
	}
}

// --- object storage (spec.objectStorage) ---

// Forgejo does not start without its bucket: a crash loop while the bucket is unusable (e.g.
// rotated credentials that are wrong, which are not in app.ini) is not blamed on app.ini, and
// pods are not replaced meanwhile.
func TestConfigRollbackWaitsForObjectStorage(t *testing.T) {
	ctx := context.Background()
	hourAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	for _, tc := range []struct {
		name        string
		ready       metav1.ConditionStatus
		since       metav1.Time
		unavailable bool
		wantRollbk  bool
	}{
		{"bucket unusable", metav1.ConditionFalse, hourAgo, true, false},
		{"bucket usable again after the crashes", metav1.ConditionTrue, metav1.Now(), false, false},
		{"bucket usable all along", metav1.ConditionTrue, hourAgo, false, true},
		// Only the operator cannot reach the bucket (UnreachableFromOperator): nothing is held.
		{"bucket unreachable from the operator", metav1.ConditionFalse, metav1.Now(), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Spec.AutoPilot.SelfHealing.Enabled = ptr.To(true)
			f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{S3: forgejov1.S3Connection{Endpoint: "http://s3:9000", Bucket: "forgejo"}}
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash}
			f.Status.Conditions = []metav1.Condition{
				{Type: forgejov1.ConditionDatabaseReady, Status: metav1.ConditionTrue, Reason: "ClusterHealthy", LastTransitionTime: hourAgo},
				{Type: forgejov1.ConditionObjectStorageReady, Status: tc.ready, Reason: "X", LastTransitionTime: tc.since},
			}
			p := readyPod(f, "fj-a", testHash, time.Minute)
			p.Status.Conditions[0].Status = corev1.ConditionFalse
			p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
				Name: serverContainerName, RestartCount: 3,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1,
					FinishedAt: metav1.NewTime(time.Now().Add(-30 * time.Second))}},
			}
			env := newTestEnv(t, f, lkgSecret(f, false), p)
			env.rc.ObjectStorageUnavailable = tc.unavailable
			if replacementsHeld(env.rc) != tc.unavailable {
				t.Fatalf("replacementsHeld = %v", !tc.unavailable)
			}
			pods, _ := serverPods(ctx, env.rc)
			if err := rollbackFailingConfig(ctx, env.rc, stateFor(f), pods); err != nil {
				t.Fatal(err)
			}
			if got := f.Status.AutoPilot.RejectedConfigHash == testHash; got != tc.wantRollbk {
				t.Fatalf("rolled back = %v, want %v (events %q)", got, tc.wantRollbk, env.events())
			}
		})
	}
}
