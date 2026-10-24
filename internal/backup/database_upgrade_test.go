package backup_test

// Backups and restores around a PostgreSQL major upgrade (docs/upgrades.md, "PostgreSQL major
// upgrades"), driven through the real reconcilers against fake clients.

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPG17 = "ghcr.io/cloudnative-pg/postgresql:17-minimal-trixie"
	testPG18 = "ghcr.io/cloudnative-pg/postgresql:18-minimal-trixie"
)

// upgradingInstance is "fb" asking for PostgreSQL 18 while its data runs 17, with the upgrade
// in phase.
func upgradingInstance(phase forgejov1.DatabaseUpgradePhase) *forgejov1.Forgejo {
	f := instance("")
	f.Spec.Database.CNPG.ImageName = testPG18
	f.Status.Image = testImage16
	f.Status.Database = &forgejov1.DatabaseStatus{Type: forgejov1.DatabaseTypeCNPG, Image: testPG17, MajorVersion: 17,
		Upgrade: &forgejov1.DatabaseUpgradeStatus{Phase: phase, FromImage: testPG17, ToImage: testPG18, FromMajor: 17, ToMajor: 18,
			BackupName: "fb-pre-pg18-1", RestoreName: "fb-pg-rollback-1"}}
	return f
}

// dbAppSecret is CNPG's "fb-db-app" Secret of instance "fb".
func dbAppSecret(password string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fb-db-app", Namespace: ns},
		Data: map[string][]byte{"username": []byte("forgejo"), testPasswordField: []byte(password)}}
}

func TestDumpTooNew(t *testing.T) {
	f := upgradingInstance(forgejov1.DatabaseUpgradeSucceeded)
	if msg := backup.DumpTooNew(f, testPG18); !strings.Contains(msg, "pg_dump of PostgreSQL 18") {
		t.Errorf("18 dump into 17: %q", msg)
	}
	for _, image := range []string{testPG17, "", "pg@sha256:0123"} {
		if msg := backup.DumpTooNew(f, image); msg != "" {
			t.Errorf("%q into 17: %q", image, msg)
		}
	}
	f.Status.Database.MajorVersion = 18
	if msg := backup.DumpTooNew(f, testPG17); msg != "" {
		t.Errorf("17 dump into 18: %q", msg)
	}
	f.Spec.Database.Type = forgejov1.DatabaseTypeExternal
	f.Status.Database.MajorVersion = 17
	if msg := backup.DumpTooNew(f, testPG18); msg != "" {
		t.Errorf("external database: %q", msg)
	}
}

// Restores wait while PostgreSQL is upgraded, except the upgrade's own rollback restore; a dump
// of a newer major than the server's is refused.
func TestRestoresAroundADatabaseUpgrade(t *testing.T) {
	s := scheme(t)
	b := completedBackup("b1")
	b.Status.PostgresImage = testPG17
	f := upgradingInstance(forgejov1.DatabaseUpgradeUpgrading)

	rs := newRestore("r1", "")
	rs.Spec.BackupName = "b1"
	rs.Finalizers = []string{forgejo.FinalizerName}
	rs.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	own := newRestore("fb-pg-rollback-1", "")
	own.Spec.BackupName = "b1"
	own.Finalizers = []string{forgejo.FinalizerName}
	own.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, b, rs, own).WithStatusSubresource(rs, own).Build()

	got := reconcileRestore(t, c, s, rs.Name, 1)
	if got.Status.Phase != forgejov1.RestorePhasePending || !strings.Contains(got.Status.Message, "PostgreSQL major upgrade") {
		t.Fatalf("phase %s (%s), want Pending, waiting for the upgrade", got.Status.Phase, got.Status.Message)
	}
	if got = reconcileRestore(t, c, s, own.Name, 1); got.Status.Phase != forgejov1.RestorePhaseScalingDown {
		t.Fatalf("own restore: phase %s (%s)", got.Status.Phase, got.Status.Message)
	}

	// A backup of PostgreSQL 18 into a cluster of 17.
	newer := completedBackup("b18")
	newer.Status.PostgresImage = testPG18
	rs18 := newRestore("r18", "")
	rs18.Spec.BackupName = "b18"
	rs18.Spec.RestoreDatabase = nil // the database too
	rs18.Finalizers = []string{forgejo.FinalizerName}
	rs18.Labels = map[string]string{forgejo.LabelForgejo: "fb"}
	idle := instance("")
	idle.Status.Database = &forgejov1.DatabaseStatus{Type: forgejov1.DatabaseTypeCNPG, Image: testPG17, MajorVersion: 17}
	c = fake.NewClientBuilder().WithScheme(s).WithObjects(idle, newer, rs18).WithStatusSubresource(rs18).Build()
	if got = reconcileRestore(t, c, s, rs18.Name, 1); got.Status.Phase != forgejov1.RestorePhaseFailed || reason(got.Status.Conditions) != "NewerDump" {
		t.Fatalf("phase %s/%s (%s), want Failed/NewerDump", got.Status.Phase, reason(got.Status.Conditions), got.Status.Message)
	}
}

// preUpgradeBackupObject is the PostgreSQL upgrade's cold backup "fb-pre-pg18-1" of instance
// "fb", before the backup controller has seen it.
func preUpgradeBackupObject() *forgejov1.ForgejoBackup {
	pre := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "fb-pre-pg18-1", Namespace: ns,
		Finalizers: []string{forgejo.FinalizerName}, Labels: map[string]string{forgejo.LabelForgejo: "fb", forgejo.LabelBackupReason: "PreUpgrade"}}}
	pre.Spec.ForgejoRef.Name = "fb"
	pre.Spec.Reason = forgejov1.BackupReasonPreUpgrade
	return pre
}

// Backups wait while PostgreSQL is upgraded, except the upgrade's own cold backup, which dumps
// with the running (old) image and records it, and does not wait for restores that have not
// started (they wait for the upgrade).
func TestBackupsAroundADatabaseUpgrade(t *testing.T) {
	ctx := context.Background()
	s := scheme(t)
	f := upgradingInstance(forgejov1.DatabaseUpgradeBackingUp)
	appSecret := dbAppSecret("pw")
	manual := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "manual", Namespace: ns,
		Finalizers: []string{forgejo.FinalizerName}, Labels: map[string]string{forgejo.LabelForgejo: "fb", forgejo.LabelBackupReason: "Manual"}}}
	manual.Spec.ForgejoRef.Name = "fb"
	pre := preUpgradeBackupObject()
	waiting := newRestore("waiting", forgejov1.RestorePhasePending)
	waiting.UID = "waiting-uid"
	unseen := newRestore("unseen", "")
	unseen.UID = "unseen-uid"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, appSecret, manual, pre, waiting, unseen).WithStatusSubresource(manual, pre).Build()

	reconcileBackup(t, c, manual.Name)
	if got := getBackup(t, c, manual.Name); got.Status.Phase != forgejov1.BackupPhasePending || !strings.Contains(got.Status.Message, "ForgejoRestore") &&
		!strings.Contains(got.Status.Message, "PostgreSQL major upgrade") {
		t.Fatalf("manual backup: phase %s (%s)", got.Status.Phase, got.Status.Message)
	}
	reconcileBackup(t, c, pre.Name)
	got := getBackup(t, c, pre.Name)
	if got.Status.Phase != forgejov1.BackupPhaseRunning || got.Status.PostgresImage != testPG17 {
		t.Fatalf("pre-upgrade backup: phase %s, postgres image %q (%s)", got.Status.Phase, got.Status.PostgresImage, got.Status.Message)
	}
	job := &batchv1.Job{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: got.Status.JobName}, job); err != nil {
		t.Fatal(err)
	}
	for _, ic := range job.Spec.Template.Spec.InitContainers {
		if ic.Name == backup.ContainerDump && ic.Image != testPG17 {
			t.Fatalf("pg_dump runs from %s, want the running %s", ic.Image, testPG17)
		}
	}
}

// A ForgejoRestore created while the PostgreSQL upgrade takes its cold backup waits for the
// upgrade, and the backup does not wait for it (each would otherwise wait for the other, with
// Forgejo stopped). Both real reconcilers run.
func TestRestoreRequestedDuringTheDatabaseUpgradeBackup(t *testing.T) {
	s := scheme(t)
	f := upgradingInstance(forgejov1.DatabaseUpgradeBackingUp)
	b := completedBackup("b1")
	b.Status.PostgresImage = testPG17
	pre := preUpgradeBackupObject()
	rs := newRestore("r1", "")
	rs.Spec.BackupName = "b1"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, b, dbAppSecret("pw"), pre, rs).
		WithStatusSubresource(&forgejov1.ForgejoRestore{}, &forgejov1.ForgejoBackup{}).Build()

	// The restore's first pass adds its finalizer, the second one waits.
	if got := reconcileRestore(t, c, s, rs.Name, 2); got.Status.Phase != forgejov1.RestorePhasePending ||
		!strings.Contains(got.Status.Message, "PostgreSQL major upgrade") {
		t.Fatalf("restore: phase %s (%s)", got.Status.Phase, got.Status.Message)
	}
	reconcileBackup(t, c, pre.Name)
	if got := getBackup(t, c, pre.Name); got.Status.Phase != forgejov1.BackupPhaseRunning {
		t.Fatalf("pre-upgrade backup: phase %s (%s), want Running", got.Status.Phase, got.Status.Message)
	}
	// It still waits for a restore that has started (a cache that lagged behind the upgrade).
	got := &forgejov1.ForgejoRestore{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: rs.Name}, got); err != nil {
		t.Fatal(err)
	}
	got.Status.Phase = forgejov1.RestorePhaseRestoring
	if err := c.Status().Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	other := preUpgradeBackupObject()
	other.Name = "fb-pre-pg18-2"
	live := &forgejov1.Forgejo{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: f.Name}, live); err != nil {
		t.Fatal(err)
	}
	live.Status.Database.Upgrade.BackupName = other.Name
	if err := c.Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, c, other.Name)
	if got := getBackup(t, c, other.Name); got.Status.Phase != forgejov1.BackupPhasePending || !strings.Contains(got.Status.Message, "ForgejoRestore r1") {
		t.Fatalf("pre-upgrade backup during a running restore: phase %s (%s)", got.Status.Phase, got.Status.Message)
	}
}
