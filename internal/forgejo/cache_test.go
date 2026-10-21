package forgejo

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

func TestSelectorImplies(t *testing.T) {
	app := labels.Set{LabelName: AppName}
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "x"}}
	in, _ := labels.Parse(LabelName + " in (forgejo)")
	inTwo, _ := labels.Parse(LabelName + " in (forgejo,gitea)")
	notEq, _ := labels.Parse(LabelName + "!=gitea")
	for _, tc := range []struct {
		name string
		sel  labels.Selector
		want bool
	}{
		{"nil", nil, false},
		{"everything", labels.Everything(), false},
		{"selector labels", labels.SelectorFromSet(SelectorLabels(f)), true},
		{"job name only", labels.SelectorFromSet(labels.Set{"batch.kubernetes.io/job-name": "j"}), false},
		{"in single", in, true},
		{"in two", inTwo, false},
		{"not equal", notEq, false},
		{"other value", labels.SelectorFromSet(labels.Set{LabelName: "gitea"}), false},
	} {
		if got := SelectorImplies(tc.sel, app); got != tc.want {
			t.Errorf("%s: SelectorImplies = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func scopedTestReader(t *testing.T) (client.Reader, *runtime.Scheme) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := forgejov1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns"}}
	owned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fj-secrets", Namespace: "ns", Labels: Labels(f, ComponentServer)}}
	user := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "smtp", Namespace: "ns"}}
	server := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fj-1", Namespace: "ns", Labels: Labels(f, ComponentServer)}}
	cnpg := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fj-db-1", Namespace: "ns", Labels: map[string]string{"cnpg.io/cluster": "fj-db"}}}
	backup := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "ns"}}

	// The cache holds what its label selectors admit; the API server holds everything.
	cached := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owned.DeepCopy(), server.DeepCopy(), backup.DeepCopy()).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owned, user, server, cnpg, backup).Build()
	r, err := NewScopedReader(cached, live, scheme, CacheScopes())
	if err != nil {
		t.Fatal(err)
	}
	return r, scheme
}

func TestScopedReaderGet(t *testing.T) {
	r, _ := scopedTestReader(t)
	ctx := context.Background()
	for _, name := range []string{"fj-secrets", "smtp"} {
		if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &corev1.Secret{}); err != nil {
			t.Errorf("Get secret %s: %v", name, err)
		}
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "fj-db-1"}, &corev1.Pod{}); err != nil {
		t.Errorf("Get unlabelled CNPG pod: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "missing"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("Get missing secret: %v, want NotFound", err)
	}
}

func TestScopedReaderList(t *testing.T) {
	r, _ := scopedTestReader(t)
	ctx := context.Background()
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj"}}

	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace("ns"), client.MatchingLabels(SelectorLabels(f))); err != nil || len(pods.Items) != 1 {
		t.Errorf("List server pods: %d pods, err %v; want 1", len(pods.Items), err)
	}
	if err := r.List(ctx, pods, client.InNamespace("ns")); err != nil || len(pods.Items) != 2 {
		t.Errorf("List all pods: %d pods, err %v; want 2 (live, including CNPG)", len(pods.Items), err)
	}
	if err := r.List(ctx, pods, client.InNamespace("ns"), client.MatchingLabels{"cnpg.io/cluster": "fj-db"}); err != nil || len(pods.Items) != 1 {
		t.Errorf("List CNPG pods: %d pods, err %v; want 1", len(pods.Items), err)
	}
	backups := &forgejov1.ForgejoBackupList{}
	if err := r.List(ctx, backups, client.InNamespace("ns")); err != nil || len(backups.Items) != 1 {
		t.Errorf("List unscoped kind: %d, err %v", len(backups.Items), err)
	}
}

func TestCacheByObjectCoversScopes(t *testing.T) {
	scopes := CacheScopes()
	by := CacheByObject(scopes)
	if len(by) != len(scopes) {
		t.Fatalf("%d ByObject entries for %d scopes", len(by), len(scopes))
	}
	for obj, b := range by {
		if b.Label == nil || b.Label.Empty() {
			t.Errorf("%T has no label selector", obj)
		}
	}
}
