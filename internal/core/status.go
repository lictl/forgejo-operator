package core

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// ReconcileStatus summarizes phase, Ready/Progressing conditions, replicas and URLs.
// It always runs last, even when an earlier step stopped the reconcile.
func ReconcileStatus(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	st := &f.Status
	st.URL = EffectiveRootURL(f)
	st.InternalURL = forgejo.InternalHTTPURL(f)
	st.Selector = labels.FormatLabels(forgejo.SelectorLabels(f))

	dep := &appsv1.Deployment{}
	err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep)
	if err != nil && !apierrors.IsNotFound(err) {
		return forgejo.StepResult{}, err
	}
	found := err == nil
	st.Replicas, st.ReadyReplicas = 0, 0
	if found {
		st.Replicas = dep.Status.Replicas
		st.ReadyReplicas = dep.Status.ReadyReplicas
	}

	ro := observeRollout(dep, found)
	if ro.complete && ro.image == rc.TargetImage {
		st.Image = rc.TargetImage
	}
	// Ready: at least one pod serves the current template and no old pods remain.
	ready := ro.upToDate && ro.desired > 0 && dep.Status.ReadyReplicas > 0

	switch {
	case ready:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionTrue, "Available",
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, ro.desired))
	case rc.Maintenance:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "Maintenance", "Scaled down: "+rc.MaintenanceReason)
	case !found:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "NotDeployed", notDeployedMessage(rc))
	default:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "NotAvailable",
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, ro.desired))
	}

	switch {
	case rc.Maintenance:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "Maintenance", rc.MaintenanceReason)
	case rc.Upgrading:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "Upgrading", "Upgrading to "+rc.TargetImage)
	case ro.deadlineExceeded:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionFalse, "ProgressDeadlineExceeded", ro.message)
	case !found:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "Provisioning", notDeployedMessage(rc))
	case !ro.complete:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "RollingOut",
			fmt.Sprintf("%d/%d replicas updated, %d available", dep.Status.UpdatedReplicas, ro.desired, dep.Status.AvailableReplicas))
	default:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionFalse, "RolledOut", "Deployment is up to date")
	}

	switch {
	case rc.Maintenance:
		st.Phase = forgejov1.PhaseRestoring
	case rc.Upgrading:
		st.Phase = forgejov1.PhaseUpgrading
	case ready:
		st.Phase = forgejov1.PhaseRunning
	case ro.inFlight && st.ReadyReplicas > 0:
		// A rolling update that is still serving traffic.
		st.Phase = forgejov1.PhaseRunning
	case st.Image == "" || ro.inFlight:
		// First install, or a rollout (e.g. a config change with Recreate) in flight.
		st.Phase = forgejov1.PhaseProvisioning
	default:
		st.Phase = forgejov1.PhaseDegraded
	}

	updateInstanceMetrics(rc, ready)
	return forgejo.StepResult{}, nil
}

type rollout struct {
	// complete: every pod runs the current template and is available.
	complete bool
	// upToDate: the current template is observed and no pods of older templates remain.
	upToDate bool
	// inFlight: the Deployment is still replacing pods (not merely missing ready ones).
	inFlight         bool
	deadlineExceeded bool
	desired          int32
	image, message   string
}

// observeRollout reports whether every pod runs the current pod template and is available.
func observeRollout(dep *appsv1.Deployment, found bool) rollout {
	var ro rollout
	if !found {
		return ro
	}
	ro.desired = 1
	if dep.Spec.Replicas != nil {
		ro.desired = *dep.Spec.Replicas
	}
	for _, c := range dep.Spec.Template.Spec.Containers {
		if c.Name == containerName {
			ro.image = c.Image
		}
	}
	s := dep.Status
	ro.complete = s.ObservedGeneration >= dep.Generation &&
		s.UpdatedReplicas == ro.desired && s.Replicas == ro.desired && s.AvailableReplicas == ro.desired
	ro.upToDate = s.ObservedGeneration >= dep.Generation && s.Replicas == s.UpdatedReplicas
	ro.inFlight = !ro.complete && s.ObservedGeneration < dep.Generation
	for _, c := range s.Conditions {
		if c.Type != appsv1.DeploymentProgressing {
			continue
		}
		switch c.Reason {
		case "ProgressDeadlineExceeded":
			ro.deadlineExceeded, ro.message = true, c.Message
		case "NewReplicaSetAvailable":
		default:
			ro.inFlight = ro.inFlight || !ro.complete
		}
	}
	return ro
}

func notDeployedMessage(rc *forgejo.ReconcileContext) string {
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDatabaseReady); c != nil && c.Status != metav1.ConditionTrue {
		return "Waiting for the database: " + c.Message
	}
	return "Waiting for the Deployment to be created"
}

func updateInstanceMetrics(rc *forgejo.ReconcileContext, ready bool) {
	f := rc.Instance
	v := 0.0
	if ready {
		v = 1
	}
	forgejo.InstanceReady.WithLabelValues(f.Namespace, f.Name).Set(v)

	image := f.Status.Image
	if image == "" {
		image = rc.TargetImage
	}
	forgejo.InstanceInfo.DeletePartialMatch(prometheus.Labels{"namespace": f.Namespace, "name": f.Name})
	forgejo.InstanceInfo.WithLabelValues(f.Namespace, f.Name, image, f.Status.Version, string(f.Status.Phase)).Set(1)
}
