package forgejo

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

func controlledSecret(f *forgejov1.Forgejo, owner metav1.OwnerReference, labelled bool) *corev1.Secret {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: f.Namespace}}
	if owner.Kind != "" {
		s.OwnerReferences = []metav1.OwnerReference{owner}
	}
	if labelled {
		s.Labels = Labels(f, ComponentServer)
	}
	return s
}

func TestControls(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: "uid-now"}}
	forgejoRef := func(name string, uid string) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: forgejov1.GroupVersion.String(), Kind: "Forgejo", Name: name, UID: k8stypes.UID(uid),
			Controller: ptr.To(true)}
	}
	for _, tc := range []struct {
		name string
		obj  *corev1.Secret
		want bool
	}{
		{"controlled by the instance", controlledSecret(f, forgejoRef("fj", "uid-now"), false), true},
		{"controlled by an earlier Forgejo of the same name", controlledSecret(f, forgejoRef("fj", "uid-before"), false), true},
		{"controlled by another Forgejo", controlledSecret(f, forgejoRef("other", "uid-other"), true), false},
		{"controlled by something else", controlledSecret(f, metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "x", UID: "x",
			Controller: ptr.To(true)}, true), false},
		{"an orphan with the instance's labels", controlledSecret(f, metav1.OwnerReference{}, true), true},
		{"nobody's and unlabelled", controlledSecret(f, metav1.OwnerReference{}, false), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := Controls(f, tc.obj); got != tc.want {
				t.Errorf("Controls = %t, want %t", got, tc.want)
			}
		})
	}
}

// CreateOrUpdate refuses objects the instance does not control; CreateOrAdopt also takes over
// one that nothing controls (a Secret restored by hand), but still not another owner's.
func TestCreateOrAdoptStillRefusesAnotherOwnersObject(t *testing.T) {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: "uid-now"}}
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	external := metav1.OwnerReference{APIVersion: "external-secrets.io/v1", Kind: "ExternalSecret", Name: "es", UID: "es-uid", Controller: ptr.To(true)}
	for _, tc := range []struct {
		name    string
		obj     *corev1.Secret
		adopt   bool
		refused bool
	}{
		{"update an unlabelled orphan", controlledSecret(f, metav1.OwnerReference{}, false), false, true},
		{"adopt an unlabelled orphan", controlledSecret(f, metav1.OwnerReference{}, false), true, false},
		{"update an ExternalSecret's", controlledSecret(f, external, false), false, true},
		{"adopt an ExternalSecret's", controlledSecret(f, external, false), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.obj).Build()
			rc := &ReconcileContext{Client: c, Scheme: s, Instance: f}
			sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "ns"}}
			write := rc.CreateOrUpdate
			if tc.adopt {
				write = rc.CreateOrAdopt
			}
			_, err := write(context.Background(), sec, false, func() error { sec.Data = map[string][]byte{"k": []byte("generated")}; return nil })
			var conflict *NameConflictError
			if tc.refused != errors.As(err, &conflict) {
				t.Fatalf("error %v, refused want %t", err, tc.refused)
			}
			got := &corev1.Secret{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(sec), got); err != nil {
				t.Fatal(err)
			}
			if written := string(got.Data["k"]) == "generated"; written == tc.refused {
				t.Errorf("object written: %t, refused: %t", written, tc.refused)
			}
		})
	}
}
