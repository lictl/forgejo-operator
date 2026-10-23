package core

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func deploymentWith(replicas, ready, updated, total int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(replicas)},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 2, Replicas: total, UpdatedReplicas: updated,
			ReadyReplicas: ready, AvailableReplicas: ready, Conditions: conds,
		},
	}
}

func TestObserveRolloutStartingAfterScaleUp(t *testing.T) {
	unavailable := appsv1.DeploymentCondition{
		Type: appsv1.DeploymentAvailable, Status: corev1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
	}
	ro := observeRollout(deploymentWith(1, 0, 1, 1, unavailable), true)
	if !ro.starting {
		t.Fatal("a pod starting one minute after a scale-up from zero should count as starting")
	}

	unavailable.LastTransitionTime = metav1.NewTime(time.Now().Add(-2 * startingGrace))
	if ro := observeRollout(deploymentWith(1, 0, 1, 1, unavailable), true); ro.starting {
		t.Fatal("pods unavailable for longer than startingGrace are no longer starting")
	}

	failure := appsv1.DeploymentCondition{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Message: "forbidden"}
	unavailable.LastTransitionTime = metav1.NewTime(time.Now())
	if ro := observeRollout(deploymentWith(1, 0, 0, 0, unavailable, failure), true); ro.starting {
		t.Fatal("a ReplicaFailure is not a start-up")
	}
}

func TestObserveRolloutRollingUpdateKeepsServing(t *testing.T) {
	progressing := appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: testReasonReplicaSetUpdated}
	// 2 desired, 1 updated, 3 pods (one surge), 2 ready: an old pod still serves.
	ro := observeRollout(deploymentWith(2, 2, 1, 3, progressing), true)
	if !ro.inFlight || ro.upToDate {
		t.Fatalf("want an in-flight rollout with old pods, got %+v", ro)
	}
}
