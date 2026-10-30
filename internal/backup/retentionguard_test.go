package backup

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testIdentity      = "aaaaaaaaaaaaaaaa"
	testOtherIdentity = "bbbbbbbbbbbbbbbb"
	// testGuardNewest is the newest remote backup of TestRemoteRetentionGuards.
	testGuardNewest = "fj-guard-3"
)

// sized is a completed scheduled backup "fb-<name>" of instance fb, completed on day of October
// 2026, with a database dump of db and a data archive of data bytes (none when 0), taken by
// the identity fp.
func sized(name string, day int, db, data int64, fp string) *forgejov1.ForgejoBackup {
	at := time.Date(2026, 10, day, 3, 0, 0, 0, time.UTC)
	b := reasonBackup("fb-"+name, forgejov1.BackupReasonScheduled)
	b.CreationTimestamp = metav1.NewTime(at.Add(-time.Minute))
	b.Status.Phase = forgejov1.BackupPhaseCompleted
	b.Status.CompletionTime = &metav1.Time{Time: at}
	b.Status.DatabaseSizeBytes, b.Status.DataSizeBytes, b.Status.IncludesData = db, data, data > 0
	b.Status.SecretKeyFingerprint = fp
	return b
}

// runPrune prunes backups (any order) of instance f with retention keep for an instance of
// identity current, and returns the size-drop finding and the names left, oldest first.
func runPrune(t *testing.T, f *forgejov1.Forgejo, keep int32, current string, backups ...*forgejov1.ForgejoBackup) (string, []string) {
	t.Helper()
	ctx := context.Background()
	s := testScheme(t)
	objs := make([]client.Object, 0, len(backups))
	for _, b := range backups {
		objs = append(objs, b)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
	f.Spec.Backup.Retention = keep
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, IdentityFingerprint: current}
	all, err := ListBackups(ctx, c, f.Namespace, f.Name)
	if err != nil {
		t.Fatal(err)
	}
	drop, err := prune(ctx, rc, scheduledBackups(all), all, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if all, err = ListBackups(ctx, c, f.Namespace, f.Name); err != nil {
		t.Fatal(err)
	}
	left := make([]string, 0, len(all))
	for _, b := range all {
		left = append(left, strings.TrimPrefix(b.Name, "fb-"))
	}
	return drop, left
}

// A backup of another identity (e.g. of an earlier instance of the same name, kept by
// retainPolicy Retain, whose Secrets were not) neither counts for retention nor is pruned; one
// without a fingerprint (taken before the operator recorded it) counts as the instance's.
func TestRetentionKeepsBackupsOfAnotherIdentity(t *testing.T) {
	backups := func() []*forgejov1.ForgejoBackup {
		return []*forgejov1.ForgejoBackup{
			sized("other-1", 1, 1000, 0, testOtherIdentity),
			sized("other-2", 2, 1000, 0, testOtherIdentity),
			sized("unknown-3", 3, 1000, 0, ""),
			sized("own-4", 4, 1000, 0, testIdentity),
			sized("own-5", 5, 1000, 0, testIdentity),
		}
	}
	if _, left := runPrune(t, ownedInstance(forgejov1.RetainPolicyRetain), 2, testIdentity, backups()...); !slices.Equal(left,
		[]string{"other-1", "other-2", "own-4", "own-5"}) {
		t.Errorf("left %v", left)
	}
	// While the instance's own fingerprint is unknown, only backups without one are its.
	if _, left := runPrune(t, ownedInstance(forgejov1.RetainPolicyRetain), 1, "", backups()...); !slices.Equal(left,
		[]string{"other-1", "other-2", "unknown-3", "own-4", "own-5"}) {
		t.Errorf("left %v with an unknown identity", left)
	}
	if _, left := runPrune(t, ownedInstance(forgejov1.RetainPolicyRetain), 1, "", sized("unknown-1", 1, 1000, 0, ""),
		sized("unknown-2", 2, 1000, 0, ""), sized("own-3", 3, 1000, 0, testIdentity)); !slices.Equal(left, []string{"unknown-2", "own-3"}) {
		t.Errorf("left %v of the backups without a fingerprint", left)
	}
}

// When the newest completed scheduled backup is less than half the size of another one that
// retention holds, nothing completed is pruned until the sizes recover or a person accepts them.
func TestRetentionHoldsWhenBackupSizesDrop(t *testing.T) {
	failed := func() *forgejov1.ForgejoBackup {
		b := sized("failed-0", 1, 0, 0, "")
		b.CreationTimestamp = metav1.NewTime(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC))
		b.Status.Phase, b.Status.CompletionTime = forgejov1.BackupPhaseFailed, nil
		return b
	}
	for _, tc := range []struct {
		name     string
		accepted string
		newest   *forgejov1.ForgejoBackup
		others   []*forgejov1.ForgejoBackup
		// drop is a part of the expected finding ("" for none); left are the backups kept.
		drop string
		left []string
	}{
		{name: "database dump halved", newest: sized("s4", 4, 100, 5000, testIdentity),
			drop: "database dump 100 bytes, fb-s2 had 1100", left: []string{"s1", "s2", "s3", "s4"}},
		{name: "data archive halved", newest: sized("s4", 4, 1000, 100, testIdentity),
			drop: "data archive 100 bytes, fb-s3 had 6000", left: []string{"s1", "s2", "s3", "s4"}},
		{name: "half is no drop", newest: sized("s4", 4, 550, 3000, testIdentity), left: []string{"s3", "s4"}},
		{name: "without a data archive only the dump counts", newest: sized("s4", 4, 1000, 0, testIdentity), left: []string{"s3", "s4"}},
		{name: "unknown sizes are not compared", newest: sized("s4", 4, 0, 0, testIdentity), left: []string{"s3", "s4"}},
		{name: "accepted newest", accepted: "fb-s4", newest: sized("s4", 4, 100, 100, testIdentity), left: []string{"s3", "s4"}},
		{name: "accepted backup gone", accepted: "fb-s0", newest: sized("s4", 4, 100, 100, testIdentity),
			drop: "database dump 100 bytes", left: []string{"s1", "s2", "s3", "s4"}},
		{name: "accepted earlier small backup", accepted: "fb-s3", newest: sized("s4", 4, 100, 100, testIdentity),
			others: []*forgejov1.ForgejoBackup{sized("s1", 1, 1000, 5000, ""), sized("s2", 2, 1100, 5000, testIdentity),
				sized("s3", 3, 100, 100, testIdentity)},
			left: []string{"s3", "s4"}},
		{name: "backups of another identity are not compared", newest: sized("s4", 4, 100, 100, testIdentity),
			others: []*forgejov1.ForgejoBackup{sized("s1", 1, 1000, 5000, testOtherIdentity), sized("s2", 2, 1100, 5000, testOtherIdentity),
				sized("s3", 3, 100, 100, testIdentity)},
			left: []string{"s1", "s2", "s3", "s4"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := ownedInstance(forgejov1.RetainPolicyRetain)
			if tc.accepted != "" {
				f.Annotations = map[string]string{AnnotationBackupSizeAccepted: tc.accepted}
			}
			others := tc.others
			if others == nil {
				others = []*forgejov1.ForgejoBackup{sized("s1", 1, 1000, 5000, ""), sized("s2", 2, 1100, 5000, testIdentity),
					sized("s3", 3, 1000, 6000, testIdentity)}
			}
			drop, left := runPrune(t, f, 2, testIdentity, append(others, tc.newest, failed())...)
			if !slices.Equal(left, tc.left) {
				t.Errorf("left %v, want %v (a failed backup is always pruned)", left, tc.left)
			}
			switch {
			case tc.drop == "" && drop != "":
				t.Errorf("unexpected finding %q", drop)
			case tc.drop != "" && (!strings.Contains(drop, tc.drop) || !strings.Contains(drop, AnnotationBackupSizeAccepted+"=fb-s4")):
				t.Errorf("finding %q, want it to contain %q and how to accept it", drop, tc.drop)
			}
		})
	}
}

// The finding takes BackupHealthy, with one Warning event per new finding.
func TestBackupHealthReportsSizeDrop(t *testing.T) {
	sched, _ := cron.ParseStandard("0 3 * * *")
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	rec := events.NewFakeRecorder(10)
	rc := &forgejo.ReconcileContext{Instance: f, Recorder: rec}
	latest := sized("s2", 2, 100, 0, testIdentity)
	latest.Status.Phase = forgejov1.BackupPhaseFailed
	for range 2 {
		setHealth(rc, sched, []*forgejov1.ForgejoBackup{latest}, time.Now(), nil, keyProblem{reason: "AnyKeyProblem"}, "dropped")
	}
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBackupHealthy)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != reasonBackupSizeDropped || c.Message != "dropped" {
		t.Fatalf("BackupHealthy %+v", c)
	}
	if n := len(rec.Events); n != 1 {
		t.Errorf("%d events for one finding, want 1", n)
	}
	if e := <-rec.Events; !strings.Contains(e, "Warning "+reasonBackupSizeDropped) {
		t.Errorf("event %q", e)
	}
}

// The scheduled backup step holds retention and reports the drop.
func TestReconcileScheduleHoldsRetentionOnSizeDrop(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := ownedInstance(forgejov1.RetainPolicyRetain)
	f.Spec.Backup.Enabled, f.Spec.Backup.Schedule, f.Spec.Backup.Retention = true, "@every 1h", 1
	now := time.Now().Truncate(time.Second)
	f.Status.Backup = &forgejov1.BackupStatusSummary{LastScheduleTime: &metav1.Time{Time: now.Add(-10 * time.Minute)}}
	big, small := sized("big", 1, 1000, 5000, testIdentity), sized("small", 2, 1000, 100, testIdentity)
	for i, b := range []*forgejov1.ForgejoBackup{big, small} {
		at := now.Add(time.Duration(i-2) * time.Hour)
		b.CreationTimestamp, b.Status.CompletionTime = metav1.NewTime(at.Add(-time.Minute)), &metav1.Time{Time: at}
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(f, big, small).WithStatusSubresource(&forgejov1.ForgejoBackup{}).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, IdentityFingerprint: testIdentity}
	if _, err := ReconcileSchedule(ctx, rc); err != nil {
		t.Fatal(err)
	}
	list := &forgejov1.ForgejoBackupList{}
	if err := c.List(ctx, list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Errorf("%d backups left, want both", len(list.Items))
	}
	if c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionBackupHealthy); c == nil || c.Reason != reasonBackupSizeDropped ||
		!strings.Contains(c.Message, "data archive 100 bytes, fb-big had 5000") {
		t.Errorf("BackupHealthy %+v", c)
	}
}

// Remote retention applies both guards to the manifests in the bucket.
func TestRemoteRetentionGuards(t *testing.T) {
	store := NewFakeS3(t)
	s := testScheme(t)
	ctx := context.Background()
	f := remoteInstance(store)
	f.Spec.Backup.Enabled, f.Spec.Backup.Retention = true, 1
	day := func(d int) time.Time { return time.Date(2026, 10, d, 3, 0, 0, 0, time.UTC) }
	put := func(name, fp string, d, db int) {
		files := map[string][]byte{DBDumpFile: []byte(strings.Repeat("x", db)), IdentityFile: FakeIdentity(t, "k")}
		store.PutBackup(t, "team/", "dr/fj/"+name, RemoteManifest{Backup: name, Namespace: remoteNS, Forgejo: "fj", ForgejoUID: string(f.UID),
			Reason: forgejov1.BackupReasonScheduled, CompletedAt: day(d).Format(time.RFC3339), SecretKeyFingerprint: fp}, files, day(d))
	}
	put("fj-guard-0", testOtherIdentity, 1, 5000)
	put("fj-guard-1", testIdentity, 2, 1000)
	put("fj-guard-2", "", 3, 1000)
	put(testGuardNewest, testIdentity, 4, 100)
	left := func() []string {
		var out []string
		for _, k := range store.Keys("team/dr/fj/") {
			if dir, ok := strings.CutSuffix(strings.TrimPrefix(k, "team/dr/fj/"), "/"+RemoteManifestFile+".bin"); ok {
				out = append(out, dir)
			}
		}
		return out
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(secretObjects(FakeSecrets(remoteNS, "fj-backup-key"))...).Build()
	rc := &forgejo.ReconcileContext{Client: c, Scheme: s, Instance: f, IdentityFingerprint: testIdentity}
	defer forgetRemoteRetention(f.Namespace, f.Name)

	now := time.Now()
	pruneRemote(ctx, rc, nil, now)
	if got := left(); !slices.Equal(got, []string{"fj-guard-0", "fj-guard-1", "fj-guard-2", testGuardNewest}) {
		t.Errorf("pruned while the newest backup is smaller: left %v", got)
	}
	drop := remoteSizeDrop(f)
	if !strings.Contains(drop, "database dump 100 bytes, fj-guard-2 had 1000") || !strings.Contains(drop, AnnotationBackupSizeAccepted+"="+testGuardNewest) {
		t.Errorf("finding %q", drop)
	}
	if msg := f.Status.Backup.Remote.Message; !strings.Contains(msg, drop) {
		t.Errorf("status.backup.remote.message %q lacks the finding", msg)
	}

	// Accepting the newest backup's size runs retention again at once: it keeps the newest
	// backup of the instance's identity and the backup of the other identity.
	f.Annotations = map[string]string{AnnotationBackupSizeAccepted: testGuardNewest}
	pruneRemote(ctx, rc, nil, now.Add(time.Minute))
	if got := left(); !slices.Equal(got, []string{"fj-guard-0", testGuardNewest}) {
		t.Errorf("left %v after the size was accepted", got)
	}
	if drop := remoteSizeDrop(f); drop != "" {
		t.Errorf("finding %q after the size was accepted", drop)
	}
}
