package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testSize20Gi        = "20Gi"
	testBackupsPVC      = "fb-backups"
	testClassExpandable = "expandable"
	testClassFixed      = "fixed"
	testSize200Gi       = "200Gi"
	testSize50Gi        = "50Gi"
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

func TestJobNamesNeverCollideAcrossKinds(t *testing.T) {
	for _, name := range []string{"x", "pre-migration", strings.Repeat("a", 60), strings.Repeat("b", 253)} {
		b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name}}
		r := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name}}
		names := map[string]string{BackupJobName(b): "backup", CleanupJobName(b): "cleanup"}
		if other, dup := names[RestoreJobName(r)]; dup {
			t.Errorf("restore %q shares its Job name with the %s Job", name, other)
		}
		if len(names) != 2 {
			t.Errorf("backup %q: backup and cleanup Jobs share a name", name)
		}
		for _, n := range []string{BackupJobName(b), CleanupJobName(b), RestoreJobName(r)} {
			if len(n) > 63 {
				t.Errorf("job name %q is longer than 63 characters", n)
			}
		}
	}
}

func TestCreateJobRefusesJobsOfOtherObjects(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	bk := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "backup-uid"}}
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "restore-uid"}}
	old := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "ns", UID: "old-restore-uid"}}

	job := func(owner client.Object) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "x-restore", Namespace: "ns"}}
		if owner != nil {
			if err := controllerutil.SetControllerReference(owner, j, s); err != nil {
				t.Fatal(err)
			}
		}
		return j
	}

	cases := []struct {
		name      string
		existing  *batchv1.Job
		conflict  bool
		transient bool
	}{
		{name: "no job yet", existing: nil},
		{name: "own job (restart after create)", existing: job(rs)},
		{name: "job of the backup with the same name", existing: job(bk), conflict: true},
		{name: "job without a controller", existing: job(nil), conflict: true},
		{name: "job of a deleted restore of the same name", existing: job(old), conflict: true, transient: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cb := fake.NewClientBuilder().WithScheme(s)
			if tc.existing != nil {
				cb = cb.WithObjects(tc.existing)
			}
			c := cb.Build()
			err := CreateJob(ctx, c, rs, job(rs))
			var conflict *JobConflictError
			if got := errors.As(err, &conflict); got != tc.conflict {
				t.Fatalf("conflict = %v, want %v (err %v)", got, tc.conflict, err)
			}
			if tc.conflict && conflict.Transient != tc.transient {
				t.Errorf("transient = %v, want %v", conflict.Transient, tc.transient)
			}
			if !tc.conflict && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func pod(name string, created time.Time, phase corev1.PodPhase, init []corev1.ContainerStatus, main ...corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)},
		Status:     corev1.PodStatus{Phase: phase, InitContainerStatuses: init, ContainerStatuses: main},
	}
}

func terminated(name string, code int32) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code}}}
}

func running(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func waiting(name string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}
}

func TestRestoreModification(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		pods []corev1.Pod
		want Modification
	}{
		{name: "no pods", want: Unmodified},
		{name: "verifying", want: Unmodified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodPending, []corev1.ContainerStatus{running(ContainerVerify), waiting(ContainerRestoreDB)}, waiting(ContainerRestoreData)),
		}},
		{name: "verification failed", want: Unmodified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 1), waiting(ContainerRestoreDB)}, waiting(ContainerRestoreData)),
		}},
		{name: "database step running", want: MaybeModified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodPending, []corev1.ContainerStatus{terminated(ContainerVerify, 0), running(ContainerRestoreDB)}, waiting(ContainerRestoreData)),
		}},
		{name: "database step failed (rolled back)", want: Unmodified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 0), terminated(ContainerRestoreDB, 1)}, waiting(ContainerRestoreData)),
		}},
		{name: "database committed", want: Modified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodPending, []corev1.ContainerStatus{terminated(ContainerVerify, 0), terminated(ContainerRestoreDB, 0)}, waiting(ContainerRestoreData)),
		}},
		{name: "data-only restore past verification", want: MaybeModified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodPending, []corev1.ContainerStatus{terminated(ContainerVerify, 0)}, waiting(ContainerRestoreData)),
		}},
		{name: "data step failed", want: Modified, pods: []corev1.Pod{
			pod("p1", now, corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 0), terminated(ContainerRestoreDB, 0)}, terminated(ContainerRestoreData, 1)),
		}},
		// Case 3 of the review: the newest pod is stuck, an older one already wiped /data.
		{name: "older pod wiped the data, newest pod stuck", want: Modified, pods: []corev1.Pod{
			pod("p2", now, corev1.PodPending, []corev1.ContainerStatus{waiting(ContainerVerify), waiting(ContainerRestoreDB)}, waiting(ContainerRestoreData)),
			pod("p1", now.Add(-time.Hour), corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 0), terminated(ContainerRestoreDB, 0)}, running(ContainerRestoreData)),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, why := RestoreModification(tc.pods); got != tc.want {
				t.Errorf("got %v (%s), want %v", got, why, tc.want)
			}
		})
	}
}

func TestPodsSettled(t *testing.T) {
	now := time.Now()
	failed := pod("p1", now, corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 0), terminated(ContainerRestoreDB, 1)})
	stale := pod("p2", now, corev1.PodFailed, []corev1.ContainerStatus{terminated(ContainerVerify, 0), running(ContainerRestoreDB)})
	job := &batchv1.Job{Status: batchv1.JobStatus{Failed: 2}}
	if PodsSettled(job, []corev1.Pod{failed}) {
		t.Error("a pod counted by the Job is missing, yet the pods were considered settled")
	}
	if PodsSettled(job, []corev1.Pod{failed, stale}) {
		t.Error("a pod still reports a running container, yet the pods were considered settled")
	}
	if !PodsSettled(job, []corev1.Pod{failed, failed}) {
		t.Error("finished pods matching the Job's counts were not considered settled")
	}
}

func restore(name string, created time.Time, phase forgejov1.RestorePhase, mod Modification) forgejov1.ForgejoRestore {
	r := forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)}}
	r.Status.Phase = phase
	if mod != Unmodified {
		meta.SetStatusCondition(&r.Status.Conditions, ModificationCondition(mod, 1, ""))
	}
	return r
}

func TestMaintenanceRestore(t *testing.T) {
	t0 := time.Now().Add(-time.Hour)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	released := restore("released", at(0), forgejov1.RestorePhaseFailed, Modified)
	released.Annotations = map[string]string{AnnotationReleaseMaintenance: valueTrue}

	cases := []struct {
		name     string
		restores []forgejov1.ForgejoRestore
		want     string
		held     bool
	}{
		{name: "none"},
		{name: "running restore", want: "r1", restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseRestoring, Unmodified)}},
		{name: "failed without changes", restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseFailed, Unmodified)}},
		{name: "failed after changing data holds", want: "r1", held: true, restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseFailed, Modified)}},
		{name: "failed with unknown outcome holds", want: "r1", held: true, restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseFailed, MaybeModified)}},
		{name: "released by annotation", restores: []forgejov1.ForgejoRestore{released}},
		{name: "a new restore takes over from a held one", want: "r2", restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseFailed, Modified),
			restore("r2", at(1), forgejov1.RestorePhaseScalingDown, Unmodified)}},
		{name: "a later successful restore releases the hold", restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseFailed, Modified),
			restore("r2", at(1), forgejov1.RestorePhaseScalingUp, Modified)}},
		{name: "an earlier successful restore does not release it", want: "r2", held: true, restores: []forgejov1.ForgejoRestore{
			restore("r1", at(0), forgejov1.RestorePhaseCompleted, Modified),
			restore("r2", at(1), forgejov1.RestorePhaseFailed, Modified)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, held := MaintenanceRestore(tc.restores)
			name := ""
			if got != nil {
				name = got.Name
			}
			if name != tc.want || held != tc.held {
				t.Errorf("got %q held=%v, want %q held=%v", name, held, tc.want, tc.held)
			}
		})
	}
}

func scheduledBackup(name string, created time.Time, phase forgejov1.BackupPhase) *forgejov1.ForgejoBackup {
	b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(created)}}
	b.Spec.Reason = forgejov1.BackupReasonScheduled
	b.Status.Phase = phase
	if phase == forgejov1.BackupPhaseCompleted {
		b.Status.CompletionTime = &metav1.Time{Time: created.Add(time.Minute)}
	}
	return b
}

func TestPruneCandidates(t *testing.T) {
	now := time.Now()
	t0 := now.Add(-24 * time.Hour)
	uploading := func(b *forgejov1.ForgejoBackup, phase forgejov1.RemoteBackupPhase) *forgejov1.ForgejoBackup {
		b.Status.Remote = &forgejov1.RemoteBackupStatus{Phase: phase, Location: "s3://backups/x/"}
		return b
	}
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	names := func(bs []*forgejov1.ForgejoBackup) []string {
		out := make([]string, 0, len(bs))
		for _, b := range bs {
			out = append(out, b.Name)
		}
		return out
	}
	// Newest first, like scheduledBackups returns them.
	cases := []struct {
		name  string
		input []*forgejov1.ForgejoBackup
		keep  int
		want  []string
	}{
		{
			name: "volume filled before retention: older failures go, newest failure stays",
			keep: 7,
			input: []*forgejov1.ForgejoBackup{
				scheduledBackup("f3", at(5), forgejov1.BackupPhaseFailed),
				scheduledBackup("f2", at(4), forgejov1.BackupPhaseFailed),
				scheduledBackup("f1", at(3), forgejov1.BackupPhaseFailed),
				scheduledBackup("c2", at(2), forgejov1.BackupPhaseCompleted),
				scheduledBackup("c1", at(1), forgejov1.BackupPhaseCompleted),
			},
			want: []string{"f2", "f1"},
		},
		{
			name: "failure older than the newest success goes",
			keep: 7,
			input: []*forgejov1.ForgejoBackup{
				scheduledBackup("c2", at(3), forgejov1.BackupPhaseCompleted),
				scheduledBackup("f1", at(2), forgejov1.BackupPhaseFailed),
				scheduledBackup("c1", at(1), forgejov1.BackupPhaseCompleted),
			},
			want: []string{"f1"},
		},
		{
			name: "completed beyond retention go, within retention stay",
			keep: 1,
			input: []*forgejov1.ForgejoBackup{
				scheduledBackup("r", at(3), forgejov1.BackupPhaseRunning),
				scheduledBackup("c2", at(2), forgejov1.BackupPhaseCompleted),
				scheduledBackup("c1", at(1), forgejov1.BackupPhaseCompleted),
			},
			want: []string{"c1"},
		},
		{
			name: "uploading beyond retention stays until its upload settles",
			keep: 1,
			input: []*forgejov1.ForgejoBackup{
				scheduledBackup("c3", at(22), forgejov1.BackupPhaseCompleted),
				uploading(scheduledBackup("c2", at(21), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupUploading),
				uploading(scheduledBackup("c1", at(20), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupPending),
			},
			want: nil,
		},
		{
			name: "uploaded or failed upload beyond retention goes",
			keep: 1,
			input: []*forgejov1.ForgejoBackup{
				scheduledBackup("c3", at(22), forgejov1.BackupPhaseCompleted),
				uploading(scheduledBackup("c2", at(21), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupUploaded),
				uploading(scheduledBackup("c1", at(20), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupFailed),
			},
			want: []string{"c2", "c1"},
		},
		{
			name: "upload pending longer than RemoteIncompleteAge goes",
			keep: 1,
			input: []*forgejov1.ForgejoBackup{
				uploading(scheduledBackup("c2", now.Add(-time.Hour), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupUploaded),
				uploading(scheduledBackup("c1", now.Add(-RemoteIncompleteAge-time.Hour), forgejov1.BackupPhaseCompleted), forgejov1.RemoteBackupPending),
			},
			want: []string{"c1"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(pruneCandidates(tc.input, tc.keep, now))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBackupPVCGrowsButNeverShrinks(t *testing.T) {
	s := testScheme(t)
	ctx := context.Background()
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fb", Namespace: "ns"}}
	claim := func(size, class string) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: testBackupsPVC, Namespace: "ns"},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To(class),
				Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}},
			},
		}
	}
	expandable := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testClassExpandable}, AllowVolumeExpansion: ptr.To(true)}
	fixed := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: testClassFixed}}

	cases := []struct {
		name, current, class, spec, want string
		grown, warned                    bool
	}{
		{name: "grows", current: testSize20Gi, class: testClassExpandable, spec: testSize200Gi, want: testSize200Gi, grown: true},
		{name: "never shrinks", current: testSize50Gi, class: testClassExpandable, spec: testSize20Gi, want: testSize50Gi},
		{name: "default size never shrinks a grown claim", current: testSize50Gi, class: testClassExpandable, spec: "", want: testSize50Gi},
		{name: "class without expansion", current: testSize20Gi, class: testClassFixed, spec: testSize200Gi, want: testSize20Gi, warned: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(s).WithObjects(claim(tc.current, tc.class), expandable, fixed).Build()
			inst := f.DeepCopy()
			if tc.spec != "" {
				inst.Spec.Backup.Storage.Size = resource.MustParse(tc.spec)
			}
			warned := false
			grown, err := GrowBackupPVC(ctx, c, inst, func(string, string) { warned = true })
			if err != nil {
				t.Fatal(err)
			}
			got := &corev1.PersistentVolumeClaim{}
			if err := c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: testBackupsPVC}, got); err != nil {
				t.Fatal(err)
			}
			size := got.Spec.Resources.Requests[corev1.ResourceStorage]
			if size.Cmp(resource.MustParse(tc.want)) != 0 || grown != tc.grown || warned != tc.warned {
				t.Errorf("size %s grown=%v warned=%v, want %s grown=%v warned=%v", size.String(), grown, warned, tc.want, tc.grown, tc.warned)
			}
		})
	}

	// A missing claim is not created by the pipeline (it is created on first use).
	c := fake.NewClientBuilder().WithScheme(s).Build()
	if grown, err := GrowBackupPVC(ctx, c, f, nil); err != nil || grown {
		t.Errorf("missing claim: grown=%v err=%v", grown, err)
	}
}

func TestBackupScriptsRemovePartialFiles(t *testing.T) {
	for name, script := range map[string]string{"dump": dumpScript, "archive": archiveScript(forgejo.LayoutForImage(forgejov1.DefaultImage))} {
		if !strings.Contains(script, removePartials) {
			t.Errorf("%s script does not remove its partial files on exit", name)
		}
	}
	for name, script := range map[string]string{"verify": verifyScript, "restore-database": restoreDBScript, "restore-data": restoreDataScript} {
		if strings.Contains(script, removePartials) {
			t.Errorf("%s script must not delete files on the read-only backup volume", name)
		}
	}
}

// A restore or a cold backup waits for the cron runner's pod too, and for one its StatefulSet
// still wants. An object of that name that the instance does not control (another instance's
// or a user's) is not its cron runner.
func TestServerPodsRunningCountsCronRunner(t *testing.T) {
	ctx := context.Background()
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: "uid-fj-cron-test"}}
	other := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Name: "fj-other", Namespace: "ns", UID: "uid-other"}}
	pod := func(name string, labels map[string]string, phase corev1.PodPhase) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace, Labels: labels}, Status: corev1.PodStatus{Phase: phase}}
	}
	cronOf := func(owner *forgejov1.Forgejo) *appsv1.StatefulSet {
		s := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f), Namespace: f.Namespace},
			Spec: appsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)}}
		if err := controllerutil.SetControllerReference(owner, s, testScheme(t)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	cron, foreign := cronOf(f), cronOf(other)
	for _, tt := range []struct {
		name string
		objs []client.Object
		want int
	}{
		{"server and cron pods", []client.Object{
			pod("fj-a", forgejo.SelectorLabels(f), corev1.PodRunning),
			pod("fj-cron-0", forgejo.CronSelectorLabels(f), corev1.PodRunning),
			pod("fj-done", forgejo.SelectorLabels(f), corev1.PodSucceeded),
			pod("fj-valkey-0", map[string]string{forgejo.LabelName: forgejo.AppName, forgejo.LabelInstance: f.Name,
				forgejo.LabelComponent: forgejo.ComponentValkey}, corev1.PodRunning),
		}, 2},
		{"cron runner still wanted, its pod gone", []client.Object{cron}, 1},
		{"cron runner wanted, with its pod", []client.Object{cron, pod("fj-cron-0", forgejo.CronSelectorLabels(f), corev1.PodPending)}, 1},
		{"cron runner at zero", []client.Object{func() *appsv1.StatefulSet {
			d := cron.DeepCopy()
			d.Spec.Replicas = ptr.To[int32](0)
			return d
		}()}, 0},
		{"not the instance's StatefulSet", []client.Object{foreign}, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(tt.objs...).Build()
			n, err := ServerPodsRunning(ctx, c, f)
			if err != nil || n != tt.want {
				t.Fatalf("ServerPodsRunning = %d, %v; want %d", n, err, tt.want)
			}
		})
	}
}
