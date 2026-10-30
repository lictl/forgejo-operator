package backup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testForgejoUID            = "forgejo-uid"
	testReasonBackupSucceeded = "BackupSucceeded"
)

func ownedInstance(policy forgejov1.RetainPolicy) *forgejov1.Forgejo {
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "ns", UID: testForgejoUID}}
	f.Spec.Storage.RetainPolicy = policy
	return f
}

func reasonBackup(name string, reason forgejov1.BackupReason) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
	b.Spec.ForgejoRef.Name = "fb"
	b.Spec.Reason = reason
	return b
}

// Switching retainPolicy must change the ownership of existing backups and of the backup PVC,
// or deleting the instance garbage-collects backups the user meant to keep (and vice versa).
func TestReconcileOwnershipFollowsRetainPolicy(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	reasons := map[string]forgejov1.BackupReason{
		"sched":       forgejov1.BackupReasonScheduled,
		"pre":         forgejov1.BackupReasonPreUpgrade,
		"pre-restore": forgejov1.BackupReasonPreRestore,
		"manual":      forgejov1.BackupReasonManual,
	}

	// Created under Delete: scheduled and pre-upgrade backups and the PVC are owned.
	deleteF := ownedInstance(forgejov1.RetainPolicyDelete)
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: "ns"}}
	if err := controllerutil.SetControllerReference(deleteF, pvc, s); err != nil {
		t.Fatal(err)
	}
	objs := append(make([]client.Object, 0, 1+len(reasons)), pvc)
	for name, reason := range reasons {
		b := reasonBackup(name, reason)
		if err := controllerutil.SetOwnerReference(deleteF, b, s); err != nil { // even a manual one
			t.Fatal(err)
		}
		objs = append(objs, b)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	refsOf := func(name string) []metav1.OwnerReference {
		b := &forgejov1.ForgejoBackup{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: name}, b); err != nil {
			t.Fatal(err)
		}
		return b.OwnerReferences
	}
	pvcController := func() *metav1.OwnerReference {
		got := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: testBackupsPVC}, got); err != nil {
			t.Fatal(err)
		}
		return metav1.GetControllerOf(got)
	}

	// Delete -> Retain: every reference to the instance goes.
	retainF := ownedInstance(forgejov1.RetainPolicyRetain)
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: retainF}
	if _, err := ReconcileOwnership(ctx, rc); err != nil {
		t.Fatal(err)
	}
	for name := range reasons {
		if refs := refsOf(name); len(refs) != 0 {
			t.Errorf("Retain: backup %s still has owner references %v", name, refs)
		}
	}
	if ref := pvcController(); ref != nil {
		t.Errorf("Retain: backup PVC is still controlled by %v", ref)
	}

	// Retain -> Delete: scheduled and pre-upgrade backups and the PVC are owned again;
	// manual backups are left alone.
	rc.Instance = ownedInstance(forgejov1.RetainPolicyDelete)
	if _, err := ReconcileOwnership(ctx, rc); err != nil {
		t.Fatal(err)
	}
	for name, reason := range reasons {
		refs := refsOf(name)
		owned := len(refs) == 1 && refs[0].UID == testForgejoUID
		if want := reason != forgejov1.BackupReasonManual; owned != want {
			t.Errorf("Delete: backup %s owned=%v, want %v (%v)", name, owned, want, refs)
		}
	}
	if ref := pvcController(); ref == nil || ref.UID != testForgejoUID {
		t.Errorf("Delete: backup PVC controller = %v, want the instance", ref)
	}

	// Idempotent: nothing left to change.
	b := &forgejov1.ForgejoBackup{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "sched"}, b); err != nil {
		t.Fatal(err)
	}
	if changed, err := SyncBackupOwnership(ctx, c, s, rc.Instance, b); err != nil || changed {
		t.Errorf("second sync changed=%v err=%v", changed, err)
	}
}

func boundClaim(name, volume string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolumeClaim {
	if len(modes) == 0 {
		modes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	}
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       corev1.PersistentVolumeClaimSpec{AccessModes: modes, VolumeName: volume},
	}
}

func hostPV(name, host string, modes ...corev1.PersistentVolumeAccessMode) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: name}}
	pv.Spec.AccessModes = modes
	if host != "" {
		pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{host}}},
		}}}}
	}
	return pv
}

func node(name string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{corev1.LabelHostname: name}}}
}

func serverPod(f *forgejov1.Forgejo) *corev1.Pod {
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "fb-0", Namespace: "ns", Labels: forgejo.SelectorLabels(f)}}
	p.Spec.NodeName = "n1"
	p.Status.Phase = corev1.PodRunning
	return p
}

func TestPlacement(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	rwx := corev1.ReadWriteMany

	hasPodAffinity := func(a *corev1.Affinity) bool { return a != nil && a.PodAffinity != nil }
	nodeTerms := func(a *corev1.Affinity) string {
		if a == nil || a.NodeAffinity == nil {
			return ""
		}
		var hosts []string
		for _, t := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			for _, r := range t.MatchExpressions {
				hosts = append(hosts, r.Values...)
			}
		}
		return strings.Join(hosts, "+")
	}

	cases := []struct {
		name       string
		objs       []client.Object
		mountsData bool
		podAff     bool
		nodes      string
		conflict   bool
	}{{
		name:       "RWO data with pods: join the pods",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "")},
		mountsData: true, podAff: true,
	}, {
		name: "RWX data with pods: no pod affinity",
		objs: []client.Object{boundClaim("fb-data", "pv-data", rwx), hostPV("pv-data", "", rwx), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, nodes: "n2",
	}, {
		name:       "database-only first bind lands next to the data",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "")},
		mountsData: false, podAff: true,
	}, {
		name:       "cleanup of a bound backup volume: no data constraint",
		objs:       []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f), boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2")},
		mountsData: false,
	}, {
		name: "backup volume on another node than the running pods",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, conflict: true,
	}, {
		name: "backup volume next to the running pods",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"), serverPod(f),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n1"), node("n1"), node("n2")},
		mountsData: true, podAff: true, nodes: "n1",
	}, {
		name: "no pods: both PVs' node affinities, disjoint",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n2"), node("n1"), node("n2")},
		mountsData: true, conflict: true,
	}, {
		name: "no pods: both PVs' node affinities, same node",
		objs: []client.Object{boundClaim("fb-data", "pv-data"), hostPV("pv-data", "n1"),
			boundClaim("fb-backups", "pv-b"), hostPV("pv-b", "n1"), node("n1"), node("n2")},
		mountsData: true, nodes: "n1",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.objs...).Build()
			aff, err := Placement(ctx, c, f, tc.mountsData, "fb-backups")
			var conflict *VolumeConflictError
			if got := errors.As(err, &conflict); got != tc.conflict {
				t.Fatalf("conflict=%v (err %v), want %v", got, err, tc.conflict)
			}
			if tc.conflict {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if hasPodAffinity(aff) != tc.podAff || nodeTerms(aff) != tc.nodes {
				t.Errorf("pod affinity=%v node terms=%q, want %v %q", hasPodAffinity(aff), nodeTerms(aff), tc.podAff, tc.nodes)
			}
		})
	}
}

func gaugeValue(t *testing.T, ns string) float64 {
	t.Helper()
	m := &dto.Metric{}
	g, err := forgejo.BackupLastSuccess.GetMetricWithLabelValues(ns, "fb")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.(prometheus.Metric).Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}

func TestLastSuccessGaugeIsExportedFromStatus(t *testing.T) {
	t1 := metav1.NewTime(time.Unix(1_700_000_000, 0))
	t2 := metav1.NewTime(t1.Add(time.Hour))
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "gauge"}}
	withSuccess := func(at *metav1.Time) *forgejov1.Forgejo {
		g := f.DeepCopy()
		g.Status.Backup = &forgejov1.BackupStatusSummary{LastSuccessfulBackupTime: at}
		return g
	}
	ExportLastSuccess(withSuccess(&t2))
	if v := gaugeValue(t, "gauge"); v != float64(t2.Unix()) {
		t.Errorf("gauge = %v, want %v", v, t2.Unix())
	}
	// A summary from a lagging cache never moves the gauge back.
	ExportLastSuccess(withSuccess(&t1))
	if v := gaugeValue(t, "gauge"); v != float64(t2.Unix()) {
		t.Errorf("gauge went back to %v", v)
	}
	// Called on every reconcile: an instance without a recorded success (status not written
	// yet, or the step that writes it skipped) does not remove the series.
	ExportLastSuccess(f)
	ExportLastSuccess(withSuccess(nil))
	if v := gaugeValue(t, "gauge"); v != float64(t2.Unix()) {
		t.Errorf("gauge = %v after exporting an empty status, want %v", v, t2.Unix())
	}
	// No successful backup left: the series is removed, and a later success starts afresh.
	ForgetLastSuccess("gauge", "fb")
	if forgejo.BackupLastSuccess.DeleteLabelValues("gauge", "fb") {
		t.Error("series still exported without a successful backup")
	}
	ExportLastSuccess(withSuccess(&t1))
	if v := gaugeValue(t, "gauge"); v != float64(t1.Unix()) {
		t.Errorf("gauge = %v after forgetting, want %v", v, t1.Unix())
	}
	ForgetLastSuccess("gauge", "fb")
}

func TestReconcileScheduleExportsLastSuccessAndDefersDuringUpgrade(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	f.Spec.Backup.Enabled = true
	f.Spec.Backup.Schedule = "@every 1h"
	done := metav1.NewTime(time.Now().Add(-30 * time.Hour).Truncate(time.Second))
	f.Status.Backup = &forgejov1.BackupStatusSummary{LastScheduleTime: &metav1.Time{Time: done.Add(-time.Minute)}}

	last := reasonBackup("fb-scheduled-old", forgejov1.BackupReasonScheduled)
	last.CreationTimestamp = metav1.NewTime(done.Add(-time.Minute))
	last.Status.Phase = forgejov1.BackupPhaseCompleted
	last.Status.CompletionTime = &done
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, last).WithStatusSubresource(&forgejov1.ForgejoBackup{}).Build()

	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, Upgrading: true}
	if _, err := ReconcileSchedule(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if v := gaugeValue(t, "ns"); v != float64(done.Unix()) {
		t.Errorf("last-success gauge = %v, want %v", v, done.Unix())
	}
	list := &forgejov1.ForgejoBackupList{}
	if err := c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Errorf("a scheduled backup was started during an upgrade: %d backups", len(list.Items))
	}
	// The run became due 29h ago, when the last backup was 1h old: not stale.
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBackupHealthy); c == nil || c.Reason != testReasonBackupSucceeded {
		t.Errorf("BackupHealthy = %+v, want BackupSucceeded while deferred", c)
	}

	// After the upgrade the deferred run starts.
	rc = &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f}
	if _, err := ReconcileSchedule(ctx, rc); err != nil {
		t.Fatal(err)
	}
	if err := c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Errorf("the deferred scheduled backup did not start: %d backups", len(list.Items))
	}
}

func TestReconcileMaintenanceFlagsRestoreMigration(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	mk := func(name string, phase forgejov1.RestorePhase, db bool) *forgejov1.ForgejoRestore {
		r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"}}
		r.Spec.ForgejoRef.Name = "fb"
		r.Spec.RestoreDatabase = ptr.To(db)
		r.Status.Phase = phase
		return r
	}
	for _, tc := range []struct {
		restore *forgejov1.ForgejoRestore
		want    bool
	}{
		{mk("db", forgejov1.RestorePhaseScalingUp, true), true},
		{mk("data-only", forgejov1.RestorePhaseScalingUp, false), false},
		{mk("done", forgejov1.RestorePhaseCompleted, true), false},
	} {
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(tc.restore).Build()
		rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f.DeepCopy()}
		if _, err := ReconcileMaintenance(ctx, rc); err != nil {
			t.Fatal(err)
		}
		if rc.RestoreMigrating != tc.want {
			t.Errorf("%s: RestoreMigrating=%v, want %v", tc.restore.Name, rc.RestoreMigrating, tc.want)
		}
		// Every restore in ScalingUp, also a data-only one, is handed to the HA step, which
		// empties Valkey for it.
		scalingUp := tc.restore.Status.Phase == forgejov1.RestorePhaseScalingUp
		if got := rc.ScalingUpRestore; (got != nil) != scalingUp || (got != nil && got.Name != tc.restore.Name) || rc.RestoreScalingUp != scalingUp {
			t.Errorf("%s: ScalingUpRestore=%v RestoreScalingUp=%v, want ScalingUp %v", tc.restore.Name, got, rc.RestoreScalingUp, scalingUp)
		}
	}
}

// A running restore is planned maintenance; a failed one that changed the instance holds it
// until a person acts (rc.MaintenanceHeld: phase Degraded, unplanned downtime).
func TestReconcileMaintenanceFlagsHeldRestore(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	for _, tc := range []struct {
		phase             forgejov1.RestorePhase
		mod               Modification
		maintenance, held bool
	}{
		{forgejov1.RestorePhaseRestoring, Unmodified, true, false},
		{forgejov1.RestorePhaseFailed, Modified, true, true},
		{forgejov1.RestorePhaseFailed, MaybeModified, true, true},
		{forgejov1.RestorePhaseFailed, Unmodified, false, false},
	} {
		r := restore("r1", time.Now(), tc.phase, tc.mod)
		r.Namespace = "ns"
		r.Spec.ForgejoRef.Name = "fb"
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(&r).Build()
		rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f.DeepCopy()}
		if _, err := ReconcileMaintenance(ctx, rc); err != nil {
			t.Fatal(err)
		}
		if rc.Maintenance != tc.maintenance || rc.MaintenanceHeld != tc.held {
			t.Errorf("%s/%v: Maintenance=%v MaintenanceHeld=%v, want %v %v", tc.phase, tc.mod, rc.Maintenance, rc.MaintenanceHeld,
				tc.maintenance, tc.held)
		}
	}
}

func TestRestoreJobResetsAdminSyncAfterDatabaseOnlyRestore(t *testing.T) {
	s := testScheme(t)
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	b := reasonBackup("b", forgejov1.BackupReasonManual)
	b.Status.PVCName = testBackupsPVC
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "r", Namespace: "ns"}}
	in := JobInput{Scheme: s, Instance: f, InstanceName: "fb", ForgejoImage: forgejov1.DefaultImage, PostgresImage: forgejov1.DefaultPostgresImage,
		DB: &forgejo.DBConnection{Host: "db", Port: 5432, Name: "app", SSLMode: "verify-full", SecretName: "s", UsernameKey: "u", PasswordKey: "p"}}

	job, err := RestoreJob(in, rs, b, true, false)
	if err != nil {
		t.Fatal(err)
	}
	main := job.Spec.Template.Spec.Containers[0]
	if main.Name != ContainerResetAdminSync || len(main.VolumeMounts) != 1 || main.VolumeMounts[0].ReadOnly {
		t.Fatalf("database-only restore ends with %s %v, want %s with the data volume read-write", main.Name, main.VolumeMounts, ContainerResetAdminSync)
	}
	if env := main.Env[0]; env.Name != "ADMIN_SYNC_MARKER" || env.Value != "/data/gitea/"+AdminSyncMarker {
		t.Errorf("marker env %v", env)
	}
	// The LevelDB queues (without highAvailability) hold work for the replaced database.
	if env := main.Env[1]; env.Name != "QUEUES_DIR" || env.Value != "/data/gitea/queues" {
		t.Errorf("queues env %v", env)
	}
	if init := job.Spec.Template.Spec.InitContainers; len(init) != 2 || init[1].Name != ContainerRestoreDB {
		t.Errorf("init containers %v, want verify and %s", init, ContainerRestoreDB)
	}

	job, err = RestoreJob(in, rs, b, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if main := job.Spec.Template.Spec.Containers[0]; main.Name != ContainerRestoreData {
		t.Errorf("full restore ends with %s, want %s (the archive brings its own marker)", main.Name, ContainerRestoreData)
	}
}

// verify-ca/verify-full without a CA must not make libpq look for ~/.postgresql/root.crt.
func TestDatabaseScriptsChooseSSLRootCert(t *testing.T) {
	for name, script := range map[string]string{"dump": dumpScript, "restore-database": restoreDBScript} {
		if !strings.Contains(script, pgSSLRoot) {
			t.Errorf("%s script does not set PGSSLROOTCERT for verify-ca/verify-full", name)
		}
	}
	for _, want := range []string{"/etc/ssl/certs/ca-certificates.crt", "PGSSLROOTCERT=system", "verify-ca|verify-full"} {
		if !strings.Contains(pgSSLRoot, want) {
			t.Errorf("pgSSLRoot lacks %q", want)
		}
	}
}

func TestArchiveScriptOrdersRepositories(t *testing.T) {
	script := archiveScript(forgejo.LayoutForImage(forgejov1.DefaultImage))
	for _, want := range []string{
		"REPOS='./git/repositories'",
		"--exclude './git/repositories/*/*.git/objects/pack'",
		`echo "$r/objects/./pack"`,
		"-T /tmp/archive.list",
		"repositories were repacked while they were archived",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("archive script lacks %q", want)
		}
	}
	if refs, objects := strings.Index(script, "for e in refs packed-refs"), strings.Index(script, `echo "$r/objects"`); refs < 0 || refs > objects {
		t.Error("refs must be listed before objects")
	}
}

func TestResizeWarningIsRateLimited(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "ratelimit"}}
	f.Spec.Backup.Storage.Size = resource.MustParse("50Gi")
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: "ratelimit"},
		Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: ptr.To("fixed"),
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(pvc, &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testClassFixed}}).Build()
	warnings := 0
	warn := func(string, string) { warnings++ }
	for range 3 {
		if _, err := GrowBackupPVC(ctx, c, f, warn); err != nil {
			t.Fatal(err)
		}
	}
	if warnings != 1 {
		t.Errorf("%d warnings for the same unresizable claim, want 1", warnings)
	}
	f.Spec.Backup.Storage.Size = resource.MustParse("60Gi") // a new request is reported right away
	if _, err := GrowBackupPVC(ctx, c, f, warn); err != nil {
		t.Fatal(err)
	}
	if warnings != 2 {
		t.Errorf("a new size was not reported (%d warnings)", warnings)
	}
}

// The step that ends a database-only restore removes the admin sync marker and the LevelDB
// queues, and nothing else of the data volume.
func TestResetAdminSyncScriptRemovesQueues(t *testing.T) {
	dir := t.TempDir()
	marker, queues, repo := filepath.Join(dir, AdminSyncMarker), filepath.Join(dir, "queues"), filepath.Join(dir, "repo")
	for _, d := range []string{filepath.Join(queues, "common"), repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{marker, filepath.Join(queues, "common", "000001.log"), filepath.Join(repo, "HEAD")} {
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", resetAdminSyncScript)
	cmd.Env = append(os.Environ(), "ADMIN_SYNC_MARKER="+marker, "QUEUES_DIR="+queues)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	for _, gone := range []string{marker, queues} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists (%v)", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err != nil {
		t.Errorf("other data removed: %v", err)
	}
	// Without queues (an HA instance, or a fresh volume) the script still succeeds.
	again := exec.Command("sh", "-c", resetAdminSyncScript)
	again.Env = cmd.Env
	if out, err := again.CombinedOutput(); err != nil {
		t.Fatalf("second run failed: %v\n%s", err, out)
	}
}
