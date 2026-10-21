// Package observability provides Level IV insights: ServiceMonitor, PrometheusRule and
// dashboard resources, operator-side health probing of Forgejo and statistics sampling.
package observability

import (
	"context"
	"maps"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors;prometheusrules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=configmaps;secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch

const componentMonitoring = "monitoring"

// ReconcileMonitoring ensures the ServiceMonitor, PrometheusRule and dashboard ConfigMap.
// prometheus-operator kinds are skipped when their CRDs are absent; each object is
// deleted again when monitoring or its own toggle is disabled.
func ReconcileMonitoring(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	m := f.Spec.Monitoring
	on := forgejo.MonitoringEnabled(f)

	smOn := on && toggle(m.ServiceMonitor, true)
	if err := applyUnstructured(ctx, rc, forgejo.ServiceMonitorGVK, forgejo.ServiceMonitorName(f),
		smOn, specLabels(m.ServiceMonitor), func() any { return serviceMonitorSpec(f) }); err != nil {
		return forgejo.StepResult{}, err
	}
	if err := applyUnstructured(ctx, rc, forgejo.PrometheusRuleGVK, forgejo.PrometheusRuleName(f),
		on && toggle(m.PrometheusRule, true), specLabels(m.PrometheusRule), func() any { return prometheusRuleSpec(f, smOn) }); err != nil {
		return forgejo.StepResult{}, err
	}
	if err := reconcileDashboard(ctx, rc, on && toggle(m.GrafanaDashboard, false)); err != nil {
		return forgejo.StepResult{}, err
	}
	return forgejo.StepResult{}, nil
}

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
// prometheus-operator object whose spec is rendered from a typed struct.
func applyUnstructured(ctx context.Context, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind,
	name string, want bool, labels map[string]string, spec func() any) error {
	if !rc.HasAPI(gvk) {
		return nil
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(rc.Instance.Namespace)
	obj.SetName(name)
	if !want {
		return deleteIfOwned(ctx, rc, obj)
	}
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec())
	if err != nil {
		return err
	}
	_, err = rc.CreateOrUpdate(ctx, obj, true, func() error {
		obj.SetLabels(objectLabels(rc.Instance, labels))
		obj.Object["spec"] = content
		return nil
	})
	return err
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
