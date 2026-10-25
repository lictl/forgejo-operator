package forgejo

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// The operator's CloudNativePG clusters always serve TLS, so every connection to them (Forgejo,
// backup and restore Jobs) is encrypted; an external database keeps the mode its spec chooses.
func TestResolveDBSSLMode(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	creds := func(name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
			Data:       map[string][]byte{credentialsUsernameKey: []byte("forgejo"), credentialsPasswordKey: []byte("pw")},
		}
	}
	cnpg := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"}}
	external := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "ext", Namespace: "ns"}}
	external.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	external.Spec.Database.External = &forgejov1.ExternalDatabaseSpec{
		Host:              "db.example.com",
		CredentialsSecret: forgejov1.SecretCredentialsRef{Name: "ext-db"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(creds(CNPGAppSecretName(cnpg)), creds("ext-db")).Build()

	for _, tt := range []struct {
		f    *forgejov1.Forgejo
		mode string
	}{
		{cnpg, "require"},
		{external, "disable"},
	} {
		db, err := ResolveDB(context.Background(), c, tt.f)
		if err != nil {
			t.Fatal(err)
		}
		if db.SSLMode != tt.mode {
			t.Errorf("%s: sslmode %q, want %q", tt.f.Name, db.SSLMode, tt.mode)
		}
	}
}
