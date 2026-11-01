package core

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const testReasonReplicaSetUpdated = "ReplicaSetUpdated"

// statusDeployment is the instance's Deployment of image with st as its status, which has
// observed the current generation.
func statusDeployment(f *forgejov1.Forgejo, image string, replicas int32, st appsv1.DeploymentStatus) *appsv1.Deployment {
	st.ObservedGeneration = 10
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: f.Namespace, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: image}}}},
		},
		Status: st,
	}
}

// updateDeployment writes dep's spec, then st as its status.
func updateDeployment(t *testing.T, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, st appsv1.DeploymentStatus) {
	t.Helper()
	ctx := context.Background()
	if err := rc.Client.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	dep.Status = st
	if err := rc.Client.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
}

func TestObserveRollout(t *testing.T) {
	f := testInstance()
	conds := func(c ...appsv1.DeploymentCondition) []appsv1.DeploymentCondition { return c }
	unavailableFor := func(d time.Duration) appsv1.DeploymentCondition {
		return appsv1.DeploymentCondition{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
			LastTransitionTime: metav1.NewTime(time.Now().Add(-d))}
	}
	progressing := func(status corev1.ConditionStatus, reason string) appsv1.DeploymentCondition {
		return appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: status, Reason: reason}
	}
	for _, tc := range []struct {
		name     string
		replicas int32
		status   appsv1.DeploymentStatus
		want     rollout // desired and image follow the Deployment's spec
	}{
		{"a pod starting one minute after a scale-up from zero is starting", 1,
			appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, Conditions: conds(unavailableFor(time.Minute))},
			rollout{upToDate: true, starting: true}},
		{"pods unavailable for longer than startingGrace are no longer starting", 1,
			appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, Conditions: conds(unavailableFor(2 * startingGrace))},
			rollout{upToDate: true}},
		{"a ReplicaFailure is not a start-up", 1,
			appsv1.DeploymentStatus{Conditions: conds(unavailableFor(0),
				appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: "forbidden"})},
			rollout{upToDate: true, replicaFailure: "forbidden"}},
		// 2 desired, 1 updated, 3 pods (one surge), 2 ready: an old pod still serves.
		{"a rolling update is in flight with old pods", 2,
			appsv1.DeploymentStatus{Replicas: 3, UpdatedReplicas: 1, ReadyReplicas: 2, AvailableReplicas: 2,
				Conditions: conds(progressing(corev1.ConditionTrue, testReasonReplicaSetUpdated))},
			rollout{inFlight: true}},
		{"a Recreate rollout whose old pod is still terminating is in flight, not Degraded", 1,
			appsv1.DeploymentStatus{Replicas: 1, Conditions: conds(progressing(corev1.ConditionTrue, "NewReplicaSetAvailable"))},
			rollout{inFlight: true}},
		{"a rollout past its deadline is not in flight", 1,
			appsv1.DeploymentStatus{Replicas: 1, Conditions: conds(progressing(corev1.ConditionFalse, "ProgressDeadlineExceeded"))},
			rollout{deadlineExceeded: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			want.desired, want.image = tc.replicas, img16
			if got := observeRollout(statusDeployment(f, img16, tc.replicas, tc.status), true); got != want {
				t.Fatalf("observeRollout = %+v, want %+v", got, want)
			}
		})
	}
}

// A Deployment scaled to 0 replicas must not verify its template's image.
func TestReconcileStatusVerifiesImageOnlyWithAvailablePods(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img15
	dep := statusDeployment(f, img16, 0, appsv1.DeploymentStatus{})
	rc := testContext(t, f, dep)
	rc.TargetImage = img16

	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img15 {
		t.Fatalf("0 replicas: status.image = %s, want it unchanged (%s)", f.Status.Image, img15)
	}

	// Maintenance with a stale Deployment status still showing an available pod.
	dep.Spec.Replicas = ptr.To[int32](1)
	updateDeployment(t, rc, dep, appsv1.DeploymentStatus{ObservedGeneration: 10, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1})
	rc.Maintenance = true
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img15 {
		t.Fatalf("maintenance: status.image = %s, want %s", f.Status.Image, img15)
	}

	rc.Maintenance = false
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img16 || f.Status.Phase != forgejov1.PhaseRunning {
		t.Fatalf("one available pod: want image %s and Running, got %s, %s", img16, f.Status.Image, f.Status.Phase)
	}
}

// A restore in progress is phase Restoring; a hold that only a person ends (a failed restore
// that changed the instance) is Degraded, so that ForgejoDown and ForgejoNotReady fire.
func TestReconcileStatusHeldMaintenanceIsDegraded(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	dep := statusDeployment(f, img16, 0, appsv1.DeploymentStatus{})
	rc := testContext(t, f, dep)
	rc.TargetImage = img16
	rc.Maintenance, rc.MaintenanceReason = true, "ForgejoRestore r1 is restoring backup b1"
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Phase != forgejov1.PhaseRestoring {
		t.Fatalf("restore in progress: phase %s, want Restoring", f.Status.Phase)
	}
	rc.MaintenanceHeld, rc.MaintenanceReason = true, "ForgejoRestore r1 failed after it changed the instance"
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionReady)
	if f.Status.Phase != forgejov1.PhaseDegraded || ready == nil || ready.Reason != "Maintenance" ||
		!strings.Contains(ready.Message, "failed after it changed") {
		t.Fatalf("held: phase %s, Ready %+v; want Degraded and Ready=False/Maintenance", f.Status.Phase, ready)
	}
}

// A ReplicaSet that cannot create pods (Pod Security admission) is reported.
func TestReconcileStatusReportsReplicaFailure(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	msg := `pods "fj-abc" is forbidden: violates PodSecurity "restricted:latest": runAsNonRoot != true`
	dep := statusDeployment(f, img16, 1, appsv1.DeploymentStatus{
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: testReasonReplicaSetUpdated},
			{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: msg},
		},
	})
	rc := testContext(t, f, dep)
	rc.TargetImage = img16
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionReady)
	prog := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionProgressing)
	if ready == nil || ready.Reason != reasonReplicaFailure || !strings.Contains(ready.Message, "PodSecurity") {
		t.Fatalf("Ready: want %s with the admission message, got %+v", reasonReplicaFailure, ready)
	}
	if prog == nil || prog.Status != metav1.ConditionFalse || prog.Reason != reasonReplicaFailure {
		t.Fatalf("Progressing: want False/%s, got %+v", reasonReplicaFailure, prog)
	}
	if f.Status.Phase != forgejov1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", f.Status.Phase)
	}
}

// The Progressing message names the upgrade's images, and a failed upgrade's held image that
// is not available makes the phase Degraded.
func TestReconcileStatusUpgradeReporting(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img15
	f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseBackingUp, FromImage: img15, ToImage: img16}
	dep := statusDeployment(f, img15, 0, appsv1.DeploymentStatus{})
	rc := testContext(t, f, dep)
	rc.TargetImage, rc.Upgrading = img15, true

	for phase, want := range map[forgejov1.UpgradePhase]string{
		forgejov1.UpgradePhaseBackingUp:   "Backing up " + img15 + " before upgrading to " + img16,
		forgejov1.UpgradePhaseRollingBack: "Rolling back from " + img16 + " to " + img15,
		forgejov1.UpgradePhaseRollingOut:  "Upgrading from " + img15 + " to " + img16,
	} {
		f.Status.Upgrade.Phase = phase
		if _, err := ReconcileStatus(ctx, rc); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionProgressing); c == nil || c.Message != want {
			t.Fatalf("%s: Progressing message %+v, want %q", phase, c, want)
		}
	}

	// The upgrade failed and its image stays deployed, still starting.
	rc.Upgrading = false
	heldUpgrade(f)
	rc.TargetImage = img16
	dep.Spec.Replicas = ptr.To[int32](1)
	dep.Spec.Template.Spec.Containers[0].Image = img16
	updateDeployment(t, rc, dep, appsv1.DeploymentStatus{ObservedGeneration: 10, Replicas: 1, UpdatedReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: testReasonReplicaSetUpdated}}})
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Phase != forgejov1.PhaseDegraded || f.Status.Image != img15 {
		t.Fatalf("held failed upgrade: want Degraded with image %s, got %s, %s", img15, f.Status.Phase, f.Status.Image)
	}
}
