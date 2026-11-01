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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

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

	run := func() *appsv1.Deployment {
		t.Helper()
		if _, err := ReconcileWorkload(ctx, rc); err != nil {
			t.Fatal(err)
		}
		return getDeployment(t, rc)
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

// rc.RestoreMigrating (set by the maintenance step) drives the migration hold.
func TestMigratingRestoreFromMaintenanceFlag(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	r := &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: f.Namespace},
		Spec:       forgejov1.ForgejoRestoreSpec{ForgejoRef: forgejov1.ForgejoRef{Name: f.Name}, BackupName: "b1"},
		Status:     forgejov1.ForgejoRestoreStatus{Phase: forgejov1.RestorePhaseScalingUp},
	}
	rc := testContext(t, f)
	if name, err := migratingRestore(ctx, rc); err != nil || name != "" {
		t.Fatalf("not migrating: got %q, %v", name, err)
	}
	rc.RestoreMigrating = true
	if name, err := migratingRestore(ctx, rc); err != nil || name != "restore" {
		t.Fatalf("migrating, restore not in the cache yet: want a hold, got %q, %v", name, err)
	}
	if err := rc.Client.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	if name, err := migratingRestore(ctx, rc); err != nil || name != "r1" {
		t.Fatalf("migrating: want the hold named r1, got %q, %v", name, err)
	}
}

// spec.replicas > 1 without highAvailability runs one pod and says so.
func TestReconcileWorkloadClampsReplicasWithoutHA(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Replicas = ptr.To[int32](3)
	f.Status.Image = img16
	rc := testContext(t, f)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	rc.TargetImage = img16
	rc.Layout = forgejo.LayoutForImage(img16)
	rc.ConfigHash, rc.ConfigSecretName = "abc", testConfigSecret

	for range 2 {
		if _, err := ReconcileWorkload(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	if d := getDeployment(t, rc); *d.Spec.Replicas != 1 {
		t.Fatalf("replicas = %d, want 1", *d.Spec.Replicas)
	}
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonReplicasRequireHA {
		t.Fatalf("Degraded: want True/%s, got %+v", ReasonReplicasRequireHA, c)
	}
	if got := drainEvents(rec); len(got) != 1 {
		t.Fatalf("want one Warning, got %v", got)
	}

	// Another feature's Degraded reason is not hidden.
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, "UpgradeFailed", "x")
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); c.Reason != "UpgradeFailed" {
		t.Fatalf("want UpgradeFailed kept, got %s", c.Reason)
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonReplicasRequireHA, "x")

	f.Spec.HighAvailability.Enabled = true
	if _, err := ReconcileWorkload(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if d, c := getDeployment(t, rc), meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); *d.Spec.Replicas != 3 || c.Status != metav1.ConditionFalse {
		t.Fatalf("with HA: want 3 replicas and Degraded cleared, got %d, %+v", *d.Spec.Replicas, c)
	}
}

// The default pod passes the restricted Pod Security Standard; the root init container is
// opt-in.
func TestPodSpecSecurity(t *testing.T) {
	f := testInstance()
	rc := &forgejo.ReconcileContext{Instance: f, TargetImage: img16, Layout: forgejo.LayoutForImage(img16), ConfigSecretName: testConfigSecret}

	spec := podSpec(rc)
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "bootstrap" {
		t.Fatalf("default: want only the bootstrap init container, got %d", len(spec.InitContainers))
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		sc := c.SecurityContext
		if sc == nil || !ptr.Deref(sc.RunAsNonRoot, false) || ptr.Deref(sc.RunAsUser, 0) == 0 ||
			ptr.Deref(sc.AllowPrivilegeEscalation, true) || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 {
			t.Fatalf("container %s is not restricted-compliant: %+v", c.Name, sc)
		}
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("want the RuntimeDefault seccomp profile")
	}
	if p := spec.Containers[0].StartupProbe; p.PeriodSeconds*p.FailureThreshold < 3600 {
		t.Fatalf("startup budget %ds, want at least an hour", p.PeriodSeconds*p.FailureThreshold)
	}

	f.Annotations = map[string]string{AnnotationFixPermissions: valueTrue}
	spec = podSpec(rc)
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != initPermissionsContainer ||
		ptr.Deref(spec.InitContainers[0].SecurityContext.RunAsUser, -1) != 0 {
		t.Fatalf("fix-permissions: want the root init-permissions container first, got %+v", spec.InitContainers)
	}
	if script := initPermissionsScript(rc.Layout); strings.Contains(script, "set -e") || !strings.Contains(script, "stat -c %u:%g") {
		t.Fatalf("init-permissions must chown only on mismatch and tolerate failures:\n%s", script)
	}
}

// tempLayout is an image layout with the CLI, app.ini and the data volume in dir.
func tempLayout(dir string) forgejo.ImageLayout {
	data := filepath.Join(dir, "data")
	return forgejo.ImageLayout{CLI: filepath.Join(dir, "forgejo"), ConfigPath: filepath.Join(dir, "app.ini"), DataMountPath: data,
		WorkPath: filepath.Join(data, "gitea"), CustomPath: filepath.Join(data, "gitea", "custom"), HomePath: filepath.Join(data, "git"),
		RepoRoot: filepath.Join(data, "git", "repositories"), LFSPath: filepath.Join(data, "git", "lfs")}
}

// bootstrapCLI writes a fake forgejo CLI into a temporary directory dir and returns the layout
// that uses it (with traits), dir and run, which runs the bootstrap script with an admin
// password and returns the subcommands the CLI got. The fake CLI lists the admin once
// dir/created exists, records the arguments and stdin of change-password in dir/args and
// dir/stdin, and fails change-password when dir/reject exists (a password policy rejecting
// the password) and migrate when dir/migrate-fails exists.
func bootstrapCLI(t *testing.T, traits forgejo.Traits) (forgejo.ImageLayout, string, func(password string) (string, error)) {
	t.Helper()
	for _, tool := range []string{"sh", "sha256sum", "awk", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	l := tempLayout(dir)
	l.Traits = traits
	calls := filepath.Join(dir, "calls")
	fakeCLI := `#!/bin/sh
echo "$*" | cut -d' ' -f1-3 >> "` + calls + `"
case "$1 $2 $3" in
  "migrate --config "*) [ ! -f "` + dir + `/migrate-fails" ] ;;
  "admin user list") echo "ID Username Email IsActive IsAdmin 2FA"; [ -f "` + dir + `/created" ] && echo "1 Admin a@b true true false"; true ;;
  "admin user create") touch "` + dir + `/created" ;;
  "admin user change-password") echo "$*" > "` + dir + `/args"; cat > "` + dir + `/stdin"; [ ! -f "` + dir + `/reject" ] ;;
esac
`
	if err := os.WriteFile(l.CLI, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(password string) (string, error) {
		_ = os.Remove(calls)
		cmd := exec.Command("sh", "-c", bootstrapScript(l))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ADMIN_USERNAME=admin", "ADMIN_EMAIL=a@b",
			"ADMIN_PASSWORD=" + password, "ADMIN_SYNC_SALT=salt"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("output: %s", out)
		}
		b, _ := os.ReadFile(calls)
		return string(b), err
	}
	return l, dir, run
}

func TestBootstrapScript(t *testing.T) {
	l, dir, run := bootstrapCLI(t, forgejo.Traits{})
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
	if args, _ := os.ReadFile(filepath.Join(dir, "args")); !strings.Contains(string(args), "--password=pw2") {
		t.Fatalf("change-password args %q: want the password with --password", args)
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

// On Forgejo 18 the admin password reaches change-password through stdin, not argv.
func TestBootstrapScriptPasswordFromStdin(t *testing.T) {
	_, dir, run := bootstrapCLI(t, forgejo.Traits{PasswordFromStdin: true})
	if err := os.WriteFile(filepath.Join(dir, "created"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	const password = `p a%s'"$x\`
	if _, err := run(password); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if strings.Contains(string(args), "--password") || !strings.Contains(string(args), "--username admin") {
		t.Fatalf("args %q", args)
	}
	if string(stdin) != password+"\n"+password+"\n" {
		t.Fatalf("stdin %q, want the password and its confirmation", stdin)
	}
}

// The opt-in root script tolerates a volume that refuses chown.
func TestInitPermissionsScriptToleratesChownFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chown succeeds as root")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	// uid/gid 0 cannot be given away by a non-root user: every chown fails.
	l := tempLayout(t.TempDir())
	out, err := exec.Command("sh", "-c", initPermissionsScript(l)).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "WARNING: cannot chown") {
		t.Fatalf("want warnings and success, got %v:\n%s", err, out)
	}
	for _, d := range dataDirs(l) {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Fatalf("%s not created: %v", d, err)
		}
	}
}
