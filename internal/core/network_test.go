package core

import (
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

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
					ObjectMeta: metav1.ObjectMeta{Name: forgejo.IngressName(f), Namespace: f.Namespace},
					Spec:       networkingv1.IngressSpec{IngressClassName: tc.existing},
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
