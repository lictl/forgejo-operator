package core

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPG17 = "ghcr.io/cloudnative-pg/postgresql:17-minimal-trixie"
	testPG18 = "ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie"
	size5Gi  = "5Gi"
	size8Gi  = "8Gi"
)

// The database step writes the database-upgrade step's image, never a major older than the
// cluster's data (CNPG's webhook refuses it, and every pass would fail), and reports the
// image, major and system ID of the data.
func TestReconcileCNPGWritesTheGatedImage(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Database.CNPG.ImageName = testPG18
	f.Status.Image = img16
	f.Status.Database = &forgejov1.DatabaseStatus{Upgrade: &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeVerifying}}
	appSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CNPGAppSecretName(f), Namespace: f.Namespace},
		Data:       map[string][]byte{adminUsernameKey: []byte("forgejo"), adminPasswordKey: []byte("pw")},
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	cluster.SetName(forgejo.DatabaseClusterName(f))
	cluster.SetNamespace(f.Namespace)
	if err := unstructured.SetNestedField(cluster.Object, testPG18, "spec", "imageName"); err != nil {
		t.Fatal(err)
	}
	cluster.Object["status"] = map[string]any{
		"phase": cnpgHealthyPhase, "readyInstances": int64(1), "currentPrimary": "fj-db-1", "targetPrimary": "fj-db-1",
		"systemID": "7002", "pgDataImageInfo": map[string]any{"image": testPG18, "majorVersion": int64(18)},
	}
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(appSecret, cluster).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, RESTMapper: mapper, DatabaseImage: testPG17}

	if _, err := ReconcileDatabase(ctx, rc); err != nil {
		t.Fatal(err)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: cluster.GetName()}, got); err != nil {
		t.Fatal(err)
	}
	if image, _, _ := unstructured.NestedString(got.Object, "spec", "imageName"); image != testPG18 {
		t.Fatalf("spec.imageName %s: a downgrade below the data's major was written", image)
	}
	st := f.Status.Database
	if st.Image != testPG18 || st.MajorVersion != 18 || st.SystemID != "7002" || st.Upgrade == nil ||
		st.Upgrade.Phase != forgejov1.DatabaseUpgradeVerifying {
		t.Fatalf("status.database %+v", st)
	}

	// The same major is written as asked.
	rc.DatabaseImage = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie"
	if _, err := ReconcileDatabase(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: cluster.GetName()}, got); err != nil {
		t.Fatal(err)
	}
	if image, _, _ := unstructured.NestedString(got.Object, "spec", "imageName"); image != rc.DatabaseImage {
		t.Fatalf("spec.imageName %s", image)
	}
}
