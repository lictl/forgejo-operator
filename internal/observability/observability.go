// Package observability provides Level IV insights: ServiceMonitor, PrometheusRule and
// dashboard resources, operator-side health probing of Forgejo and statistics sampling.
package observability

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

const componentMonitoring = "monitoring"

// ReasonMonitoringFailed is the Degraded reason owned by the monitoring step.
const ReasonMonitoringFailed = "MonitoringFailed"

// monitoringRetry is how soon a failed monitoring step is retried.
const monitoringRetry = time.Minute

// ReconcileMonitoring ensures the ServiceMonitor, PrometheusRule and dashboard ConfigMap.
// prometheus-operator kinds are skipped when their CRDs are absent; each object is
// deleted again when monitoring or its own toggle is disabled.
//
// Monitoring is optional, so its failures (an invalid scrape interval or label, a
// same-named object owned by someone else, a rejected update) never stop the pipeline:
// the three objects are applied independently, and failures are reported as
// Degraded=True/MonitoringFailed plus a Warning event and retried, while the later steps
// (backup schedule, health, autopilot) keep running. Only conflicts are returned, so the
// orchestrator retries them right away.
func ReconcileMonitoring(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	m := f.Spec.Monitoring
	on := forgejo.MonitoringEnabled(f)

	var errs []error
	smOn := on && toggle(m.ServiceMonitor, true)
	smExists, err := applyUnstructured(ctx, rc, forgejo.ServiceMonitorGVK, forgejo.ServiceMonitorName(f),
		smOn, specLabels(m.ServiceMonitor), func() (any, error) { return serviceMonitorSpec(f) })
	if err != nil {
		errs = append(errs, fmt.Errorf("ServiceMonitor %s: %w", forgejo.ServiceMonitorName(f), err))
	}
	// ForgejoDown needs a scrape target: render it while a ServiceMonitor exists, also when
	// updating it just failed, so its pending state is not reset by a transient error.
	scraped := smOn && (err == nil || smExists)
	if _, err := applyUnstructured(ctx, rc, forgejo.PrometheusRuleGVK, forgejo.PrometheusRuleName(f),
		on && toggle(m.PrometheusRule, true), specLabels(m.PrometheusRule),
		func() (any, error) { return prometheusRuleSpec(f, scraped), nil }); err != nil {
		errs = append(errs, fmt.Errorf("PrometheusRule %s: %w", forgejo.PrometheusRuleName(f), err))
	}
	if err := reconcileDashboard(ctx, rc, on && toggle(m.GrafanaDashboard, false)); err != nil {
		errs = append(errs, fmt.Errorf("dashboard ConfigMap %s: %w", forgejo.DashboardName(f), err))
	}
	return monitoringResult(ctx, rc, errs)
}

// monitoringResult reports the outcome of the monitoring step (see ReconcileMonitoring).
func monitoringResult(ctx context.Context, rc *forgejo.ReconcileContext, errs []error) (forgejo.StepResult, error) {
	f := rc.Instance
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	if len(errs) == 0 {
		monitoringWarned.Delete(key)
		if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c != nil &&
			c.Status == metav1.ConditionTrue && c.Reason == ReasonMonitoringFailed {
			rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "")
		}
		return forgejo.StepResult{}, nil
	}
	for _, err := range errs {
		if apierrors.IsConflict(err) {
			return forgejo.StepResult{}, err
		}
	}

	err := errors.Join(errs...)
	msg := "Monitoring resources could not be applied; backups, health checks and autopilot are not affected: " +
		strings.ReplaceAll(err.Error(), "\n", "; ")
	logf.FromContext(ctx).Info("monitoring step failed", "error", msg)
	// Degraded is shared: do not hide another feature's reason behind this optional one.
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c == nil ||
		c.Status != metav1.ConditionTrue || c.Reason == ReasonMonitoringFailed || c.Reason == reasonReconcileError {
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonMonitoringFailed, msg)
	}
	// One event per distinct failure, repeated at most hourly while it persists.
	now := time.Now()
	if prev, ok := monitoringWarned.Load(key); !ok || prev.(warned).msg != msg || now.Sub(prev.(warned).at) >= time.Hour {
		monitoringWarned.Store(key, warned{msg: msg, at: now})
		rc.Event(corev1.EventTypeWarning, ReasonMonitoringFailed, "Monitoring", "%s", msg)
	}
	rc.Requeue(monitoringRetry)
	return forgejo.StepResult{}, nil
}

// reasonReconcileError is the orchestrator's Degraded reason for a failed step. When the
// monitoring step runs it can only be left over from an earlier reconcile (a step failing
// now either stops before this one or sets it again afterwards), so it may be replaced.
const reasonReconcileError = "ReconcileError"

type warned struct {
	msg string
	at  time.Time
}

// monitoringWarned rate-limits MonitoringFailed events per instance.
var monitoringWarned sync.Map // types.NamespacedName -> warned

func toggle(s *forgejov1.PrometheusResourceSpec, def bool) bool {
	if s == nil {
		return def
	}
	return forgejo.BoolOr(s.Enabled, def)
}

func specLabels(s *forgejov1.PrometheusResourceSpec) map[string]string {
	if s == nil {
		return nil
	}
	return s.Labels
}

// objectLabels returns the standard labels plus user labels; standard keys win.
func objectLabels(f *forgejov1.Forgejo, extra map[string]string) map[string]string {
	l := maps.Clone(extra)
	if l == nil {
		l = map[string]string{}
	}
	maps.Copy(l, forgejo.Labels(f, componentMonitoring))
	return l
}

// applyUnstructured creates/updates (want=true) or deletes (want=false) a namespaced
// prometheus-operator object whose spec is rendered from a typed struct. exists reports
// whether an object of that name is in the cluster afterwards (also when updating it
// failed). Invalid user input is reported before anything is sent; the existing object,
// if any, is then left as it is.
func applyUnstructured(ctx context.Context, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind,
	name string, want bool, labels map[string]string, spec func() (any, error)) (exists bool, err error) {
	if !rc.HasAPI(gvk) {
		return false, nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(rc.Instance.Namespace)
	obj.SetName(name)
	if !want {
		return false, deleteIfOwned(ctx, rc, obj)
	}
	s, err := spec()
	if err = errors.Join(err, validateLabels(labels)); err != nil {
		getErr := rc.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, obj)
		return getErr == nil, err
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(s)
	if err != nil {
		return false, err
	}
	_, err = rc.CreateOrUpdate(ctx, obj, true, func() error {
		obj.SetLabels(objectLabels(rc.Instance, labels))
		obj.Object["spec"] = content
		return nil
	})
	// CreateOrUpdate reads the object first, so a UID means it exists.
	return err == nil || obj.GetUID() != "", err
}

// validateLabels checks user-supplied labels the way the API server would.
func validateLabels(labels map[string]string) error {
	var problems []string
	for _, k := range slices.Sorted(maps.Keys(labels)) {
		for _, msg := range validation.IsQualifiedName(k) {
			problems = append(problems, fmt.Sprintf("label key %q: %s", k, msg))
		}
		for _, msg := range validation.IsValidLabelValue(labels[k]) {
			problems = append(problems, fmt.Sprintf("label %q value %q: %s", k, labels[k], msg))
		}
	}
	if len(problems) > 0 {
		return errors.New("invalid labels: " + strings.Join(problems, "; "))
	}
	return nil
}

// deleteIfOwned removes obj if it exists and is controlled by the instance, so that
// user-created objects with the same name are left alone.
func deleteIfOwned(ctx context.Context, rc *forgejo.ReconcileContext, obj client.Object) error {
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, obj); err != nil {
		return forgejo.IgnoreNoMatch(err)
	}
	if !metav1.IsControlledBy(obj, rc.Instance) {
		return nil
	}
	return forgejo.IgnoreNoMatch(client.IgnoreNotFound(rc.Client.Delete(ctx, obj)))
}
