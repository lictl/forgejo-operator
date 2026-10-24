package core

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	if err := reportCronRunner(ctx, rc); err != nil {
		return forgejo.StepResult{}, err
	}
	st.Replicas, st.ReadyReplicas = 0, 0
	if found {
		st.Replicas = dep.Status.Replicas
		st.ReadyReplicas = dep.Status.ReadyReplicas
	}

	ro := observeRollout(dep, found)
	// status.image is the image verified running: every pod of the current template is
	// available, and there is at least one. A Deployment at 0 replicas (spec.replicas 0,
	// maintenance, a cold backup) proves nothing about its template's image.
	if ro.complete && ro.desired > 0 && ro.image == rc.TargetImage && !rc.Maintenance {
		st.Image = rc.TargetImage
	}
	// Ready: at least one pod serves the current template and no old pods remain, or a
	// rolling update is replacing pods while ready ones keep serving.
	ready := ro.upToDate && ro.desired > 0 && dep.Status.ReadyReplicas > 0
	rolling := !ready && ro.inFlight && ro.desired > 0 && dep.Status.ReadyReplicas > 0
	// replicaFailure: the ReplicaSet cannot create pods (e.g. rejected by Pod Security
	// admission, a quota or a webhook); without it the reason only shows in its events.
	replicaFailure := !ready && !rc.Maintenance && ro.replicaFailure != ""

	switch {
	case ready:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionTrue, "Available",
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, ro.desired))
	case rolling && !rc.Maintenance:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionTrue, "RollingUpdate",
			fmt.Sprintf("%d replicas serving while %d/%d are updated", dep.Status.ReadyReplicas, dep.Status.UpdatedReplicas, ro.desired))
	case rc.Maintenance:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "Maintenance", "Scaled down: "+rc.MaintenanceReason)
	case !found:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "NotDeployed", notDeployedMessage(rc))
	case replicaFailure:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, reasonReplicaFailure,
			"Pods cannot be created: "+ro.replicaFailure)
	default:
		rc.SetCondition(forgejov1.ConditionReady, metav1.ConditionFalse, "NotAvailable",
			fmt.Sprintf("%d/%d replicas ready", dep.Status.ReadyReplicas, ro.desired))
	}

	setProgressingCondition(rc, dep, ro, found, replicaFailure)
	setPhase(rc, ro, ready, replicaFailure)

	updateInstanceMetrics(rc, ready || (rolling && !rc.Maintenance))
	return forgejo.StepResult{}, nil
}

// setProgressingCondition reports the rollout state of the Deployment as Progressing.
func setProgressingCondition(rc *forgejo.ReconcileContext, dep *appsv1.Deployment, ro rollout, found, replicaFailure bool) {
	switch {
	case rc.Maintenance:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "Maintenance", rc.MaintenanceReason)
	case replicaFailure:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionFalse, reasonReplicaFailure, ro.replicaFailure)
	case rc.Upgrading:
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionTrue, "Upgrading", upgradingMessage(rc))
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
}

// setPhase derives status.phase from the observed rollout.
func setPhase(rc *forgejo.ReconcileContext, ro rollout, ready, replicaFailure bool) {
	f := rc.Instance
	st := &f.Status
	switch {
	case rc.MaintenanceHeld:
		// A failed restore or bootstrap holds the instance at zero until a person acts: an
		// outage, not a restore in progress (ForgejoDown and ForgejoNotReady are not suppressed).
		st.Phase = forgejov1.PhaseDegraded
	case rc.Maintenance:
		st.Phase = forgejov1.PhaseRestoring
	case rc.Upgrading:
		st.Phase = forgejov1.PhaseUpgrading
	case ready:
		st.Phase = forgejov1.PhaseRunning
	case forgejo.UpgradeHoldsWorkload(f) || replicaFailure:
		// A failed upgrade's image that never became available, or pods that cannot be
		// created: not a rollout that will finish by itself.
		st.Phase = forgejov1.PhaseDegraded
	case ro.inFlight && st.ReadyReplicas > 0:
		// A rolling update that is still serving traffic.
		st.Phase = forgejov1.PhaseRunning
	case st.Image == "" || ro.inFlight || ro.starting:
		// First install, a rollout (e.g. a config change with Recreate) in flight, or pods
		// still starting after a scale-up from zero (e.g. the end of a restore).
		st.Phase = forgejov1.PhaseProvisioning
	default:
		st.Phase = forgejov1.PhaseDegraded
	}
}

// startingGrace is how long pods may take to become ready after a scale-up from zero before
// the phase turns Degraded (the startup probe allows longer; this only affects the phase).
const startingGrace = 10 * time.Minute

// reasonReplicaFailure reports the Deployment's ReplicaFailure condition.
const reasonReplicaFailure = "ReplicaFailure"

type rollout struct {
	// complete: every pod runs the current template and is available.
	complete bool
	// replicaFailure is the message of a true ReplicaFailure condition (FailedCreate).
	replicaFailure string
	// upToDate: the current template is observed and no pods of older templates remain.
	upToDate bool
	// inFlight: the Deployment is still replacing pods (not merely missing ready ones).
	inFlight bool
	// starting: no pod is ready yet, but the Deployment became unavailable less than
	// startingGrace ago (a scale-up from zero) and nothing reports a failure.
	starting         bool
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
	// Pods of an older template remain: with Recreate the Progressing condition keeps its
	// NewReplicaSetAvailable reason until they are gone and the new ReplicaSet scales up.
	oldPods := s.ObservedGeneration >= dep.Generation && s.Replicas != s.UpdatedReplicas
	for _, c := range s.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			ro.replicaFailure = c.Message
			if ro.replicaFailure == "" {
				ro.replicaFailure = c.Reason
			}
		}
		if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionFalse &&
			s.ReadyReplicas == 0 && ro.desired > 0 && time.Since(c.LastTransitionTime.Time) < startingGrace {
			ro.starting = true
		}
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
	if oldPods && !ro.complete && !ro.deadlineExceeded {
		ro.inFlight = true
	}
	if ro.deadlineExceeded || ro.replicaFailure != "" {
		ro.starting = false
	}
	return ro
}

// upgradingMessage names the images of the upgrade in flight. rc.TargetImage alone would
// name the old image while the pre-upgrade backup or the rollback runs.
func upgradingMessage(rc *forgejo.ReconcileContext) string {
	u := rc.Instance.Status.Upgrade
	if u == nil || u.ToImage == "" || u.FromImage == "" {
		return "Upgrading to " + rc.TargetImage
	}
	switch u.Phase {
	case forgejov1.UpgradePhasePending:
		return fmt.Sprintf("Upgrade from %s to %s is pending", u.FromImage, u.ToImage)
	case forgejov1.UpgradePhaseBackingUp:
		return fmt.Sprintf("Backing up %s before upgrading to %s", u.FromImage, u.ToImage)
	case forgejov1.UpgradePhaseRollingBack:
		return fmt.Sprintf("Rolling back from %s to %s", u.ToImage, u.FromImage)
	}
	return fmt.Sprintf("Upgrading from %s to %s", u.FromImage, u.ToImage)
}

func notDeployedMessage(rc *forgejo.ReconcileContext) string {
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDatabaseReady); c != nil && c.Status != metav1.ConditionTrue {
		return "Waiting for the database: " + c.Message
	}
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionObjectStorageReady); c != nil &&
		(c.Reason == reasonCredentialsUnavailable || c.Reason == reasonObjectStorageInvalid) {
		return "Waiting for spec.objectStorage: " + c.Message
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
