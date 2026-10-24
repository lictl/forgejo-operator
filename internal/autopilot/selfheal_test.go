package autopilot

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/observability"
)

const (
	testCheckPass     = "pass"
	testCheckDatabase = "database"
	testCheckFail     = "fail"
)

const (
	testHash    = "currenthash0123456789"
	testOldHash = "oldhash0123456789"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testInstance() *forgejov1.Forgejo {
	return &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: types.UID("uid-" + time.Now().Format("150405.000000000"))},
	}
}

// testDeployment is fully rolled out with the given config hash.
func testDeployment(f *forgejov1.Forgejo, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: f.Namespace, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash},
			}},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas,
			ReadyReplicas: replicas, AvailableReplicas: replicas,
		},
	}
}

func readyPod(f *forgejov1.Forgejo, name, hash string, since time.Duration) *corev1.Pod {
	start := metav1.NewTime(time.Now().Add(-since))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: f.Namespace, UID: types.UID(name),
			Labels:      forgejo.SelectorLabels(f),
			Annotations: map[string]string{forgejo.AnnotationConfigHash: hash},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: start}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: serverContainerName, Ready: true, Started: ptr.To(true),
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: start}},
			}},
		},
	}
}

// evictedPod is a pod the kubelet evicted under node pressure: phase Failed, never deleted.
func evictedPod(f *forgejov1.Forgejo, name, hash string) *corev1.Pod {
	ended := metav1.NewTime(time.Now().Add(-time.Hour))
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: f.Namespace, UID: types.UID(name),
			Labels:      forgejo.SelectorLabels(f),
			Annotations: map[string]string{forgejo.AnnotationConfigHash: hash},
		},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "Evicted",
			Message: "The node was low on resource: ephemeral-storage.",
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "PodFailed", LastTransitionTime: ended,
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: serverContainerName, Started: ptr.To(false),
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137, Reason: "ContainerStatusUnknown", FinishedAt: ended,
				}},
			}},
		},
	}
}

func configSecret(f *forgejov1.Forgejo) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: forgejo.ConfigSecretName(f), Namespace: f.Namespace,
			Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash},
		},
		Data: map[string][]byte{"app.ini": []byte("APP_NAME = test\n")},
	}
}

type testEnv struct {
	rc       *forgejo.ReconcileContext
	recorder *events.FakeRecorder
}

func newTestEnv(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) testEnv {
	t.Helper()
	stubNodeZones(t, false)
	if !nodeCapacityStubbed {
		stubNodeCapacity(t, 1, 0)
	}
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rec := events.NewFakeRecorder(100)
	return testEnv{
		rc: &forgejo.ReconcileContext{
			Client: c, Scheme: s, Recorder: rec, Instance: f,
			DatabaseReady: true, ConfigHash: testHash, DesiredConfigHash: testHash,
			TargetImage: forgejov1.DefaultImage, Layout: forgejo.LayoutForImage(forgejov1.DefaultImage),
		},
		recorder: rec,
	}
}

func (e testEnv) events() []string {
	var out []string
	for {
		select {
		case ev := <-e.recorder.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestServerPodsSkipsTerminalPods(t *testing.T) {
	f := testInstance()
	done := readyPod(f, "fj-done", testHash, time.Hour)
	done.Status.Phase = corev1.PodSucceeded
	env := newTestEnv(t, f,
		readyPod(f, "fj-live", testHash, time.Hour),
		evictedPod(f, "fj-evicted", testOldHash),
		done,
	)
	pods, err := serverPods(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Name != "fj-live" {
		names := make([]string, 0, len(pods))
		for _, p := range pods {
			names = append(names, p.Name)
		}
		t.Fatalf("serverPods = %v, want [fj-live]", names)
	}
}

// An evicted pod left behind in phase Failed must neither block LKG bookkeeping nor the
// instance-level restart.
func TestEvictedPodDoesNotBlockLastKnownGood(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true, Checks: map[string]string{testCheckDatabase: testCheckPass}}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, configSecret(f),
		readyPod(f, "fj-live", testHash, 2*lkgSoak),
		evictedPod(f, "fj-evicted", testOldHash),
	)
	ctx := context.Background()
	pods, err := serverPods(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if !allRunning(pods) {
		t.Fatal("allRunning = false with only a live pod besides the evicted one")
	}
	if err := recordLastKnownGood(ctx, env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	if ap := f.Status.AutoPilot; ap == nil || ap.LastKnownGoodConfigHash != testHash {
		t.Fatalf("LKG not recorded: %+v", f.Status.AutoPilot)
	}
	lkg := &corev1.Secret{}
	if err := env.rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.LKGConfigSecretName(f)}, lkg); err != nil {
		t.Fatalf("LKG secret: %v", err)
	}
	if lkg.Annotations[forgejo.AnnotationConfigHash] != testHash {
		t.Fatalf("LKG secret hash = %q", lkg.Annotations[forgejo.AnnotationConfigHash])
	}
}

// Fewer listed pods than desired replicas (cache lag) must not pass the soak check vacuously.
func TestLastKnownGoodNeedsEveryReplicaListed(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true, Checks: map[string]string{testCheckDatabase: testCheckPass}}
	dep := testDeployment(f, 2)
	env := newTestEnv(t, f, dep, configSecret(f), readyPod(f, "fj-a", testHash, 2*lkgSoak))
	pods, err := serverPods(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLastKnownGood(context.Background(), env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	if ap := f.Status.AutoPilot; ap != nil && ap.LastKnownGoodConfigHash != "" {
		t.Fatalf("LKG recorded with 1 of 2 pods listed: %q", ap.LastKnownGoodConfigHash)
	}
}

// unreachableHealth is status.health after probes that got no answer, as the health step
// reports them: either as Unreachable (streak reset) or, in older versions, as failures
// without Forgejo's checks.
func unreachableHealth(f *forgejov1.Forgejo, asFailures bool) {
	now := metav1.Now()
	msg := `GET /api/healthz: Get "http://fj-http.ns.svc.cluster.local:3000/api/healthz": ` +
		`dial tcp 10.0.0.1:3000: connect: connection refused`
	f.Status.Health = &forgejov1.HealthStatus{LastCheckTime: &now, Message: msg}
	if asFailures {
		f.Status.Health.ConsecutiveFailures = 12
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
			Type: forgejov1.ConditionHealthy, Status: metav1.ConditionFalse, Reason: "HealthCheckFailed", Message: msg,
		})
		return
	}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
		Type: forgejov1.ConditionHealthy, Status: metav1.ConditionUnknown, Reason: healthReasonUnreachable, Message: msg,
	})
}

// A probe that Forgejo accepted and then left hanging (or answered with an error page) has no
// checks but is a real failure: the health step counts it, and the instance-level restart
// must not mistake it for an unreachable Forgejo.
func TestHungProbeWithoutChecksRestarts(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, true)
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, readyPod(f, "fj-live", testHash, time.Hour))
	ctx := context.Background()
	if probeUnanswered(f) {
		t.Fatal("counted failures without checks classified as unanswered")
	}
	pods, _ := serverPods(ctx, env.rc)
	if err := restartUnhealthy(ctx, env.rc, dep, pods); err != nil {
		t.Fatal(err)
	}
	got := &appsv1.Deployment{}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; !ok {
		t.Fatal("no restart for a Forgejo that accepted the probes and hung")
	}
}

// The operator cannot reach the Service (NetworkPolicy, mesh) while the kubelet's readiness
// probe on the same endpoint passes: no restart, Degraded explains it, LKG is still recorded.
func TestUnreachableProbeDoesNotRestart(t *testing.T) {
	for _, tc := range []struct {
		name       string
		asFailures bool
	}{{"reported as Unreachable", false}} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			unreachableHealth(f, tc.asFailures)
			dep := testDeployment(f, 1)
			env := newTestEnv(t, f, dep, configSecret(f), readyPod(f, "fj-live", testHash, time.Hour))
			ctx := context.Background()

			// The first reconcile only starts the clock.
			if _, err := Reconcile(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c != nil && c.Status == metav1.ConditionTrue {
				t.Fatalf("Degraded set before %s: %+v", unreachableAfter, c)
			}
			stateFor(f).unansweredSince = time.Now().Add(-2 * unreachableAfter)
			if _, err := Reconcile(ctx, env.rc); err != nil {
				t.Fatal(err)
			}

			got := &appsv1.Deployment{}
			if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
				t.Fatal(err)
			}
			if v, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok {
				t.Fatalf("Deployment restarted (restarted-at=%s) on probes Forgejo never answered", v)
			}
			if !lastAction(f, ActionPodRestarted).IsZero() {
				t.Fatalf("PodRestarted recorded: %+v", f.Status.AutoPilot.RecentActions)
			}
			c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded)
			if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonHealthProbeUnreachable {
				t.Fatalf("Degraded = %+v, want True/%s", c, ReasonHealthProbeUnreachable)
			}
			if ap := f.Status.AutoPilot; ap == nil || ap.LastKnownGoodConfigHash != testHash {
				t.Fatalf("LKG not recorded from pod readiness: %+v", f.Status.AutoPilot)
			}
			evs := env.events()
			var warned bool
			for _, e := range evs {
				if strings.HasPrefix(e, "Warning "+ReasonHealthProbeUnreachable) {
					warned = true
				}
			}
			if !warned {
				t.Fatalf("no %s warning event, got %q", ReasonHealthProbeUnreachable, evs)
			}

			// The warning is rate limited.
			if _, err := Reconcile(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			for _, e := range env.events() {
				if strings.HasPrefix(e, "Warning "+ReasonHealthProbeUnreachable) {
					t.Fatalf("warning repeated within %s: %q", warnInterval, e)
				}
			}

			// Once the probe gets through again, Degraded is cleared.
			now := metav1.Now()
			f.Status.Health = &forgejov1.HealthStatus{Healthy: true, LastCheckTime: &now, LastHealthyTime: &now, Checks: map[string]string{testCheckDatabase: testCheckPass}}
			meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
				Type: forgejov1.ConditionHealthy, Status: metav1.ConditionTrue, Reason: "HealthCheckPassed",
			})
			if _, err := Reconcile(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
				t.Fatalf("Degraded = %+v, want False", c)
			}
			if !stateFor(f).unansweredSince.IsZero() {
				t.Fatal("unanswered clock not reset after a healthy probe")
			}
		})
	}
}

// Degraded set by another feature is not overwritten.
func TestUnreachableKeepsOtherDegradedReason(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, false)
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
		Type: forgejov1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: ReasonConfigRolledBack, Message: "rolled back",
	})
	env := newTestEnv(t, f, readyPod(f, "fj-live", testHash, time.Hour))
	pods, _ := serverPods(context.Background(), env.rc)
	reportProbeUnreachable(env.rc, stateFor(f), pods, true)
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded)
	if c.Reason != ReasonConfigRolledBack {
		t.Fatalf("Degraded reason = %s, want %s", c.Reason, ReasonConfigRolledBack)
	}
}

// A healthz that Forgejo itself answers with failing checks, while the pod stays Ready, still
// triggers the instance-level restart.
func TestAnsweredFailuresRestart(t *testing.T) {
	f := testInstance()
	now := metav1.Now()
	f.Status.Health = &forgejov1.HealthStatus{
		LastCheckTime: &now, ConsecutiveFailures: 5,
		Message: "status fail: database=fail", Checks: map[string]string{"cache": testCheckPass, testCheckDatabase: testCheckFail},
	}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, readyPod(f, "fj-live", testHash, time.Hour))
	ctx := context.Background()
	pods, _ := serverPods(ctx, env.rc)
	if probeUnanswered(f) {
		t.Fatal("answered failures classified as unanswered")
	}
	got := &appsv1.Deployment{}

	// While the Secret of SSH host keys is unusable, a restarted pod could not start.
	env.rc.SSHHostKeysUnavailable = true
	if err := restartUnhealthy(ctx, env.rc, dep, pods); err != nil {
		t.Fatal(err)
	}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok {
		t.Fatal("restarted while new pods could not start")
	}

	env.rc.SSHHostKeysUnavailable = false
	if err := restartUnhealthy(ctx, env.rc, dep, pods); err != nil {
		t.Fatal(err)
	}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; !ok {
		t.Fatal("no restart for failures Forgejo reported itself")
	}
}

// The unanswered clock only runs while a pod is Ready: a Forgejo that really stopped answering
// goes NotReady through the kubelet's probe on the same endpoint, which is not "unreachable".
func TestUnreachableNeedsReadyPods(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, false)
	st := stateFor(f)
	pods := []corev1.Pod{*readyPod(f, "fj-live", testHash, time.Hour)}
	if trackUnreachable(st, f, pods) {
		t.Fatal("unreachable right after the first unanswered probe")
	}
	st.unansweredSince = time.Now().Add(-2 * unreachableAfter)
	if !trackUnreachable(st, f, pods) {
		t.Fatalf("not unreachable after %s", 2*unreachableAfter)
	}
	pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
	if trackUnreachable(st, f, pods) || !st.unansweredSince.IsZero() {
		t.Fatal("unreachable without a Ready pod")
	}
	// Failures Forgejo answered itself are not unanswered.
	f.Status.Conditions = nil
	f.Status.Health = &forgejov1.HealthStatus{ConsecutiveFailures: 9, Checks: map[string]string{testCheckDatabase: testCheckFail}}
	if probeUnanswered(f) {
		t.Fatal("answered failures classified as unanswered")
	}
}

// healthReasonUnreachable must track the reason the health step sets.
func TestHealthReasonUnreachableMatchesObservability(t *testing.T) {
	if healthReasonUnreachable != observability.ReasonUnreachable {
		t.Fatalf("healthReasonUnreachable = %q, observability.ReasonUnreachable = %q", healthReasonUnreachable, observability.ReasonUnreachable)
	}
}

// nodeCapacityStubbed is set while a test has replaced nodeCapacityFn.
var nodeCapacityStubbed bool

// stubNodeCapacity keeps tests away from the cluster of the developer's kubeconfig.
func stubNodeCapacity(t *testing.T, nodes int, maxMem int64) {
	t.Helper()
	old, oldStubbed := nodeCapacityFn, nodeCapacityStubbed
	nodeCapacityFn = func(context.Context) (int, int64, error) { return nodes, maxMem, nil }
	nodeCapacityStubbed = true
	t.Cleanup(func() { nodeCapacityFn, nodeCapacityStubbed = old, oldStubbed })
}

// stubNodeZones keeps tests away from the cluster of the developer's kubeconfig.
func stubNodeZones(t *testing.T, zoned bool) {
	t.Helper()
	old := nodeZonesFn
	nodeZonesFn = func(context.Context) (bool, error) { return zoned, nil }
	t.Cleanup(func() { nodeZonesFn = old })
}
