package core

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

var (
	tcpRouteV1       = forgejo.TCPRouteGVKs[0]
	tcpRouteV1alpha2 = forgejo.TCPRouteGVKs[1]
	referenceGrantV1 = forgejo.ReferenceGrantGVKs[0]
)

// allOptionalAPIs are the kinds of a cluster with the Gateway API 1.6 experimental channel and
// cert-manager.
var allOptionalAPIs = []schema.GroupVersionKind{
	forgejo.HTTPRouteGVK, tcpRouteV1, tcpRouteV1alpha2, referenceGrantV1, forgejo.CertificateGVK,
}

// optionalRC is a reconcile context whose RESTMapper serves the given optional kinds. The
// fake client knows all of them, so tests can tell that nothing was created for a kind the
// "cluster" does not serve.
func optionalRC(t *testing.T, f *forgejov1.Forgejo, served []schema.GroupVersionKind) (*forgejo.ReconcileContext, *events.FakeRecorder) {
	t.Helper()
	f.UID = types.UID(t.Name()) // the deletions this process made are remembered per UID
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	for _, gvk := range allOptionalAPIs {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	for _, gvk := range served {
		mapper.Add(gvk, meta.RESTScopeNamespace)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	rec := events.NewFakeRecorder(50)
	return &forgejo.ReconcileContext{Client: c, Scheme: s, Recorder: rec, RESTMapper: mapper, Instance: f}, rec
}

func getOptional(t *testing.T, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: rc.Instance.Namespace, Name: name}, u); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

func gatewayInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.Server.Domain = "git.example.com"
	f.Spec.Server.SSH.ExternalPort = 2222
	f.Spec.Gateway = &forgejov1.GatewaySpec{
		ParentRefs: []forgejov1.GatewayParentReference{{Name: "eg", Namespace: "gateways"}},
		SSH: &forgejov1.GatewaySSHSpec{ParentRefs: []forgejov1.GatewayParentReference{
			{Name: "eg", Namespace: "gateways", SectionName: "ssh"},
		}},
	}
	return f
}

func condition(f *forgejov1.Forgejo, t string) *metav1.Condition {
	return meta.FindStatusCondition(f.Status.Conditions, t)
}

func runStep(t *testing.T, rc *forgejo.ReconcileContext, step forgejo.Step) forgejo.StepResult {
	t.Helper()
	res, err := step(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func nested(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, found, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !found {
		t.Fatalf("%s: %v missing (%v)", u.GetName(), path, err)
	}
	return v
}

func TestGatewayRendersRoutes(t *testing.T) {
	f := gatewayInstance()
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	res := runStep(t, rc, ReconcileGateway)

	route := getOptional(t, rc, forgejo.HTTPRouteGVK, "fj")
	if route == nil {
		t.Fatal("no HTTPRoute")
	}
	if !metav1.IsControlledBy(route, f) || route.GetLabels()[forgejo.LabelInstance] != "fj" {
		t.Errorf("HTTPRoute not owned and labelled: %v %v", route.GetOwnerReferences(), route.GetLabels())
	}
	want := map[string]any{
		"parentRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": "gateways", "name": "eg"}},
		"hostnames":  []any{"git.example.com"},
		"rules": []any{map[string]any{
			"matches": []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/"}}},
			"filters": []any{map[string]any{
				"type":                  "RequestHeaderModifier",
				"requestHeaderModifier": map[string]any{"remove": []any{"X-Real-IP"}},
			}},
			"backendRefs": []any{map[string]any{"group": "", "kind": "Service", "name": "fj-http", "port": int64(3000), "weight": int64(1)}},
			"timeouts":    map[string]any{"request": "0s"},
		}},
	}
	if got := route.Object["spec"]; !reflect.DeepEqual(got, want) {
		t.Errorf("HTTPRoute spec\n got %#v\nwant %#v", got, want)
	}

	// TCPRoute v1 is preferred to v1alpha2.
	ssh := getOptional(t, rc, tcpRouteV1, "fj-ssh")
	if ssh == nil {
		t.Fatal("no TCPRoute v1")
	}
	wantSSH := map[string]any{
		"parentRefs": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": "gateways", "name": "eg", "sectionName": "ssh"}},
		"rules": []any{map[string]any{"backendRefs": []any{
			map[string]any{"group": "", "kind": "Service", "name": "fj-ssh", "port": int64(22), "weight": int64(1)},
		}}},
	}
	if got := ssh.Object["spec"]; !reflect.DeepEqual(got, wantSSH) {
		t.Errorf("TCPRoute spec\n got %#v\nwant %#v", got, wantSSH)
	}

	c := condition(f, forgejov1.ConditionRoutesReady)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != reasonRoutesPending {
		t.Fatalf("RoutesReady before any controller reported: %+v", c)
	}
	if res.RequeueAfter == 0 {
		t.Error("pending routes must be looked at again soon")
	}

	// An unchanged spec does not update the routes.
	rv := route.GetResourceVersion()
	runStep(t, rc, ReconcileGateway)
	if again := getOptional(t, rc, forgejo.HTTPRouteGVK, "fj"); again.GetResourceVersion() != rv {
		t.Errorf("unchanged HTTPRoute was updated: %s -> %s", rv, again.GetResourceVersion())
	}
}

func TestGatewayRouteOptions(t *testing.T) {
	f := gatewayInstance()
	f.Spec.Gateway.SSH = nil
	f.Spec.Gateway.Path = "/git/"
	f.Spec.Gateway.Hostnames = []string{"*.example.com", "code.example.com"}
	f.Spec.Gateway.RequestTimeout = ptr.To("")
	f.Spec.Gateway.RemoveRequestHeaders = []string{"x-real-ip", "X-Forwarded-For"}
	f.Spec.Gateway.ParentRefs[0].Namespace = f.Namespace // same namespace: left out
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileGateway)

	route := getOptional(t, rc, forgejo.HTTPRouteGVK, "fj")
	rule := nested(t, route, "spec", "rules").([]any)[0].(map[string]any)
	if _, ok := rule["timeouts"]; ok {
		t.Errorf("requestTimeout \"\" must leave timeouts out: %v", rule)
	}
	// X-Real-IP is removed whatever else is listed, once.
	wantFilters := []any{map[string]any{
		"type":                  "RequestHeaderModifier",
		"requestHeaderModifier": map[string]any{"remove": []any{"X-Real-IP", "X-Forwarded-For"}},
	}, map[string]any{
		"type":       "URLRewrite",
		"urlRewrite": map[string]any{"path": map[string]any{"type": "ReplacePrefixMatch", "replacePrefixMatch": "/"}},
	}}
	if !reflect.DeepEqual(rule["filters"], wantFilters) {
		t.Errorf("filters: got %#v, want X-Real-IP and X-Forwarded-For removed, then the prefix rewrite", rule["filters"])
	}
	if got := rule["matches"]; !reflect.DeepEqual(got, []any{map[string]any{"path": map[string]any{"type": "PathPrefix", "value": "/git"}}}) {
		t.Errorf("matches: %#v", got)
	}
	if got := nested(t, route, "spec", "hostnames"); !reflect.DeepEqual(got, []any{"*.example.com", "code.example.com"}) {
		t.Errorf("hostnames: %#v", got)
	}
	if ref := nested(t, route, "spec", "parentRefs").([]any)[0].(map[string]any); ref["namespace"] != nil {
		t.Errorf("parentRef in the instance's namespace: %v", ref)
	}
	if getOptional(t, rc, tcpRouteV1, "fj-ssh") != nil {
		t.Error("TCPRoute without spec.gateway.ssh")
	}

	// The prefix is part of ROOT_URL; spec.server.domain stays the public host.
	if got := RootURL(f); got != "http://git.example.com/git/" {
		t.Errorf("ROOT_URL %q", got)
	}
	f.Spec.Server.Domain = ""
	if got, d := RootURL(f), Domain(f); got != "http://code.example.com/git/" || d != "code.example.com" {
		t.Errorf("without a domain: ROOT_URL %q, DOMAIN %q, want the first host name that is not a wildcard", got, d)
	}
}

// Without gateway.hostnames, the HTTPRoute and the certificate take the public host names:
// DOMAIN and the host of ROOT_URL, wherever they come from.
func TestRouteHostnamesFromPublicHosts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *forgejov1.Forgejo)
		want   []string
	}{
		{"domain", func(*forgejov1.Forgejo) {}, []string{"git.example.com"}},
		{"rootURL", func(f *forgejov1.Forgejo) {
			f.Spec.Server.Domain = ""
			f.Spec.Server.RootURL = "https://Code.Example.com/"
		}, []string{"code.example.com"}},
		{"ROOT_URL in spec.config", func(f *forgejov1.Forgejo) {
			f.Spec.Server.Domain = ""
			f.Spec.Config = map[string]map[string]string{"server": {"ROOT_URL": "https://code.example.com/"}}
		}, []string{"code.example.com"}},
		{"domain and another rootURL", func(f *forgejov1.Forgejo) {
			f.Spec.Server.RootURL = "https://code.example.com/"
		}, []string{"git.example.com", "code.example.com"}},
		{"ingress host", func(f *forgejov1.Forgejo) {
			f.Spec.Server.Domain = ""
			f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: "www.example.com"}
		}, []string{"www.example.com"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewayInstance()
			f.Spec.TLS = &forgejov1.TLSSpec{CertManager: &forgejov1.CertManagerTLSSpec{IssuerRef: forgejov1.CertManagerIssuerRef{Name: "ca"}}}
			tc.mutate(f)
			rc, _ := optionalRC(t, f, allOptionalAPIs)
			runStep(t, rc, ReconcileGateway)
			route := getOptional(t, rc, forgejo.HTTPRouteGVK, "fj")
			if route == nil {
				t.Fatal("no HTTPRoute")
			}
			want := make([]any, 0, len(tc.want))
			for _, h := range tc.want {
				want = append(want, h)
			}
			if got := nested(t, route, "spec", "hostnames"); !reflect.DeepEqual(got, want) {
				t.Errorf("hostnames %v, want %v", got, tc.want)
			}
			if got := CertificateDNSNames(f); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("certificate DNS names %v, want %v", got, tc.want)
			}
		})
	}
}

// An HTTPRoute without host names would take every host name of its listeners, also other
// applications' on a shared Gateway: without a DNS name, the operator creates none.
func TestNoHTTPRouteWithoutHostnames(t *testing.T) {
	for name, mutate := range map[string]func(f *forgejov1.Forgejo){
		"parentRefs only": func(f *forgejov1.Forgejo) { f.Spec.Server.Domain = "" },
		"IP address":      func(f *forgejov1.Forgejo) { f.Spec.Server.Domain = "192.0.2.10" },
	} {
		t.Run(name, func(t *testing.T) {
			f := gatewayInstance()
			rc, _ := optionalRC(t, f, allOptionalAPIs)
			runStep(t, rc, ReconcileGateway)
			if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") == nil {
				t.Fatal("no HTTPRoute with spec.server.domain")
			}

			mutate(f)
			res := runStep(t, rc, ReconcileGateway)
			if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") != nil {
				t.Error("an HTTPRoute without host names must not exist")
			}
			if getOptional(t, rc, tcpRouteV1, "fj-ssh") == nil {
				t.Error("the TCPRoute needs no host name")
			}
			c := condition(f, forgejov1.ConditionRoutesReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonNoHostnames ||
				!strings.Contains(c.Message, "spec.gateway.hostnames") || !strings.Contains(c.Message, "TCPRoute fj-ssh") {
				t.Errorf("RoutesReady: %+v", c)
			}
			if res.RequeueAfter == 0 {
				t.Error("RoutesReady=False must be looked at again")
			}

			f.Spec.Gateway.Hostnames = []string{"git.example.org"}
			runStep(t, rc, ReconcileGateway)
			route := getOptional(t, rc, forgejo.HTTPRouteGVK, "fj")
			if route == nil || !reflect.DeepEqual(nested(t, route, "spec", "hostnames"), []any{"git.example.org"}) {
				t.Errorf("explicit host names: %v", route)
			}
		})
	}
}

// Clients of a LoadBalancer or NodePort Service reach Forgejo with a node's private address
// (externalTrafficPolicy Cluster), so only loopback is trusted by default.
func TestTrustedProxiesForDirectServices(t *testing.T) {
	for typ, want := range map[corev1.ServiceType]string{
		corev1.ServiceTypeClusterIP:    defaultTrustedProxies,
		"":                             defaultTrustedProxies,
		corev1.ServiceTypeNodePort:     "127.0.0.0/8,::1/128",
		corev1.ServiceTypeLoadBalancer: "127.0.0.0/8,::1/128",
	} {
		f := testInstance()
		f.Spec.Server.HTTP.Type = typ
		if got := trustedProxies(f); got != want {
			t.Errorf("%q: %q, want %q", typ, got, want)
		}
	}
	f := testInstance()
	f.Spec.Server.HTTP.Type = corev1.ServiceTypeLoadBalancer
	f.Spec.Server.TrustedProxies = []string{"10.42.0.0/16"}
	if got := trustedProxies(f); got != "10.42.0.0/16" {
		t.Errorf("spec.server.trustedProxies must win: %q", got)
	}
}

func TestTCPRouteVersionDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		served []schema.GroupVersionKind
		want   *schema.GroupVersionKind
	}{
		{"v1 and v1alpha2", []schema.GroupVersionKind{forgejo.HTTPRouteGVK, tcpRouteV1, tcpRouteV1alpha2}, &tcpRouteV1},
		{"v1alpha2 only", []schema.GroupVersionKind{forgejo.HTTPRouteGVK, tcpRouteV1alpha2}, &tcpRouteV1alpha2},
		{"standard channel without TCPRoute", []schema.GroupVersionKind{forgejo.HTTPRouteGVK}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gatewayInstance()
			rc, _ := optionalRC(t, f, tc.served)
			runStep(t, rc, ReconcileGateway)
			for _, gvk := range forgejo.TCPRouteGVKs {
				got := getOptional(t, rc, gvk, "fj-ssh") != nil
				if want := tc.want != nil && *tc.want == gvk; got != want {
					t.Errorf("TCPRoute %s created: %v, want %v", gvk.Version, got, want)
				}
			}
			if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") == nil {
				t.Error("the HTTPRoute does not depend on TCPRoute")
			}
			if tc.want != nil {
				return
			}
			// Once the HTTPRoute is accepted, the missing TCPRoute is what RoutesReady reports.
			acceptRoute(t, rc, forgejo.HTTPRouteGVK, "fj", "True", "True")
			runStep(t, rc, ReconcileGateway)
			c := condition(f, forgejov1.ConditionRoutesReady)
			if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonTCPRouteUnavailable ||
				!strings.Contains(c.Message, "fj-ssh") {
				t.Errorf("RoutesReady: %+v", c)
			}
		})
	}
}

// acceptRoute writes a status.parents entry for every parentRef of the stored route, as a
// Gateway controller would.
func acceptRoute(t *testing.T, rc *forgejo.ReconcileContext, gvk schema.GroupVersionKind, name, accepted, resolved string) {
	t.Helper()
	route := getOptional(t, rc, gvk, name)
	refs := nested(t, route, "spec", "parentRefs").([]any)
	parents := make([]any, 0, len(refs))
	for _, r := range refs {
		parents = append(parents, map[string]any{
			"controllerName": "gateway.envoyproxy.io/gatewayclass-controller",
			"parentRef":      r,
			"conditions": []any{
				map[string]any{"type": "Accepted", "status": accepted, "reason": "Accepted", "message": "accepted?",
					"lastTransitionTime": "2026-10-22T00:00:00Z", "observedGeneration": route.GetGeneration()},
				map[string]any{"type": "ResolvedRefs", "status": resolved, "reason": "BackendNotFound", "message": "service not found",
					"lastTransitionTime": "2026-10-22T00:00:00Z", "observedGeneration": route.GetGeneration()},
			},
		})
	}
	if err := unstructured.SetNestedSlice(route.Object, parents, "status", "parents"); err != nil {
		t.Fatal(err)
	}
	if err := rc.Client.Update(context.Background(), route); err != nil {
		t.Fatal(err)
	}
}

func TestRoutesReadyFollowsRouteStatus(t *testing.T) {
	f := gatewayInstance()
	rc, rec := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileGateway)

	acceptRoute(t, rc, forgejo.HTTPRouteGVK, "fj", "True", "True")
	runStep(t, rc, ReconcileGateway)
	if c := condition(f, forgejov1.ConditionRoutesReady); c.Status != metav1.ConditionUnknown ||
		!strings.Contains(c.Message, "TCPRoute fj-ssh on gateways/eg listener ssh") {
		t.Fatalf("one route still pending: %+v", c)
	}

	acceptRoute(t, rc, tcpRouteV1, "fj-ssh", "True", "True")
	if res := runStep(t, rc, ReconcileGateway); res.RequeueAfter != 0 {
		t.Errorf("ready routes need no early requeue: %v", res.RequeueAfter)
	}
	if c := condition(f, forgejov1.ConditionRoutesReady); c.Status != metav1.ConditionTrue || c.Reason != reasonRoutesAccepted {
		t.Fatalf("both accepted: %+v", c)
	}

	acceptRoute(t, rc, tcpRouteV1, "fj-ssh", "True", "False")
	runStep(t, rc, ReconcileGateway)
	if c := condition(f, forgejov1.ConditionRoutesReady); c.Status != metav1.ConditionFalse || c.Reason != reasonRoutesRefsNotResolved ||
		!strings.Contains(c.Message, "BackendNotFound") {
		t.Fatalf("unresolved backend: %+v", c)
	}

	acceptRoute(t, rc, forgejo.HTTPRouteGVK, "fj", "False", "True")
	runStep(t, rc, ReconcileGateway)
	if c := condition(f, forgejov1.ConditionRoutesReady); c.Status != metav1.ConditionFalse || c.Reason != reasonRoutesNotAccepted {
		t.Fatalf("not accepted: %+v", c)
	}

	evs := strings.Join(drainEvents(rec), "\n")
	for _, want := range []string{"Normal RoutesReady", "Warning RefsNotResolved", "Warning NotAccepted"} {
		if !strings.Contains(evs, want) {
			t.Errorf("no %q event in:\n%s", want, evs)
		}
	}
}

func TestRoutesConditionIgnoresStaleStatus(t *testing.T) {
	route := &unstructured.Unstructured{}
	route.SetName("fj")
	route.SetNamespace("ns")
	route.SetGeneration(3)
	parent := gwParentRef{Group: forgejo.GatewayAPIGroup, Kind: "Gateway", Name: "eg"}
	cond := func(gen int64) []any {
		return []any{map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted",
			"lastTransitionTime": "2026-10-22T00:00:00Z", "observedGeneration": gen}}
	}
	_ = unstructured.SetNestedSlice(route.Object, []any{map[string]any{
		// A controller may write the defaulted fields and the namespace into status.
		"parentRef":  map[string]any{"group": forgejo.GatewayAPIGroup, "kind": "Gateway", "namespace": "ns", "name": "eg"},
		"conditions": cond(2),
	}}, "status", "parents")
	routes := []routeStatus{{kind: "HTTPRoute", obj: route, parents: []gwParentRef{parent}}}
	if s, r, _ := routesCondition(routes); s != metav1.ConditionUnknown || r != reasonRoutesPending {
		t.Errorf("status of generation 2 for generation 3: %s/%s", s, r)
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "status", "parents")
	parents[0].(map[string]any)["conditions"] = cond(3)
	_ = unstructured.SetNestedSlice(route.Object, parents, "status", "parents")
	if s, r, m := routesCondition(routes); s != metav1.ConditionTrue {
		t.Errorf("current status: %s/%s %s", s, r, m)
	}
	// Another listener of the same Gateway is another parent.
	routes[0].parents[0].SectionName = "https"
	if s, _, _ := routesCondition(routes); s != metav1.ConditionUnknown {
		t.Errorf("status of the whole Gateway must not count for one listener: %s", s)
	}
}

func TestRemovingGatewayDeletesRoutes(t *testing.T) {
	f := gatewayInstance()
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileGateway)
	if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") == nil || getOptional(t, rc, tcpRouteV1, "fj-ssh") == nil {
		t.Fatal("routes not created")
	}

	f.Spec.Gateway.SSH = nil
	runStep(t, rc, ReconcileGateway)
	if getOptional(t, rc, tcpRouteV1, "fj-ssh") != nil || getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") == nil {
		t.Fatal("removing spec.gateway.ssh must delete only the TCPRoute")
	}

	f.Spec.Gateway = nil
	runStep(t, rc, ReconcileGateway)
	if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") != nil {
		t.Fatal("removing spec.gateway must delete the HTTPRoute")
	}
	if condition(f, forgejov1.ConditionRoutesReady) != nil {
		t.Error("RoutesReady must go away with spec.gateway")
	}

	// A route of the same name that the instance does not control is left alone.
	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(forgejo.HTTPRouteGVK)
	foreign.SetNamespace(f.Namespace)
	foreign.SetName("fj")
	if err := rc.Client.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}
	ForgetInstance(f.Namespace, f.Name) // as after an operator restart
	runStep(t, rc, ReconcileGateway)
	if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") == nil {
		t.Error("a route the instance does not control was deleted")
	}
}

func TestNoRoutesOrCertificatesWithoutCRDs(t *testing.T) {
	f := gatewayInstance()
	f.Spec.TLS = &forgejov1.TLSSpec{
		CertManager:       &forgejov1.CertManagerTLSSpec{IssuerRef: forgejov1.CertManagerIssuerRef{Name: "ca", Kind: "ClusterIssuer"}},
		GatewayNamespaces: []string{"gateways"},
	}
	rc, _ := optionalRC(t, f, nil)
	if res := runStep(t, rc, ReconcileGateway); res.Stop {
		t.Error("missing CRDs must not stop the pipeline")
	}
	runStep(t, rc, ReconcileTLS)
	for _, gvk := range allOptionalAPIs {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := rc.Client.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
		if len(list.Items) > 0 {
			t.Errorf("%s created although the cluster does not serve it", gvk)
		}
	}
	if c := condition(f, forgejov1.ConditionRoutesReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonGatewayAPIUnavailable {
		t.Errorf("RoutesReady: %+v", c)
	}
	if c := condition(f, forgejov1.ConditionTLSReady); c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonCertManagerUnavailable {
		t.Errorf("TLSReady: %+v", c)
	}

	// An instance without spec.gateway and spec.tls gets no condition at all.
	plain := testInstance()
	rc, _ = optionalRC(t, plain, nil)
	runStep(t, rc, ReconcileGateway)
	runStep(t, rc, ReconcileTLS)
	if len(plain.Status.Conditions) != 0 {
		t.Errorf("conditions without spec.gateway/spec.tls: %+v", plain.Status.Conditions)
	}
}

func certInstance() *forgejov1.Forgejo {
	f := gatewayInstance()
	f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: "www.example.com"}
	f.Spec.Gateway.Hostnames = []string{"git.example.com", "*.git.example.com"}
	f.Spec.TLS = &forgejov1.TLSSpec{
		CertManager:       &forgejov1.CertManagerTLSSpec{IssuerRef: forgejov1.CertManagerIssuerRef{Name: "lab-ca", Kind: "ClusterIssuer"}},
		GatewayNamespaces: []string{"gateways", "ns", "gateways"},
	}
	return f
}

// setCertificateStatus writes the Certificate's conditions as cert-manager would.
func setCertificateStatus(t *testing.T, rc *forgejo.ReconcileContext, conds ...map[string]any) {
	t.Helper()
	cert := getOptional(t, rc, forgejo.CertificateGVK, "fj-tls")
	items := make([]any, 0, len(conds))
	for _, c := range conds {
		c["lastTransitionTime"] = "2026-10-22T00:00:00Z"
		items = append(items, c)
	}
	_ = unstructured.SetNestedSlice(cert.Object, items, "status", "conditions")
	_ = unstructured.SetNestedField(cert.Object, "2027-01-20T00:00:00Z", "status", "notAfter")
	if err := rc.Client.Update(context.Background(), cert); err != nil {
		t.Fatal(err)
	}
}

func TestCertManagerCertificate(t *testing.T) {
	f := certInstance()
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileTLS)

	cert := getOptional(t, rc, forgejo.CertificateGVK, "fj-tls")
	if cert == nil || !metav1.IsControlledBy(cert, f) {
		t.Fatalf("Certificate not created and owned: %v", cert)
	}
	want := map[string]any{
		"secretName": "fj-tls",
		"dnsNames":   []any{"git.example.com", "www.example.com", "*.git.example.com"},
		"issuerRef":  map[string]any{"name": "lab-ca", "kind": "ClusterIssuer", "group": "cert-manager.io"},
	}
	if got := cert.Object["spec"]; !reflect.DeepEqual(got, want) {
		t.Errorf("Certificate spec\n got %#v\nwant %#v", got, want)
	}

	grant := getOptional(t, rc, referenceGrantV1, "fj-tls")
	if grant == nil {
		t.Fatal("no ReferenceGrant")
	}
	wantGrant := map[string]any{
		"from": []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "namespace": "gateways"}},
		"to":   []any{map[string]any{"group": "", "kind": "Secret", "name": "fj-tls"}},
	}
	if got := grant.Object["spec"]; !reflect.DeepEqual(got, wantGrant) {
		t.Errorf("ReferenceGrant spec\n got %#v\nwant %#v", got, wantGrant)
	}

	for _, tc := range []struct {
		conds  []map[string]any
		status metav1.ConditionStatus
		reason string
	}{
		{nil, metav1.ConditionUnknown, reasonTLSPending},
		{[]map[string]any{
			{"type": "Ready", "status": "False", "reason": "DoesNotExist", "message": "Issuing certificate as Secret does not exist"},
			{"type": "Issuing", "status": "True", "reason": "DoesNotExist"},
		}, metav1.ConditionUnknown, reasonTLSPending},
		{[]map[string]any{{"type": "Ready", "status": "True", "reason": "Ready", "message": "up to date"}}, metav1.ConditionTrue, reasonTLSReady},
		{[]map[string]any{{"type": "Ready", "status": "False", "reason": "Expired", "message": "expired"}}, metav1.ConditionFalse, reasonCertificateNotReady},
	} {
		if tc.conds != nil {
			setCertificateStatus(t, rc, tc.conds...)
		}
		runStep(t, rc, ReconcileTLS)
		c := condition(f, forgejov1.ConditionTLSReady)
		if c == nil || c.Status != tc.status || c.Reason != tc.reason {
			t.Errorf("Certificate conditions %v: TLSReady %+v, want %s/%s", tc.conds, c, tc.status, tc.reason)
		}
		if tc.status == metav1.ConditionTrue && (!strings.Contains(c.Message, "2027-01-20") || !strings.Contains(c.Message, "ReferenceGrant fj-tls")) {
			t.Errorf("TLSReady message: %s", c.Message)
		}
	}

	// The Ingress serves the certificate's Secret, and ROOT_URL is https.
	if err := reconcileIngress(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	ing := &networkingv1.Ingress{}
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: f.Namespace, Name: "fj"}, ing); err != nil {
		t.Fatal(err)
	}
	if len(ing.Spec.TLS) != 1 || ing.Spec.TLS[0].SecretName != "fj-tls" || !reflect.DeepEqual(ing.Spec.TLS[0].Hosts, []string{"www.example.com"}) {
		t.Errorf("Ingress TLS: %+v", ing.Spec.TLS)
	}
	if got := RootURL(f); got != "https://www.example.com/" {
		t.Errorf("ROOT_URL with the Ingress: %q", got)
	}
	f.Spec.Ingress = nil
	if got := RootURL(f); got != "https://git.example.com/" {
		t.Errorf("ROOT_URL with the HTTPRoute: %q", got)
	}

	// Removing spec.tls deletes the Certificate and the grant, and TLSReady.
	f.Spec.TLS = nil
	runStep(t, rc, ReconcileTLS)
	if getOptional(t, rc, forgejo.CertificateGVK, "fj-tls") != nil || getOptional(t, rc, referenceGrantV1, "fj-tls") != nil {
		t.Error("Certificate or ReferenceGrant left after removing spec.tls")
	}
	if condition(f, forgejov1.ConditionTLSReady) != nil {
		t.Error("TLSReady left after removing spec.tls")
	}
	if got := RootURL(f); got != "http://git.example.com/" {
		t.Errorf("ROOT_URL without spec.tls: %q", got)
	}
}

func TestCertificateNeedsDNSNames(t *testing.T) {
	f := testInstance()
	f.Spec.TLS = &forgejov1.TLSSpec{CertManager: &forgejov1.CertManagerTLSSpec{IssuerRef: forgejov1.CertManagerIssuerRef{Name: "ca"}}}
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileTLS)
	if c := condition(f, forgejov1.ConditionTLSReady); c == nil || c.Reason != reasonNoDNSNames {
		t.Errorf("TLSReady: %+v", c)
	}
	if getOptional(t, rc, forgejo.CertificateGVK, "fj-tls") != nil {
		t.Error("Certificate without DNS names")
	}

	// An IP address is not a DNS name; explicit dnsNames win, and the issuer kind defaults.
	f.Spec.Server.Domain = "10.0.0.1"
	f.Spec.TLS.CertManager.DNSNames = []string{"git.internal"}
	runStep(t, rc, ReconcileTLS)
	cert := getOptional(t, rc, forgejo.CertificateGVK, "fj-tls")
	if got := nested(t, cert, "spec", "dnsNames"); !reflect.DeepEqual(got, []any{"git.internal"}) {
		t.Errorf("dnsNames %v", got)
	}
	if got := nested(t, cert, "spec", "issuerRef", "kind"); got != "Issuer" {
		t.Errorf("issuerRef.kind %v", got)
	}
}

// Without spec.gateway and spec.tls, DOMAIN and ROOT_URL come from the domain, the Ingress
// and rootURL as documented in docs/networking.md.
func TestPublicIdentityWithoutGatewayOrTLS(t *testing.T) {
	ing := func(host, secret string) *forgejov1.IngressSpec {
		return &forgejov1.IngressSpec{Enabled: true, Host: host, TLSSecretName: secret}
	}
	for _, tc := range []struct {
		name         string
		server       forgejov1.ServerSpec
		ingress      *forgejov1.IngressSpec
		domain, root string
	}{
		{"defaults", forgejov1.ServerSpec{}, nil, "fj-http.ns.svc.cluster.local", "http://fj-http.ns.svc.cluster.local:3000/"},
		{"domain", forgejov1.ServerSpec{Domain: "git.example.com"}, nil, "git.example.com", "http://git.example.com/"},
		{"ingress", forgejov1.ServerSpec{}, ing("git.example.com", ""), "git.example.com", "http://git.example.com/"},
		{"ingress tls", forgejov1.ServerSpec{}, ing("git.example.com", "s"), "git.example.com", "https://git.example.com/"},
		{"rootURL", forgejov1.ServerSpec{RootURL: "https://x.example.com/f"}, nil, "x.example.com", "https://x.example.com/f/"},
	} {
		f := testInstance()
		f.Spec.Server = tc.server
		f.Spec.Ingress = tc.ingress
		if d, r := Domain(f), RootURL(f); d != tc.domain || r != tc.root {
			t.Errorf("%s: DOMAIN %q ROOT_URL %q, want %q %q", tc.name, d, r, tc.domain, tc.root)
		}
		if got := trustedProxies(f); got != defaultTrustedProxies {
			t.Errorf("%s: trusted proxies %q", tc.name, got)
		}
	}
}

func TestConfigWithGatewayAndTLS(t *testing.T) {
	ctx := context.Background()
	f := gatewayInstance()
	f.Spec.Server.Domain = ""
	f.Spec.Gateway.Hostnames = []string{"code.example.com"}
	f.Spec.TLS = &forgejov1.TLSSpec{}
	f.Spec.Server.TrustedProxies = []string{"10.42.0.0/16", "fd00::/8"}
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace}}
	rc := testContext(t, f, secrets)
	rc.TargetImage = img15
	rc.Layout = forgejo.LayoutForImage(img15)
	rc.DB = &forgejo.DBConnection{Host: "db", Port: 5432, Name: "forgejo", User: "forgejo", Password: "pw", SSLMode: "disable"}
	if _, err := ReconcileConfig(ctx, rc); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, sec); err != nil {
		t.Fatal(err)
	}
	ini := string(sec.Data[forgejo.ConfigSecretKey])
	server := section(ini, "server")
	for _, want := range []string{
		"DOMAIN = code.example.com", "ROOT_URL = https://code.example.com/",
		"SSH_DOMAIN = code.example.com", "SSH_PORT = 2222",
	} {
		if !strings.Contains(server, want+"\n") {
			t.Errorf("[server] lacks %q:\n%s", want, server)
		}
	}
	if !strings.Contains(section(ini, "security"), "REVERSE_PROXY_TRUSTED_PROXIES = 10.42.0.0/16,fd00::/8\n") {
		t.Errorf("[security]:\n%s", section(ini, "security"))
	}
}

// countingMapper counts the RESTMapper lookups, which query API discovery for kinds the
// cluster does not serve.
type countingMapper struct {
	meta.RESTMapper
	lookups int
}

func (m *countingMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	m.lookups++
	return m.RESTMapper.RESTMapping(gk, versions...)
}

// An instance that uses neither feature costs no lookup and no request once this process has
// made sure that it has no routes, Certificate or ReferenceGrant.
func TestUnusedFeaturesCostNothingPerPass(t *testing.T) {
	for name, served := range map[string][]schema.GroupVersionKind{"served": allOptionalAPIs, "not served": nil} {
		t.Run(name, func(t *testing.T) { unusedFeaturesPasses(t, served) })
	}

	// Enabling the feature creates the route again; disabling it deletes it again.
	f := gatewayInstance()
	f.Spec.Gateway.SSH = nil
	rc, _ := optionalRC(t, f, allOptionalAPIs)
	runStep(t, rc, ReconcileGateway)
	g := f.Spec.Gateway
	f.Spec.Gateway = nil
	runStep(t, rc, ReconcileGateway)
	f.Spec.Gateway = g
	runStep(t, rc, ReconcileGateway)
	f.Spec.Gateway = nil
	runStep(t, rc, ReconcileGateway)
	if getOptional(t, rc, forgejo.HTTPRouteGVK, "fj") != nil {
		t.Error("an HTTPRoute created after the first deletion was not deleted")
	}
}

func unusedFeaturesPasses(t *testing.T, served []schema.GroupVersionKind) {
	{
		f := testInstance()
		rc, _ := optionalRC(t, f, served)
		mapper := &countingMapper{RESTMapper: rc.RESTMapper}
		rc.RESTMapper = mapper
		gets := 0
		rc.Client = interceptor.NewClient(rc.Client.(client.WithWatch), interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				gets++
				return c.Get(ctx, key, obj, opts...)
			},
		})
		pass := func() {
			runStep(t, rc, ReconcileGateway)
			runStep(t, rc, ReconcileTLS)
		}
		pass()
		if len(served) > 0 && gets == 0 {
			t.Error("the first pass must look for leftovers of an earlier spec")
		}
		mapper.lookups, gets = 0, 0
		pass()
		if mapper.lookups != 0 || gets != 0 {
			t.Errorf("second pass made %d lookups and %d reads", mapper.lookups, gets)
		}
	}
}
