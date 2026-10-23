package forgejo

import (
	"context"
	"errors"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
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
// Any discovery error counts as not served; APIServed tells the two apart.
func (rc *ReconcileContext) HasAPI(gvk schema.GroupVersionKind) bool {
	if rc.RESTMapper == nil {
		return false
	}
	_, err := rc.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	return err == nil
}

// APIServed reports whether the cluster serves the given group/version/kind. It answers
// false only when API discovery says so (a NoMatch answer); any other discovery error, such
// as a failed or timed-out discovery request, is returned, so that the caller retries instead
// of acting on an absence it does not know about.
func (rc *ReconcileContext) APIServed(gvk schema.GroupVersionKind) (bool, error) {
	if rc.RESTMapper == nil {
		return false, nil
	}
	_, err := rc.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	switch {
	case err == nil:
		return true, nil
	case notServed(err):
		return false, nil
	}
	return false, err
}

// notServed reports whether a RESTMapping error says that the kind is not served. A
// *apiutil.ErrResourceDiscoveryFailed wraps one error per group version, and errors.Is (thus
// meta.IsNoMatchError) matches when any one of them does; it only counts when every entry
// is NotFound or NoMatch.
func notServed(err error) bool {
	var failed *apiutil.ErrResourceDiscoveryFailed
	if errors.As(err, &failed) {
		for _, e := range *failed {
			if !apierrors.IsNotFound(e) && !meta.IsNoMatchError(e) {
				return false
			}
		}
		return true
	}
	return meta.IsNoMatchError(err)
}

// FirstServedAPI returns the first of gvks that the cluster serves (see APIServed); ok is
// false when discovery says that none is served, and err is set when discovery failed.
func (rc *ReconcileContext) FirstServedAPI(gvks ...schema.GroupVersionKind) (gvk schema.GroupVersionKind, ok bool, err error) {
	for _, gvk := range gvks {
		served, err := rc.APIServed(gvk)
		if err != nil {
			return schema.GroupVersionKind{}, false, err
		}
		if served {
			return gvk, true, nil
		}
	}
	return schema.GroupVersionKind{}, false, nil
}

// FirstAPI returns the first of gvks that the cluster serves, for kinds whose version
// depends on the installed CRDs (e.g. TCPRoute v1 or v1alpha2).
func (rc *ReconcileContext) FirstAPI(gvks ...schema.GroupVersionKind) (schema.GroupVersionKind, bool) {
	for _, gvk := range gvks {
		if rc.HasAPI(gvk) {
			return gvk, true
		}
	}
	return schema.GroupVersionKind{}, false
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
	HTTPRouteGVK      = schema.GroupVersionKind{Group: GatewayAPIGroup, Version: "v1", Kind: "HTTPRoute"}
	CertificateGVK    = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}
	// TCPRouteGVKs are the TCPRoute versions the operator writes, preferred first: v1 (Gateway
	// API 1.6 and later) and v1alpha2 (older experimental channels; Traefik before 3.8).
	TCPRouteGVKs = []schema.GroupVersionKind{
		{Group: GatewayAPIGroup, Version: "v1", Kind: "TCPRoute"},
		{Group: GatewayAPIGroup, Version: "v1alpha2", Kind: "TCPRoute"},
	}
	// ReferenceGrantGVKs are the ReferenceGrant versions, preferred first.
	ReferenceGrantGVKs = []schema.GroupVersionKind{
		{Group: GatewayAPIGroup, Version: "v1", Kind: "ReferenceGrant"},
		{Group: GatewayAPIGroup, Version: "v1beta1", Kind: "ReferenceGrant"},
	}
)

// GatewayAPIGroup is the API group of Gateway API.
const GatewayAPIGroup = "gateway.networking.k8s.io"
