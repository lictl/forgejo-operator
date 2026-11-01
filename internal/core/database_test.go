package core

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPodDB1          = "db-1"
	testPodDB2          = "db-2"
	testClusterPodDB1   = "fj-db-1"
	testPhaseNewReplica = "Creating a new replica"
	testPG17            = "ghcr.io/cloudnative-pg/postgresql:17-minimal-trixie"
	testPG18            = "ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie"
)

// cnpgContext is a ReconcileContext whose client serves the CNPG API.
func cnpgContext(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) *forgejo.ReconcileContext {
	t.Helper()
	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(objs...).Build()
	return &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: s, RESTMapper: mapper, Instance: f, TargetImage: f.Status.Image}
}

func cnpgCluster(f *forgejov1.Forgejo, annotations map[string]string) *unstructured.Unstructured {
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	c.SetName(forgejo.DatabaseClusterName(f))
	c.SetNamespace(f.Namespace)
	c.SetAnnotations(annotations)
	return c
}

// cnpgAppSecret is CNPG's "-app" Secret of the cluster, which has the keys of the admin Secret.
func cnpgAppSecret(f *forgejov1.Forgejo) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CNPGAppSecretName(f), Namespace: f.Namespace},
		Data: map[string][]byte{adminUsernameKey: []byte("forgejo"), adminPasswordKey: []byte("pw")}}
}

func TestProbeHosts(t *testing.T) {
	tests := map[string][]string{
		"postgres":       {"postgres.ns.svc", "postgres.ns.svc.cluster.local", "postgres"},
		"pg.example.com": {"pg.example.com"},
		"pg.other":       {"pg.other"},
		"10.0.0.5":       {"10.0.0.5"},
		"::1":            {"::1"},
		"localhost":      {"localhost"},
	}
	for host, want := range tests {
		if got := probeHosts(host, "ns"); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("probeHosts(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestCNPGPrimaryUnavailable(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	pod := func(name string, ready corev1.ConditionStatus) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace},
			Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}},
		}
	}
	rc := testContext(t, f, pod("db-1", corev1.ConditionTrue), pod("db-2", corev1.ConditionFalse))
	tests := []struct {
		name, phase, current, target string
		ready                        int64
		available                    bool
	}{
		{"replica missing", testPhaseNewReplica, testPodDB1, testPodDB1, 2, true},
		{"healthy phase, replica down", cnpgHealthyPhase, testPodDB1, testPodDB1, 2, true},
		{"not provisioned", "Waiting for the instances to become active", "", testPodDB1, 0, false},
		{"switchover", "Switchover in progress", testPodDB1, testPodDB2, 2, false},
		{"failing over", "Failing over", testPodDB1, testPodDB1, 2, false},
		{"primary not ready", testPhaseNewReplica, testPodDB2, testPodDB2, 1, false},
		{"primary pod gone", testPhaseNewReplica, "db-3", "db-3", 1, false},
		{"nothing ready", testPhaseNewReplica, testPodDB1, testPodDB1, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			why, err := cnpgPrimaryUnavailable(ctx, rc, tt.phase, tt.current, tt.target, tt.ready)
			if err != nil {
				t.Fatal(err)
			}
			if (why == "") != tt.available {
				t.Fatalf("available = %v (%q), want %v", why == "", why, tt.available)
			}
		})
	}
}

func TestReconcileCNPGDegradedKeepsPipelineRunning(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	primary := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterPodDB1, Namespace: f.Namespace},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	cluster := cnpgCluster(f, nil)
	cluster.Object["status"] = map[string]any{
		"phase": "Applying configuration", "readyInstances": int64(1),
		"currentPrimary": testClusterPodDB1, "targetPrimary": testClusterPodDB1,
	}
	rc := cnpgContext(t, f, cnpgAppSecret(f), primary, cluster)

	res, err := ReconcileDatabase(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop || !rc.DatabaseReady || rc.DB == nil {
		t.Fatalf("degraded cluster with a ready primary: want the pipeline to continue, got stop=%v ready=%v db=%v", res.Stop, rc.DatabaseReady, rc.DB != nil)
	}
	cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != reasonClusterDegraded {
		t.Fatalf("want DatabaseReady=True/%s, got %+v", reasonClusterDegraded, cond)
	}

	// Before the first rollout the whole cluster is waited for.
	f.Status.Image = ""
	if res, err := ReconcileDatabase(ctx, rc); err != nil || !res.Stop {
		t.Fatalf("first install with a degraded cluster: want stop, got stop=%v err=%v", res.Stop, err)
	}
	f.Status.Image = img16

	// The primary goes away: stop as before.
	primary.Status.Conditions[0].Status = corev1.ConditionFalse
	if err := rc.Client.Status().Update(ctx, primary); err != nil {
		t.Fatal(err)
	}
	res, err = ReconcileDatabase(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stop || rc.DatabaseReady {
		t.Fatalf("primary not ready: want stop, got stop=%v ready=%v", res.Stop, rc.DatabaseReady)
	}
}

// The database step writes the database-upgrade step's image, never a major older than the
// cluster's data (CNPG's webhook refuses it, and every pass would fail), and reports the
// image, major and system ID of the data.
func TestReconcileCNPGWritesTheGatedImage(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Database.CNPG.ImageName = testPG18
	f.Status.Image = img16
	f.Status.Database = &forgejov1.DatabaseStatus{Upgrade: &forgejov1.DatabaseUpgradeStatus{Phase: forgejov1.DatabaseUpgradeVerifying}}
	cluster := cnpgCluster(f, nil)
	if err := unstructured.SetNestedField(cluster.Object, testPG18, "spec", "imageName"); err != nil {
		t.Fatal(err)
	}
	cluster.Object["status"] = map[string]any{
		"phase": cnpgHealthyPhase, "readyInstances": int64(1), "currentPrimary": testClusterPodDB1, "targetPrimary": testClusterPodDB1,
		"systemID": "7002", "pgDataImageInfo": map[string]any{"image": testPG18, "majorVersion": int64(18)},
	}
	rc := cnpgContext(t, f, cnpgAppSecret(f), cluster)
	rc.DatabaseImage = testPG17
	imageName := func() string {
		t.Helper()
		if _, err := ReconcileDatabase(ctx, rc); err != nil {
			t.Fatal(err)
		}
		got := cnpgCluster(f, nil)
		if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(got), got); err != nil {
			t.Fatal(err)
		}
		image, _, _ := unstructured.NestedString(got.Object, "spec", "imageName")
		return image
	}

	if image := imageName(); image != testPG18 {
		t.Fatalf("spec.imageName %s: a downgrade below the data's major was written", image)
	}
	st := f.Status.Database
	if st.Image != testPG18 || st.MajorVersion != 18 || st.SystemID != "7002" || st.Upgrade == nil ||
		st.Upgrade.Phase != forgejov1.DatabaseUpgradeVerifying {
		t.Fatalf("status.database %+v", st)
	}

	// The same major is written as asked.
	rc.DatabaseImage = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie"
	if image := imageName(); image != rc.DatabaseImage {
		t.Fatalf("spec.imageName %s", image)
	}
}

// An external database the operator cannot dial does not stop the pipeline.
func TestReconcileExternalDBUnreachableFromOperator(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	f.Spec.Database.External = &forgejov1.ExternalDatabaseSpec{
		Host: "127.0.0.1", Port: 1, CredentialsSecret: forgejov1.SecretCredentialsRef{Name: "pg"},
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: f.Namespace},
		Data: map[string][]byte{"username": []byte("u"), "password": []byte("p")}}
	rc := testContext(t, f, sec)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec

	res, err := ReconcileDatabase(ctx, rc)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stop || rc.DB == nil || rc.DatabaseReady || res.RequeueAfter == 0 {
		t.Fatalf("want the pipeline to continue with rc.DB set, not ready, requeued; got stop=%v db=%v ready=%v requeue=%v",
			res.Stop, rc.DB != nil, rc.DatabaseReady, res.RequeueAfter)
	}
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseReady)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonUnreachableFromOperator {
		t.Fatalf("DatabaseReady: want False/%s, got %+v", reasonUnreachableFromOperator, c)
	}
	if _, err := ReconcileDatabase(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if got := drainEvents(rec); len(got) != 1 {
		t.Fatalf("want one rate-limited Warning, got %v", got)
	}

	// Missing credentials still stop the pipeline.
	if err := rc.Client.Delete(ctx, sec); err != nil {
		t.Fatal(err)
	}
	rc.DB = nil
	if res, err := ReconcileDatabase(ctx, rc); err != nil || !res.Stop || rc.DB != nil {
		t.Fatalf("no credentials: want stop without rc.DB, got stop=%v db=%v err=%v", res.Stop, rc.DB != nil, err)
	}
}
