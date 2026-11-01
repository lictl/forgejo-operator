package autopilot

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
)

const (
	testCheckPass     = "pass"
	testCheckDatabase = "database"
	testCheckFail     = "fail"
)

const (
	testHash     = "currenthash0123456789"
	testOldHash  = "oldhash0123456789"
	testNewImage = "new"
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

func haInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.HighAvailability.Enabled = true
	return f
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

// A failed upgrade whose unverified image is still deployed holds the workload
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
