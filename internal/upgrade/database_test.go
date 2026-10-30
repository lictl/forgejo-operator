package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	pg14          = "ghcr.io/cloudnative-pg/postgresql:14-minimal-trixie"
	pg15          = "ghcr.io/cloudnative-pg/postgresql:15-minimal-trixie"
	pg16          = "ghcr.io/cloudnative-pg/postgresql:16-minimal-trixie"
	pg17          = "ghcr.io/cloudnative-pg/postgresql:17-minimal-trixie"
	pg18          = "ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie"
	pg18Patch     = "ghcr.io/cloudnative-pg/postgresql:18.6-minimal-trixie"
	pg18Bookworm  = "ghcr.io/cloudnative-pg/postgresql:18-minimal-bookworm"
	pg17Legacy    = "ghcr.io/cloudnative-pg/postgresql:17"
	sysOld        = "7000000000000000001"
	sysNew        = "7000000000000000002"
	testCluster   = "fj-db"
	testPrimary   = "fj-db-1"
	testBackupPVC = "fj-backups"
)

// dbInstance runs Forgejo imgA (no Forgejo upgrade) with spec.database.cnpg.imageName desired.
func dbInstance(desired string) *forgejov1.Forgejo {
	f := fixture(nil)
	f.Spec.Image = imgA
	f.Spec.Database.CNPG.ImageName = desired
	return f
}

// cnpgCluster is Cluster "fj-db" whose data runs dataImage (major) with spec.imageName and
// status.image image.
func cnpgCluster(image, dataImage string, major int, sys string, healthy bool, created time.Time) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	u.SetName(testCluster)
	u.SetNamespace("ns")
	u.SetUID("cluster-uid")
	u.SetCreationTimestamp(metav1.NewTime(created))
	phase, ready := forgejo.CNPGHealthyPhase, int64(1)
	if !healthy {
		phase, ready = forgejo.CNPGMajorUpgradePhase, 0
	}
	u.Object["spec"] = map[string]any{"instances": int64(1), "imageName": image, "storage": map[string]any{"size": size8Gi}}
	u.Object["status"] = map[string]any{"phase": phase, "readyInstances": ready, "image": image, "systemID": sys,
		"currentPrimary": testPrimary, "targetPrimary": testPrimary,
		"pgDataImageInfo": map[string]any{"image": dataImage, "majorVersion": int64(major)}}
	return u
}

// primaryPod is the pod of Cluster fj-db's primary.
func primaryPod(ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPrimary, Namespace: "ns", Labels: map[string]string{forgejo.CNPGClusterLabel: testCluster}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}}}}
}

// preUpgradeBackup is a usable pre-upgrade backup "pre" of the upgrade 17 → 18, and its volume.
func preUpgradeBackup() (*forgejov1.ForgejoBackup, *corev1.PersistentVolumeClaim) {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: testPreBackup, Namespace: "ns"},
		Spec: forgejov1.ForgejoBackupSpec{ForgejoRef: forgejov1.ForgejoRef{Name: "fj"}, Reason: forgejov1.BackupReasonPreUpgrade, IncludeData: ptr.To(false)},
		Status: forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhaseCompleted, PVCName: testBackupPVC, PostgresImage: pg17,
			DatabaseSizeBytes: 4096}}
	return b, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testBackupPVC, Namespace: "ns"}}
}

// running17 is a healthy cluster on PostgreSQL 17, created long ago.
func running17() *unstructured.Unstructured {
	return cnpgCluster(pg17, pg17, 17, sysOld, true, time.Now().Add(-48*time.Hour))
}

func dbScheme(t *testing.T) *runtime.Scheme {
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

func dbMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(forgejo.CNPGClusterGVK, meta.RESTScopeNamespace)
	return m
}

func dbClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(dbScheme(t)).WithRESTMapper(dbMapper()).WithObjects(objs...).Build()
}

// dbPass runs the database-upgrade step once, as the pipeline does (prepare sets what earlier
// steps decided), and returns its context and result.
func dbPass(t *testing.T, c client.Client, f *forgejov1.Forgejo, prepare ...func(*forgejo.ReconcileContext)) (*forgejo.ReconcileContext, forgejo.StepResult) {
	t.Helper()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image,
		RESTMapper: dbMapper(), Recorder: events.NewFakeRecorder(100)}
	for _, p := range prepare {
		p(rc)
	}
	res, err := ReconcileDatabase(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	return rc, res
}

// recorded drains the events of a pass.
func recorded(rc *forgejo.ReconcileContext) []string {
	rec := rc.Recorder.(*events.FakeRecorder)
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

func hasEvent(evs []string, reason string) bool {
	for _, e := range evs {
		if strings.Contains(e, " "+reason+" ") {
			return true
		}
	}
	return false
}

// getCluster reads Cluster fj-db (nil when it does not exist).
func getCluster(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: testCluster}, u); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil
		}
		t.Fatal(err)
	}
	return u
}

// setCluster replaces Cluster fj-db by u (as CNPG and the database step would have changed it).
func setCluster(t *testing.T, c client.Client, u *unstructured.Unstructured) {
	t.Helper()
	if old := getCluster(t, c); old != nil {
		if err := c.Delete(context.Background(), old); err != nil {
			t.Fatal(err)
		}
	}
	u.SetResourceVersion("")
	if err := c.Create(context.Background(), u); err != nil {
		t.Fatal(err)
	}
}

func condition(f *forgejov1.Forgejo, t string) *metav1.Condition {
	return meta.FindStatusCondition(f.Status.Conditions, t)
}

func zeroReplicas(rc *forgejo.ReconcileContext) bool {
	return rc.ReplicasOverride != nil && *rc.ReplicasOverride == 0
}

// The version gate: what passes straight to CNPG, what is refused and what is orchestrated.
func TestDatabaseVersionGate(t *testing.T) {
	for _, tt := range []struct {
		name, running, desired string
		major                  int
		// write is the image the database step must write; reason the DatabaseUpgrading reason
		// and msg a part of its message; degraded the Degraded reason ("" for none); upgrade
		// whether an upgrade starts.
		write, reason, msg, degraded string
		upgrade                      bool
	}{
		{name: "same image", running: pg17, desired: pg17, major: 17, write: pg17, reason: reasonDatabaseUpToDate},
		{name: "minor pin within the major", running: pg18, desired: pg18Patch, major: 18, write: pg18Patch, reason: reasonDatabaseRollout},
		{name: "another Debian release within the major", running: pg18, desired: pg18Bookworm, major: 18, write: pg18Bookworm, reason: reasonDatabaseRollout},
		{name: "downgrade", running: pg18, desired: pg17, major: 18, write: pg18, reason: ReasonPostgresDowngradeRefused, degraded: ReasonPostgresDowngradeRefused},
		{name: "another Debian release across majors", running: pg17, desired: pg18Bookworm, major: 17, write: pg17,
			reason: ReasonPostgresUpgradePathUnsupported, degraded: ReasonPostgresUpgradePathUnsupported},
		{name: "legacy bullseye tag to trixie", running: pg17Legacy, desired: pg18, major: 17, write: pg17Legacy,
			reason: ReasonPostgresUpgradePathUnsupported, degraded: ReasonPostgresUpgradePathUnsupported},
		{name: "newer than CNPG supports", running: pg18, desired: "ghcr.io/cloudnative-pg/postgresql:19rc1-minimal-trixie", major: 18, write: pg18,
			reason: ReasonPostgresVersionUnsupported, degraded: ReasonPostgresVersionUnsupported},
		{name: "older than Forgejo supports", running: "ghcr.io/cloudnative-pg/postgresql:13-minimal-trixie",
			desired: "ghcr.io/cloudnative-pg/postgresql:13.20-minimal-trixie", major: 13,
			write: "ghcr.io/cloudnative-pg/postgresql:13-minimal-trixie", reason: ReasonPostgresVersionUnsupported, degraded: ReasonPostgresVersionUnsupported},
		{name: "upgrade", running: pg17, desired: pg18, major: 17, write: pg17, reason: string(forgejov1.DatabaseUpgradePending), upgrade: true},
		{name: "upgrade to an unverified major", running: pg15, desired: pg16, major: 15, write: pg15,
			reason: string(forgejov1.DatabaseUpgradePending), upgrade: true},
		// CloudNativePG's webhook refuses a newer major below 16: refused before Forgejo stops.
		{name: "upgrade to a major below 16", running: pg14, desired: pg15, major: 14, write: pg14,
			reason: ReasonPostgresUpgradePathUnsupported, msg: "only to PostgreSQL 16 or newer", degraded: ReasonPostgresUpgradePathUnsupported},
		{name: "upgrade from 14 to 16", running: pg14, desired: pg16, major: 14, write: pg14,
			reason: string(forgejov1.DatabaseUpgradePending), upgrade: true},
		{name: "unknown Debian release", running: pg17, desired: "registry.example.com/pg:18-custom", major: 17, write: pg17,
			reason: string(forgejov1.DatabaseUpgradePending), upgrade: true},
		{name: "no version in the tag", running: pg17, desired: "registry.example.com/pg@sha256:0123", major: 17,
			write: "registry.example.com/pg@sha256:0123", reason: reasonDatabaseRollout},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := dbInstance(tt.desired)
			c := dbClient(t, cnpgCluster(tt.running, tt.running, tt.major, sysOld, true, time.Now().Add(-time.Hour)))
			rc, res := dbPass(t, c, f)
			if res.Stop || rc.DatabaseImage != tt.write || rc.ReplicasOverride != nil {
				t.Fatalf("stop %v, writes %s (want %s), override %v", res.Stop, rc.DatabaseImage, tt.write, rc.ReplicasOverride)
			}
			if cond := condition(f, forgejov1.ConditionDatabaseUpgrading); cond == nil || cond.Reason != tt.reason || !strings.Contains(cond.Message, tt.msg) {
				t.Fatalf("DatabaseUpgrading %+v, want reason %s and %q", cond, tt.reason, tt.msg)
			}
			d := condition(f, forgejov1.ConditionDegraded)
			if (tt.degraded == "") != (d == nil || d.Status != metav1.ConditionTrue) || (d != nil && tt.degraded != "" && d.Reason != tt.degraded) {
				t.Fatalf("Degraded %+v, want %q", d, tt.degraded)
			}
			u := forgejo.DatabaseUpgrade(f)
			if tt.upgrade != (u != nil) || rc.Upgrading != tt.upgrade || rc.DatabaseUpgrading != tt.upgrade {
				t.Fatalf("upgrade %+v, upgrading %v/%v", u, rc.Upgrading, rc.DatabaseUpgrading)
			}
			evs := recorded(rc)
			if tt.degraded != "" && !hasEvent(evs, tt.degraded) {
				t.Errorf("no %s event: %v", tt.degraded, evs)
			}
			// A refusal is reported once.
			if tt.degraded != "" {
				rc, _ = dbPass(t, c, f)
				if evs := recorded(rc); hasEvent(evs, tt.degraded) {
					t.Errorf("repeated %s event: %v", tt.degraded, evs)
				}
			}
		})
	}
}

// A new instance creates its cluster with the requested image, but not with a major the
// operator does not run.
func TestDatabaseNewCluster(t *testing.T) {
	f := dbInstance(pg18)
	rc, res := dbPass(t, dbClient(t), f)
	if res.Stop || rc.DatabaseImage != pg18 || rc.Upgrading {
		t.Fatalf("stop %v, image %s", res.Stop, rc.DatabaseImage)
	}
	f = dbInstance("ghcr.io/cloudnative-pg/postgresql:19-minimal-trixie")
	rc, res = dbPass(t, dbClient(t), f)
	db := condition(f, forgejov1.ConditionDatabaseReady)
	if !res.Stop || db == nil || db.Reason != ReasonPostgresVersionUnsupported || !strings.Contains(db.Message, "newer than 18") {
		t.Fatalf("stop %v, DatabaseReady %+v", res.Stop, db)
	}
	if !hasEvent(recorded(rc), ReasonPostgresVersionUnsupported) {
		t.Error("no PostgresVersionUnsupported event")
	}
	f = dbInstance(pg16)
	f.UID = "new-cluster-uid" // the warning is emitted once per instance and image
	if rc, _ = dbPass(t, dbClient(t), f); !hasEvent(recorded(rc), reasonUnsupportedVersion) {
		t.Error("an unverified major is not warned about")
	}
}

// An external database is never orchestrated.
func TestDatabaseUpgradeIgnoresExternal(t *testing.T) {
	f := dbInstance(pg18)
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	rc, res := dbPass(t, dbClient(t, running17()), f)
	if res.Stop || rc.Upgrading || forgejo.DatabaseUpgrade(f) != nil || condition(f, forgejov1.ConditionDatabaseUpgrading) != nil {
		t.Fatalf("external database orchestrated: %+v", f.Status.Database)
	}
}

// One upgrade at a time, and none while a restore or the verification's prerequisites are missing.
func TestDatabaseUpgradeWaits(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reason  string
		cluster *unstructured.Unstructured
		prepare func(*forgejov1.Forgejo, *forgejo.ReconcileContext)
		objs    []client.Object
	}{
		{"restore", reasonWaitingForRestore, running17(), func(_ *forgejov1.Forgejo, rc *forgejo.ReconcileContext) {
			rc.Maintenance, rc.MaintenanceReason = true, "ForgejoRestore r is restoring backup b"
		}, nil},
		// A restore asked for earlier goes first, also before it has started: once the upgrade
		// is in flight it would wait for the upgrade.
		{"new restore", reasonWaitingForRestore, running17(), nil, []client.Object{restore("r", "", ago(time.Minute))}},
		{"pending restore", reasonWaitingForRestore, running17(), nil, []client.Object{restore("r", forgejov1.RestorePhasePending, ago(time.Minute))}},
		// Its first pod migrates the restored database, which the backup phase would stop.
		{"restore scaling up", reasonWaitingForRestore, running17(), func(_ *forgejov1.Forgejo, rc *forgejo.ReconcileContext) {
			rc.RestoreScalingUp, rc.RestoreMigrating = true, true
		}, []client.Object{restore("r", forgejov1.RestorePhaseScalingUp, ago(time.Minute))}},
		{"spec.restoreFrom", reasonWaitingForRestore, running17(), func(f *forgejov1.Forgejo, _ *forgejo.ReconcileContext) {
			f.Spec.RestoreFrom = &forgejov1.RestoreFromSpec{}
		}, nil},
		{"Forgejo upgrade", reasonWaitingForForgejoUpgrade, running17(), func(f *forgejov1.Forgejo, _ *forgejo.ReconcileContext) {
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseRollingOut, FromImage: imgA, ToImage: imgB}
		}, nil},
		{"Forgejo rollback restore failed", reasonWaitingForForgejoUpgrade, running17(), func(f *forgejov1.Forgejo, _ *forgejo.ReconcileContext) {
			f.Status.Upgrade = &forgejov1.UpgradeStatus{Phase: forgejov1.UpgradePhaseFailed, RollbackRestoreFailed: true, ToImage: imgB}
		}, nil},
		{"first rollout", "WaitingForFirstRollout", running17(), func(f *forgejov1.Forgejo, _ *forgejo.ReconcileContext) {
			f.Status.Image = ""
		}, nil},
		{"unhealthy cluster", "WaitingForDatabase", cnpgCluster(pg17, pg17, 17, sysOld, false, time.Now().Add(-time.Hour)), nil, nil},
		{"SSH host keys", reasonWaitingForSSHHostKeys, running17(), func(f *forgejov1.Forgejo, _ *forgejo.ReconcileContext) {
			f.Status.SSH = &forgejov1.SSHStatus{Message: "Secret fj-ssh-host-keys is missing"}
		}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := dbInstance(pg18)
			rc, _ := dbPass(t, dbClient(t, append([]client.Object{tt.cluster}, tt.objs...)...), f, func(rc *forgejo.ReconcileContext) {
				if tt.prepare != nil {
					tt.prepare(f, rc)
				}
			})
			cond := condition(f, forgejov1.ConditionDatabaseUpgrading)
			if forgejo.DatabaseUpgrade(f) != nil || rc.DatabaseImage != pg17 || rc.DatabaseUpgrading || rc.Upgrading ||
				rc.ReplicasOverride != nil || cond == nil || cond.Reason != tt.reason {
				t.Fatalf("upgrade %+v, image %s, condition %+v", forgejo.DatabaseUpgrade(f), rc.DatabaseImage, cond)
			}
		})
	}
	// A finished restore holds nothing.
	f := dbInstance(pg18)
	done := restore("r", forgejov1.RestorePhaseCompleted, ago(time.Hour))
	if dbPass(t, dbClient(t, running17(), done), f); forgejo.DatabaseUpgrade(f) == nil {
		t.Fatalf("no upgrade after a completed restore: %+v", condition(f, forgejov1.ConditionDatabaseUpgrading))
	}
}

// pendingUpgrade starts the upgrade 17 → 18 and returns the instance and the client.
func pendingUpgrade(t *testing.T, objs ...client.Object) (*forgejov1.Forgejo, client.Client) {
	t.Helper()
	f := dbInstance(pg18)
	c := dbClient(t, append([]client.Object{running17()}, objs...)...)
	rc, _ := dbPass(t, c, f)
	u := forgejo.DatabaseUpgrade(f)
	if u == nil || u.Phase != forgejov1.DatabaseUpgradePending || u.FromImage != pg17 || u.ToImage != pg18 ||
		u.FromMajor != 17 || u.ToMajor != 18 || u.FromSystemID != sysOld || !strings.HasPrefix(u.BackupName, "fj-pre-pg18-") {
		t.Fatalf("upgrade %+v", u)
	}
	if rc.DatabaseImage != pg17 || zeroReplicas(rc) || !hasEvent(recorded(rc), "DatabaseUpgradeStarted") {
		t.Fatalf("Pending writes %s, override %v", rc.DatabaseImage, rc.ReplicasOverride)
	}
	u.StartTime = ago(0) // as persisted (second precision)
	return f, c
}

func serverPodRunning(f *forgejov1.Forgejo, name string) *corev1.Pod {
	p := serverPod(f, name, imgA, ago(time.Hour), corev1.ContainerState{}, 0)
	p.Status.Phase = corev1.PodRunning
	return p
}

func backupObject(t *testing.T, c client.Client, name string) *forgejov1.ForgejoBackup {
	t.Helper()
	b := &forgejov1.ForgejoBackup{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: name}, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func setBackupPhase(t *testing.T, c client.Client, name string, phase forgejov1.BackupPhase, msg string) {
	t.Helper()
	b := backupObject(t, c, name)
	b.Status.Phase, b.Status.Message = phase, msg
	if err := c.Update(context.Background(), b); err != nil {
		t.Fatal(err)
	}
}

// expectPass checks the outcome of a pass: the phase, the image the database step writes and
// whether Forgejo is held at zero replicas.
func expectPass(t *testing.T, rc *forgejo.ReconcileContext, u *forgejov1.DatabaseUpgradeStatus, phase forgejov1.DatabaseUpgradePhase,
	image string, stopped bool) {
	t.Helper()
	if u.Phase != phase || rc.DatabaseImage != image || zeroReplicas(rc) != stopped {
		t.Fatalf("phase %s (want %s), image %s (want %s), stopped %v (want %v): %s",
			u.Phase, phase, rc.DatabaseImage, image, zeroReplicas(rc), stopped, u.Message)
	}
}

// The upgrade from start to Succeeded: Forgejo stopped, a cold database-only backup with the
// old image, the new image written, Forgejo verified on the new major.
func TestDatabaseUpgradeSucceeds(t *testing.T) {
	ctx := context.Background()
	f0 := dbInstance(pg18)
	server := serverPodRunning(f0, "fj-1")
	registration := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "r-register-x", Namespace: "ns", Labels: map[string]string{
		forgejo.LabelName: forgejo.AppName, forgejo.LabelComponent: forgejo.ComponentRunnerRegistration, forgejo.LabelForgejo: "fj",
		forgejo.LabelInstance: "r"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	f, c := pendingUpgrade(t, server, registration)
	u := forgejo.DatabaseUpgrade(f)

	// Pending → BackingUp: Forgejo is stopped.
	rc, _ := dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeBackingUp, pg17, true)
	if !rc.Upgrading || !rc.DatabaseUpgrading {
		t.Fatal("not upgrading")
	}
	// Everything that uses the database stops before the backup.
	for _, pod := range []client.Object{server, registration} {
		dbPass(t, c, f)
		if backups(t, c) != 0 || !strings.Contains(u.Message, "Waiting for") {
			t.Fatalf("backup taken while %s runs: %s", pod.GetName(), u.Message)
		}
		if err := c.Delete(ctx, pod); err != nil {
			t.Fatal(err)
		}
	}
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeBackingUp, pg17, true)
	if b := backupObject(t, c, u.BackupName); b.Spec.Reason != forgejov1.BackupReasonPreUpgrade || ptr.Deref(b.Spec.IncludeData, true) {
		t.Fatalf("backup %+v", b.Spec)
	}
	// The backup completes: the new image is written.
	setBackupPhase(t, c, u.BackupName, forgejov1.BackupPhaseCompleted, "")
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeUpgrading, pg18, true)
	if u.RolloutStartTime == nil {
		t.Fatal("no rolloutStartTime")
	}
	// CloudNativePG upgrades: Job running, the data still on 17.
	setCluster(t, c, cnpgCluster(pg18, pg17, 17, "", false, time.Now().Add(-48*time.Hour)))
	if err := c.Create(ctx, upgradeJob()); err != nil {
		t.Fatal(err)
	}
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeUpgrading, pg18, true)
	if !strings.Contains(u.Message, "running") {
		t.Fatalf("message %s", u.Message)
	}
	// pg_upgrade succeeded: Forgejo starts (one replica through rc.Upgrading, no override).
	setCluster(t, c, cnpgCluster(pg18, pg18, 18, sysNew, true, time.Now().Add(-48*time.Hour)))
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeVerifying, pg18, false)
	if rc.ReplicasOverride != nil || !rc.Upgrading {
		t.Fatalf("override %v, upgrading %v", rc.ReplicasOverride, rc.Upgrading)
	}
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeVerifying, pg18, false)

	before := counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", resultSucceeded))
	if err := c.Create(ctx, deployment(imgA, true)); err != nil {
		t.Fatal(err)
	}
	rc, _ = dbPass(t, c, f)
	expectPass(t, rc, u, forgejov1.DatabaseUpgradeSucceeded, pg18, false)
	if rc.Upgrading || u.FailedImage != "" || counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", resultSucceeded)) != before+1 {
		t.Fatalf("upgrading %v, failed image %q", rc.Upgrading, u.FailedImage)
	}
	// Idle afterwards.
	rc, _ = dbPass(t, c, f)
	if cond := condition(f, forgejov1.ConditionDatabaseUpgrading); cond.Reason != reasonPostgresUpgradeSucceeded || rc.DatabaseImage != pg18 || rc.Upgrading {
		t.Fatalf("condition %+v", cond)
	}
}

const (
	resultSucceeded  = "succeeded"
	resultRolledBack = "rolledback"
	testUpgradeJob   = "fj-db-1-major-upgrade"
	size8Gi          = "8Gi"
)

func upgradeJob() *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: testUpgradeJob, Namespace: "ns", CreationTimestamp: *ago(time.Minute), Labels: map[string]string{
		forgejo.CNPGClusterLabel: testCluster, forgejo.CNPGJobRoleLabel: forgejo.CNPGMajorUpgradeJobRole}}}
}

func upgradePod(job string, created *metav1.Time, main corev1.ContainerState) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: job + "-abc", Namespace: "ns", CreationTimestamp: *created,
			Labels: map[string]string{batchv1.JobNameLabel: job}},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "prepare", State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: forgejo.CNPGMajorUpgradeContainer, State: main}},
		},
	}
}

// upgrading is an upgrade 17 → 18 in phase Upgrading whose image was written rolledOutAgo.
func upgrading(rolledOutAgo time.Duration) *forgejov1.Forgejo {
	f := dbInstance(pg18)
	f.Status.Database = &forgejov1.DatabaseStatus{Image: pg17, MajorVersion: 17, Upgrade: &forgejov1.DatabaseUpgradeStatus{
		Phase: forgejov1.DatabaseUpgradeUpgrading, FromImage: pg17, ToImage: pg18, FromMajor: 17, ToMajor: 18, FromSystemID: sysOld,
		BackupName: testPreBackup, StartTime: ago(rolledOutAgo + 5*time.Minute), RolloutStartTime: ago(rolledOutAgo),
		PhaseStartTime: ago(rolledOutAgo),
	}}
	return f
}

// A failed pg_upgrade writes the old image back; the old data comes back with its system ID:
// rolled back without a restore. The failed image is held until a retry.
func TestDatabaseUpgradeJobFailureReverts(t *testing.T) {
	ctx := context.Background()
	f := upgrading(time.Minute)
	job := upgradeJob()
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now().Add(-48*time.Hour)), job)
	u := forgejo.DatabaseUpgrade(f)

	rc, _ := dbPass(t, c, f)
	if u.Phase != forgejov1.DatabaseUpgradeReverting || rc.DatabaseImage != pg17 || !zeroReplicas(rc) ||
		!strings.Contains(u.FailureReason, "BackoffLimitExceeded") || !hasEvent(recorded(rc), "DatabaseUpgradeFailed") {
		t.Fatalf("phase %s, image %s: %s", u.Phase, rc.DatabaseImage, u.FailureReason)
	}
	// CNPG deleted the Job and starts the old data.
	if err := c.Delete(ctx, job); err != nil {
		t.Fatal(err)
	}
	setCluster(t, c, cnpgCluster(pg17, pg17, 17, sysOld, false, time.Now().Add(-48*time.Hour)))
	if rc, _ = dbPass(t, c, f); u.Phase != forgejov1.DatabaseUpgradeReverting || !zeroReplicas(rc) {
		t.Fatalf("phase %s before the old data runs", u.Phase)
	}
	if !u.PGUpgradeStarted {
		t.Fatal("a failed Job whose pods are gone does not count as a pg_upgrade that may have run")
	}
	before := counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", resultRolledBack))
	setCluster(t, c, running17())
	if err := c.Create(ctx, primaryPod(true)); err != nil {
		t.Fatal(err)
	}
	rc, _ = dbPass(t, c, f)
	d := condition(f, forgejov1.ConditionDegraded)
	if u.Phase != forgejov1.DatabaseUpgradeRolledBack || u.FailedImage != pg18 || rc.DatabaseImage != pg17 ||
		rc.Upgrading || rc.ReplicasOverride != nil || d == nil || d.Reason != ReasonPostgresUpgradeRolledBack ||
		counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", resultRolledBack)) != before+1 {
		t.Fatalf("phase %s, image %s, degraded %+v: %s", u.Phase, rc.DatabaseImage, d, u.Message)
	}
	// Held: the failed image is not tried again.
	if rc, _ = dbPass(t, c, f); rc.DatabaseImage != pg17 || u.Phase != forgejov1.DatabaseUpgradeRolledBack {
		t.Fatalf("held pass writes %s", rc.DatabaseImage)
	}
	// The retry annotation starts a new upgrade (with a new backup).
	f.Annotations = map[string]string{AnnotationRetryDatabase: valueTrue}
	rc, _ = dbPass(t, c, f)
	if u = forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradePending || u.FailedImage != "" || !hasEvent(recorded(rc), "DatabaseUpgradeRetry") {
		t.Fatalf("retry: %+v", u)
	}
}

// An upgrade pod that cannot pull its image never ran pg_upgrade: reverted after the pull
// grace. A running pg_upgrade is never interrupted, however long it takes.
func TestDatabaseUpgradePodChecks(t *testing.T) {
	waitingPull := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingImagePullBackOff, Message: "not found"}}
	creating := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
	t.Run("pull grace", func(t *testing.T) {
		f := upgrading(time.Minute)
		job := upgradeJob()
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, upgradePod(job.Name, ago(time.Minute), waitingPull))
		if dbPass(t, c, f); forgejo.DatabaseUpgrade(f).Phase != forgejov1.DatabaseUpgradeUpgrading {
			t.Fatalf("reverted within the pull grace: %s", forgejo.DatabaseUpgrade(f).Phase)
		}
	})
	t.Run("image not pulled", func(t *testing.T) {
		f := upgrading(5 * time.Minute)
		job := upgradeJob()
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, upgradePod(job.Name, ago(3*time.Minute), waitingPull))
		rc, _ := dbPass(t, c, f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeReverting || rc.DatabaseImage != pg17 ||
			!strings.Contains(u.FailureReason, "could not be pulled") || u.PGUpgradeStarted {
			t.Fatalf("phase %s, pg_upgrade started %v: %s", u.Phase, u.PGUpgradeStarted, u.FailureReason)
		}
	})
	t.Run("a retried pod that cannot pull after pg_upgrade ran", func(t *testing.T) {
		f := upgrading(5 * time.Minute)
		job := upgradeJob()
		ran := upgradePod(job.Name, ago(4*time.Minute), corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}})
		ran.Name = job.Name + "-first"
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, ran, upgradePod(job.Name, ago(3*time.Minute), waitingPull))
		dbPass(t, c, f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeReverting || !u.PGUpgradeStarted {
			t.Fatalf("phase %s, pg_upgrade started %v", u.Phase, u.PGUpgradeStarted)
		}
	})
	t.Run("long pg_upgrade", func(t *testing.T) {
		f := upgrading(time.Hour)
		job := upgradeJob()
		pod := upgradePod(job.Name, ago(59*time.Minute), corev1.ContainerState{Running: &corev1.ContainerStateRunning{}})
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, pod)
		rc, _ := dbPass(t, c, f)
		u := forgejo.DatabaseUpgrade(f)
		if u.Phase != forgejov1.DatabaseUpgradeUpgrading || rc.DatabaseImage != pg18 || !strings.Contains(u.Message, "never interrupted") ||
			!hasEvent(recorded(rc), "DatabaseUpgradeSlow") {
			t.Fatalf("phase %s: %s", u.Phase, u.Message)
		}
		if rc, _ = dbPass(t, c, f); hasEvent(recorded(rc), "DatabaseUpgradeSlow") {
			t.Error("DatabaseUpgradeSlow repeated")
		}
	})
	t.Run("a short spec.upgrade.timeout does not cut the start short", func(t *testing.T) {
		f := upgrading(3 * time.Minute)
		f.Spec.Upgrade.Timeout = &metav1.Duration{Duration: 5 * time.Second}
		job := upgradeJob()
		pending := upgradePod(job.Name, ago(3*time.Minute), creating)
		pending.Status.Phase = corev1.PodPending
		dbPass(t, dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, pending), f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeUpgrading {
			t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
		}
	})
	t.Run("pg_upgrade does not start within the timeout", func(t *testing.T) {
		f := upgrading(20 * time.Minute)
		job := upgradeJob()
		pending := upgradePod(job.Name, ago(19*time.Minute), creating)
		pending.Status.Phase = corev1.PodPending
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()), job, pending)
		rc, _ := dbPass(t, c, f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeReverting || rc.DatabaseImage != pg17 || u.PGUpgradeStarted ||
			!strings.Contains(u.FailureReason, "did not start within 15m0s") {
			t.Fatalf("phase %s, image %s: %s", u.Phase, rc.DatabaseImage, u.FailureReason)
		}
		// Without any Job either.
		f = upgrading(20 * time.Minute)
		dbPass(t, dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now())), f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeReverting || !strings.Contains(u.FailureReason, "no upgrade Job") {
			t.Fatalf("no Job: phase %s: %s", u.Phase, u.FailureReason)
		}
	})
	t.Run("image never written", func(t *testing.T) {
		f := upgrading(3 * time.Minute)
		c := dbClient(t, running17())
		rc, _ := dbPass(t, c, f)
		if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeFailed || rc.DatabaseImage != pg17 || rc.ReplicasOverride != nil ||
			!strings.Contains(u.Message, "aborted before the new image was written") {
			t.Fatalf("phase %s, image %s: %s", u.Phase, rc.DatabaseImage, u.Message)
		}
	})
	t.Run("spec change is queued", func(t *testing.T) {
		f := upgrading(time.Minute)
		f.Spec.Database.CNPG.ImageName = pg17
		c := dbClient(t, cnpgCluster(pg18, pg17, 17, "", false, time.Now()))
		rc, _ := dbPass(t, c, f)
		cond := condition(f, forgejov1.ConditionDatabaseUpgrading)
		if forgejo.DatabaseUpgrade(f).Phase != forgejov1.DatabaseUpgradeUpgrading || rc.DatabaseImage != pg18 ||
			!strings.Contains(cond.Message, "handled once this upgrade has finished") || !hasEvent(recorded(rc), "DatabaseUpgradeQueued") {
			t.Fatalf("phase %s, image %s: %s", forgejo.DatabaseUpgrade(f).Phase, rc.DatabaseImage, cond.Message)
		}
	})
}

// Changing the image back before the new one was written cancels the upgrade; a running
// pre-upgrade backup finishes first.
func TestDatabaseUpgradeCancel(t *testing.T) {
	f, c := pendingUpgrade(t)
	f.Spec.Database.CNPG.ImageName = pg17
	rc, _ := dbPass(t, c, f)
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeCancelled || rc.DatabaseImage != pg17 || rc.Upgrading {
		t.Fatalf("phase %s, image %s", u.Phase, rc.DatabaseImage)
	}

	f, c = pendingUpgrade(t)
	dbPass(t, c, f) // BackingUp
	dbPass(t, c, f) // creates the backup
	u := forgejo.DatabaseUpgrade(f)
	setBackupPhase(t, c, u.BackupName, forgejov1.BackupPhaseRunning, "")
	f.Spec.Database.CNPG.ImageName = pg17
	if rc, _ = dbPass(t, c, f); u.Phase != forgejov1.DatabaseUpgradeBackingUp || !zeroReplicas(rc) || rc.DatabaseImage != pg17 {
		t.Fatalf("cancelled during the backup: %s", u.Phase)
	}
	setBackupPhase(t, c, u.BackupName, forgejov1.BackupPhaseCompleted, "")
	if rc, _ = dbPass(t, c, f); u.Phase != forgejov1.DatabaseUpgradeCancelled || rc.DatabaseImage != pg17 || u.FailedImage != "" {
		t.Fatalf("phase %s", u.Phase)
	}
	if dbPass(t, c, f); condition(f, forgejov1.ConditionDatabaseUpgrading).Reason != reasonPostgresUpgradeCancelled {
		t.Fatalf("condition %+v", condition(f, forgejov1.ConditionDatabaseUpgrading))
	}

	// A backup that has not started (it may itself wait, e.g. for a restore) is not waited for.
	f, c = pendingUpgrade(t)
	dbPass(t, c, f) // BackingUp
	dbPass(t, c, f) // creates the backup
	u = forgejo.DatabaseUpgrade(f)
	setBackupPhase(t, c, u.BackupName, forgejov1.BackupPhasePending, "Waiting for ForgejoRestore r1 to finish")
	f.Spec.Database.CNPG.ImageName = pg17
	if rc, _ = dbPass(t, c, f); u.Phase != forgejov1.DatabaseUpgradeCancelled || rc.ReplicasOverride != nil || rc.Upgrading {
		t.Fatalf("phase %s, override %v: %s", u.Phase, rc.ReplicasOverride, u.Message)
	}
}

// A failed pre-upgrade backup, and pods that do not stop, abort before anything changed.
func TestDatabaseUpgradeAbortsBeforeTheImageIsWritten(t *testing.T) {
	f, c := pendingUpgrade(t)
	dbPass(t, c, f)
	dbPass(t, c, f)
	u := forgejo.DatabaseUpgrade(f)
	setBackupPhase(t, c, u.BackupName, forgejov1.BackupPhaseFailed, "pg_dump failed")
	rc, _ := dbPass(t, c, f)
	if u.Phase != forgejov1.DatabaseUpgradeFailed || u.FailedImage != pg18 || rc.DatabaseImage != pg17 || rc.ReplicasOverride != nil ||
		!strings.Contains(u.Message, "pg_dump failed") {
		t.Fatalf("phase %s, image %s: %s", u.Phase, rc.DatabaseImage, u.Message)
	}

	f0 := dbInstance(pg18)
	f, c = pendingUpgrade(t, serverPodRunning(f0, "stuck"))
	dbPass(t, c, f)
	u = forgejo.DatabaseUpgrade(f)
	u.StartTime = ago(16 * time.Minute)
	if dbPass(t, c, f); u.Phase != forgejov1.DatabaseUpgradeFailed || backups(t, c) != 0 || !strings.Contains(u.FailureReason, "did not stop") {
		t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
	}
}

// A restore that is not the upgrade's own pauses the backup phase; nothing is created.
func TestDatabaseUpgradePausedByMaintenance(t *testing.T) {
	f, c := pendingUpgrade(t)
	dbPass(t, c, f) // BackingUp
	inMaintenance := func(rc *forgejo.ReconcileContext) {
		rc.Maintenance, rc.MaintenanceReason = true, "ForgejoRestore manual is restoring backup b"
	}
	rc, _ := dbPass(t, c, f, inMaintenance)
	u := forgejo.DatabaseUpgrade(f)
	if backups(t, c) != 0 || u.Phase != forgejov1.DatabaseUpgradeBackingUp || !strings.HasPrefix(u.Message, "Paused") ||
		!hasEvent(recorded(rc), "DatabaseUpgradePaused") {
		t.Fatalf("backups %d, phase %s: %s", backups(t, c), u.Phase, u.Message)
	}
	if rc, _ = dbPass(t, c, f); backups(t, c) != 1 || !hasEvent(recorded(rc), "DatabaseUpgradeResumed") {
		t.Fatalf("after maintenance: backups %d", backups(t, c))
	}
}

// verifying is an upgrade 17 → 18 in phase Verifying since verifyingFor.
func verifying(verifyingFor time.Duration) *forgejov1.Forgejo {
	f := upgrading(verifyingFor + time.Minute)
	u := forgejo.DatabaseUpgrade(f)
	u.Phase, u.PhaseStartTime = forgejov1.DatabaseUpgradeVerifying, ago(verifyingFor)
	f.Status.Database.Image, f.Status.Database.MajorVersion = pg18, 18
	return f
}

// brokenReason is the reason of condition Broken, or "".
func brokenReason(f *forgejov1.Forgejo) string {
	if c := condition(f, forgejov1.ConditionBroken); c != nil && c.Status == metav1.ConditionTrue {
		return c.Reason
	}
	return ""
}

// Forgejo that does not become available on the new major: nothing is rolled back or deleted.
// Forgejo stays stopped and the instance is Broken; another image does not release it, the
// retry annotation does while the cluster serves the new major (Forgejo starts on it again).
func TestDatabaseVerifyFailureHoldsForgejo(t *testing.T) {
	ctx := context.Background()
	f := verifying(20 * time.Minute)
	pre, prePVC := preUpgradeBackup()
	c := dbClient(t, cnpgCluster(pg18, pg18, 18, sysNew, true, time.Now().Add(-48*time.Hour)), serverPodRunning(f, "fj-1"), pre, prePVC,
		f.DeepCopy())
	u := forgejo.DatabaseUpgrade(f)
	before := counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", "failed"))
	rc, res := dbPass(t, c, f)
	if u.Phase != forgejov1.DatabaseUpgradeFailed || !u.HoldsForgejo || !zeroReplicas(rc) || res.Stop || rc.DatabaseImage != pg18 ||
		u.FailedImage != pg18 || getCluster(t, c) == nil || restores(t, c) != 0 || brokenReason(f) != forgejo.BrokenDatabaseUpgradeFailed ||
		!strings.Contains(u.FailureReason, "did not become available") || !strings.Contains(u.Message, testPreBackup) ||
		!hasEvent(recorded(rc), "DatabaseUpgradeFailed") ||
		counterValue(t, forgejo.DatabaseUpgradeTotal.WithLabelValues("ns", "fj", "failed")) != before+1 {
		t.Fatalf("phase %s, holds %v, override %v, image %s, Broken %q: %s", u.Phase, u.HoldsForgejo, rc.ReplicasOverride,
			rc.DatabaseImage, brokenReason(f), u.Message)
	}
	u.CompletionTime = ago(0)

	// Another image does not lift the hold.
	f.Spec.Database.CNPG.ImageName = pg17
	if rc, _ = dbPass(t, c, f); !zeroReplicas(rc) || !u.HoldsForgejo || brokenReason(f) != forgejo.BrokenDatabaseUpgradeFailed {
		t.Fatalf("hold lifted by a spec change: override %v, Broken %q", rc.ReplicasOverride, brokenReason(f))
	}
	// The retry annotation accepts the new major the cluster serves.
	f.Spec.Database.CNPG.ImageName = pg18
	f.Annotations = map[string]string{AnnotationRetryDatabase: valueTrue}
	rc, _ = dbPass(t, c, f)
	if u.HoldsForgejo || u.FailedImage != "" || rc.ReplicasOverride != nil || rc.Upgrading || brokenReason(f) != "" ||
		rc.DatabaseImage != pg18 || backups(t, c) != 1 || !hasEvent(recorded(rc), "DatabaseUpgradeRetry") {
		t.Fatalf("retry: holds %v, failed image %q, override %v, Broken %q, backups %d", u.HoldsForgejo, u.FailedImage,
			rc.ReplicasOverride, brokenReason(f), backups(t, c))
	}
	// The annotation is removed in the next pass, which finds nothing to retry.
	live := &forgejov1.Forgejo{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "fj"}, live); err != nil {
		t.Fatal(err)
	}
	live.Annotations = map[string]string{AnnotationRetryDatabase: valueTrue}
	if err := c.Update(ctx, live); err != nil {
		t.Fatal(err)
	}
	dbPass(t, c, f)
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "fj"}, live); err != nil {
		t.Fatal(err)
	}
	if _, ok := live.Annotations[AnnotationRetryDatabase]; ok || u.FailedImage != "" {
		t.Fatalf("annotation kept %v, upgrade %+v", ok, u)
	}
}

// heldFailure is an upgrade 17 → 18 that failed with the hold after pg_upgrade had started, on a
// cluster that does not serve.
func heldFailure(t *testing.T) (*forgejov1.Forgejo, client.Client) {
	t.Helper()
	f := upgrading(time.Hour)
	u := forgejo.DatabaseUpgrade(f)
	u.Phase, u.HoldsForgejo, u.PGUpgradeStarted, u.FailedImage = forgejov1.DatabaseUpgradeFailed, true, true, pg18
	u.FailureReason, u.Message, u.CompletionTime = "the old data did not come back", "Upgrade of PostgreSQL failed", ago(time.Minute)
	return f, dbClient(t, cnpgCluster(pg17, pg17, 17, sysOld, false, time.Now().Add(-48*time.Hour)))
}

// Only a later restore of the database lifts the hold on a cluster that does not serve: neither
// the retry annotation nor a restore of the data volume alone, nor one from before the failure.
func TestDatabaseHoldNeedsADatabaseRestore(t *testing.T) {
	ctx := context.Background()
	f, c := heldFailure(t)
	u := forgejo.DatabaseUpgrade(f)
	start := u.StartTime
	f.Annotations = map[string]string{AnnotationRetryDatabase: valueTrue}
	rc, _ := dbPass(t, c, f)
	if cond := condition(f, forgejov1.ConditionDatabaseUpgrading); !zeroReplicas(rc) || !u.HoldsForgejo || u.StartTime != start ||
		cond.Reason != reasonWaitingForRestore || brokenReason(f) != forgejo.BrokenDatabaseUpgradeFailed || rc.Upgrading {
		t.Fatalf("retry on a cluster that does not serve: override %v, holds %v, DatabaseUpgrading %+v", rc.ReplicasOverride, u.HoldsForgejo, cond)
	}
	f.Annotations = nil
	earlier := restore("earlier", forgejov1.RestorePhaseCompleted, ago(time.Hour))
	dataOnly := restore("data-only", forgejov1.RestorePhaseScalingUp, ptr.To(metav1.NewTime(time.Now().Add(time.Minute))))
	dataOnly.Spec.RestoreDatabase = ptr.To(false)
	for _, r := range []*forgejov1.ForgejoRestore{earlier, dataOnly} {
		if err := c.Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if rc, _ = dbPass(t, c, f); !zeroReplicas(rc) || !u.HoldsForgejo || hasEvent(recorded(rc), "DatabaseUpgradeRestored") {
		t.Fatalf("hold lifted by an earlier or data-only restore: override %v", rc.ReplicasOverride)
	}
	later := restore("database", forgejov1.RestorePhaseScalingUp, ptr.To(metav1.NewTime(time.Now().Add(2*time.Minute))))
	if err := c.Create(ctx, later); err != nil {
		t.Fatal(err)
	}
	if rc, _ = dbPass(t, c, f); rc.ReplicasOverride != nil || u.HoldsForgejo || brokenReason(f) != "" ||
		!strings.Contains(u.Message, "ForgejoRestore database") || !hasEvent(recorded(rc), "DatabaseUpgradeRestored") {
		t.Fatalf("hold kept after a database restore: override %v, Broken %q: %s", rc.ReplicasOverride, brokenReason(f), u.Message)
	}
}

// The annotation recreate-database: the cluster of an instance that ran is created again, empty,
// and Forgejo stays stopped (Broken) until a restore of the database created after that cluster;
// then the annotation is removed.
func TestDatabaseRecreateHoldsUntilARestore(t *testing.T) {
	ctx := context.Background()
	f := dbInstance(pg17)
	f.Annotations = map[string]string{forgejo.AnnotationRecreateDatabase: valueTrue}
	running := availableDeployment(imgA, 1)
	c := dbClient(t, f.DeepCopy(), restore("earlier", forgejov1.RestorePhaseCompleted, ago(time.Hour)), running)
	rc, res := dbPass(t, c, f)
	if !zeroReplicas(rc) || res.Stop || brokenReason(f) != forgejo.BrokenDatabaseRestorePending || rc.DatabaseImage != pg17 {
		t.Fatalf("before the cluster: override %v, stop %v, Broken %q", rc.ReplicasOverride, res.Stop, brokenReason(f))
	}
	// Scaled down at once, also in passes whose workload step does not run.
	dep := &appsv1.Deployment{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "fj"}, dep); err != nil || dep.Spec.Replicas == nil || *dep.Spec.Replicas != 0 {
		t.Fatalf("Deployment replicas %v, err %v", dep.Spec.Replicas, err)
	}
	// The database step created it, marked; a restore from before it does not count.
	recreated := cnpgCluster(pg17, pg17, 17, sysNew, true, time.Now().Add(-time.Minute))
	recreated.SetAnnotations(map[string]string{forgejo.AnnotationRestorePending: valueTrue})
	setCluster(t, c, recreated)
	if rc, _ = dbPass(t, c, f); !zeroReplicas(rc) || brokenReason(f) != forgejo.BrokenDatabaseRestorePending {
		t.Fatalf("recreated: override %v, Broken %q", rc.ReplicasOverride, brokenReason(f))
	}
	if err := c.Create(ctx, restore("database", forgejov1.RestorePhaseScalingUp, ago(0))); err != nil {
		t.Fatal(err)
	}
	rc, _ = dbPass(t, c, f)
	live := &forgejov1.Forgejo{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "fj"}, live); err != nil {
		t.Fatal(err)
	}
	if _, ok := live.Annotations[forgejo.AnnotationRecreateDatabase]; ok || rc.ReplicasOverride != nil || brokenReason(f) != "" ||
		!hasEvent(recorded(rc), "DatabaseRestored") || getCluster(t, c).GetAnnotations()[forgejo.AnnotationRestorePending] != "" {
		t.Fatalf("after the restore: annotation kept %v, override %v, Broken %q", ok, rc.ReplicasOverride, brokenReason(f))
	}
	// Applied again (a GitOps tool), it has no effect on the cluster that is no longer marked.
	if rc, _ = dbPass(t, c, f); rc.ReplicasOverride != nil || brokenReason(f) != "" {
		t.Fatalf("annotation applied again: override %v, Broken %q", rc.ReplicasOverride, brokenReason(f))
	}
}

// A Forgejo upgrade does not start while the instance is Broken: its pre-upgrade backup would
// back up an empty, recreated database, and its rollback restore would release the hold.
func TestForgejoUpgradeWaitsWhileBroken(t *testing.T) {
	f := fixture(nil) // spec.image imgB, running imgA
	c := newClient(t)
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image}
	rc.SetBroken(forgejo.BrokenDatabaseRestorePending, "recreated")
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	if cond := condition(f, forgejov1.ConditionUpgrading); f.Status.Upgrade != nil || rc.TargetImage != imgA || cond.Reason != "WaitingForBroken" {
		t.Fatalf("upgrade %+v, target %s, condition %+v", f.Status.Upgrade, rc.TargetImage, cond)
	}
}

// Switching to an external database ends a failed upgrade's hold of the bundled cluster.
func TestDatabaseHoldEndsWithAnExternalDatabase(t *testing.T) {
	f, c := heldFailure(t)
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	rc, _ := dbPass(t, c, f)
	if forgejo.DatabaseUpgrade(f).HoldsForgejo || rc.ReplicasOverride != nil || brokenReason(f) != "" {
		t.Fatalf("holds %v, override %v, Broken %q", forgejo.DatabaseUpgrade(f).HoldsForgejo, rc.ReplicasOverride, brokenReason(f))
	}
}

// A revert whose old data does not come back fails with the hold (Forgejo stopped, Broken) when
// pg_upgrade had started, and without it, touching nothing, when it never started; it waits at
// least dbRevertTimeout, whatever spec.upgrade.timeout says. The old data is back once the primary
// serves it; a revert that finds the upgrade completed verifies the new major.
func TestDatabaseRevertOutcomes(t *testing.T) {
	reverting := func(since time.Duration, started bool) *forgejov1.Forgejo {
		f := upgrading(since + time.Minute)
		u := forgejo.DatabaseUpgrade(f)
		u.Phase, u.PhaseStartTime, u.FailureReason, u.PGUpgradeStarted = forgejov1.DatabaseUpgradeReverting, ago(since), "Job failed", started
		return f
	}
	broken := func() *unstructured.Unstructured {
		return cnpgCluster(pg17, pg17, 17, sysOld, false, time.Now().Add(-48*time.Hour))
	}
	phase := func(f *forgejov1.Forgejo) forgejov1.DatabaseUpgradePhase { return forgejo.DatabaseUpgrade(f).Phase }

	f := reverting(16*time.Minute, true)
	c := dbClient(t, broken())
	rc, res := dbPass(t, c, f)
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeFailed || !u.HoldsForgejo || !zeroReplicas(rc) ||
		getCluster(t, c) == nil || brokenReason(f) != forgejo.BrokenDatabaseUpgradeFailed || restores(t, c) != 0 {
		t.Fatalf("phase %s, holds %v, override %v, Broken %q", u.Phase, u.HoldsForgejo, rc.ReplicasOverride, brokenReason(f))
	}
	// pg_upgrade never started: the untouched old data is kept, and Forgejo is not held.
	f = reverting(16*time.Minute, false)
	c = dbClient(t, broken())
	rc, res = dbPass(t, c, f)
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeFailed || u.HoldsForgejo || res.Stop || getCluster(t, c) == nil ||
		rc.DatabaseImage != pg17 || rc.ReplicasOverride != nil || u.FailedImage != pg18 || !strings.Contains(u.Message, "pg_upgrade never started") {
		t.Fatalf("phase %s, image %s, override %v: %s", u.Phase, rc.DatabaseImage, rc.ReplicasOverride, u.Message)
	}
	// A short spec.upgrade.timeout does not cut the revert short.
	f = reverting(5*time.Minute, true)
	f.Spec.Upgrade.Timeout = &metav1.Duration{Duration: 5 * time.Second}
	if dbPass(t, dbClient(t, broken()), f); phase(f) != forgejov1.DatabaseUpgradeReverting {
		t.Fatalf("phase %s", phase(f))
	}
	f = reverting(time.Minute, true)
	rc, _ = dbPass(t, dbClient(t, cnpgCluster(pg18, pg18, 18, sysNew, true, time.Now().Add(-48*time.Hour))), f)
	if phase(f) != forgejov1.DatabaseUpgradeVerifying || rc.DatabaseImage != pg18 {
		t.Fatalf("phase %s, image %s", phase(f), rc.DatabaseImage)
	}
	// Another system ID is not the old data.
	f = reverting(time.Minute, true)
	if dbPass(t, dbClient(t, cnpgCluster(pg17, pg17, 17, "7999", true, time.Now().Add(-48*time.Hour)), primaryPod(true)), f); phase(f) !=
		forgejov1.DatabaseUpgradeReverting {
		t.Fatalf("phase %s", phase(f))
	}
	// The primary serves the old data while a replica is still starting: reverted.
	twoInstances := func(primaryReady bool) []client.Object {
		cl := running17()
		cl.Object["spec"].(map[string]any)["instances"] = int64(2)
		cl.Object["status"].(map[string]any)["phase"] = "Creating a new replica"
		return []client.Object{cl, primaryPod(primaryReady)}
	}
	f = reverting(time.Minute, true)
	if dbPass(t, dbClient(t, twoInstances(false)...), f); phase(f) != forgejov1.DatabaseUpgradeReverting {
		t.Fatalf("reverted before the primary is ready: phase %s", phase(f))
	}
	f = reverting(time.Minute, true)
	if dbPass(t, dbClient(t, twoInstances(true)...), f); phase(f) != forgejov1.DatabaseUpgradeRolledBack {
		t.Fatalf("phase %s: %s", phase(f), forgejo.DatabaseUpgrade(f).Message)
	}
}

// The verification judges only pods started for it: a pod left from before the upgrade (here an
// evicted one that had restarted, with the same image) is not a failure on the new major.
func TestDatabaseVerifyIgnoresLeftoverPods(t *testing.T) {
	f := verifying(time.Minute)
	evicted := serverPod(f, "fj-evicted", imgA, ago(3*time.Hour), corev1.ContainerState{}, 3)
	evicted.Status.Phase, evicted.Status.Reason = corev1.PodFailed, "Evicted"
	old := serverPod(f, "fj-old", imgA, ago(3*time.Hour), corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingCrashLoopBackOff}}, 4)
	finishedNew := serverPod(f, "fj-shutdown", imgA, ago(30*time.Second), corev1.ContainerState{}, 3)
	finishedNew.Status.Phase = corev1.PodFailed
	c := dbClient(t, cnpgCluster(pg18, pg18, 18, sysNew, true, time.Now().Add(-48*time.Hour)), evicted, old, finishedNew)
	dbPass(t, c, f)
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeVerifying {
		t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
	}
	// A pod of the verification that crashes still fails it.
	crashing := serverPod(f, "fj-new", imgA, ago(10*time.Second), corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingCrashLoopBackOff}}, 3)
	if err := c.Create(context.Background(), crashing); err != nil {
		t.Fatal(err)
	}
	dbPass(t, c, f)
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeFailed || !u.HoldsForgejo || !strings.Contains(u.FailureReason, "fj-new") {
		t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
	}
}

// A Forgejo upgrade does not start while PostgreSQL is upgraded; the retry annotation waits.
func TestForgejoUpgradeWaitsForDatabaseUpgrade(t *testing.T) {
	f := fixture(nil) // spec.image imgB, running imgA
	c := newClient(t)
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image,
		DatabaseUpgrading: true, Upgrading: true}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	cond := condition(f, forgejov1.ConditionUpgrading)
	if f.Status.Upgrade != nil || rc.TargetImage != imgA || cond == nil || cond.Reason != "WaitingForDatabaseUpgrade" {
		t.Fatalf("upgrade %+v, target %s, condition %+v", f.Status.Upgrade, rc.TargetImage, cond)
	}
	if reconcileWith(t, c, c.Scheme(), f); f.Status.Upgrade == nil || !inFlight(f.Status.Upgrade.Phase) {
		t.Fatalf("after the database upgrade the Forgejo upgrade starts: %+v", f.Status.Upgrade)
	}
}

// A Cluster deleted while no upgrade is in flight (the manual rollback of docs/upgrades.md, with
// the annotation recreate-database) is created again only once it and its pods, volumes, Secrets
// and Services are gone: a new cluster of that name would take them over. The step stops
// meanwhile, so that the database step neither updates the Cluster being deleted nor creates the
// new one too early, and Forgejo stays stopped until a restore.
func TestDatabaseRecreatesADeletedClusterOnceItsObjectsAreGone(t *testing.T) {
	ctx := context.Background()
	// Rolled back by hand after the upgrade 17 → 18: the image set back to 17 was refused while
	// the Cluster existed; then the Cluster was deleted.
	f := dbInstance(pg17)
	f.Annotations = map[string]string{forgejo.AnnotationRecreateDatabase: valueTrue}
	f.Status.Database = &forgejov1.DatabaseStatus{Image: pg18, MajorVersion: 18, SystemID: sysNew, Upgrade: &forgejov1.DatabaseUpgradeStatus{
		Phase: forgejov1.DatabaseUpgradeSucceeded, FromImage: pg17, ToImage: pg18, FromMajor: 17, ToMajor: 18, BackupName: testPreBackup,
		StartTime: ago(time.Hour), CompletionTime: ago(50 * time.Minute)}}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{Type: forgejov1.ConditionDegraded, Status: metav1.ConditionTrue,
		Reason: ReasonPostgresDowngradeRefused, Message: "refused"})
	deleting := cnpgCluster(pg18, pg18, 18, sysNew, true, time.Now().Add(-48*time.Hour))
	deleting.SetFinalizers([]string{"example.com/hold"})
	deleting.SetDeletionTimestamp(ago(0))
	labels := map[string]string{forgejo.CNPGClusterLabel: testCluster}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: testPrimary, Namespace: "ns", Labels: labels}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: testPrimary + "-pod", Namespace: "ns", Labels: labels}}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "fj-db-rw", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{{
		APIVersion: "postgresql.cnpg.io/v1", Kind: "Cluster", Name: testCluster, UID: "cluster-uid"}}}}
	c := dbClient(t, deleting, pvc, pod, svc)

	waits := func(want string) {
		t.Helper()
		rc, res := dbPass(t, c, f)
		db, d := condition(f, forgejov1.ConditionDatabaseReady), condition(f, forgejov1.ConditionDegraded)
		if !res.Stop || db == nil || db.Status != metav1.ConditionFalse || db.Reason != reasonClusterDeleting || !strings.Contains(db.Message, want) ||
			d.Status == metav1.ConditionTrue || rc.Upgrading || !zeroReplicas(rc) {
			t.Fatalf("stop %v, DatabaseReady %+v, Degraded %+v", res.Stop, db, d)
		}
	}
	waits("is being deleted")
	cl := getCluster(t, c)
	cl.SetFinalizers(nil)
	if err := c.Update(ctx, cl); err != nil {
		t.Fatal(err)
	}
	if getCluster(t, c) != nil {
		t.Fatal("the Cluster was not deleted")
	}
	for _, obj := range []client.Object{pvc, svc, pod} {
		waits(obj.GetName())
		if err := c.Delete(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	// Then the database step creates it again, on the requested image.
	rc, res := dbPass(t, c, f)
	if cond := condition(f, forgejov1.ConditionDatabaseUpgrading); res.Stop || rc.DatabaseImage != pg17 || cond.Reason != reasonDatabaseUpToDate ||
		!strings.Contains(cond.Message, "Creating CNPG cluster fj-db") || !zeroReplicas(rc) || brokenReason(f) != forgejo.BrokenDatabaseRestorePending {
		t.Fatalf("stop %v, image %s, DatabaseUpgrading %+v", res.Stop, rc.DatabaseImage, cond)
	}
}

// A cluster that disappears during the upgrade is not recreated: Forgejo stays stopped, Broken.
func TestDatabaseUpgradeClusterLost(t *testing.T) {
	f := upgrading(time.Minute)
	rc, res := dbPass(t, dbClient(t), f)
	// Stopped, so that the database step does not create a cluster in this pass: only the idle
	// path waits for what the deleted one left.
	if u := forgejo.DatabaseUpgrade(f); u.Phase != forgejov1.DatabaseUpgradeFailed || !u.HoldsForgejo || !zeroReplicas(rc) ||
		brokenReason(f) != forgejo.BrokenDatabaseUpgradeFailed || !res.Stop {
		t.Fatalf("phase %s, holds %v, Broken %q, stop %v", u.Phase, u.HoldsForgejo, brokenReason(f), res.Stop)
	}
}

// counterValue reads a counter.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}
