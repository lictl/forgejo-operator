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
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	img15 = "codeberg.org/forgejo/forgejo:15"
	img16 = "codeberg.org/forgejo/forgejo:16"
)

func statusDeployment(f *forgejov1.Forgejo, image string, replicas int32, st appsv1.DeploymentStatus) *appsv1.Deployment {
	st.ObservedGeneration = 10
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DeploymentName(f), Namespace: f.Namespace, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: containerName, Image: image}}}},
		},
		Status: st,
	}
}

// updateDeployment writes dep's spec, then st as its status.
func updateDeployment(t *testing.T, rc *forgejo.ReconcileContext, dep *appsv1.Deployment, st appsv1.DeploymentStatus) {
	t.Helper()
	ctx := context.Background()
	if err := rc.Client.Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
	dep.Status = st
	if err := rc.Client.Status().Update(ctx, dep); err != nil {
		t.Fatal(err)
	}
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

// F15: a Deployment scaled to 0 replicas must not verify its template's image.
func TestReconcileStatusVerifiesImageOnlyWithAvailablePods(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img15
	dep := statusDeployment(f, img16, 0, appsv1.DeploymentStatus{})
	rc := testContext(t, f, dep)
	rc.TargetImage = img16

	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img15 {
		t.Fatalf("0 replicas: status.image = %s, want it unchanged (%s)", f.Status.Image, img15)
	}

	// Maintenance with a stale Deployment status still showing an available pod.
	dep.Spec.Replicas = ptr.To[int32](1)
	updateDeployment(t, rc, dep, appsv1.DeploymentStatus{ObservedGeneration: 10, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1})
	rc.Maintenance = true
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img15 {
		t.Fatalf("maintenance: status.image = %s, want %s", f.Status.Image, img15)
	}

	rc.Maintenance = false
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Image != img16 || f.Status.Phase != forgejov1.PhaseRunning {
		t.Fatalf("one available pod: want image %s and Running, got %s, %s", img16, f.Status.Image, f.Status.Phase)
	}
}

// F18: a ReplicaSet that cannot create pods (Pod Security admission) is reported.
func TestReconcileStatusReportsReplicaFailure(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img16
	msg := `pods "fj-abc" is forbidden: violates PodSecurity "restricted:latest": runAsNonRoot != true`
	dep := statusDeployment(f, img16, 1, appsv1.DeploymentStatus{
		Conditions: []appsv1.DeploymentCondition{
			{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "ReplicaSetUpdated"},
			{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: msg},
		},
	})
	rc := testContext(t, f, dep)
	rc.TargetImage = img16
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionReady)
	prog := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionProgressing)
	if ready == nil || ready.Reason != reasonReplicaFailure || !strings.Contains(ready.Message, "PodSecurity") {
		t.Fatalf("Ready: want %s with the admission message, got %+v", reasonReplicaFailure, ready)
	}
	if prog == nil || prog.Status != metav1.ConditionFalse || prog.Reason != reasonReplicaFailure {
		t.Fatalf("Progressing: want False/%s, got %+v", reasonReplicaFailure, prog)
	}
	if f.Status.Phase != forgejov1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", f.Status.Phase)
	}
}

// A Recreate rollout whose old pod is still terminating is in flight, not Degraded.
func TestObserveRolloutRecreateOldPodTerminating(t *testing.T) {
	f := testInstance()
	dep := statusDeployment(f, img16, 1, appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 0,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"}}})
	if ro := observeRollout(dep, true); !ro.inFlight || ro.complete {
		t.Fatalf("old pod terminating: want in flight, got %+v", ro)
	}
	dep.Status.Conditions[0] = appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}
	if ro := observeRollout(dep, true); ro.inFlight || !ro.deadlineExceeded {
		t.Fatalf("deadline exceeded: want not in flight, got %+v", ro)
	}
}

// Follow-ups: the Progressing message names the upgrade's images, and a failed upgrade's
// held image that is not available makes the phase Degraded.
func TestReconcileStatusUpgradeReporting(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Status.Image = img15
	f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseBackingUp, FromImage: img15, ToImage: img16}
	dep := statusDeployment(f, img15, 0, appsv1.DeploymentStatus{})
	rc := testContext(t, f, dep)
	rc.TargetImage, rc.Upgrading = img15, true

	for phase, want := range map[forgejov1.UpgradePhase]string{
		forgejov1.UpgradePhaseBackingUp:   "Backing up " + img15 + " before upgrading to " + img16,
		forgejov1.UpgradePhaseRollingBack: "Rolling back from " + img16 + " to " + img15,
		forgejov1.UpgradePhaseRollingOut:  "Upgrading from " + img15 + " to " + img16,
	} {
		f.Status.Upgrade.Phase = phase
		if _, err := ReconcileStatus(ctx, rc); err != nil {
			t.Fatal(err)
		}
		if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionProgressing); c == nil || c.Message != want {
			t.Fatalf("%s: Progressing message %+v, want %q", phase, c, want)
		}
	}

	// The upgrade failed and its image stays deployed, still starting.
	rc.Upgrading = false
	heldUpgrade(f)
	rc.TargetImage = img16
	dep.Spec.Replicas = ptr.To[int32](1)
	dep.Spec.Template.Spec.Containers[0].Image = img16
	updateDeployment(t, rc, dep, appsv1.DeploymentStatus{ObservedGeneration: 10, Replicas: 1, UpdatedReplicas: 1,
		Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "ReplicaSetUpdated"}}})
	if _, err := ReconcileStatus(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Phase != forgejov1.PhaseDegraded || f.Status.Image != img15 {
		t.Fatalf("held failed upgrade: want Degraded with image %s, got %s, %s", img15, f.Status.Phase, f.Status.Image)
	}
}

// F52: an external database the operator cannot dial does not stop the pipeline.
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

// Follow-up: the resize warning for a non-expandable class is not repeated every reconcile.
func TestStorageResizeWarningRateLimited(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Storage.Size = resource.MustParse("20Gi")
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "fixed"}, Provisioner: "x", AllowVolumeExpansion: ptr.To(false)}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("fixed"),
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
	}
	rc := testContext(t, f, sc, pvc)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	for range 3 {
		if _, err := ReconcileStorage(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	if got := drainEvents(rec); len(got) != 1 || !strings.Contains(got[0], "StorageResizeUnsupported") {
		t.Fatalf("want a single StorageResizeUnsupported event, got %v", got)
	}
	// A different requested size is a new problem, reported at once.
	f.Spec.Storage.Size = resource.MustParse("30Gi")
	if _, err := ReconcileStorage(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if got := drainEvents(rec); len(got) != 1 {
		t.Fatalf("new size: want one event, got %v", got)
	}
}

// F17: spec.replicas > 1 without highAvailability runs one pod and says so.
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
	rc.ConfigHash, rc.ConfigSecretName = "abc", "fj-config"
	key := types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}

	for range 2 {
		if _, err := ReconcileWorkload(ctx, rc); err != nil {
			t.Fatal(err)
		}
	}
	d := &appsv1.Deployment{}
	if err := rc.Client.Get(ctx, key, d); err != nil {
		t.Fatal(err)
	}
	if *d.Spec.Replicas != 1 {
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
	if err := rc.Client.Get(ctx, key, d); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded); *d.Spec.Replicas != 3 || c.Status != metav1.ConditionFalse {
		t.Fatalf("with HA: want 3 replicas and Degraded cleared, got %d, %+v", *d.Spec.Replicas, c)
	}
}

// F51/F18/F19: the default pod passes the restricted Pod Security Standard; the root init
// container is opt-in.
func TestPodSpecSecurity(t *testing.T) {
	f := testInstance()
	rc := &forgejo.ReconcileContext{Instance: f, TargetImage: img16, Layout: forgejo.LayoutForImage(img16), ConfigSecretName: "fj-config"}

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

	f.Annotations = map[string]string{AnnotationFixPermissions: "true"}
	spec = podSpec(rc)
	if len(spec.InitContainers) != 2 || spec.InitContainers[0].Name != initPermissionsContainer ||
		ptr.Deref(spec.InitContainers[0].SecurityContext.RunAsUser, -1) != 0 {
		t.Fatalf("fix-permissions: want the root init-permissions container first, got %+v", spec.InitContainers)
	}
	if script := initPermissionsScript(rc.Layout); strings.Contains(script, "set -e") || !strings.Contains(script, "stat -c %u:%g") {
		t.Fatalf("init-permissions must chown only on mismatch and tolerate failures:\n%s", script)
	}
}

// F18: the opt-in root script tolerates a volume that refuses chown.
func TestInitPermissionsScriptToleratesChownFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("chown succeeds as root")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	data := filepath.Join(t.TempDir(), "data")
	// uid/gid 0 cannot be given away by a non-root user: every chown fails.
	l := forgejo.ImageLayout{DataMountPath: data, WorkPath: filepath.Join(data, "gitea"), CustomPath: filepath.Join(data, "gitea", "custom"),
		HomePath: filepath.Join(data, "git"), RepoRoot: filepath.Join(data, "git", "repositories"), LFSPath: filepath.Join(data, "git", "lfs")}
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

// Follow-up: rc.RestoreMigrating (set by the maintenance step) drives the migration hold.
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

// F16: rendered names follow the detected cluster domain.
func TestClusterDomainNames(t *testing.T) {
	old := forgejo.ClusterDomain
	defer func() { forgejo.ClusterDomain = old }()
	forgejo.ClusterDomain = "k8s.example"
	f := testInstance()
	if got := RootURL(f); got != "http://fj-http.ns.svc.k8s.example:3000/" {
		t.Fatalf("ROOT_URL = %s", got)
	}
	if got := sshDomain(f); got != "fj-ssh.ns.svc.k8s.example" {
		t.Fatalf("SSH_DOMAIN = %s", got)
	}
	if got := probeHosts("pg", "ns"); strings.Join(got, ",") != "pg.ns.svc,pg.ns.svc.k8s.example,pg" {
		t.Fatalf("probeHosts = %v", got)
	}
}
