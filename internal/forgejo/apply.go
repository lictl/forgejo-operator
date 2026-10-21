package forgejo

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// CreateOrUpdate wraps controllerutil.CreateOrUpdate and sets the instance as controller
// owner of obj (unless setOwner is false, used for data-bearing resources under Retain).
func (rc *ReconcileContext) CreateOrUpdate(ctx context.Context, obj client.Object, setOwner bool, mutate func() error) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrUpdate(ctx, rc.Client, obj, func() error {
		if err := mutate(); err != nil {
			return err
		}
		if setOwner {
			return controllerutil.SetControllerReference(rc.Instance, obj, rc.Scheme)
		}
		return nil
	})
}

// HasAPI reports whether the cluster serves the given group/version/kind (e.g. CNPG
// Cluster, ServiceMonitor). Optional integrations must check this before creating objects.
func (rc *ReconcileContext) HasAPI(gvk schema.GroupVersionKind) bool {
	if rc.RESTMapper == nil {
		return false
	}
	_, err := rc.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// IgnoreNoMatch treats "no kind registered" errors as success (optional CRDs absent).
func IgnoreNoMatch(err error) error {
	if err == nil || meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// Well-known external GVKs.
var (
	CNPGClusterGVK    = schema.GroupVersionKind{Group: "postgresql.cnpg.io", Version: "v1", Kind: "Cluster"}
	ServiceMonitorGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"}
	PrometheusRuleGVK = schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "PrometheusRule"}
)
