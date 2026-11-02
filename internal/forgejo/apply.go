package forgejo

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// NameConflictError reports an existing object that has the name the operator would use but
// that the instance does not control. The operator neither adopts nor changes it; the step
// reports the conflict (a condition, or a failed step) instead.
type NameConflictError struct {
	Kind, Name string
	// Controller is the object's controller reference, nil when it has none.
	Controller *metav1.OwnerReference
}

func (e *NameConflictError) Error() string {
	by := "it has no controller"
	if c := e.Controller; c != nil {
		by = fmt.Sprintf("it is controlled by %s %s", c.Kind, c.Name)
	}
	return fmt.Sprintf("%s %s already exists and does not belong to this Forgejo (%s): the operator does not take "+
		"over or change it. Delete it (or have its owner delete it) for the operator to create its own", e.Kind, e.Name, by)
}

// Controls reports whether the instance may write an existing object: its controller is the
// instance, or an earlier Forgejo of the same name (whose children are taken over when the
// name is used again, as controllerutil.SetControllerReference does), or the object has no
// controller and carries the instance's identifying labels (it was left behind when an earlier
// instance of this name was deleted with orphaning). The second result is the controller
// reference of an object that another owner controls.
func Controls(f *forgejov1.Forgejo, obj client.Object) (bool, *metav1.OwnerReference) {
	if c := metav1.GetControllerOf(obj); c != nil {
		return isInstanceRef(*c, f), c
	}
	labels := obj.GetLabels()
	for _, k := range []string{LabelName, LabelInstance, LabelManagedBy} {
		if labels[k] != Labels(f, "")[k] {
			return false, nil
		}
	}
	return true, nil
}

// CreateOrUpdate wraps controllerutil.CreateOrUpdate and sets the instance as controller
// owner of obj (unless setOwner is false, used for data-bearing resources under Retain). It
// writes an existing object only while the instance controls it (Controls); a same-named
// object of anyone else is left untouched and the error is a *NameConflictError, so that a
// user's Secret or Service that happens to have the name is never overwritten or deleted
// later as the operator's own.
func (rc *ReconcileContext) CreateOrUpdate(ctx context.Context, obj client.Object, setOwner bool, mutate func() error) (controllerutil.OperationResult, error) {
	return rc.createOrUpdate(ctx, obj, setOwner, false, mutate)
}

// CreateOrAdopt is CreateOrUpdate for the objects a person may put in place on purpose to
// recover an instance, and that the operator then takes over as they are: the identity
// Secrets, the data volume and the CNPG cluster (see ReconcileIdentity). It adopts an object
// that nothing controls, with or without the instance's labels; one that another owner
// controls (an ExternalSecret, say) is still refused.
func (rc *ReconcileContext) CreateOrAdopt(ctx context.Context, obj client.Object, setOwner bool, mutate func() error) (controllerutil.OperationResult, error) {
	return rc.createOrUpdate(ctx, obj, setOwner, true, mutate)
}

func (rc *ReconcileContext) createOrUpdate(ctx context.Context, obj client.Object, setOwner, adopt bool, mutate func() error) (controllerutil.OperationResult, error) {
	return controllerutil.CreateOrUpdate(ctx, rc.Client, obj, func() error {
		if obj.GetResourceVersion() != "" {
			ok, c := Controls(rc.Instance, obj)
			if adopt && c == nil {
				ok = true
			}
			if !ok {
				return &NameConflictError{Kind: rc.kindOf(obj), Name: obj.GetName(), Controller: c}
			}
		}
		if err := mutate(); err != nil {
			return err
		}
		if setOwner {
			return controllerutil.SetControllerReference(rc.Instance, obj, rc.Scheme)
		}
		return nil
	})
}

// kindOf is the kind of obj for messages.
func (rc *ReconcileContext) kindOf(obj client.Object) string {
	if k := obj.GetObjectKind().GroupVersionKind().Kind; k != "" {
		return k
	}
	if gvks, _, err := rc.Scheme.ObjectKinds(obj); err == nil && len(gvks) > 0 {
		return gvks[0].Kind
	}
	return "object"
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
