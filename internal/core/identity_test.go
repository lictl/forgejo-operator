package core

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

func identitySecret(f *forgejov1.Forgejo, secretKey string) *corev1.Secret {
	data := map[string][]byte{}
	for _, k := range forgejo.IdentitySecretKeys {
		data[k] = []byte("value-" + k)
	}
	data[forgejo.SecretKeySecretKey] = []byte(secretKey)
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace}, Data: data}
}

func dataPVC(f *forgejov1.Forgejo, annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: forgejo.DataPVCName(f), Namespace: f.Namespace, Annotations: annotations}}
}

func cnpgCluster(f *forgejov1.Forgejo, annotations map[string]string) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	c.SetName(forgejo.DatabaseClusterName(f))
	c.SetNamespace(f.Namespace)
	c.SetAnnotations(annotations)
	return c
}

func identityOf(sec *corev1.Secret) string { return forgejo.SecretKeyFingerprint(sec.Data) }

// identityContext is a ReconcileContext whose client serves the CNPG API.
func identityContext(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) *forgejo.ReconcileContext {
	t.Helper()
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, RESTMapper: mapper, Instance: f, TargetImage: f.Status.Image}
}

func reconcileIdentity(t *testing.T, rc *forgejo.ReconcileContext) forgejo.StepResult {
	t.Helper()
	res, err := ReconcileIdentity(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func brokenReason(f *forgejov1.Forgejo) string {
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBroken); c != nil && c.Status == metav1.ConditionTrue {
		return c.Reason
	}
	return ""
}

func TestIdentityNewInstanceIsNotBroken(t *testing.T) {
	f := testInstance()
	rc := identityContext(t, f)
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" || rc.IdentityInitialized {
		t.Fatalf("stop %v, broken %q, initialized %v", res.Stop, brokenReason(f), rc.IdentityInitialized)
	}

	// A first install interrupted after the secrets step: the volume and the cluster are created later.
	sec := identitySecret(f, "k1")
	rc = identityContext(t, f, sec, cnpgCluster(f, map[string]string{forgejo.AnnotationIdentity: identityOf(sec)}))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" || rc.IdentityFingerprint != identityOf(sec) {
		t.Fatalf("stop %v, broken %q, fingerprint %q", res.Stop, brokenReason(f), rc.IdentityFingerprint)
	}
}

func TestIdentityReportsMissingObjectsOfAnInstanceThatRan(t *testing.T) {
	ran := func() *forgejov1.Forgejo {
		f := testInstance()
		f.Status.Image = img16
		return f
	}
	for _, tc := range []struct {
		name   string
		objs   func(f *forgejov1.Forgejo) []client.Object
		reason string
	}{
		{"secret", func(f *forgejov1.Forgejo) []client.Object {
			return []client.Object{dataPVC(f, nil), cnpgCluster(f, nil)}
		}, forgejo.BrokenSecretsMissing},
		{"secret key", func(f *forgejov1.Forgejo) []client.Object {
			sec := identitySecret(f, "k1")
			delete(sec.Data, forgejo.SecretKeyInternalToken)
			return []client.Object{sec, dataPVC(f, nil), cnpgCluster(f, nil)}
		}, forgejo.BrokenSecretsMissing},
		{"volume", func(f *forgejov1.Forgejo) []client.Object {
			return []client.Object{identitySecret(f, "k1"), cnpgCluster(f, nil)}
		}, forgejo.BrokenDataVolumeMissing},
		{"cluster", func(f *forgejov1.Forgejo) []client.Object {
			return []client.Object{identitySecret(f, "k1"), dataPVC(f, nil)}
		}, forgejo.BrokenDatabaseMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ran()
			rc := identityContext(t, f, tc.objs(f)...)
			if res := reconcileIdentity(t, rc); !res.Stop || brokenReason(f) != tc.reason {
				t.Fatalf("stop %v, broken %q", res.Stop, brokenReason(f))
			}
		})
	}

	// Status is gone too: the running-image annotation on the volume shows that it ran.
	f := testInstance()
	rc := identityContext(t, f, dataPVC(f, map[string]string{forgejo.AnnotationRunningImage: img16}))
	if res := reconcileIdentity(t, rc); !res.Stop || brokenReason(f) != forgejo.BrokenSecretsMissing {
		t.Fatalf("stop %v, broken %q", res.Stop, brokenReason(f))
	}
}

func TestIdentityMismatch(t *testing.T) {
	f := testInstance()
	f.Status.Image = img16
	sec := identitySecret(f, "new")
	old := identitySecret(f, "old")
	rc := identityContext(t, f, sec, dataPVC(f, map[string]string{forgejo.AnnotationIdentity: identityOf(old)}), cnpgCluster(f, nil))
	if res := reconcileIdentity(t, rc); !res.Stop || brokenReason(f) != forgejo.BrokenIdentityMismatch {
		t.Fatalf("other identity: stop %v, broken %q", res.Stop, brokenReason(f))
	}

	// A cluster without the annotation next to a volume with it is adopted: an adoption that a
	// failed write left half-done, or one a person put in place (the operator never creates one
	// for an instance that has run).
	f = testInstance()
	f.Status.Image = img16
	rc = identityContext(t, f, sec, dataPVC(f, map[string]string{forgejo.AnnotationIdentity: identityOf(sec)}), cnpgCluster(f, nil))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" {
		t.Fatalf("unstamped cluster: stop %v, broken %q", res.Stop, brokenReason(f))
	}
	cluster := cnpgCluster(f, nil)
	if err := rc.Client.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil ||
		cluster.GetAnnotations()[forgejo.AnnotationIdentity] != identityOf(sec) {
		t.Fatalf("cluster annotations %v, err %v", cluster.GetAnnotations(), err)
	}
}

// While a PostgreSQL major upgrade is in flight, a lost cluster is the upgrade's to report.
func TestIdentityLeavesALostClusterToAnInFlightDatabaseUpgrade(t *testing.T) {
	f := testInstance()
	f.Status.Image = img16
	f.Status.Database = &forgejov1.DatabaseStatus{Upgrade: &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeUpgrading}}
	sec := identitySecret(f, "k1")
	rc := identityContext(t, f, sec, dataPVC(f, map[string]string{forgejo.AnnotationIdentity: identityOf(sec)}))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" {
		t.Fatalf("stop %v, broken %q", res.Stop, brokenReason(f))
	}
}

func TestIdentityAdoptsAndRecoversStatusImage(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	sec := identitySecret(f, "k1")
	rc := identityContext(t, f, sec, dataPVC(f, nil), cnpgCluster(f, nil))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" {
		t.Fatalf("adoption: stop %v, broken %q", res.Stop, brokenReason(f))
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(dataPVC(f, nil)), pvc); err != nil {
		t.Fatal(err)
	}
	cluster := cnpgCluster(f, nil)
	if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	for _, o := range []client.Object{pvc, cluster} {
		if a := o.GetAnnotations(); a[forgejo.AnnotationIdentity] != identityOf(sec) || a[forgejo.AnnotationRunningImage] != img16 {
			t.Fatalf("%s annotations %v", o.GetName(), a)
		}
	}

	// Status lost (the Forgejo object was created again): status.image comes back from the
	// volume, so the upgrade step compares spec.image with what ran instead of a first install.
	f = testInstance()
	f.Spec.Image = img17
	rc = &forgejo.ReconcileContext{Client: rc.Client, APIReader: rc.Client, Scheme: rc.Scheme, RESTMapper: rc.RESTMapper,
		Instance: f, TargetImage: img17}
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" || f.Status.Image != img16 || rc.TargetImage != img16 ||
		!rc.IdentityInitialized {
		t.Fatalf("stop %v, broken %q, status.image %q, target %q", res.Stop, brokenReason(f), f.Status.Image, rc.TargetImage)
	}
}

func TestIdentityClearsBrokenOnceRestored(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	sec := identitySecret(f, "k1")
	stamp := map[string]string{forgejo.AnnotationIdentity: identityOf(sec)}
	rc := identityContext(t, f, dataPVC(f, stamp), cnpgCluster(f, stamp))
	if reconcileIdentity(t, rc); brokenReason(f) != forgejo.BrokenSecretsMissing {
		t.Fatalf("broken %q", brokenReason(f))
	}
	if err := rc.Client.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" {
		t.Fatalf("after restore: stop %v, broken %q", res.Stop, brokenReason(f))
	}

	// Another step's reason stays.
	rc.SetBroken("DatabaseUpgradeFailed", "kept")
	if reconcileIdentity(t, rc); brokenReason(f) != "DatabaseUpgradeFailed" {
		t.Fatalf("broken %q", brokenReason(f))
	}
}

func TestIdentityLeavesRestoreFromToTheBootstrap(t *testing.T) {
	f := testInstance()
	f.Spec.RestoreFrom = &forgejov1.RestoreFromSpec{}
	sec := identitySecret(f, "from-backup")
	leftover := identitySecret(f, "leftover")
	rc := identityContext(t, f, sec, dataPVC(f, map[string]string{
		forgejo.AnnotationIdentity: identityOf(leftover), forgejo.AnnotationRunningImage: img16}))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" || rc.IdentityInitialized {
		t.Fatalf("stop %v, broken %q, initialized %v", res.Stop, brokenReason(f), rc.IdentityInitialized)
	}
}

func TestCreationGuardsForAnInstanceThatRan(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	rc := identityContext(t, f, identitySecret(f, "k1"))
	rc.IdentityInitialized = true
	if _, err := ReconcileStorage(ctx, rc); err == nil {
		t.Fatal("storage step created a data PVC for an instance that ran")
	}

	partial := identitySecret(f, "k1")
	delete(partial.Data, forgejo.SecretKeySecretKey)
	rc = identityContext(t, f, partial)
	rc.IdentityInitialized = true
	if _, err := ReconcileSecrets(ctx, rc); err == nil {
		t.Fatal("secrets step generated SECRET_KEY for an instance that ran")
	}

	// A new instance's volume carries its identity.
	f = testInstance()
	rc = identityContext(t, f)
	rc.IdentityFingerprint = "fingerprint-1"
	if _, err := ReconcileStorage(ctx, rc); err != nil {
		t.Fatal(err)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(dataPVC(f, nil)), pvc); err != nil || pvc.Annotations[forgejo.AnnotationIdentity] != rc.IdentityFingerprint {
		t.Fatalf("annotations %v, err %v", pvc.Annotations, err)
	}
}

// The annotation recreate-database lets the database step create the missing cluster of an
// instance that ran (the database-upgrade step then holds Forgejo until a restore).
func TestIdentityRecreateDatabaseAnnotation(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	f.Annotations = map[string]string{forgejo.AnnotationRecreateDatabase: valueTrue}
	sec := identitySecret(f, "k1")
	rc := identityContext(t, f, sec, dataPVC(f, map[string]string{forgejo.AnnotationIdentity: identityOf(sec)}))
	if res := reconcileIdentity(t, rc); res.Stop || brokenReason(f) != "" || !rc.DatabaseRecreate {
		t.Fatalf("stop %v, broken %q, recreate %v", res.Stop, brokenReason(f), rc.DatabaseRecreate)
	}
	// CNPG's "-app" Secret has the same keys as the admin Secret.
	appSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CNPGAppSecretName(f), Namespace: f.Namespace},
		Data: map[string][]byte{adminUsernameKey: []byte("forgejo"), adminPasswordKey: []byte("pw")}}
	if err := rc.Client.Create(ctx, appSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileDatabase(ctx, rc); err != nil {
		t.Fatal(err)
	}
	cluster := cnpgCluster(f, nil)
	if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(cluster), cluster); err != nil ||
		cluster.GetAnnotations()[forgejo.AnnotationIdentity] != identityOf(sec) ||
		cluster.GetAnnotations()[forgejo.AnnotationRestorePending] != "true" {
		t.Fatalf("cluster annotations %v, err %v", cluster.GetAnnotations(), err)
	}
}
