package core

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const filterRequestHeaderModifier = "RequestHeaderModifier"

// Gateway API objects are read live (never listed or watched), written with CreateOrUpdate and
// deleted by deleteOptional; ReferenceGrants are written by the tls step.
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes;tcproutes;referencegrants,verbs=get;create;update;delete

// Reasons of the RoutesReady condition.
const (
	reasonRoutesAccepted        = "Accepted"
	reasonRoutesPending         = "Pending"
	reasonRoutesNotAccepted     = "NotAccepted"
	reasonRoutesRefsNotResolved = "RefsNotResolved"
	reasonGatewayAPIUnavailable = "GatewayAPIUnavailable"
	reasonTCPRouteUnavailable   = "TCPRouteUnavailable"
	reasonNoHostnames           = "NoHostnames"
	// reasonNameConflict (RoutesReady, TLSReady): an object of the name the operator would use
	// exists, but the instance does not control it.
	reasonNameConflict = "NameConflict"
)

// notReadyRequeue is how soon a route or certificate that is not ready yet is looked at again;
// the operator does not watch these kinds.
const notReadyRequeue = 15 * time.Second

// gwReasonEndpointsNotFound is the ResolvedRefs reason Envoy Gateway reports while a route's
// backend Service has no ready endpoints.
const gwReasonEndpointsNotFound = "EndpointsNotFound"

// endpointsGrace is how long after a Forgejo pod became Ready a Gateway may still report that the
// routes' Services have no ready endpoints: Gateway controllers update route status
// asynchronously.
const endpointsGrace = time.Minute

// Gateway API kinds and values the routes use.
const (
	kindGateway    = "Gateway"
	kindService    = "Service"
	pathPrefixType = "PathPrefix"
)

// defaultRequestTimeout disables the Gateway's request timeout (see GatewaySpec.RequestTimeout).
const defaultRequestTimeout = "0s"

// realIPHeader is always removed from requests (see GatewaySpec.RemoveRequestHeaders):
// Forgejo prefers it to X-Forwarded-For, and Gateways such as Envoy Gateway pass a client's
// own X-Real-IP through.
const realIPHeader = "X-Real-IP"

// Minimal typed mirrors of the Gateway API, so that the operator has no Go dependency on it.
// Fields the Gateway API CRDs default are rendered with their defaults, so that an unchanged
// route compares equal to the stored one and is not updated on every pass.

type gwParentRef struct {
	Group       string `json:"group"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	SectionName string `json:"sectionName,omitempty"`
	Port        *int32 `json:"port,omitempty"`
}

type gwBackendRef struct {
	Group  string `json:"group"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Port   int32  `json:"port"`
	Weight int32  `json:"weight"`
}

type httpRouteSpec struct {
	ParentRefs []gwParentRef   `json:"parentRefs"`
	Hostnames  []string        `json:"hostnames,omitempty"`
	Rules      []httpRouteRule `json:"rules"`
}

type httpRouteRule struct {
	Matches     []httpRouteMatch   `json:"matches"`
	Filters     []httpRouteFilter  `json:"filters,omitempty"`
	BackendRefs []gwBackendRef     `json:"backendRefs"`
	Timeouts    *httpRouteTimeouts `json:"timeouts,omitempty"`
}

type httpRouteMatch struct {
	Path httpPathMatch `json:"path"`
}

type httpPathMatch struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type httpRouteFilter struct {
	Type                  string              `json:"type"`
	RequestHeaderModifier *httpHeaderModifier `json:"requestHeaderModifier,omitempty"`
	URLRewrite            *httpURLRewrite     `json:"urlRewrite,omitempty"`
}

type httpHeaderModifier struct {
	Remove []string `json:"remove"`
}

type httpURLRewrite struct {
	Path httpPathModifier `json:"path"`
}

type httpPathModifier struct {
	Type               string `json:"type"`
	ReplacePrefixMatch string `json:"replacePrefixMatch"`
}

type httpRouteTimeouts struct {
	Request string `json:"request"`
}

type tcpRouteSpec struct {
	ParentRefs []gwParentRef  `json:"parentRefs"`
	Rules      []tcpRouteRule `json:"rules"`
}

type tcpRouteRule struct {
	BackendRefs []gwBackendRef `json:"backendRefs"`
}

// ReconcileGateway attaches the instance to the Gateways of spec.gateway: HTTPRoute "<name>"
// for HTTP and, with spec.gateway.ssh, TCPRoute "<name>-ssh" for SSH. Routes the spec no
// longer asks for are deleted. The routes' status (Accepted, ResolvedRefs per parent) becomes
// the RoutesReady condition. Without the Gateway API CRDs nothing is created, and RoutesReady
// says so while spec.gateway is set. Without a host name (RouteHostnames) there is no
// HTTPRoute: one without host names would take every host name of the listeners it attaches
// to, also other applications' on a shared Gateway. A route of the same name that the instance
// does not control is left alone (RoutesReady=False/NameConflict, see applyOptional).
func ReconcileGateway(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if !forgejo.GatewayEnabled(f) {
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionRoutesReady)
		if err := deleteOptional(ctx, rc, forgejo.HTTPRouteName(f), forgejo.HTTPRouteGVK); err != nil {
			return forgejo.StepResult{}, err
		}
		if err := deleteOptional(ctx, rc, forgejo.SSHRouteName(f), forgejo.TCPRouteGVKs...); err != nil {
			return forgejo.StepResult{}, err
		}
		return forgejo.StepResult{}, nil
	}

	if served, err := rc.APIServed(forgejo.HTTPRouteGVK); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("discovering HTTPRoute: %w", err)
	} else if !served {
		setRoutesReady(rc, metav1.ConditionFalse, reasonGatewayAPIUnavailable,
			"spec.gateway is set, but the cluster does not serve HTTPRoute "+forgejo.HTTPRouteGVK.GroupVersion().String()+
				": install the Gateway API CRDs (and a Gateway controller)")
		return forgejo.StepResult{}, nil
	}

	var routes []routeStatus
	// conflicts are routes the operator would create, whose name another object has: the
	// other route is still reconciled.
	var conflicts []string
	hostnames := RouteHostnames(f)
	if len(hostnames) == 0 {
		if err := deleteOptional(ctx, rc, forgejo.HTTPRouteName(f), forgejo.HTTPRouteGVK); err != nil {
			return forgejo.StepResult{}, err
		}
	} else {
		route, err := applyRoute(ctx, rc, forgejo.HTTPRouteGVK, forgejo.HTTPRouteName(f), httpRoute(f, hostnames))
		var conflict *nameConflictError
		switch {
		case errors.As(err, &conflict):
			conflicts = append(conflicts, conflict.Error())
		case err != nil:
			return forgejo.StepResult{}, fmt.Errorf("HTTPRoute %s: %w", forgejo.HTTPRouteName(f), err)
		default:
			routes = append(routes, routeStatus{kind: "HTTPRoute", obj: route, parents: parentRefs(f, f.Spec.Gateway.ParentRefs)})
		}
	}

	tcpMissing := false
	if forgejo.GatewaySSHEnabled(f) {
		tcpGVK, ok, err := rc.FirstServedAPI(forgejo.TCPRouteGVKs...)
		if err != nil {
			return forgejo.StepResult{}, fmt.Errorf("discovering TCPRoute: %w", err)
		}
		if ok {
			route, err := applyRoute(ctx, rc, tcpGVK, forgejo.SSHRouteName(f), tcpRoute(f))
			var conflict *nameConflictError
			switch {
			case errors.As(err, &conflict):
				conflicts = append(conflicts, conflict.Error())
			case err != nil:
				return forgejo.StepResult{}, fmt.Errorf("TCPRoute %s: %w", forgejo.SSHRouteName(f), err)
			default:
				routes = append(routes, routeStatus{kind: "TCPRoute", obj: route, parents: parentRefs(f, f.Spec.Gateway.SSH.ParentRefs)})
			}
		} else {
			tcpMissing = true
		}
	} else if err := deleteOptional(ctx, rc, forgejo.SSHRouteName(f), forgejo.TCPRouteGVKs...); err != nil {
		return forgejo.StepResult{}, err
	}

	podsPending, recheck := false, time.Duration(0)
	if len(routes) > 0 {
		var err error
		if podsPending, recheck, err = serverPodsPending(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	}
	status, reason, msg, waiting := routesCondition(routes, podsPending)
	if tcpMissing && status == metav1.ConditionTrue {
		status, reason = metav1.ConditionFalse, reasonTCPRouteUnavailable
		msg = "spec.gateway.ssh is set, but the cluster serves no TCPRoute version (" + gvkVersions(forgejo.TCPRouteGVKs) +
			"; it is in the Gateway API experimental channel before 1.6): SSH is only reachable through Service " +
			forgejo.SSHServiceName(f) + ". " + msg
	} else if tcpMissing {
		msg += "; no TCPRoute version is served (" + gvkVersions(forgejo.TCPRouteGVKs) + ")"
	}
	if len(hostnames) == 0 {
		noHosts := "spec.gateway has no host name for HTTPRoute " + forgejo.HTTPRouteName(f) +
			": set spec.gateway.hostnames, or spec.server.domain or spec.server.rootURL to a DNS name. " +
			"The operator creates no HTTPRoute without host names, which would take every host name of its listeners"
		if len(routes) > 0 {
			noHosts += "; " + msg
		}
		status, reason, msg = metav1.ConditionFalse, reasonNoHostnames, noHosts
	}
	if len(conflicts) > 0 {
		m := strings.Join(conflicts, "; ")
		switch {
		case len(routes) > 0 || len(hostnames) == 0:
			m += "; " + msg
		case tcpMissing:
			m += "; no TCPRoute version is served (" + gvkVersions(forgejo.TCPRouteGVKs) + ")"
		}
		status, reason, msg = metav1.ConditionFalse, reasonNameConflict, m
	}
	setRoutesReady(rc, status, reason, msg)
	if status != metav1.ConditionTrue {
		return forgejo.StepResult{RequeueAfter: notReadyRequeue}, nil
	}
	if waiting {
		// Look again once the youngest Ready pod is past the grace period; while none is
		// Ready, a pod becoming Ready starts a pass.
		if recheck <= 0 {
			recheck = notReadyRequeue
		}
		return forgejo.StepResult{RequeueAfter: recheck}, nil
	}
	return forgejo.StepResult{}, nil
}

// serverPodsPending reports whether no Forgejo pod has been Ready for endpointsGrace, so that a
// Gateway finding no ready endpoints behind the routes is expected: Ready reports the pods, during
// installs, restarts, upgrades and restores alike. recheck is when the youngest Ready pod leaves
// the grace period (0 when no pod is Ready).
func serverPodsPending(ctx context.Context, rc *forgejo.ReconcileContext) (pending bool, recheck time.Duration, err error) {
	pods := &corev1.PodList{}
	if err := rc.Client.List(ctx, pods, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.SelectorLabels(rc.Instance))); err != nil {
		return false, 0, fmt.Errorf("listing Forgejo pods: %w", err)
	}
	now := time.Now()
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
				continue
			}
			wait := c.LastTransitionTime.Add(endpointsGrace).Sub(now)
			if wait <= 0 {
				return false, 0, nil
			}
			if recheck == 0 || wait < recheck {
				recheck = wait
			}
		}
	}
	return true, recheck, nil
}

// setRoutesReady sets RoutesReady and records an event when it changes to or from True, or
// from one problem to another.
func setRoutesReady(rc *forgejo.ReconcileContext, status metav1.ConditionStatus, reason, msg string) {
	setConditionWithEvent(rc, forgejov1.ConditionRoutesReady, status, reason, msg, "Gateway")
}

// setConditionWithEvent sets a condition and emits a Normal event when it becomes True, and a
// Warning event when it turns False or changes its reason while False. Unknown (waiting for a
// controller to report) emits nothing.
func setConditionWithEvent(rc *forgejo.ReconcileContext, condType string, status metav1.ConditionStatus, reason, msg, action string) {
	var prev *metav1.Condition
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, condType); c != nil {
		prev = c.DeepCopy() // SetCondition updates the element in place
	}
	rc.SetCondition(condType, status, reason, msg)
	switch {
	case status == metav1.ConditionTrue && (prev == nil || prev.Status != metav1.ConditionTrue):
		rc.Event(corev1.EventTypeNormal, condType, action, "%s", msg)
	case status == metav1.ConditionFalse && (prev == nil || prev.Status != metav1.ConditionFalse || prev.Reason != reason):
		rc.Event(corev1.EventTypeWarning, reason, action, "%s", msg)
	}
}

func gvkVersions(gvks []schema.GroupVersionKind) string {
	out := make([]string, 0, len(gvks))
	for _, g := range gvks {
		out = append(out, g.GroupVersion().String())
	}
	return strings.Join(out, ", ")
}

// parentRefs renders spec parent references with the defaults of the Gateway API.
func parentRefs(f *forgejov1.Forgejo, refs []forgejov1.GatewayParentReference) []gwParentRef {
	out := make([]gwParentRef, 0, len(refs))
	for _, r := range refs {
		p := gwParentRef{
			Group:       r.Group,
			Kind:        r.Kind,
			Namespace:   r.Namespace,
			Name:        r.Name,
			SectionName: r.SectionName,
			Port:        r.Port,
		}
		if p.Group == "" {
			p.Group = forgejo.GatewayAPIGroup
		}
		if p.Kind == "" {
			p.Kind = kindGateway
		}
		if p.Namespace == f.Namespace {
			p.Namespace = ""
		}
		out = append(out, p)
	}
	return out
}

// RouteHostnames are the host names of the HTTPRoute: spec.gateway.hostnames, else the public
// host names (publicHosts). None means no HTTPRoute (RoutesReady=False/NoHostnames).
func RouteHostnames(f *forgejov1.Forgejo) []string {
	if f.Spec.Gateway != nil && len(f.Spec.Gateway.Hostnames) > 0 {
		return f.Spec.Gateway.Hostnames
	}
	return publicHosts(f)
}

// publicHosts are the host names clients reach the instance at, as far as they are DNS names:
// DOMAIN (spec.server.domain, ingress.host, a gateway host name or the host of
// spec.server.rootURL) and the host of the effective ROOT_URL. The in-cluster name of the HTTP
// Service, which both fall back to, is not one.
func publicHosts(f *forgejov1.Forgejo) []string {
	var out []string
	add := func(h string) {
		h = strings.ToLower(h)
		if isDNSName(h) && h != httpServiceFQDN(f) && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	add(Domain(f))
	if u, err := url.Parse(EffectiveRootURL(f)); err == nil {
		add(u.Hostname())
	}
	return out
}

// isDNSName reports whether h is a DNS name (not an IP address) that Gateway API hostnames
// and certificates accept.
func isDNSName(h string) bool {
	return h != "" && net.ParseIP(h) == nil && len(validation.IsDNS1123Subdomain(h)) == 0
}

// gatewayHost is the public host name behind the HTTPRoute: spec.server.domain, else the first
// spec.gateway.hostnames entry that is not a wildcard; "" without spec.gateway.
func gatewayHost(f *forgejov1.Forgejo) string {
	if !forgejo.GatewayEnabled(f) {
		return ""
	}
	if f.Spec.Server.Domain != "" {
		return f.Spec.Server.Domain
	}
	for _, h := range f.Spec.Gateway.Hostnames {
		if !strings.HasPrefix(h, "*") {
			return h
		}
	}
	return ""
}

// gatewayPrefix is the path prefix the HTTPRoute matches, without a trailing slash ("/" for
// the root).
func gatewayPrefix(f *forgejov1.Forgejo) string {
	if f.Spec.Gateway == nil {
		return "/"
	}
	if p := strings.TrimRight(f.Spec.Gateway.Path, "/"); p != "" {
		return p
	}
	return "/"
}

// gatewayPath is the path of ROOT_URL behind the HTTPRoute, with a trailing slash.
func gatewayPath(f *forgejov1.Forgejo) string {
	if p := gatewayPrefix(f); p != "/" {
		return p + "/"
	}
	return "/"
}

func requestTimeout(g *forgejov1.GatewaySpec) string {
	if g.RequestTimeout == nil {
		return defaultRequestTimeout
	}
	return *g.RequestTimeout
}

// removedHeaders are X-Real-IP and spec.gateway.removeRequestHeaders (without duplicates;
// header names are case-insensitive).
func removedHeaders(g *forgejov1.GatewaySpec) []string {
	out := []string{realIPHeader}
	for _, h := range g.RemoveRequestHeaders {
		if !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, h) }) {
			out = append(out, h)
		}
	}
	return out
}

// httpRoute renders HTTPRoute "<name>" for the given host names: the path prefix to Service
// "<name>-http", without X-Real-IP, with the prefix removed and no request timeout (by default).
func httpRoute(f *forgejov1.Forgejo, hostnames []string) *httpRouteSpec {
	g := f.Spec.Gateway
	prefix := gatewayPrefix(f)
	rule := httpRouteRule{
		Matches: []httpRouteMatch{{Path: httpPathMatch{Type: pathPrefixType, Value: prefix}}},
		BackendRefs: []gwBackendRef{{
			Kind: kindService, Name: forgejo.HTTPServiceName(f), Port: forgejo.HTTPServicePort(f), Weight: 1,
		}},
	}
	rule.Filters = append(rule.Filters, httpRouteFilter{
		Type:                  filterRequestHeaderModifier,
		RequestHeaderModifier: &httpHeaderModifier{Remove: removedHeaders(g)},
	})
	if prefix != "/" {
		rule.Filters = append(rule.Filters, httpRouteFilter{
			Type:       "URLRewrite",
			URLRewrite: &httpURLRewrite{Path: httpPathModifier{Type: "ReplacePrefixMatch", ReplacePrefixMatch: "/"}},
		})
	}
	if t := requestTimeout(g); t != "" {
		rule.Timeouts = &httpRouteTimeouts{Request: t}
	}
	return &httpRouteSpec{
		ParentRefs: parentRefs(f, g.ParentRefs),
		Hostnames:  hostnames,
		Rules:      []httpRouteRule{rule},
	}
}

// tcpRoute renders TCPRoute "<name>-ssh" to Service "<name>-ssh".
func tcpRoute(f *forgejov1.Forgejo) *tcpRouteSpec {
	return &tcpRouteSpec{
		ParentRefs: parentRefs(f, f.Spec.Gateway.SSH.ParentRefs),
		Rules: []tcpRouteRule{{BackendRefs: []gwBackendRef{{
			Kind: kindService, Name: forgejo.SSHServiceName(f), Port: forgejo.SSHServicePort(f), Weight: 1,
		}}}},
	}
}

// applyRoute creates or updates a route owned by the instance and returns it as stored,
// status included.
func applyRoute(ctx context.Context, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind, name string, spec any) (*unstructured.Unstructured, error) {
	f := rc.Instance
	var annotations map[string]string
	if f.Spec.Gateway != nil {
		annotations = f.Spec.Gateway.Annotations
	}
	return applyOptional(ctx, rc, gvk, name, spec, annotations)
}

// nameConflictError reports an object of an optional integration that has the name the
// operator would use, but that the instance does not control. The operator neither adopts nor
// changes it; callers report it in a condition (NameConflict) instead of failing the step.
type nameConflictError struct {
	kind, name string
	// controller is the object's controller reference, nil when it has none.
	controller *metav1.OwnerReference
}

func (e *nameConflictError) Error() string {
	by := "it has no controller"
	if c := e.controller; c != nil {
		by = fmt.Sprintf("it is controlled by %s %s", c.Kind, c.Name)
	}
	return fmt.Sprintf("%s %s already exists and does not belong to this Forgejo (%s): the operator does not take "+
		"over or change it. Delete it (or have its owner delete it) for the operator to create its own", e.kind, e.name, by)
}

// controlledBy reports whether the instance may write an existing optional object: it is the
// object's controller, or the object has no controller and carries the instance's labels (it
// was left behind when an earlier instance of this name was deleted with orphaning).
func controlledBy(f *forgejov1.Forgejo, obj client.Object) (bool, *metav1.OwnerReference) {
	c := metav1.GetControllerOf(obj)
	if c != nil {
		return c.UID == f.UID, c
	}
	labels := obj.GetLabels()
	for k, v := range forgejo.Labels(f, forgejo.ComponentServer) {
		if labels[k] != v {
			return false, nil
		}
	}
	return true, nil
}

// applyOptional creates or updates a namespaced object of an optional integration (Gateway
// API, cert-manager), owned by the instance, whose spec is rendered from a typed mirror (a
// pointer to a struct). It returns the object as stored, status included. An existing object
// is only updated while the instance controls it (controlledBy); otherwise nothing is
// written, and the error is a *nameConflictError.
func applyOptional(ctx context.Context, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind, name string,
	spec any, annotations map[string]string) (*unstructured.Unstructured, error) {
	f := rc.Instance
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(spec)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(f.Namespace)
	obj.SetName(name)
	optionalAbsent.Delete(optionalKey(f, gvk.GroupKind(), name))
	err = apply(ctx, rc, obj, true, func() error {
		if obj.GetResourceVersion() != "" {
			if ok, c := controlledBy(f, obj); !ok {
				return &nameConflictError{kind: gvk.Kind, name: name, controller: c}
			}
		}
		obj.SetLabels(mergeMaps(obj.GetLabels(), forgejo.Labels(f, forgejo.ComponentServer)))
		if a := mergeMaps(obj.GetAnnotations(), annotations); len(a) > 0 {
			obj.SetAnnotations(a)
		}
		obj.Object["spec"] = content
		return nil
	})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

// optionalAbsent remembers the optional children (routes, Certificate, ReferenceGrant) that
// this process has made sure are gone, so that an instance that does not use a feature costs
// no API request per pass: these kinds are read live, not from the informer cache, and asking
// the RESTMapper for a kind the cluster does not serve queries API discovery every time.
// Only applyOptional creates these objects, and it clears the entry first.
var optionalAbsent sync.Map // optionalKey -> struct{}

func optionalKey(f *forgejov1.Forgejo, gk schema.GroupKind, name string) string {
	return f.Namespace + "/" + f.Name + "/" + string(f.UID) + "/" + gk.String() + "/" + name
}

// deleteOptional deletes an optional child the instance controls, in the first of gvks (the
// versions of one kind) that the cluster serves. Once it is gone, or discovery says that the
// kind is not served, it is not looked up again in this process until applyOptional creates
// it. A failed discovery is returned, and nothing is remembered, so that the pass retries.
func deleteOptional(ctx context.Context, rc *forgejo.ReconcileContext, name string, gvks ...schema.GroupVersionKind) error {
	key := optionalKey(rc.Instance, gvks[0].GroupKind(), name)
	if _, ok := optionalAbsent.Load(key); ok {
		return nil
	}
	gvk, served, err := rc.FirstServedAPI(gvks...)
	if err != nil {
		return fmt.Errorf("discovering %s: %w", gvks[0].Kind, err)
	}
	if served {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk)
		if err := deleteOwned(ctx, rc, obj, name); err != nil {
			return fmt.Errorf("%s %s: %w", gvk.Kind, name, err)
		}
	}
	optionalAbsent.Store(key, struct{}{})
	return nil
}

// ForgetInstance drops the in-memory state of a deleted instance.
func ForgetInstance(namespace, name string) {
	prefix := namespace + "/" + name + "/"
	for _, m := range []*sync.Map{&optionalAbsent, &foreignHostKeys} {
		m.Range(func(k, _ any) bool {
			if strings.HasPrefix(k.(string), prefix) {
				m.Delete(k)
			}
			return true
		})
	}
}

// routeStatus is one route and the parents its spec names.
type routeStatus struct {
	kind    string
	obj     *unstructured.Unstructured
	parents []gwParentRef
}

// routesCondition summarizes the routes' status.parents: True when every route is accepted
// by every parent of its spec and its references resolve; False (NotAccepted or
// RefsNotResolved) when a controller reports a problem; Unknown (Pending) while a parent has
// not reported on the route's current generation. While podsPending, a backend without ready
// endpoints counts as resolved, and waiting says that one did.
func routesCondition(routes []routeStatus, podsPending bool) (status metav1.ConditionStatus, reason, msg string, waiting bool) {
	var failed, pending, ok []string
	failReason := ""
	for _, r := range routes {
		for _, p := range r.parents {
			what := fmt.Sprintf("%s %s on %s", r.kind, r.obj.GetName(), parentName(r.obj.GetNamespace(), p))
			conds, found := parentConditions(r.obj, p)
			if !found {
				pending = append(pending, what+": not reported by a Gateway controller yet")
				continue
			}
			accepted := findCondition(conds, "Accepted")
			if accepted == nil || stale(accepted, r.obj.GetGeneration()) {
				pending = append(pending, what+": waiting for the Gateway controller")
				continue
			}
			if accepted.Status != metav1.ConditionTrue {
				failed = append(failed, fmt.Sprintf("%s: not accepted (%s): %s", what, accepted.Reason, accepted.Message))
				if failReason == "" {
					failReason = reasonRoutesNotAccepted
				}
				continue
			}
			if refs := findCondition(conds, "ResolvedRefs"); refs != nil && refs.Status != metav1.ConditionTrue {
				if podsPending && refs.Reason == gwReasonEndpointsNotFound {
					ok = append(ok, what+" (no ready Forgejo pod behind it yet)")
					waiting = true
					continue
				}
				failed = append(failed, fmt.Sprintf("%s: references not resolved (%s): %s", what, refs.Reason, refs.Message))
				if failReason == "" {
					failReason = reasonRoutesRefsNotResolved
				}
				continue
			}
			ok = append(ok, what)
		}
	}
	switch {
	case len(failed) > 0:
		return metav1.ConditionFalse, failReason, strings.Join(append(failed, pending...), "; "), false
	case len(pending) > 0:
		return metav1.ConditionUnknown, reasonRoutesPending, strings.Join(pending, "; "), false
	}
	return metav1.ConditionTrue, reasonRoutesAccepted, "Accepted: " + strings.Join(ok, "; "), waiting
}

func parentName(routeNamespace string, p gwParentRef) string {
	ns := p.Namespace
	if ns == "" {
		ns = routeNamespace
	}
	s := ns + "/" + p.Name
	if p.Kind != kindGateway {
		s = p.Kind + " " + s
	}
	if p.SectionName != "" {
		s += " listener " + p.SectionName
	}
	if p.Port != nil {
		s += fmt.Sprintf(" port %d", *p.Port)
	}
	return s
}

// parentConditions returns the conditions of the route's status.parents entry for p. Of
// several entries (one per Gateway controller), an accepted one wins.
func parentConditions(route *unstructured.Unstructured, p gwParentRef) ([]metav1.Condition, bool) {
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	var best []metav1.Condition
	found := false
	for _, item := range parents {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ref, _, _ := unstructured.NestedMap(entry, "parentRef")
		if !sameParent(route.GetNamespace(), p, ref) {
			continue
		}
		conds := decodeConditions(entry["conditions"])
		if a := findCondition(conds, "Accepted"); !found || (a != nil && a.Status == metav1.ConditionTrue) {
			best, found = conds, true
		}
	}
	return best, found
}

// sameParent compares a spec parent reference with one from status.parents, applying the
// Gateway API defaults to both.
func sameParent(routeNamespace string, p gwParentRef, ref map[string]any) bool {
	str := func(k, def string) string {
		if v, ok := ref[k].(string); ok && v != "" {
			return v
		}
		return def
	}
	pns := p.Namespace
	if pns == "" {
		pns = routeNamespace
	}
	if str("group", forgejo.GatewayAPIGroup) != p.Group || str("kind", kindGateway) != p.Kind ||
		str("namespace", routeNamespace) != pns || str("name", "") != p.Name || str("sectionName", "") != p.SectionName {
		return false
	}
	var port int64
	switch v := ref["port"].(type) {
	case int64:
		port = v
	case float64:
		port = int64(v)
	}
	if p.Port == nil {
		return port == 0
	}
	return port == int64(*p.Port)
}

// decodeConditions converts unstructured conditions; malformed entries are skipped.
func decodeConditions(v any) []metav1.Condition {
	items, _ := v.([]any)
	out := make([]metav1.Condition, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c metav1.Condition
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &c); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

func findCondition(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

// stale reports whether a condition describes an older generation of its object. A condition
// without observedGeneration (optional in the Gateway API and cert-manager) counts as current.
func stale(c *metav1.Condition, generation int64) bool {
	return c.ObservedGeneration != 0 && c.ObservedGeneration < generation
}
