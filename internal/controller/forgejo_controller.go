/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/autopilot"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/core"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/observability"
	"github.com/lictl/forgejo-operator/internal/upgrade"
)

// defaultResync bounds how long the operator goes without re-checking an instance, which
// drives health probing, statistics sampling and autopilot decisions.
const defaultResync = 60 * time.Second

// ForgejoReconciler reconciles a Forgejo object by running an ordered list of steps,
// each owned by a feature package.
type ForgejoReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

type namedStep struct {
	name string
	fn   forgejo.Step
}

// steps run in order. Later steps read the outputs earlier steps put on the context.
var steps = []namedStep{
	{"secrets", core.ReconcileSecrets},
	{"database", core.ReconcileDatabase},
	{"high-availability", autopilot.ReconcileHA},
	{"storage", core.ReconcileStorage},
	{"maintenance", backup.ReconcileMaintenance},
	{"upgrade", upgrade.Reconcile},
	{"tuning", autopilot.ReconcileTuning},
	{"config", core.ReconcileConfig},
	{"config-guard", autopilot.ReconcileConfigGuard},
	{"workload", core.ReconcileWorkload},
	{"network", core.ReconcileNetwork},
	{"scaling", autopilot.ReconcileScaling},
	{"monitoring", observability.ReconcileMonitoring},
	{"backup-schedule", backup.ReconcileSchedule},
	{"health", observability.ReconcileHealth},
	{"autopilot", autopilot.Reconcile},
}

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=secrets;services;persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives a Forgejo instance towards its spec.
func (r *ForgejoReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	instance := &forgejov1.Forgejo{}
	if err := r.Get(ctx, req.NamespacedName, instance); err != nil {
		if apierrors.IsNotFound(err) {
			forgejo.DeleteInstanceMetrics(req.Namespace, req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !instance.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	original := instance.DeepCopy()
	rc := &forgejo.ReconcileContext{
		Client:     r.Client,
		Scheme:     r.Scheme,
		Recorder:   r.Recorder,
		RESTMapper: r.RESTMapper(),
		Instance:   instance,
	}
	rc.TargetImage = forgejo.Image(instance)
	if instance.Status.Image != "" {
		rc.TargetImage = instance.Status.Image
	}
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.ConfigSecretName = forgejo.ConfigSecretName(instance)

	if instance.Spec.Paused {
		instance.Status.Phase = forgejov1.PhasePaused
		rc.SetCondition(forgejov1.ConditionProgressing, metav1.ConditionFalse, "Paused", "Reconciliation is paused by spec.paused")
		return ctrl.Result{}, r.patchStatus(ctx, original, instance)
	}

	var stepErr error
	failedStep := ""
	for _, s := range steps {
		res, err := s.fn(ctx, rc)
		if err != nil {
			stepErr, failedStep = err, s.name
			break
		}
		rc.Requeue(res.RequeueAfter)
		if res.Stop {
			log.V(1).Info("step stopped reconciliation", "step", s.name)
			break
		}
	}

	if stepErr != nil {
		log.Error(stepErr, "reconcile step failed", "step", failedStep)
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, "ReconcileError",
			fmt.Sprintf("step %q: %v", failedStep, stepErr))
		rc.Event(corev1.EventTypeWarning, "ReconcileError", "Reconcile", "step %s failed: %v", failedStep, stepErr)
	}

	if _, err := core.ReconcileStatus(ctx, rc); err != nil && stepErr == nil {
		stepErr = err
	}
	instance.Status.ObservedGeneration = instance.Generation

	if err := r.patchStatus(ctx, original, instance); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}
	if stepErr != nil {
		return ctrl.Result{}, stepErr
	}

	rc.Requeue(defaultResync)
	return ctrl.Result{RequeueAfter: rc.RequeueAfter}, nil
}

func (r *ForgejoReconciler) patchStatus(ctx context.Context, original, instance *forgejov1.Forgejo) error {
	return r.Status().Patch(ctx, instance, client.MergeFrom(original))
}

// SetupWithManager sets up the controller with the Manager.
func (r *ForgejoReconciler) SetupWithManager(mgr ctrl.Manager) error {
	b := ctrl.NewControllerManagedBy(mgr).
		For(&forgejov1.Forgejo{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{},
			predicate.AnnotationChangedPredicate{},
			predicate.LabelChangedPredicate{},
		))).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&autoscalingv2.HorizontalPodAutoscaler{}).
		Owns(&policyv1.PodDisruptionBudget{}).
		Watches(&forgejov1.ForgejoBackup{}, handler.EnqueueRequestsFromMapFunc(mapBackupToForgejo)).
		Watches(&forgejov1.ForgejoRestore{}, handler.EnqueueRequestsFromMapFunc(mapRestoreToForgejo)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(mapByInstanceLabel),
			builder.WithPredicates(managedByOperator())).
		Named("forgejo")

	// CloudNativePG is optional at startup; watch its Clusters and app secrets when installed.
	if _, err := mgr.GetRESTMapper().RESTMapping(forgejo.CNPGClusterGVK.GroupKind(), forgejo.CNPGClusterGVK.Version); err == nil {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
		b = b.Owns(cluster)
	} else {
		mgr.GetLogger().Info("CloudNativePG CRDs not found; managed databases will be unavailable until the operator restarts")
	}
	// CNPG-generated credential secrets are owned by the CNPG Cluster, not by us.
	b = b.Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(mapCNPGSecretToForgejo))

	return b.Complete(r)
}

func mapBackupToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*forgejov1.ForgejoBackup)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.ForgejoRef.Name}}}
}

func mapRestoreToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	r, ok := obj.(*forgejov1.ForgejoRestore)
	if !ok {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: r.Spec.ForgejoRef.Name}}}
}

func mapByInstanceLabel(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[forgejo.LabelInstance]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// mapCNPGSecretToForgejo maps "<forgejo>-db-app" (labelled cnpg.io/cluster=<forgejo>-db)
// back to the Forgejo instance so password rotations re-render app.ini.
func mapCNPGSecretToForgejo(_ context.Context, obj client.Object) []reconcile.Request {
	cluster := obj.GetLabels()["cnpg.io/cluster"]
	if cluster == "" || !strings.HasSuffix(cluster, "-db") {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: strings.TrimSuffix(cluster, "-db")}}}
}

func managedByOperator() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		l := obj.GetLabels()
		return l[forgejo.LabelName] == forgejo.AppName && l[forgejo.LabelInstance] != ""
	})
}
