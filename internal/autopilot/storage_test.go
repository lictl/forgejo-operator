package autopilot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

func dataPVC(f *forgejov1.Forgejo, request, capacity string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DataPVCName(f), Namespace: f.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("standard"),
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(request)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)}},
	}
}

func mountingPod(f *forgejov1.Forgejo) *corev1.Pod {
	p := readyPod(f, "fj-data", testHash, time.Hour)
	p.Spec.Volumes = []corev1.Volume{{Name: volData, VolumeSource: corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: forgejo.DataPVCName(f)}}}}
	p.Spec.Containers = []corev1.Container{{Name: serverContainerName, VolumeMounts: []corev1.VolumeMount{{Name: volData, MountPath: "/data"}}}}
	return p
}

func storageInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	f.Spec.AutoPilot.StorageAutoExpand.ThresholdPercent = 80
	return f
}

type fakeExec struct {
	mu    sync.Mutex
	calls map[string]int
	df    func() (string, error)
	du    func() (string, error)
}

func (e *fakeExec) install(t *testing.T) {
	t.Helper()
	e.calls = map[string]int{}
	old, oldWait := execFn, measureWait
	measureWait = 2 * time.Second
	execFn = func(_ context.Context, _, _, _ string, cmd []string) (string, error) {
		e.mu.Lock()
		e.calls[cmd[0]]++
		e.mu.Unlock()
		if cmd[0] == "df" {
			return e.df()
		}
		return e.du()
	}
	t.Cleanup(func() { execFn, measureWait = old, oldWait })
}

func (e *fakeExec) count(cmd string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[cmd]
}

// df of a 1 GiB volume 90% and 10% full, and of a larger file system the claim is a
// directory of (which du measures).
const (
	df90     = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 943718 104858 90% /data\n"
	df10     = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 104858 943718 10% /data\n"
	nodeFSDF = "Filesystem     1024-blocks      Used Available Capacity Mounted on\n/dev/vda1 409600000 100000000 309600000  25% /data\n"
)

// installDF answers every df with out.
func installDF(t *testing.T, out string) *fakeExec {
	t.Helper()
	fx := &fakeExec{df: func() (string, error) { return out, nil }}
	fx.install(t)
	return fx
}

// With auto-expansion off, usage is still measured and reported; nothing is expanded.
func TestStorageMeasuredWithAutoExpandDisabled(t *testing.T) {
	f := storageInstance()
	f.Spec.AutoPilot.StorageAutoExpand.Enabled = ptr.To(false)
	pvc := dataPVC(f, "1Gi", "1Gi")
	env := newTestEnv(t, f, pvc, mountingPod(f))
	installDF(t, df90)
	if err := reconcileStorage(context.Background(), env.rc, stateFor(f), nil); err != nil {
		t.Fatal(err)
	}
	if f.Status.Storage == nil || len(f.Status.Storage.Volumes) != 1 || f.Status.Storage.Volumes[0].UsedPercent != 89 {
		t.Fatalf("status.storage = %+v", f.Status.Storage)
	}
	var m dto.Metric
	if err := forgejo.VolumeUsedRatio.WithLabelValues(f.Namespace, f.Name, pvc.Name).Write(&m); err != nil {
		t.Fatal(err)
	}
	if v := m.GetGauge().GetValue(); v < 0.89 || v > 0.91 {
		t.Fatalf("volume_used_ratio = %v", v)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonAutoExpandDisabled {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	got := &corev1.PersistentVolumeClaim{}
	_ = env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(pvc), got)
	if !got.Spec.Resources.Requests.Storage().Equal(resource.MustParse("1Gi")) {
		t.Fatal("PVC expanded with auto-expansion disabled")
	}
}

// A slow du never holds the reconcile; its result is reported by a later one. Failures are
// backed off instead of retried on every check.
func TestDuRunsInBackgroundAndBacksOff(t *testing.T) {
	f := storageInstance()
	env := newTestEnv(t, f, dataPVC(f, "1Gi", "1Gi"), mountingPod(f))
	release := make(chan struct{})
	var fail atomic.Bool
	fx := &fakeExec{
		df: func() (string, error) { return nodeFSDF, nil },
		du: func() (string, error) {
			<-release
			if fail.Load() {
				return "", errors.New("command terminated with exit code 137")
			}
			return "524288\t/data\n", nil
		},
	}
	fx.install(t)
	st := stateFor(f)
	ctx := context.Background()

	start := time.Now()
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > measureWait+time.Second {
		t.Fatalf("reconcile waited %s for du", d)
	}
	if c := condition(f, forgejov1.ConditionStorageHealthy); c == nil || c.Reason != "MeasurementUnavailable" {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	if env.rc.RequeueAfter == 0 {
		t.Fatal("no requeue while du runs")
	}
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for st.measure.anyRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if v := f.Status.Storage.Volumes[0]; v.UsedBytes != 512<<20 || v.UsedPercent != 50 {
		t.Fatalf("du result not reported: %+v", v)
	}

	// du fails: the next attempts back off.
	fail.Store(true)
	st.measure.mu.Lock()
	c := st.measure.claims[forgejo.DataPVCName(f)]
	c.attempt, c.duAttempt = time.Time{}, time.Time{}
	st.measure.mu.Unlock()
	st.lastStorageCheck = time.Time{}
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	for st.measure.anyRunning() && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	calls := fx.count("sh")
	st.lastStorageCheck = time.Time{}
	st.measure.mu.Lock()
	c.attempt = time.Now().Add(-storageCheckInterval - time.Second) // the plain interval has passed
	st.measure.mu.Unlock()
	if err := reconcileStorage(ctx, env.rc, st, nil); err != nil {
		t.Fatal(err)
	}
	if fx.count("sh") != calls {
		t.Fatal("du retried right after a failure")
	}
	if v := f.Status.Storage.Volumes[0]; v.UsedBytes != 512<<20 {
		t.Fatalf("last good measurement not kept after a failure: %+v", v)
	}
}

func TestStalledResizeIsReportedAndFSResizeRestarts(t *testing.T) {
	f := storageInstance()
	pvc := dataPVC(f, "2Gi", "1Gi")
	long := metav1.NewTime(time.Now().Add(-40 * time.Minute))
	pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue, LastTransitionTime: long,
		Message: "Waiting for user to (re-)start a pod to finish file system resize of volume on node.",
	}}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, pvc, mountingPod(f), dep)
	installDF(t, df90)
	st := stateFor(f)
	if err := reconcileStorage(context.Background(), env.rc, st, dep); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonExpansionStalled || !strings.Contains(c.Message, "FileSystemResizePending") {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	got := &appsv1.Deployment{}
	_ = env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(dep), got)
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; !ok || lastAction(f, ActionPodRestarted).IsZero() {
		t.Fatal("no restart for a pending file system resize")
	}
	// Once per pending resize.
	f.Status.AutoPilot.RecentActions = nil
	st.lastStorageCheck = time.Time{}
	if err := reconcileStorage(context.Background(), env.rc, st, got); err != nil {
		t.Fatal(err)
	}
	if !lastAction(f, ActionPodRestarted).IsZero() {
		t.Fatal("restarted twice for the same resize")
	}
}

func TestParseDF(t *testing.T) {
	used, size, err := parseDF("Filesystem     1024-blocks    Used Available Capacity Mounted on\nserver:/export with space 2048 1024 1024 50% /data\n")
	if err != nil || used != 1024*1024 || size != 2048*1024 {
		t.Fatalf("used=%d size=%d err=%v", used, size, err)
	}
	if _, _, err := parseDF("df: /data: No such file or directory\n"); err == nil {
		t.Fatal("garbage parsed")
	}
}

// Without ClusterRole volume-usage bound (pods/exec), nothing is measured or expanded:
// StorageHealthy is Unknown/MeasurementUnavailable and names the ClusterRole, and du is not
// tried after a refused df.
func TestStorageWithoutPodsExec(t *testing.T) {
	f := storageInstance()
	pvc := dataPVC(f, "1Gi", "1Gi")
	env := newTestEnv(t, f, pvc, mountingPod(f))
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods/exec"}, "pod",
		errors.New(`User "system:serviceaccount:forgejo-operator-system:forgejo-operator-controller-manager" cannot create resource "pods/exec"`))
	fx := &fakeExec{
		df: func() (string, error) { return "", forbidden },
		du: func() (string, error) { return "", forbidden },
	}
	fx.install(t)
	if err := reconcileStorage(context.Background(), env.rc, stateFor(f), nil); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != "MeasurementUnavailable" ||
		!strings.Contains(c.Message, "forgejo-operator-volume-usage") {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	if n := fx.count("df"); n != 1 {
		t.Fatalf("df ran %d times", n)
	}
	if n := fx.count("sh"); n != 0 {
		t.Fatalf("du ran %d times after a refused df", n)
	}
	got := &corev1.PersistentVolumeClaim{}
	if err := env.rc.Client.Get(context.Background(), client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Resources.Requests.Storage().Equal(resource.MustParse("1Gi")) {
		t.Fatal("PVC changed without a measurement")
	}
}

// --- storage is its own step ---

// cnpgTestEnv is newTestEnv with the CNPG Cluster kind known to the fake client and the
// database not resolved, as when the database step stopped.
func cnpgTestEnv(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) testEnv {
	t.Helper()
	s := testScheme(t)
	s.AddKnownTypeWithName(forgejo.CNPGClusterGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(forgejo.CNPGClusterGVK.GroupVersion().WithKind("ClusterList"), &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	rec := events.NewFakeRecorder(100)
	return testEnv{rc: &forgejo.ReconcileContext{Client: c, Scheme: s, Recorder: rec, Instance: f}, recorder: rec}
}

func expandableClass() *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}, Provisioner: "test", AllowVolumeExpansion: ptr.To(true)}
}

func cnpgCluster(f *forgejov1.Forgejo, size string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	u.SetNamespace(f.Namespace)
	u.SetName(forgejo.DatabaseClusterName(f))
	_ = unstructured.SetNestedField(u.Object, size, "spec", "storage", "size")
	return u
}

func clusterSize(t *testing.T, env testEnv, f *forgejov1.Forgejo) string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	if err := env.rc.Client.Get(context.Background(), client.ObjectKey{Namespace: f.Namespace, Name: forgejo.DatabaseClusterName(f)}, u); err != nil {
		t.Fatal(err)
	}
	size, _, _ := unstructured.NestedString(u.Object, "spec", "storage", "size")
	return size
}

func dbPVC(f *forgejov1.Forgejo, size string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.DatabaseClusterName(f) + "-1", Namespace: f.Namespace, Labels: map[string]string{
			cnpgClusterLabel: forgejo.DatabaseClusterName(f), cnpgPVCRoleLabel: "PG_DATA",
		}},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: ptr.To("standard"),
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
	}
}

// crashingPostgres mounts the database volume but cannot run: PostgreSQL on a full disk.
func crashingPostgres(f *forgejov1.Forgejo, claim string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: claim, Namespace: f.Namespace, Labels: map[string]string{cnpgClusterLabel: forgejo.DatabaseClusterName(f)}},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "pgdata", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}},
			Containers: []corev1.Container{{Name: "postgres", VolumeMounts: []corev1.VolumeMount{{Name: "pgdata", MountPath: "/var/lib/postgresql/data"}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
			Name: "postgres", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
		}}},
	}
}

// fullDBInstance is a CNPG instance whose last measurement found its database volume 96%
// full; claim is that volume.
func fullDBInstance() (f *forgejov1.Forgejo, claim string) {
	f = testInstance()
	claim = forgejo.DatabaseClusterName(f) + "-1"
	f.Spec.Database.Type = forgejov1.DatabaseTypeCNPG
	f.Spec.AutoPilot.StorageAutoExpand.ThresholdPercent = 80
	f.Status.Storage = &forgejov1.StorageStatus{Volumes: []forgejov1.VolumeUsage{{
		ClaimName: claim, RequestedSize: "1Gi", UsedBytes: 1 << 30 / 100 * 97, CapacityBytes: 1 << 30, UsedPercent: 96,
	}}}
	return f, claim
}

// The database step stops when PostgreSQL cannot start on a full volume; the storage step
// still grows that volume, without rc.DB, a Deployment or a running database pod, and does
// not grow it again on the measurement taken before the resize.
func TestStorageStepGrowsFullDatabaseVolumeWhileDatabaseDown(t *testing.T) {
	ctx := context.Background()
	f, claim := fullDBInstance()
	pvc := dbPVC(f, "1Gi")
	env := cnpgTestEnv(t, f, pvc, expandableClass(), cnpgCluster(f, "1Gi"), crashingPostgres(f, claim))
	(&fakeExec{df: func() (string, error) { t.Fatal("measured a container that is not running"); return "", nil }}).install(t)

	if _, err := ReconcileStorage(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if got := clusterSize(t, env, f); got != "1536Mi" {
		t.Fatalf("CNPG storage size = %q, want 1536Mi", got)
	}
	if lastAction(f, ActionStorageExpanded).IsZero() {
		t.Fatal("no StorageExpanded action")
	}
	if c := condition(f, forgejov1.ConditionStorageHealthy); c == nil || c.Reason != "Expanding" {
		t.Fatalf("StorageHealthy = %+v", c)
	}

	// CNPG has resized the PVC, PostgreSQL still crash-loops and the settle time is over:
	// the 96% measurement is of the old 1Gi volume and must not trigger another expansion.
	got := &corev1.PersistentVolumeClaim{}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(pvc), got); err != nil {
		t.Fatal(err)
	}
	got.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("1536Mi")
	if err := env.rc.Client.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got.Status.Capacity[corev1.ResourceStorage] = resource.MustParse("1536Mi")
	if err := env.rc.Client.Status().Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if resizePending(got) {
		t.Fatal("fixture: resize still pending")
	}
	past := metav1.NewTime(time.Now().Add(-2 * expansionSettle))
	f.Status.Storage.Volumes[0].LastExpansionTime = &past
	f.Status.AutoPilot.RecentActions = nil
	stateFor(f).lastStorageCheck = time.Time{}
	if _, err := ReconcileStorage(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if got := clusterSize(t, env, f); got != "1536Mi" {
		t.Fatalf("grown again on a stale measurement: CNPG storage size = %q", got)
	}
	if !lastAction(f, ActionStorageExpanded).IsZero() {
		t.Fatal("StorageExpanded recorded on a stale measurement")
	}
	if c := condition(f, forgejov1.ConditionStorageHealthy); c == nil || c.Reason != "Expanding" {
		t.Fatalf("StorageHealthy = %+v", c)
	}
}

// A database volume whose CNPG Cluster is gone is reported, not turned into a step error;
// maintenance does not stop the step either.
func TestStorageStepToleratesMissingCluster(t *testing.T) {
	f, claim := fullDBInstance()
	env := cnpgTestEnv(t, f, dbPVC(f, "1Gi"), expandableClass(), crashingPostgres(f, claim))
	env.rc.Maintenance = true
	installDF(t, "")

	if _, err := ReconcileStorage(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	c := condition(f, forgejov1.ConditionStorageHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonDatabaseClusterMissing {
		t.Fatalf("StorageHealthy = %+v", c)
	}
	if !lastAction(f, ActionStorageExpanded).IsZero() {
		t.Fatal("StorageExpanded recorded without a Cluster")
	}
	if !hasEvent(env.events(), "Warning "+ReasonDatabaseClusterMissing) {
		t.Fatal("no DatabaseClusterMissing event")
	}
	if f.Status.Storage == nil || f.Status.Storage.Volumes[0].UsedPercent != 96 {
		t.Fatalf("status.storage = %+v", f.Status.Storage)
	}
}

// Reconcile no longer measures volumes; the storage step does, once per interval.
func TestStoragePassRunsOnlyInItsStep(t *testing.T) {
	ctx := context.Background()
	f := storageInstance()
	env := newTestEnv(t, f, dataPVC(f, "1Gi", "1Gi"), mountingPod(f), testDeployment(f, 1), expandableClass())
	fx := installDF(t, df10)

	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Storage != nil || fx.count("df") != 0 {
		t.Fatalf("Reconcile ran the storage pass: %+v, %d df", f.Status.Storage, fx.count("df"))
	}
	if _, err := ReconcileStorage(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if f.Status.Storage == nil || f.Status.Storage.Volumes[0].UsedPercent != 10 || fx.count("df") != 1 {
		t.Fatalf("status.storage = %+v, %d df", f.Status.Storage, fx.count("df"))
	}
	checked := *f.Status.Storage.LastCheckTime
	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if _, err := ReconcileStorage(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if fx.count("df") != 1 || !f.Status.Storage.LastCheckTime.Equal(&checked) {
		t.Fatal("storage pass ran twice within its interval")
	}
}

// The step may run before the upgrade step, or after a failed maintenance step: the restart
// for an offline file system resize also honours the restore and upgrade recorded in status,
// and is pointless at zero replicas.
func TestFilesystemResizeRestartHonoursStatusHolds(t *testing.T) {
	for name, mutate := range map[string]func(*forgejov1.Forgejo, *appsv1.Deployment){
		"active restore": func(f *forgejov1.Forgejo, _ *appsv1.Deployment) {
			f.Status.Backup = &forgejov1.BackupStatusSummary{ActiveRestore: "restore-1"}
		},
		"upgrade in flight": func(f *forgejov1.Forgejo, _ *appsv1.Deployment) {
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseRollingOut}
		},
		"scaled to zero": func(_ *forgejov1.Forgejo, dep *appsv1.Deployment) { dep.Spec.Replicas = ptr.To[int32](0) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := storageInstance()
			pvc := dataPVC(f, "2Gi", "1Gi")
			pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
				Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Now().Add(-40 * time.Minute)),
			}}
			dep := testDeployment(f, 1)
			mutate(f, dep)
			env := newTestEnv(t, f, pvc, mountingPod(f), dep)
			installDF(t, df10)
			if _, err := ReconcileStorage(ctx, env.rc); err != nil {
				t.Fatal(err)
			}
			got := &appsv1.Deployment{}
			_ = env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got)
			if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok || !lastAction(f, ActionPodRestarted).IsZero() {
				t.Fatal("restarted for a file system resize during a hold")
			}
		})
	}
}

func TestMeasurementPredatesResize(t *testing.T) {
	pvc := dataPVC(testInstance(), "1536Mi", "1536Mi")
	for _, tc := range []struct {
		name string
		m    measurement
		want bool
	}{
		{"taken at the current capacity", measurement{capacity: 1 << 30, claimCapacity: 1536 << 20}, false},
		{"taken before the resize", measurement{capacity: 1 << 30, claimCapacity: 1 << 30}, true},
		{"carried over, file system of the old volume", measurement{capacity: 1 << 30 * 97 / 100}, true},
		{"carried over, file system of the current volume", measurement{capacity: 1536 << 20 * 95 / 100}, false},
	} {
		if got := tc.m.predatesResize(pvc); got != tc.want {
			t.Errorf("%s: predatesResize = %v, want %v", tc.name, got, tc.want)
		}
	}
	if (measurement{capacity: 1}).predatesResize(&corev1.PersistentVolumeClaim{}) {
		t.Error("a PVC without a reported capacity counts as resized")
	}
}
