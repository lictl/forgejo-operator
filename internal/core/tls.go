package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete

// Reasons of the TLSReady condition.
const (
	reasonTLSReady                  = "Ready"
	reasonTLSPending                = "Pending"
	reasonCertificateNotReady       = "CertificateNotReady"
	reasonCertManagerUnavailable    = "CertManagerUnavailable"
	reasonNoDNSNames                = "NoDNSNames"
	reasonReferenceGrantUnavailable = "ReferenceGrantUnavailable"
)

// Minimal typed mirrors of cert-manager's Certificate and Gateway API's ReferenceGrant.

type certificateSpec struct {
	SecretName string        `json:"secretName"`
	DNSNames   []string      `json:"dnsNames"`
	IssuerRef  certIssuerRef `json:"issuerRef"`
}

type certIssuerRef struct {
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	Group string `json:"group"`
}

type referenceGrantSpec struct {
	From []referenceGrantFrom `json:"from"`
	To   []referenceGrantTo   `json:"to"`
}

type referenceGrantFrom struct {
	Group     string `json:"group"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
}

type referenceGrantTo struct {
	Group string `json:"group"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
}

// ReconcileTLS requests the certificate of spec.tls.certManager (Certificate "<name>-tls",
// Secret "<name>-tls") and lets the Gateways of spec.tls.gatewayNamespaces read that Secret
// (ReferenceGrant "<name>-tls"). Objects the spec no longer asks for are deleted. The
// Certificate's Ready condition becomes TLSReady. Without the CRDs nothing is created, and
// TLSReady says so.
func ReconcileTLS(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	name := forgejo.TLSSecretName(f)

	grantProblem, err := reconcileReferenceGrant(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}

	if !forgejo.CertManagerEnabled(f) {
		if err := deleteOptional(ctx, rc, name, forgejo.CertificateGVK); err != nil {
			return forgejo.StepResult{}, err
		}
		namespaces := grantNamespaces(f)
		switch {
		case grantProblem != "":
			setTLSReady(rc, metav1.ConditionFalse, reasonReferenceGrantUnavailable, grantProblem)
			return forgejo.StepResult{RequeueAfter: notReadyRequeue}, nil
		case len(namespaces) > 0:
			setTLSReady(rc, metav1.ConditionTrue, reasonTLSReady, grantMessage(f, namespaces))
		default:
			meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionTLSReady)
		}
		return forgejo.StepResult{}, nil
	}

	if !rc.HasAPI(forgejo.CertificateGVK) {
		setTLSReady(rc, metav1.ConditionFalse, reasonCertManagerUnavailable,
			"spec.tls.certManager is set, but the cluster does not serve Certificate "+
				forgejo.CertificateGVK.GroupVersion().String()+": install cert-manager")
		return forgejo.StepResult{}, nil
	}
	dnsNames := CertificateDNSNames(f)
	if len(dnsNames) == 0 {
		setTLSReady(rc, metav1.ConditionFalse, reasonNoDNSNames,
			"spec.tls.certManager has no DNS names: set spec.server.domain, spec.server.rootURL, ingress.host, "+
				"gateway.hostnames or spec.tls.certManager.dnsNames")
		return forgejo.StepResult{}, nil
	}
	cert, err := applyOptional(ctx, rc, forgejo.CertificateGVK, name, certificate(f, dnsNames), nil)
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("certificate %s: %w", name, err)
	}

	status, reason, msg := certificateCondition(cert, dnsNames)
	if grantProblem != "" {
		if status == metav1.ConditionTrue {
			status, reason, msg = metav1.ConditionFalse, reasonReferenceGrantUnavailable, grantProblem
		} else {
			msg += "; " + grantProblem
		}
	} else if namespaces := grantNamespaces(f); len(namespaces) > 0 && status == metav1.ConditionTrue {
		msg += ". " + grantMessage(f, namespaces)
	}
	setTLSReady(rc, status, reason, msg)
	if status != metav1.ConditionTrue {
		return forgejo.StepResult{RequeueAfter: notReadyRequeue}, nil
	}
	return forgejo.StepResult{}, nil
}

func setTLSReady(rc *forgejo.ReconcileContext, status metav1.ConditionStatus, reason, msg string) {
	setConditionWithEvent(rc, forgejov1.ConditionTLSReady, status, reason, msg, "TLS")
}

// certificate renders Certificate "<name>-tls".
func certificate(f *forgejov1.Forgejo, dnsNames []string) *certificateSpec {
	ref := f.Spec.TLS.CertManager.IssuerRef
	kind, group := ref.Kind, ref.Group
	if kind == "" {
		kind = "Issuer"
	}
	if group == "" {
		group = "cert-manager.io"
	}
	return &certificateSpec{
		SecretName: forgejo.TLSSecretName(f),
		DNSNames:   dnsNames,
		IssuerRef:  certIssuerRef{Name: ref.Name, Kind: kind, Group: group},
	}
}

// CertificateDNSNames are the DNS names of the certificate: spec.tls.certManager.dnsNames,
// else the public host names (the hosts of ROOT_URL and DOMAIN, the Ingress host and the
// HTTPRoute's host names) that are DNS names.
func CertificateDNSNames(f *forgejov1.Forgejo) []string {
	if cm := f.Spec.TLS.CertManager; cm != nil && len(cm.DNSNames) > 0 {
		return cm.DNSNames
	}
	var out []string
	add := func(h string) {
		h = strings.ToLower(h)
		if isDNSName(strings.TrimPrefix(h, "*.")) && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, h := range publicHosts(f) {
		add(h)
	}
	if ingressEnabled(f) {
		add(f.Spec.Ingress.Host)
	}
	if forgejo.GatewayEnabled(f) {
		for _, h := range RouteHostnames(f) {
			add(h)
		}
	}
	return out
}

// certificateCondition maps the Certificate's Ready condition: True when the certificate is
// issued and current, Unknown while cert-manager issues it or has not looked at this
// generation yet, False when it reports a problem.
func certificateCondition(cert *unstructured.Unstructured, dnsNames []string) (metav1.ConditionStatus, string, string) {
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	decoded := decodeConditions(any(conds))
	ready := findCondition(decoded, "Ready")
	name := cert.GetName()
	switch {
	case ready == nil || stale(ready, cert.GetGeneration()):
		return metav1.ConditionUnknown, reasonTLSPending, fmt.Sprintf("Certificate %s: waiting for cert-manager", name)
	case ready.Status == metav1.ConditionTrue:
		msg := fmt.Sprintf("Certificate %s is ready: Secret %s for %s", name, name, strings.Join(dnsNames, ", "))
		if notAfter, ok, _ := unstructured.NestedString(cert.Object, "status", "notAfter"); ok && notAfter != "" {
			msg += ", valid until " + notAfter
		}
		return metav1.ConditionTrue, reasonTLSReady, msg
	}
	msg := fmt.Sprintf("Certificate %s is not ready (%s): %s", name, ready.Reason, ready.Message)
	if issuing := findCondition(decoded, "Issuing"); issuing != nil && issuing.Status == metav1.ConditionTrue {
		return metav1.ConditionUnknown, reasonTLSPending, msg
	}
	return metav1.ConditionFalse, reasonCertificateNotReady, msg
}

// grantNamespaces are the namespaces of spec.tls.gatewayNamespaces other than the instance's.
func grantNamespaces(f *forgejov1.Forgejo) []string {
	if f.Spec.TLS == nil {
		return nil
	}
	var out []string
	for _, ns := range f.Spec.TLS.GatewayNamespaces {
		if ns != f.Namespace && !slices.Contains(out, ns) {
			out = append(out, ns)
		}
	}
	return out
}

func grantMessage(f *forgejov1.Forgejo, namespaces []string) string {
	return fmt.Sprintf("ReferenceGrant %s lets Gateways in %s read Secret %s",
		forgejo.TLSSecretName(f), strings.Join(namespaces, ", "), forgejo.TLSSecretName(f))
}

// reconcileReferenceGrant creates, updates or deletes ReferenceGrant "<name>-tls". It returns
// a problem to report when the grant is wanted but the cluster has no ReferenceGrant API.
func reconcileReferenceGrant(ctx context.Context, rc *forgejo.ReconcileContext) (string, error) {
	f := rc.Instance
	name := forgejo.TLSSecretName(f)
	namespaces := grantNamespaces(f)
	if len(namespaces) == 0 {
		return "", deleteOptional(ctx, rc, name, forgejo.ReferenceGrantGVKs...)
	}
	gvk, served := rc.FirstAPI(forgejo.ReferenceGrantGVKs...)
	if !served {
		return "spec.tls.gatewayNamespaces is set, but the cluster does not serve ReferenceGrant (" +
			gvkVersions(forgejo.ReferenceGrantGVKs) + "): install the Gateway API CRDs", nil
	}
	spec := referenceGrantSpec{To: []referenceGrantTo{{Group: "", Kind: "Secret", Name: name}}}
	for _, ns := range namespaces {
		spec.From = append(spec.From, referenceGrantFrom{Group: forgejo.GatewayAPIGroup, Kind: kindGateway, Namespace: ns})
	}
	if _, err := applyOptional(ctx, rc, gvk, name, &spec, nil); err != nil {
		return "", fmt.Errorf("ReferenceGrant %s: %w", name, err)
	}
	return "", nil
}
