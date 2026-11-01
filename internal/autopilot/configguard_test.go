package autopilot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// valkeyDBURL is how core renders a database of rc.ValkeyURL (for test fixtures).
func valkeyDBURL(u string, db int) string {
	base, query, _ := strings.Cut(u, "?")
	return base + "/" + strconv.Itoa(db) + "?" + query
}

func lkgSecret(f *forgejov1.Forgejo, redis bool) *corev1.Secret {
	ini := "[cache]\nADAPTER = memory\n"
	if redis {
		ini = "[cache]\nADAPTER = redis\nHOST = " + valkeyDBURL(valkeyURL(f, "pw"), 0) + "\n"
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.LKGConfigSecretName(f), Namespace: f.Namespace,
			Annotations: map[string]string{forgejo.AnnotationConfigHash: testOldHash}},
		Data: map[string][]byte{forgejo.ConfigSecretKey: []byte(ini)},
	}
}

const socketProfile = "internal-socket"

// A rejected config is rolled back only to a last known good app.ini that can run now. It is
// only mounted for an image of the same config profile: one rendered before profiles existed
// (with INTERNAL_TOKEN) still serves Forgejo 15-17, but not a Forgejo 18 image, and an 18 one
// (without the token) would crash-loop 17 and older. With HA off and Valkey gone, it must not
// be one that needs Valkey.
func TestReconcileConfigGuard(t *testing.T) {
	old := forgejo.Versions
	forgejo.Versions = old.WithVerified(18)
	t.Cleanup(func() { forgejo.Versions = old })
	for _, tt := range []struct {
		name     string
		major    string
		profile  string // of the LKG; "": rendered before profiles existed
		redis    bool   // the LKG uses Valkey
		unusable string // in the ConfigRollbackUnavailable event; "": rolled back to the LKG
	}{
		{"legacy LKG for 17", "17", "", false, ""},
		{"legacy LKG for 18", "18", "", false, "rendered for another Forgejo version"},
		{"LKG without INTERNAL_TOKEN for 17", "17", socketProfile, false, "rendered for another Forgejo version"},
		{"matching LKG for 18", "18", socketProfile, false, ""},
		{"LKG that needs Valkey, without Valkey", "17", "", true, "needs Valkey"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := testInstance()
			f.Spec.AutoPilot.SelfHealing.Enabled = ptr.To(true)
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash, RejectedConfigHash: testHash}
			lkg := lkgSecret(f, tt.redis)
			if tt.profile != "" {
				lkg.Annotations[forgejo.AnnotationConfigProfile] = tt.profile
			}
			env := newTestEnv(t, f, lkg)
			image := "codeberg.org/forgejo/forgejo:" + tt.major
			env.rc.TargetImage, env.rc.Layout = image, forgejo.LayoutForImage(image)
			env.rc.ConfigSecretName = forgejo.ConfigSecretName(f)
			if _, err := ReconcileConfigGuard(context.Background(), env.rc); err != nil {
				t.Fatal(err)
			}
			evs := env.events()
			if tt.unusable == "" {
				if env.rc.ConfigSecretName != forgejo.LKGConfigSecretName(f) || env.rc.ConfigHash != testOldHash {
					t.Fatalf("LKG not used: %s %s %q", env.rc.ConfigSecretName, env.rc.ConfigHash, evs)
				}
				return
			}
			if env.rc.ConfigSecretName != forgejo.ConfigSecretName(f) || env.rc.ConfigHash != testHash {
				t.Fatalf("switched to the LKG: %s %s", env.rc.ConfigSecretName, env.rc.ConfigHash)
			}
			if f.Status.AutoPilot.RejectedConfigHash != "" || !hasEvent(evs, "Warning ConfigRollbackUnavailable") ||
				!strings.Contains(strings.Join(evs, "\n"), tt.unusable) {
				t.Fatalf("rejection kept or no ConfigRollbackUnavailable event for %q: %q", tt.unusable, evs)
			}
		})
	}
}

// With highAvailability, an LKG rendered without it (LevelDB queues, cron tasks in every pod)
// is not rolled back to, nor one rendered with a Valkey password that Valkey no longer has.
func TestLKGWithoutHAUnusableWithHA(t *testing.T) {
	f := haInstance()
	env := newTestEnv(t, f)
	env.rc.ValkeyURL = valkeyURL(f, "new-password")
	plain := lkgSecret(f, false)
	if why, err := lkgUnusable(context.Background(), env.rc, plain); err != nil || !strings.Contains(why, "without highAvailability") {
		t.Fatalf("why = %q, err = %v", why, err)
	}
	ha := lkgSecret(f, true)
	ha.Annotations[annotationUsesValkey] = valueTrue
	ha.Annotations[annotationValkeyURL] = valkeyURLDigest(env.rc.ValkeyURL)
	ha.Data[forgejo.ConfigSecretKeyWeb] = []byte("[cron]\nENABLED = false\n")
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || why != "" {
		t.Fatalf("HA LKG: why = %q, err = %v", why, err)
	}
	// valkey-password changed after the LKG was recorded: Valkey is emptied and restarted
	// with the new password only, so the LKG's app.ini could not reach it.
	ha.Annotations[annotationValkeyURL] = valkeyURLDigest(valkeyURL(f, "old-password"))
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || !strings.Contains(why, "another Valkey password") {
		t.Fatalf("LKG with the old password: why = %q, err = %v", why, err)
	}
	delete(ha.Annotations, annotationValkeyURL)
	if why, err := lkgUnusable(context.Background(), env.rc, ha); err != nil || !strings.Contains(why, "another Valkey password") {
		t.Fatalf("LKG without a password digest: why = %q, err = %v", why, err)
	}
}

// Terminal pods left behind (evicted under node pressure in phase Failed, or Succeeded) are not
// listed: they must neither block LKG bookkeeping nor the instance-level restart, which both
// get the pods of forgejoPods.
func TestTerminalPodsDoNotBlockLastKnownGood(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true, Checks: map[string]string{testCheckDatabase: testCheckPass}}
	dep := testDeployment(f, 1)
	done := readyPod(f, "fj-done", testHash, time.Hour)
	done.Status.Phase = corev1.PodSucceeded
	env := newTestEnv(t, f, dep, configSecret(f),
		readyPod(f, "fj-live", testHash, 2*lkgSoak),
		evictedPod(f, "fj-evicted", testOldHash),
		done,
	)
	ctx := context.Background()
	pods, err := forgejoPods(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(pods))
	for _, p := range pods {
		names = append(names, p.Name)
	}
	if len(names) != 1 || names[0] != "fj-live" {
		t.Fatalf("forgejoPods = %v, want [fj-live]", names)
	}
	if err := recordLastKnownGood(ctx, env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	if ap := f.Status.AutoPilot; ap == nil || ap.LastKnownGoodConfigHash != testHash {
		t.Fatalf("LKG not recorded: %+v", f.Status.AutoPilot)
	}
	lkg := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), lkg) || lkg.Annotations[forgejo.AnnotationConfigHash] != testHash {
		t.Fatalf("LKG secret hash = %q", lkg.Annotations[forgejo.AnnotationConfigHash])
	}
}

// Fewer listed pods than desired replicas (cache lag) must not pass the soak check vacuously.
func TestLastKnownGoodNeedsEveryReplicaListed(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true, Checks: map[string]string{testCheckDatabase: testCheckPass}}
	dep := testDeployment(f, 2)
	env := newTestEnv(t, f, dep, configSecret(f), readyPod(f, "fj-a", testHash, 2*lkgSoak))
	pods, err := serverPods(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if err := recordLastKnownGood(context.Background(), env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	if ap := f.Status.AutoPilot; ap != nil && ap.LastKnownGoodConfigHash != "" {
		t.Fatalf("LKG recorded with 1 of 2 pods listed: %q", ap.LastKnownGoodConfigHash)
	}
}

// The LKG records what its app.ini was rendered for: the config profile, and that it uses
// Valkey with a digest of the Valkey URL that does not reveal it.
func TestLastKnownGoodAnnotations(t *testing.T) {
	f := testInstance()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
	dep := testDeployment(f, 1)
	src := configSecret(f)
	src.Annotations[forgejo.AnnotationConfigProfile] = socketProfile
	env := newTestEnv(t, f, dep, src, readyPod(f, "fj-a", testHash, time.Hour))
	env.rc.ValkeyURL = "redis://:pw@fj-valkey.ns.svc.cluster.local:6379"
	pods, _ := serverPods(context.Background(), env.rc)
	if err := recordLastKnownGood(context.Background(), env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	lkg := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), lkg) || lkg.Annotations[forgejo.AnnotationConfigProfile] != socketProfile {
		t.Fatalf("LKG profile not recorded: %+v", lkg.Annotations)
	}
	if lkg.Annotations[annotationUsesValkey] != valueTrue || lkg.Annotations[annotationValkeyURL] != valkeyURLDigest(env.rc.ValkeyURL) {
		t.Fatalf("LKG not marked as using Valkey with this password: %+v", lkg.Annotations)
	}
	if strings.Contains(fmt.Sprint(lkg.Annotations), "pw@") {
		t.Fatalf("LKG annotations reveal the Valkey URL: %+v", lkg.Annotations)
	}

	// An LKG recorded without the digest gets it on the next pass, although the config is
	// the same.
	delete(lkg.Annotations, annotationValkeyURL)
	if err := env.rc.Client.Update(context.Background(), lkg); err != nil {
		t.Fatal(err)
	}
	if err := recordLastKnownGood(context.Background(), env.rc, dep, nil, pods, false); err != nil {
		t.Fatal(err)
	}
	if !found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), lkg) || lkg.Annotations[annotationValkeyURL] != valkeyURLDigest(env.rc.ValkeyURL) {
		t.Fatalf("LKG digest not added: %+v", lkg.Annotations)
	}
}

// The last known good app.ini also covers the cron runner: it is recorded only once that pod
// has run it healthy too. A cron runner pod that has not started any container for
// cronStartTimeout no longer holds it back, and the server pods decide alone; one that
// started (and fails) or is merely new still does.
func TestLastKnownGoodWithCronRunner(t *testing.T) {
	ctx := context.Background()
	crashing := func(f *forgejov1.Forgejo) *corev1.Pod {
		p := pendingCronPod(f, time.Hour)
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: serverContainerName, RestartCount: 4,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}
		return p
	}
	multiAttach := func(f *forgejov1.Forgejo) *corev1.Pod {
		p := pendingCronPod(f, time.Hour)
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "ssh-host-keys",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}}}
		return p
	}
	for _, tt := range []struct {
		name      string
		rolledOut bool
		pod       func(*forgejov1.Forgejo) *corev1.Pod // the cron runner's pod; nil: none
		event     string                               // in the LastKnownGoodConfig event; "": nothing recorded
	}{
		{"cron runner not available", false, nil, ""},
		{"cron runner pod not soaked", true, func(f *forgejov1.Forgejo) *corev1.Pod { return cronPod(f, "fj-cron-0", 10*time.Second) }, ""},
		{"cron runner soaked", true, func(f *forgejov1.Forgejo) *corev1.Pod { return cronPod(f, "fj-cron-0", time.Hour) }, "ran healthy; stored"},
		{"unschedulable for an hour", false, func(f *forgejov1.Forgejo) *corev1.Pod { return pendingCronPod(f, time.Hour) }, "by the server pods"},
		{"waiting for its volume for an hour", false, multiAttach, "by the server pods"},
		{"unschedulable for a minute", false, func(f *forgejov1.Forgejo) *corev1.Pod { return pendingCronPod(f, time.Minute) }, ""},
		{"crash-looping", false, crashing, ""},
		{"of an older template", false, func(f *forgejov1.Forgejo) *corev1.Pod {
			p := pendingCronPod(f, time.Hour)
			p.Annotations[forgejo.AnnotationConfigHash] = testOldHash
			return p
		}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
			dep := testDeployment(f, 1)
			objs := []client.Object{dep, cronRunnerFor(f, tt.rolledOut), configSecret(f), readyPod(f, "fj-a", testHash, time.Hour)}
			if tt.pod != nil {
				objs = append(objs, tt.pod(f))
			}
			env := newTestEnv(t, f, objs...)
			pods, err := forgejoPods(ctx, env.rc)
			if err != nil {
				t.Fatal(err)
			}
			cron, err := cronRunner(ctx, env.rc)
			if err != nil || cron == nil {
				t.Fatalf("cronRunner = %v, %v", cron, err)
			}
			if err := recordLastKnownGood(ctx, env.rc, dep, cron, pods, false); err != nil {
				t.Fatal(err)
			}
			if got := found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), &corev1.Secret{}); got != (tt.event != "") {
				t.Fatalf("LKG recorded = %v, want %v", got, tt.event != "")
			}
			if evs := env.events(); tt.event != "" && !strings.Contains(strings.Join(evs, "\n"), tt.event) {
				t.Fatalf("events %v, want %q", evs, tt.event)
			}
		})
	}
}

// --- config rollback detection ---

func TestPodFailureCountsOnlyCurrentFailures(t *testing.T) {
	f := testInstance()
	tenMinAgo := metav1.NewTime(time.Now().Add(-10 * time.Minute))

	// Init container failed during a database restart, then completed; the pod is Ready.
	p := readyPod(f, "fj-a", testHash, time.Minute)
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name: "bootstrap", RestartCount: 4,
		State:                corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, FinishedAt: tenMinAgo}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: tenMinAgo}},
	}}
	if got := podFailure(p, time.Time{}); got != "" {
		t.Fatalf("Ready pod with a completed init container failed: %s", got)
	}

	// Crash loop whose last exit was before the database became ready again.
	q := readyPod(f, "fj-b", testHash, time.Minute)
	q.Status.Conditions[0].Status = corev1.ConditionFalse
	q.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: serverContainerName, RestartCount: 3,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, FinishedAt: tenMinAgo}},
	}
	if got := podFailure(q, time.Now().Add(-5*time.Minute)); got != "" {
		t.Fatalf("restarts from before the database recovered counted: %s", got)
	}
	if got := podFailure(q, time.Now().Add(-20*time.Minute)); got == "" {
		t.Fatal("crash loop after the database recovered not counted")
	}
}

// --- object storage (spec.objectStorage) ---

// Forgejo does not start without its bucket: a crash loop while the bucket is unusable (e.g.
// rotated credentials that are wrong, which are not in app.ini) is not blamed on app.ini, and
// pods are not replaced meanwhile.
func TestConfigRollbackWaitsForObjectStorage(t *testing.T) {
	ctx := context.Background()
	hourAgo := metav1.NewTime(time.Now().Add(-time.Hour))
	for _, tc := range []struct {
		name        string
		ready       metav1.ConditionStatus
		since       metav1.Time
		unavailable bool
		wantRollbk  bool
	}{
		{"bucket unusable", metav1.ConditionFalse, hourAgo, true, false},
		{"bucket usable again after the crashes", metav1.ConditionTrue, metav1.Now(), false, false},
		{"bucket usable all along", metav1.ConditionTrue, hourAgo, false, true},
		// Only the operator cannot reach the bucket (UnreachableFromOperator): nothing is held.
		{"bucket unreachable from the operator", metav1.ConditionFalse, metav1.Now(), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.Spec.AutoPilot.SelfHealing.Enabled = ptr.To(true)
			f.Spec.ObjectStorage = &forgejov1.ObjectStorageSpec{S3: forgejov1.S3Connection{Endpoint: "http://s3:9000", Bucket: "forgejo"}}
			f.Status.AutoPilot = &forgejov1.AutoPilotStatus{LastKnownGoodConfigHash: testOldHash}
			f.Status.Conditions = []metav1.Condition{
				{Type: forgejov1.ConditionDatabaseReady, Status: metav1.ConditionTrue, Reason: "ClusterHealthy", LastTransitionTime: hourAgo},
				{Type: forgejov1.ConditionObjectStorageReady, Status: tc.ready, Reason: "X", LastTransitionTime: tc.since},
			}
			p := readyPod(f, "fj-a", testHash, time.Minute)
			p.Status.Conditions[0].Status = corev1.ConditionFalse
			p.Status.ContainerStatuses[0] = corev1.ContainerStatus{
				Name: serverContainerName, RestartCount: 3,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoopBackOff}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1,
					FinishedAt: metav1.NewTime(time.Now().Add(-30 * time.Second))}},
			}
			env := newTestEnv(t, f, lkgSecret(f, false), p)
			env.rc.ObjectStorageUnavailable = tc.unavailable
			if replacementsHeld(env.rc) != tc.unavailable {
				t.Fatalf("replacementsHeld = %v", !tc.unavailable)
			}
			pods, _ := serverPods(ctx, env.rc)
			if err := rollbackFailingConfig(ctx, env.rc, stateFor(f), pods); err != nil {
				t.Fatal(err)
			}
			if got := f.Status.AutoPilot.RejectedConfigHash == testHash; got != tc.wantRollbk {
				t.Fatalf("rolled back = %v, want %v (events %q)", got, tc.wantRollbk, env.events())
			}
		})
	}
}
