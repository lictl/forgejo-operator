package autopilot

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// --- F09: storage is its own step ---

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
	(&fakeExec{df: func() (string, error) { return "", nil }}).install(t)

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
	fx := &fakeExec{df: func() (string, error) {
		return "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 104858 943718 10% /data\n", nil
	}}
	fx.install(t)

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
			(&fakeExec{df: func() (string, error) {
				return "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/sdb 1048576 104858 943718 10% /data\n", nil
			}}).install(t)
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

// --- F57: rc.ValkeyURL is deterministic; maintenance does not wait for Valkey ---

func haInstance() *forgejov1.Forgejo {
	f := testInstance()
	f.Spec.HighAvailability.Enabled = true
	return f
}

// valkeyDBURL is how core renders a database of rc.ValkeyURL (for test fixtures).
func valkeyDBURL(u string, db int) string {
	base, query, _ := strings.Cut(u, "?")
	return base + "/" + strconv.Itoa(db) + "?" + query
}

func secretsWithValkeyPassword(f *forgejov1.Forgejo, pw string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data:       map[string][]byte{valkeyPasswordKey: []byte(pw)},
	}
}

func TestReconcileHASetsValkeyURLWhileValkeyNotReady(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		f := haInstance()
		env := newTestEnv(t, f, secretsWithValkeyPassword(f, "pw"))
		env.rc.Maintenance = maintenance
		res, err := ReconcileHA(context.Background(), env.rc)
		if err != nil {
			t.Fatal(err)
		}
		if res.Stop == maintenance {
			t.Fatalf("maintenance=%v: Stop = %v", maintenance, res.Stop)
		}
		if res.RequeueAfter == 0 {
			t.Fatalf("maintenance=%v: no requeue while Valkey is not ready", maintenance)
		}
		if want := valkeyURL(f, "pw"); env.rc.ValkeyURL != want {
			t.Fatalf("maintenance=%v: ValkeyURL = %q, want %q", maintenance, env.rc.ValkeyURL, want)
		}
	}
}

func TestReconcileHAValkeyURLSameWhenReady(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, "pw"))
	if _, err := ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	notReady := env.rc.ValkeyURL
	sts := &appsv1.StatefulSet{}
	if err := env.rc.Client.Get(ctx, client.ObjectKey{Namespace: f.Namespace, Name: forgejo.ValkeyName(f)}, sts); err != nil {
		t.Fatal(err)
	}
	sts.Status = appsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: 3, ReadyReplicas: 1, AvailableReplicas: 1}
	if err := env.rc.Client.Status().Update(ctx, sts); err != nil {
		t.Fatal(err)
	}
	env.rc.ValkeyURL = ""
	res, err := ReconcileHA(ctx, env.rc)
	if err != nil || res.Stop {
		t.Fatalf("ready Valkey: res=%+v err=%v", res, err)
	}
	if env.rc.ValkeyURL == "" || env.rc.ValkeyURL != notReady {
		t.Fatalf("ValkeyURL changed with readiness: %q vs %q", env.rc.ValkeyURL, notReady)
	}
}

// The secrets step just created "<name>-secrets" and the cache has not seen it yet.
func TestReconcileHAReadsPasswordLiveWhenCacheLags(t *testing.T) {
	f := haInstance()
	env := newTestEnv(t, f)
	env.rc.APIReader = fake.NewClientBuilder().WithScheme(env.rc.Scheme).WithObjects(secretsWithValkeyPassword(f, "live")).Build()
	res, err := ReconcileHA(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if want := valkeyURL(f, "live"); env.rc.ValkeyURL != want {
		t.Fatalf("ValkeyURL = %q, want %q (res %+v)", env.rc.ValkeyURL, want, res)
	}
}

// Without the Secret anywhere the step waits, except in maintenance.
func TestReconcileHAMissingSecretStopsOnlyOutsideMaintenance(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		f := haInstance()
		env := newTestEnv(t, f)
		env.rc.Maintenance = maintenance
		res, err := ReconcileHA(context.Background(), env.rc)
		if err != nil {
			t.Fatal(err)
		}
		if res.Stop == maintenance || res.RequeueAfter == 0 {
			t.Fatalf("maintenance=%v: res = %+v", maintenance, res)
		}
	}
}
