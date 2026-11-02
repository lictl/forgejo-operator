package core

import (
	"context"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testWildcardHost     = "*.example.com"
	testIngressTLSSecret = "git-tls"
)

func TestReconcileIngressRequiresExplicitHost(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*forgejov1.Forgejo)
	}{
		{"no public host", func(*forgejov1.Forgejo) {}},
		{"server domain", func(f *forgejov1.Forgejo) { f.Spec.Server.Domain = testDomain }},
		{"server root URL", func(f *forgejov1.Forgejo) { f.Spec.Server.RootURL = "https://git.example.com:8443/git/" }},
		{"gateway host", func(f *forgejov1.Forgejo) { f.Spec.Gateway = &forgejov1.GatewaySpec{Hostnames: []string{testDomain}} }},
		{"gateway wildcard", func(f *forgejov1.Forgejo) {
			f.Spec.Gateway = &forgejov1.GatewaySpec{Hostnames: []string{testWildcardHost}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, TLSSecretName: testIngressTLSSecret}
			tc.mutate(f)
			rc := testContext(t, f)
			ctx := context.Background()
			if err := reconcileIngress(ctx, rc); err == nil || !strings.Contains(err.Error(), "ingress.host") {
				t.Fatalf("missing ingress.host: %v", err)
			}
			ing := &networkingv1.Ingress{}
			if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(f), ing); !apierrors.IsNotFound(err) {
				t.Fatalf("hostless Ingress was created: %v", err)
			}

			// Removing a previously valid host must not rewrite the existing route to catch-all.
			f.Spec.Ingress.Host = testDomain
			if err := reconcileIngress(ctx, rc); err != nil {
				t.Fatal(err)
			}
			f.Spec.Ingress.Host = ""
			if err := reconcileIngress(ctx, rc); err == nil {
				t.Fatal("removed host was accepted")
			}
			if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(f), ing); err != nil {
				t.Fatal(err)
			}
			if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != testDomain || len(ing.Spec.TLS) != 1 ||
				!slices.Equal(ing.Spec.TLS[0].Hosts, []string{testDomain}) {
				t.Fatalf("existing route changed: %+v", ing.Spec)
			}
		})
	}
}

func TestIngressHostControlsRuleAndTLS(t *testing.T) {
	for _, certificate := range []bool{false, true} {
		f := testInstance()
		f.Spec.Server.Domain = "ssh.example.com"
		f.Spec.Server.RootURL = "https://public.example.com/git/"
		f.Spec.Gateway = &forgejov1.GatewaySpec{Hostnames: []string{testWildcardHost, "gateway.example.com"}}
		f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: testDomain, TLSSecretName: testIngressTLSSecret}
		secret := testIngressTLSSecret
		if certificate {
			f.Spec.Ingress.TLSSecretName = ""
			f.Spec.TLS = &forgejov1.TLSSpec{CertManager: &forgejov1.CertManagerTLSSpec{}}
			secret = forgejo.TLSSecretName(f)
		}
		rc := testContext(t, f)
		if err := reconcileIngress(context.Background(), rc); err != nil {
			t.Fatal(err)
		}
		ing := &networkingv1.Ingress{}
		if err := rc.Client.Get(context.Background(), client.ObjectKeyFromObject(f), ing); err != nil {
			t.Fatal(err)
		}
		if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != testDomain || len(ing.Spec.TLS) != 1 ||
			!slices.Equal(ing.Spec.TLS[0].Hosts, []string{testDomain}) || ing.Spec.TLS[0].SecretName != secret {
			t.Fatalf("rule and TLS must use ingress.host: %+v", ing.Spec)
		}
		if certificate && !slices.Contains(CertificateDNSNames(f), testDomain) {
			t.Fatal("certificate omits the Ingress host")
		}
		if RootURL(f) != f.Spec.Server.RootURL || Domain(f) != f.Spec.Server.Domain {
			t.Fatal("explicit public identity overrides were lost")
		}
	}
}

func TestReconcileIngressClassName(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		existing *string // the class of the existing Ingress; nil: there is none
		spec     *string // spec.ingress.className
		want     *string
	}{
		{"created without a class, left to admission", nil, nil, nil},
		// As set by the API server's DefaultIngressClass admission plugin.
		{"class set by admission kept", ptr.To("nginx"), nil, ptr.To("nginx")},
		{"spec.ingress.className wins", ptr.To("nginx"), ptr.To("traefik"), ptr.To("traefik")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: testDomain, ClassName: tc.spec}
			var objs []client.Object
			if tc.existing != nil {
				objs = append(objs, &networkingv1.Ingress{
					ObjectMeta: metav1.ObjectMeta{Name: forgejo.IngressName(f), Namespace: f.Namespace,
						Labels: forgejo.Labels(f, forgejo.ComponentServer)},
					Spec: networkingv1.IngressSpec{IngressClassName: tc.existing},
				})
			}
			rc := testContext(t, f, objs...)
			if err := reconcileIngress(ctx, rc); err != nil {
				t.Fatal(err)
			}
			ing := &networkingv1.Ingress{}
			if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.IngressName(f)}, ing); err != nil {
				t.Fatal(err)
			}
			if !ptr.Equal(ing.Spec.IngressClassName, tc.want) {
				t.Fatalf("className %v, want %v", ptr.Deref(ing.Spec.IngressClassName, "<nil>"), ptr.Deref(tc.want, "<nil>"))
			}
			if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != testDomain {
				t.Fatalf("rules not reconciled: %+v", ing.Spec.Rules)
			}
		})
	}
}
