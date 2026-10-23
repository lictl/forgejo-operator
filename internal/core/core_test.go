package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPodDB1          = "db-1"
	testDomain          = "git.example.com"
	testConfigSecret    = "fj-config"
	testPhaseNewReplica = "Creating a new replica"
	testPodDB2          = "db-2"
	testClusterPodDB1   = "fj-db-1"
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

func TestReconcileIngressKeepsAdmissionClass(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: testDomain}
	// As created by the API server's DefaultIngressClass admission plugin.
	existing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.IngressName(f), Namespace: f.Namespace},
		Spec:       networkingv1.IngressSpec{IngressClassName: ptr.To("nginx")},
	}
	rc := testContext(t, f, existing)

	get := func() *networkingv1.Ingress {
		ing := &networkingv1.Ingress{}
		if err := rc.Client.Get(ctx, client.ObjectKeyFromObject(existing), ing); err != nil {
			t.Fatal(err)
		}
		return ing
	}

	if err := reconcileIngress(ctx, rc); err != nil {
		t.Fatal(err)
	}
	ing := get()
	if ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "nginx" {
		t.Fatalf("className unset: want the admission-set class nginx kept, got %v", ing.Spec.IngressClassName)
	}
	if len(ing.Spec.Rules) != 1 || ing.Spec.Rules[0].Host != testDomain {
		t.Fatalf("rules not reconciled: %+v", ing.Spec.Rules)
	}

	f.Spec.Ingress.ClassName = ptr.To("traefik")
	if err := reconcileIngress(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if ing := get(); ing.Spec.IngressClassName == nil || *ing.Spec.IngressClassName != "traefik" {
		t.Fatalf("className set: want traefik, got %v", ing.Spec.IngressClassName)
	}
}

func TestReconcileIngressCreateWithoutClass(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Ingress = &forgejov1.IngressSpec{Enabled: true, Host: testDomain}
	rc := testContext(t, f)
	if err := reconcileIngress(ctx, rc); err != nil {
		t.Fatal(err)
	}
	ing := &networkingv1.Ingress{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.IngressName(f)}, ing); err != nil {
		t.Fatal(err)
	}
	if ing.Spec.IngressClassName != nil {
		t.Fatalf("want no class (left to admission), got %q", *ing.Spec.IngressClassName)
	}
}

func TestDesiredReplicas(t *testing.T) {
	hpa := func(minR, maxR int32) *forgejov1.Forgejo {
		f := testInstance()
		f.Status.Image = img16
		f.Spec.Replicas = ptr.To[int32](3)
		f.Spec.HighAvailability.Enabled = true
		f.Spec.Autoscaling.Horizontal = &forgejov1.HorizontalAutoscalingSpec{Enabled: true, MinReplicas: minR, MaxReplicas: maxR}
		return f
	}
	plain := func(replicas int32) *forgejov1.Forgejo {
		f := testInstance()
		f.Status.Image = img16
		f.Spec.Replicas = ptr.To(replicas)
		return f
	}
	tests := []struct {
		name    string
		f       *forgejov1.Forgejo
		rc      forgejo.ReconcileContext
		exists  bool
		current *int32
		hold    bool
		want    int32
	}{
		{name: "maintenance", f: hpa(2, 5), rc: forgejo.ReconcileContext{Maintenance: true, HPAManaged: false}, exists: true, current: ptr.To[int32](3), want: 0},
		{name: "override", f: hpa(2, 5), rc: forgejo.ReconcileContext{ReplicasOverride: ptr.To[int32](0)}, exists: true, current: ptr.To[int32](3), want: 0},
		{name: "upgrading", f: hpa(2, 5), rc: forgejo.ReconcileContext{Upgrading: true}, exists: true, current: ptr.To[int32](0), want: 1},
		{name: "hpa keeps its count", f: hpa(2, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](3), want: 3},
		{name: "hpa below min left to hpa", f: hpa(2, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](1), want: 1},
		{name: "hpa at zero after restore", f: hpa(2, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](0), want: 2},
		{name: "hpa at zero, min 0 clamps to 1", f: hpa(0, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](0), want: 1},
		{name: "hpa at zero, max below min", f: hpa(4, 3), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](0), want: 3},
		{name: "hpa, deployment recreated", f: hpa(2, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: false, want: 2},
		{name: "hpa at zero during migration hold", f: hpa(2, 5), rc: forgejo.ReconcileContext{HPAManaged: true}, exists: true, current: ptr.To[int32](0), hold: true, want: 1},
		{name: "migration hold", f: plain(3), exists: true, current: ptr.To[int32](0), hold: true, want: 1},
		{name: "migration hold, stopped instance", f: plain(0), exists: true, current: ptr.To[int32](0), hold: true, want: 0},
		{name: "spec replicas without HA clamp to 1", f: plain(3), exists: true, current: ptr.To[int32](0), want: 1},
		{name: "spec replicas 0", f: plain(0), exists: true, current: ptr.To[int32](1), want: 0},
		{name: "spec replicas with HA", f: withHA(plain(3)), exists: true, current: ptr.To[int32](0), want: 3},
		{name: "failed upgrade holds its image", f: heldUpgrade(hpa(2, 5)), rc: forgejo.ReconcileContext{HPAManaged: true},
			exists: true, current: ptr.To[int32](3), want: 1},
		{name: "first install", f: func() *forgejov1.Forgejo { f := plain(3); f.Status.Image = ""; return f }(), want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rc := tt.rc
			rc.Instance = tt.f
			if got := desiredReplicas(&rc, tt.exists, tt.current, tt.hold); got != tt.want {
				t.Fatalf("desiredReplicas = %d, want %d", got, tt.want)
			}
		})
	}
}

func withHA(f *forgejov1.Forgejo) *forgejov1.Forgejo {
	f.Spec.HighAvailability.Enabled = true
	return f
}

// heldUpgrade marks f with a failed upgrade whose new image is still deployed, unverified.
func heldUpgrade(f *forgejov1.Forgejo) *forgejov1.Forgejo {
	f.Status.Image = "codeberg.org/forgejo/forgejo:15"
	f.Status.Upgrade = &forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseFailed, FromImage: f.Status.Image, ToImage: img16,
		FailedImage: img16, RolloutStartTime: ptr.To(metav1.Now()),
	}
	return f
}

func TestDeploymentStrategyDuringMigrationHold(t *testing.T) {
	f := testInstance()
	f.Spec.HighAvailability.Enabled = true
	rc := &forgejo.ReconcileContext{Instance: f}
	if s := deploymentStrategy(rc, false); s.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("HA: want RollingUpdate, got %s", s.Type)
	}
	if s := deploymentStrategy(rc, true); s.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("HA during migration hold: want Recreate, got %s", s.Type)
	}
	heldUpgrade(f)
	if !forgejo.UpgradeHoldsWorkload(f) {
		t.Fatal("test setup: want UpgradeHoldsWorkload")
	}
	if s := deploymentStrategy(rc, false); s.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("HA while a failed upgrade holds its image: want Recreate, got %s", s.Type)
	}
}

func TestMigrationHold(t *testing.T) {
	dep := func(annotated bool, specReplicas, gen, observed, updated, available, replicas int32) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Generation: int64(gen)}}
		if annotated {
			d.Annotations = map[string]string{annotationMigrationHold: "r1"}
		}
		d.Spec.Replicas = ptr.To(specReplicas)
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: int64(observed), UpdatedReplicas: updated, AvailableReplicas: available, Replicas: replicas}
		return d
	}
	tests := []struct {
		name    string
		dep     *appsv1.Deployment
		restore string
		want    string
	}{
		{"restore migrating", dep(false, 0, 5, 5, 0, 0, 0), "r2", "r2"},
		{"no restore, no hold", dep(false, 3, 5, 5, 3, 3, 3), "", ""},
		{"held, not yet observed", dep(true, 1, 6, 5, 0, 0, 0), "", "r1"},
		{"held, pod starting", dep(true, 1, 6, 6, 1, 0, 1), "", "r1"},
		{"held, pod available", dep(true, 1, 6, 6, 1, 1, 1), "", ""},
		{"held, stopped", dep(true, 0, 6, 6, 0, 0, 0), "", "r1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := migrationHold(tt.dep, tt.restore); got != tt.want {
				t.Fatalf("migrationHold = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReconcileWorkloadHoldsSingleReplicaWhileRestoredDBMigrates(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Replicas = ptr.To[int32](3)
	f.Spec.HighAvailability.Enabled = true
	f.Status.Image = img16
	restore := &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: f.Namespace},
		Spec:       forgejov1.ForgejoRestoreSpec{ForgejoRef: forgejov1.ForgejoRef{Name: f.Name}, BackupName: "b1"},
		Status:     forgejov1.ForgejoRestoreStatus{Phase: forgejov1.RestorePhaseScalingUp},
	}
	rc := testContext(t, f, restore)
	rc.RestoreMigrating = true // set by the maintenance step while r1 is in ScalingUp
	rc.TargetImage = f.Status.Image
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.ConfigHash, rc.ConfigSecretName = "abc", testConfigSecret

	key := types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}
	run := func() *appsv1.Deployment {
		t.Helper()
		if _, err := ReconcileWorkload(ctx, rc); err != nil {
			t.Fatal(err)
		}
		d := &appsv1.Deployment{}
		if err := rc.Client.Get(ctx, key, d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	d := run()
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || d.Annotations[annotationMigrationHold] != "r1" {
		t.Fatalf("restore in ScalingUp: want 1 replica, Recreate, hold r1; got %d, %s, %q",
			*d.Spec.Replicas, d.Spec.Strategy.Type, d.Annotations[annotationMigrationHold])
	}

	// The restore gives up (timeout) before the pod is available: the hold stays.
	restore.Status.Phase = forgejov1.RestorePhaseCompleted
	if err := rc.Client.Update(ctx, restore); err != nil {
		t.Fatal(err)
	}
	rc.RestoreMigrating = false
	if d := run(); *d.Spec.Replicas != 1 {
		t.Fatalf("restore done, pod not available: want 1 replica, got %d", *d.Spec.Replicas)
	}

	// The single pod migrated and is available: release to spec.replicas.
	d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1}
	if err := rc.Client.Status().Update(ctx, d); err != nil {
		t.Fatal(err)
	}
	d = run()
	if *d.Spec.Replicas != 3 || d.Spec.Strategy.Type != appsv1.RollingUpdateDeploymentStrategyType {
		t.Fatalf("after migration: want 3 replicas, RollingUpdate; got %d, %s", *d.Spec.Replicas, d.Spec.Strategy.Type)
	}
	if _, ok := d.Annotations[annotationMigrationHold]; ok {
		t.Fatal("hold annotation not removed")
	}
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
	f.Spec.Database.CNPG.Instances = 3
	f.Status.Image = img16
	appSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CNPGAppSecretName(f), Namespace: f.Namespace},
		Data:       map[string][]byte{"username": []byte("forgejo"), "password": []byte("pw")},
	}
	primary := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: testClusterPodDB1, Namespace: f.Namespace},
		Status:     corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}},
	}
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	cluster.SetName(forgejo.DatabaseClusterName(f))
	cluster.SetNamespace(f.Namespace)
	cluster.Object["status"] = map[string]any{
		"phase": testPhaseNewReplica, "readyInstances": int64(2),
		"currentPrimary": testClusterPodDB1, "targetPrimary": testClusterPodDB1,
	}

	s := testScheme(t)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	c := fake.NewClientBuilder().WithScheme(s).WithRESTMapper(mapper).WithObjects(appSecret, primary, cluster).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, RESTMapper: mapper}

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
	if err := c.Status().Update(ctx, primary); err != nil {
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

// TestBootstrapScript runs the bootstrap script against a fake forgejo CLI.
func TestBootstrapScript(t *testing.T) {
	for _, tool := range []string{"sh", "sha256sum", "awk", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "forgejo")
	calls := filepath.Join(dir, "calls")
	// The fake CLI logs its subcommand, lists the admin once "created" exists and fails
	// change-password when "reject" exists (a password policy rejecting the password).
	fakeCLI := `#!/bin/sh
echo "$*" | cut -d' ' -f1-3 >> "` + calls + `"
case "$1 $2 $3" in
  "migrate --config "*) [ ! -f "` + dir + `/migrate-fails" ] ;;
  "admin user list") echo "ID Username Email IsActive IsAdmin 2FA"; [ -f "` + dir + `/created" ] && echo "1 Admin a@b true true false"; true ;;
  "admin user create") touch "` + dir + `/created" ;;
  "admin user change-password") [ ! -f "` + dir + `/reject" ] ;;
esac
`
	if err := os.WriteFile(cli, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	l := forgejo.ImageLayout{CLI: cli, ConfigPath: filepath.Join(dir, "app.ini"), WorkPath: filepath.Join(data, "gitea"),
		CustomPath: filepath.Join(data, "gitea", "custom"), HomePath: filepath.Join(data, "git"),
		RepoRoot: filepath.Join(data, "git", "repositories"), LFSPath: filepath.Join(data, "git", "lfs")}
	run := func(password string) (string, error) {
		_ = os.Remove(calls)
		cmd := exec.Command("sh", "-c", bootstrapScript(l))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ADMIN_USERNAME=admin", "ADMIN_EMAIL=a@b",
			"ADMIN_PASSWORD=" + password, "ADMIN_SYNC_SALT=salt"}
		out, err := cmd.CombinedOutput()
		b, _ := os.ReadFile(calls)
		return string(b), func() error {
			if err != nil {
				t.Logf("output: %s", out)
			}
			return err
		}()
	}

	got, err := run("-pw1")
	if err != nil || !strings.Contains(got, "admin user create") {
		t.Fatalf("first start: want create, got %q, %v", got, err)
	}
	for _, d := range dataDirs(l) {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("data directory %s not created: %v", d, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(l.HomePath, ".ssh")); fi.Mode().Perm() != 0o700 {
		t.Fatalf(".ssh mode %v, want 0700", fi.Mode().Perm())
	}
	if got, err = run("-pw1"); err != nil || strings.Contains(got, "change-password") {
		t.Fatalf("unchanged Secret: want no password reset, got %q, %v", got, err)
	}
	if got, err = run("pw2"); err != nil || !strings.Contains(got, "change-password") {
		t.Fatalf("changed Secret: want a reset, got %q, %v", got, err)
	}
	if got, err = run("pw2"); err != nil || strings.Contains(got, "change-password") {
		t.Fatalf("synced: want no reset, got %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reject"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = run("pw3"); err != nil || !strings.Contains(got, "change-password") {
		t.Fatalf("rejected password: want a non-fatal attempt, got %q, %v", got, err)
	}
	if got, err = run("pw3"); err != nil || !strings.Contains(got, "change-password") {
		t.Fatalf("rejected password: want a retry on the next start, got %q, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "migrate-fails"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = run("pw3"); err == nil {
		t.Fatal("a failed migration must fail the init container")
	}
	_ = os.Remove(filepath.Join(dir, "migrate-fails"))

	// A data directory the Forgejo user cannot write fails before anything else runs.
	if os.Getuid() != 0 {
		if err := os.Chmod(l.LFSPath, 0o555); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(l.LFSPath, 0o755) }()
		if got, err := run("pw3"); err == nil || strings.Contains(got, "migrate") {
			t.Fatalf("unwritable %s: want a failure before migrate, got %q, %v", l.LFSPath, got, err)
		}
	}
}
