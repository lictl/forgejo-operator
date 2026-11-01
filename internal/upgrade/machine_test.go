package upgrade

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	testPreBackup       = "pre"
	testRollbackRestore = "rollback"
)

const (
	imgA = "codeberg.org/forgejo/forgejo:15"
	imgB = "codeberg.org/forgejo/forgejo:16"
)

func ago(d time.Duration) *metav1.Time {
	return &metav1.Time{Time: time.Now().Add(-d).Truncate(time.Second)}
}

// fixture is an instance that runs imgA with spec.image imgB and the given upgrade status.
func fixture(u *forgejov1.UpgradeStatus) *forgejov1.Forgejo {
	return &forgejov1.Forgejo{
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", UID: "uid"},
		Spec:       forgejov1.ForgejoSpec{Image: imgB},
		Status:     forgejov1.ForgejoStatus{Image: imgA, Upgrade: u},
	}
}

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

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
}

func reconcile(t *testing.T, f *forgejov1.Forgejo, objs ...client.Object) (*forgejo.ReconcileContext, client.Client) {
	t.Helper()
	c := newClient(t, objs...)
	return reconcileWith(t, c, f), c
}

func reconcileWith(t *testing.T, c client.Client, f *forgejov1.Forgejo) *forgejo.ReconcileContext {
	t.Helper()
	rc := &forgejo.ReconcileContext{Client: c, APIReader: c, Scheme: c.Scheme(), Instance: f, TargetImage: f.Status.Image}
	if _, err := Reconcile(context.Background(), rc); err != nil {
		t.Fatal(err)
	}
	return rc
}

func restore(name string, phase forgejov1.RestorePhase, created *metav1.Time) *forgejov1.ForgejoRestore {
	return &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", CreationTimestamp: *created},
		Spec:       forgejov1.ForgejoRestoreSpec{ForgejoRef: forgejov1.ForgejoRef{Name: "fj"}, BackupName: testPreBackup},
		Status:     forgejov1.ForgejoRestoreStatus{Phase: phase},
	}
}

func serverPod(f *forgejov1.Forgejo, name, image string, created *metav1.Time, init corev1.ContainerState, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: forgejo.SelectorLabels(f), CreationTimestamp: *created},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: serverContainer, Image: image}}},
		Status: corev1.PodStatus{
			Phase:                 corev1.PodPending,
			InitContainerStatuses: []corev1.ContainerStatus{{Name: migrateContainer, State: init, RestartCount: restarts}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: serverContainer,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}},
		},
	}
}

// serverPodRunning is a server pod of imgA that runs.
func serverPodRunning(f *forgejov1.Forgejo, name string) *corev1.Pod {
	p := serverPod(f, name, imgA, ago(time.Hour), corev1.ContainerState{}, 0)
	p.Status.Phase = corev1.PodRunning
	return p
}

// deployment is core's server Deployment with replicas replicas of image, available of them
// available.
func deployment(image string, replicas, available int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fj", Namespace: "ns", Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To(replicas), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: serverContainer, Image: image}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: replicas, UpdatedReplicas: replicas, AvailableReplicas: available},
	}
}

// A rollback whose restore failed keeps Forgejo stopped (a later restore lifts that, see
// TestFailedRollbackRestoreLiftedByLaterRestore). Setting spec.image back to the old image starts
// it without a restore.
func TestFailedRollbackRestoreKeepsForgejoStopped(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingBack, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, RestoreName: testRollbackRestore,
		StartTime: ago(time.Hour), RolloutStartTime: ago(50 * time.Minute),
	})
	rc, c := reconcile(t, f, restore("rollback", forgejov1.RestorePhaseFailed, ago(40*time.Minute)))
	u := f.Status.Upgrade
	if u.Phase != forgejov1.UpgradePhaseFailed || !u.RollbackRestoreFailed {
		t.Fatalf("phase %s, rollbackRestoreFailed %v", u.Phase, u.RollbackRestoreFailed)
	}
	if rc.TargetImage != imgA || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 || rc.Upgrading {
		t.Fatalf("target %s, override %v, upgrading %v", rc.TargetImage, rc.ReplicasOverride, rc.Upgrading)
	}

	// Still stopped in the idle passes that follow.
	if rc = reconcileWith(t, c, f); rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 || rc.TargetImage != imgA {
		t.Fatalf("idle: override %v, target %s", rc.ReplicasOverride, rc.TargetImage)
	}
	f.Spec.Image = imgA
	if rc = reconcileWith(t, c, f); rc.ReplicasOverride != nil || rc.TargetImage != imgA {
		t.Fatalf("spec.image set back: override %v, target %s", rc.ReplicasOverride, rc.TargetImage)
	}
}

func TestKeptFailureHoldsSingleReplica(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingOut, FromImage: imgA, ToImage: imgB,
		StartTime: ago(time.Hour), RolloutStartTime: ago(20 * time.Minute),
	})
	f.Spec.Replicas = ptr.To[int32](3)
	rc, c := reconcile(t, f)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseFailed || rc.Upgrading {
		t.Fatalf("phase %s, upgrading %v", f.Status.Upgrade.Phase, rc.Upgrading)
	}
	if rc.TargetImage != imgB || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 1 || !forgejo.UpgradeHoldsWorkload(f) {
		t.Fatalf("target %s, override %v", rc.TargetImage, rc.ReplicasOverride)
	}
	if rc = reconcileWith(t, c, f); rc.ReplicasOverride == nil || *rc.ReplicasOverride != 1 {
		t.Fatalf("idle override %v", rc.ReplicasOverride)
	}

	// The kept image became available: the hold ends.
	f.Status.Image = imgB
	if rc = reconcileWith(t, c, f); rc.ReplicasOverride != nil || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseSucceeded {
		t.Fatalf("override %v, phase %s", rc.ReplicasOverride, f.Status.Upgrade.Phase)
	}
}

// The live Deployment decides whether an upgrade is verified: every replica of the new image
// available. status.image alone is not enough. A Deployment that rolled out wins over the timeout
// and the restart counts, also while status.image still names the old image (core had no pass to
// record it, for example while the operator was away), and status.image follows.
func TestRolloutVerifiedByTheDeployment(t *testing.T) {
	stale := deployment(imgB, 1, 1)
	stale.Status.ObservedGeneration = 1
	// restarted is the new pod after it crashed a few times and then became available.
	restarted := serverPod(fixture(nil), "new", imgB, ago(time.Hour), corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}, 0)
	restarted.Status.Phase = corev1.PodRunning
	restarted.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: serverContainer, Ready: true, RestartCount: 5,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	for _, tt := range []struct {
		name string
		// statusImage is status.image; rolledOutAgo is when the new image was deployed (the
		// timeout is 15m).
		statusImage  string
		rolledOutAgo time.Duration
		objs         []client.Object
		want         forgejov1.UpgradePhase
	}{
		{"no Deployment", imgB, 5 * time.Minute, nil, forgejov1.UpgradePhaseVerifying},
		{"not available", imgB, 5 * time.Minute, []client.Object{deployment(imgB, 1, 0)}, forgejov1.UpgradePhaseVerifying},
		{"old image", imgB, 5 * time.Minute, []client.Object{deployment(imgA, 1, 1)}, forgejov1.UpgradePhaseVerifying},
		{"generation not observed", imgB, 5 * time.Minute, []client.Object{stale}, forgejov1.UpgradePhaseVerifying},
		{"rolled out", imgA, 5 * time.Minute, []client.Object{deployment(imgB, 1, 1)}, forgejov1.UpgradePhaseSucceeded},
		{"rolled out past the timeout after restarts", imgA, time.Hour, []client.Object{deployment(imgB, 1, 1), restarted},
			forgejov1.UpgradePhaseSucceeded},
		{"partly available past the timeout", imgA, time.Hour, []client.Object{deployment(imgB, 2, 1)}, forgejov1.UpgradePhaseRollingBack},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := fixture(&forgejov1.UpgradeStatus{
				Phase: forgejov1.UpgradePhaseVerifying, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup,
				StartTime: ago(2 * time.Hour), RolloutStartTime: ago(tt.rolledOutAgo),
			})
			f.Status.Image = tt.statusImage
			rc, c := reconcile(t, f, tt.objs...)
			u := f.Status.Upgrade
			if u.Phase != tt.want {
				t.Fatalf("phase %s (want %s): %s", u.Phase, tt.want, u.Message)
			}
			if tt.want != forgejov1.UpgradePhaseSucceeded {
				if f.Status.Image != tt.statusImage {
					t.Fatalf("status.image %s", f.Status.Image)
				}
				return
			}
			if u.RestoreName != "" || restores(t, c) != 0 || rc.TargetImage != imgB || rc.Upgrading || f.Status.Image != imgB {
				t.Fatalf("restore %q, target %s, upgrading %v, status.image %s", u.RestoreName, rc.TargetImage, rc.Upgrading, f.Status.Image)
			}
			// The next pass stays idle: status.image no longer lags behind the finished upgrade.
			if rc = reconcileWith(t, c, f); f.Status.Upgrade.Phase != forgejov1.UpgradePhaseSucceeded || rc.Upgrading {
				t.Fatalf("next pass: phase %s, upgrading %v", f.Status.Upgrade.Phase, rc.Upgrading)
			}
		})
	}
}

func TestBackingUpGivesUpOnPodsThatDoNotStop(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, StartTime: ago(16 * time.Minute),
	})
	rc, _ := reconcile(t, f, serverPodRunning(f, "stuck"))
	u := f.Status.Upgrade
	if u.Phase != forgejov1.UpgradePhaseFailed || u.RolloutStartTime != nil || rc.ReplicasOverride != nil || rc.TargetImage != imgA {
		t.Fatalf("phase %s, override %v, target %s", u.Phase, rc.ReplicasOverride, rc.TargetImage)
	}
}

func TestFreshUpgradeDeploysWithRolloutStartTime(t *testing.T) {
	f := fixture(nil)
	f.Spec.Upgrade.BackupBeforeUpgrade = ptr.To(false)
	rc, c := reconcile(t, f)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhasePending || rc.TargetImage != imgA {
		t.Fatalf("Pending deploys %s", rc.TargetImage)
	}
	rc = reconcileWith(t, c, f)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingOut || u.RolloutStartTime == nil || rc.TargetImage != imgB {
		t.Fatalf("phase %s, target %s", u.Phase, rc.TargetImage)
	}
}

func TestCancelBeforeDeploy(t *testing.T) {
	pending := func() *forgejov1.Forgejo {
		f := fixture(&forgejov1.UpgradeStatus{
			Phase: forgejov1.UpgradePhaseBackingUp, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup, StartTime: ago(time.Minute),
		})
		f.Spec.Image = imgA
		return f
	}
	bk := &forgejov1.ForgejoBackup{
		ObjectMeta: metav1.ObjectMeta{Name: testPreBackup, Namespace: "ns"},
		Status:     forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhaseRunning},
	}

	// A running backup finishes first, with Forgejo stopped.
	f := pending()
	rc, c := reconcile(t, f, deployment(imgA, 1, 1), bk)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseBackingUp || rc.ReplicasOverride == nil || *rc.ReplicasOverride != 0 {
		t.Fatalf("phase %s, override %v", f.Status.Upgrade.Phase, rc.ReplicasOverride)
	}
	bk.Status.Phase = forgejov1.BackupPhaseCompleted
	if err := c.Update(context.Background(), bk); err != nil {
		t.Fatal(err)
	}
	rc = reconcileWith(t, c, f)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseCancelled || u.FailedImage != "" || rc.TargetImage != imgA || rc.ReplicasOverride != nil {
		t.Fatalf("phase %s, failedImage %q, target %s", u.Phase, u.FailedImage, rc.TargetImage)
	}
	if rc = reconcileWith(t, c, f); rc.TargetImage != imgA || f.Status.Upgrade.Phase != forgejov1.UpgradePhaseCancelled {
		t.Fatalf("idle after cancel: target %s", rc.TargetImage)
	}

	// The rollout already started in a reconcile whose status write was lost: not cancelled.
	f = pending()
	bk.Status.Phase = forgejov1.BackupPhaseCompleted
	bk.ResourceVersion = ""
	rc, _ = reconcile(t, f, deployment(imgB, 1, 0), bk)
	if f.Status.Upgrade.Phase != forgejov1.UpgradePhaseRollingOut || rc.TargetImage != imgB {
		t.Fatalf("phase %s, target %s", f.Status.Upgrade.Phase, rc.TargetImage)
	}
}

func TestRetryKeepsMigratingPodAndDeletesFailingOne(t *testing.T) {
	f := fixture(&forgejov1.UpgradeStatus{
		Phase: forgejov1.UpgradePhaseRollingOut, FromImage: imgA, ToImage: imgB, Retry: true,
		StartTime: ago(time.Minute), RolloutStartTime: ago(time.Minute),
	})
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	crashing := corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: waitingCrashLoopBackOff}}
	migrating := serverPod(f, "migrating", imgB, ago(time.Hour), running, 5)
	failed := serverPod(f, "failed", imgB, ago(time.Hour), crashing, 5)
	_, c := reconcile(t, f, migrating, failed)
	if u := f.Status.Upgrade; u.Phase != forgejov1.UpgradePhaseRollingOut {
		t.Fatalf("phase %s: %s", u.Phase, u.FailureReason)
	}
	pods := &corev1.PodList{}
	if err := c.List(context.Background(), pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "migrating" {
		t.Fatalf("pods left: %v", pods.Items)
	}
}

// After a failed rollback restore, a restore created later lifts the hold for good once it has
// restored (ScalingUp, when it waits for Forgejo to come back) the database, and the data volume
// when its backup has a data archive.
func TestFailedRollbackRestoreLiftedByLaterRestore(t *testing.T) {
	cases := []struct {
		name          string
		phase         forgejov1.RestorePhase
		db, data      *bool
		backupHasData bool
		backupGone    bool
		lifts         bool
	}{
		{name: "restore still running", phase: forgejov1.RestorePhaseRestoring, backupHasData: true},
		{name: "restore scaling up", phase: forgejov1.RestorePhaseScalingUp, backupHasData: true, lifts: true},
		{name: "database and data", phase: forgejov1.RestorePhaseCompleted, backupHasData: true, lifts: true},
		{name: "data only", phase: forgejov1.RestorePhaseCompleted, db: ptr.To(false), backupHasData: true},
		{name: "database only from a backup with data", phase: forgejov1.RestorePhaseCompleted, data: ptr.To(false), backupHasData: true},
		{name: "database only from a backup without data", phase: forgejov1.RestorePhaseCompleted, data: ptr.To(false), lifts: true},
		{name: "database only from a deleted backup", phase: forgejov1.RestorePhaseCompleted, data: ptr.To(false), backupGone: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := fixture(&forgejov1.UpgradeStatus{
				Phase: forgejov1.UpgradePhaseFailed, FromImage: imgA, ToImage: imgB, BackupName: testPreBackup,
				RestoreName: testRollbackRestore, StartTime: ago(time.Hour), RolloutStartTime: ago(50 * time.Minute),
				CompletionTime: ago(10 * time.Minute), FailedImage: imgB, RollbackRestoreFailed: true,
			})
			r := restore("manual", tc.phase, ago(time.Minute))
			r.Spec.BackupName = "later"
			r.Spec.RestoreDatabase, r.Spec.RestoreData = tc.db, tc.data
			objs := []client.Object{r}
			if !tc.backupGone {
				objs = append(objs, &forgejov1.ForgejoBackup{
					ObjectMeta: metav1.ObjectMeta{Name: "later", Namespace: "ns"},
					Spec:       forgejov1.ForgejoBackupSpec{ForgejoRef: forgejov1.ForgejoRef{Name: "fj"}},
					Status:     forgejov1.ForgejoBackupStatus{Phase: forgejov1.BackupPhaseCompleted, IncludesData: tc.backupHasData},
				})
			}
			rc, _ := reconcile(t, f, objs...)
			stopped := rc.ReplicasOverride != nil && *rc.ReplicasOverride == 0
			if stopped == tc.lifts || f.Status.Upgrade.RollbackRestoreFailed == tc.lifts || rc.TargetImage != imgA {
				t.Fatalf("want lifted %v: stopped %v, rollbackRestoreFailed %v, target %s", tc.lifts, stopped,
					f.Status.Upgrade.RollbackRestoreFailed, rc.TargetImage)
			}
		})
	}
}
