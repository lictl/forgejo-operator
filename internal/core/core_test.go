package core

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testDomain       = "git.example.com"
	testConfigSecret = "fj-config"
)

const (
	img15 = "codeberg.org/forgejo/forgejo:15"
	img16 = "codeberg.org/forgejo/forgejo:16"
	img17 = "codeberg.org/forgejo/forgejo:17"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testInstance() *forgejov1.Forgejo {
	return &forgejov1.Forgejo{
		TypeMeta:   metav1.TypeMeta{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo"},
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: types.UID("uid-1")},
	}
}

func testContext(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) *forgejo.ReconcileContext {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	return &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
}

// getDeployment returns the instance's server Deployment.
func getDeployment(t *testing.T, rc *forgejo.ReconcileContext) *appsv1.Deployment {
	t.Helper()
	d := &appsv1.Deployment{}
	if err := rc.Client.Get(context.Background(), types.NamespacedName{Namespace: rc.Instance.Namespace, Name: forgejo.DeploymentName(rc.Instance)}, d); err != nil {
		t.Fatal(err)
	}
	return d
}

func drainEvents(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// envOf maps a container's environment by name.
func envOf(c corev1.Container) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, e := range c.Env {
		env[e.Name] = e
	}
	return env
}

func TestRandomPassword(t *testing.T) {
	for range 2000 {
		pw, err := randomPassword(adminPasswordLength)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != adminPasswordLength {
			t.Fatalf("length %d", len(pw))
		}
		for _, class := range []string{lowerChars, upperChars, digitChars, passwordSymbols} {
			if !strings.ContainsAny(pw, class) {
				t.Fatalf("%q lacks a character of %q", pw, class)
			}
		}
		if strings.ContainsAny(pw[:1], passwordSymbols) {
			t.Fatalf("%q starts with a symbol", pw)
		}
		if strings.Trim(pw, alphanumeric+passwordSymbols) != "" {
			t.Fatalf("%q has characters outside the alphabet", pw)
		}
	}
}
