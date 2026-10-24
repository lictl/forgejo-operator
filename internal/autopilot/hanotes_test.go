package autopilot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// expireSentinelAnswers makes the next ReportValkey ask the Sentinels again.
func expireSentinelAnswers(f *forgejov1.Forgejo) {
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sentinels.at = time.Now().Add(-time.Hour)
}

// ageSentinelAnswers moves the last query d into the past.
func ageSentinelAnswers(f *forgejov1.Forgejo, d time.Duration) {
	st := stateFor(f)
	st.mu.Lock()
	defer st.mu.Unlock()
	st.sentinels.at = st.sentinels.at.Add(-d)
}

// countDials counts the Sentinel addresses the operator resolves, one per pod and query.
func countDials(t *testing.T) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	inner := sentinelAddr
	sentinelAddr = func(p *corev1.Pod) string {
		mu.Lock()
		n++
		mu.Unlock()
		return inner(p)
	}
	t.Cleanup(func() { sentinelAddr = inner })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

// --- valkey-status asks the Sentinels at most every interval ---

func TestReportValkeyReusesSentinelAnswers(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	pods := []*corev1.Pod{valkeyPod(f, 0, true), valkeyPod(f, 1, true), valkeyPod(f, 2, true)}
	env := newTestEnv(t, f, valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword), pods[0], pods[1], pods[2])
	replicas := [][]string{replicaEntry(host(f, 1), "ok", "slave"), replicaEntry(host(f, 2), "ok", "slave")}
	sentinels := map[string]*fakeSentinel{}
	for i := range 3 {
		sentinels[fmt.Sprintf("10.0.0.%d", i+1)] = newFakeSentinel(t, host(f, 0), replicas...)
	}
	withSentinels(t, sentinels)
	conns := func() int {
		n := 0
		for _, s := range sentinels {
			n += s.connections()
		}
		return n
	}

	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	first := f.Status.HighAvailability.Valkey.LastCheckTime
	if n := conns(); n != 3 {
		t.Fatalf("%d connections on the first look, want 3", n)
	}
	// Reconciles in between (a rollout's status changes) reuse the answers.
	for range 5 {
		if _, err := ReportValkey(ctx, env.rc); err != nil {
			t.Fatal(err)
		}
	}
	if n := conns(); n != 3 {
		t.Fatalf("%d connections after 5 more reconciles within %s, want 3", n, sentinelQueryInterval)
	}
	if got := f.Status.HighAvailability.Valkey.LastCheckTime; !got.Equal(first) {
		t.Fatalf("lastCheckTime moved to %v without a query (was %v)", got, first)
	}

	// A pod that changes (here: its readiness) is asked about at once, and so is everything
	// after the interval.
	pods[2].Status.Conditions[0].Status = corev1.ConditionFalse
	if err := env.rc.Client.Status().Update(ctx, pods[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := conns(); n != 6 {
		t.Fatalf("%d connections after a pod changed, want 6", n)
	}
	ageSentinelAnswers(f, sentinelQueryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := conns(); n != 9 {
		t.Fatalf("%d connections after %s, want 9", n, sentinelQueryInterval)
	}
}

// Sentinels the operator cannot reach while the pods are Ready are asked again only every
// sentinelRetryInterval, and the instance is not requeued every 10 s for it.
func TestReportValkeyBacksOffWhileSentinelsUnreachable(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	env := newTestEnv(t, f, valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword),
		valkeyPod(f, 0, true), valkeyPod(f, 1, true), valkeyPod(f, 2, true))
	withSentinels(t, map[string]*fakeSentinel{}) // every dial is refused
	dials := countDials(t)

	res, err := ReportValkey(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionValkeyReady); c == nil || c.Reason != ReasonValkeyUnreachable {
		t.Fatalf("ValkeyReady %+v", c)
	}
	if res.RequeueAfter <= valkeyStatusRequeueHealthy || res.RequeueAfter > sentinelRetryInterval {
		t.Fatalf("requeue %v, want up to %v", res.RequeueAfter, sentinelRetryInterval)
	}
	if n := dials(); n != 3 {
		t.Fatalf("%d dials, want 3", n)
	}
	ageSentinelAnswers(f, 2*sentinelQueryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := dials(); n != 3 {
		t.Fatalf("%d dials within %s of an unanswered query, want 3", n, sentinelRetryInterval)
	}
	ageSentinelAnswers(f, sentinelRetryInterval)
	if _, err := ReportValkey(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if n := dials(); n != 6 {
		t.Fatalf("%d dials after %s, want 6", n, sentinelRetryInterval)
	}
}

// --- a new replication group is no failover ---

func uidPod(f *forgejov1.Forgejo, i int, uid string) *corev1.Pod {
	p := valkeyPod(f, i, true)
	p.UID = types.UID(uid)
	return p
}

func TestValkeyNewGroupIsNoFailover(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	sentinels := map[string]*fakeSentinel{}
	for i := range 3 {
		sentinels[fmt.Sprintf("10.0.0.%d", i+1)] = newFakeSentinel(t, host(f, 1),
			replicaEntry(host(f, 0), "ok", "slave"), replicaEntry(host(f, 2), "ok", "slave"))
	}
	withSentinels(t, sentinels)
	setMaster := func(i int) {
		var reps [][]string
		for j := range 3 {
			if j != i {
				reps = append(reps, replicaEntry(host(f, j), "ok", "slave"))
			}
		}
		for _, s := range sentinels {
			s.set(host(f, i), reps...)
		}
	}
	report := func(env testEnv) []string {
		t.Helper()
		expireSentinelAnswers(f)
		if _, err := ReportValkey(ctx, env.rc); err != nil {
			t.Fatal(err)
		}
		return env.events()
	}
	podsOf := func(gen string) []client.Object {
		return []client.Object{uidPod(f, 0, gen+"0"), uidPod(f, 1, gen+"1"), uidPod(f, 2, gen+"2")}
	}

	// The group runs with pod 1 as the primary.
	objs := append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("a")...)
	env := newTestEnv(t, f, objs...)
	report(env)
	if got := f.Status.HighAvailability.Valkey.Master; got != testValkeyPod1 {
		t.Fatalf("master %q", got)
	}

	// Every pod is replaced (Valkey emptied after a restore, the StatefulSet recreated): the new
	// group starts with pod 0 as its primary. Not a failover.
	objs = append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("b")...)
	env.rc.Client = newTestEnv(t, f, objs...).rc.Client
	setMaster(0)
	if evs := report(env); hasEvent(evs, "Warning ValkeyFailedOver") || f.Status.HighAvailability.Valkey.LastFailoverTime != nil {
		t.Fatalf("a new replication group reported as a failover: %v", evs)
	}

	// Within that group, a move of the primary is one.
	setMaster(2)
	if evs := report(env); !hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("no ValkeyFailedOver for a failover within the group: %v", evs)
	}

	// highAvailability off, then on again in the same operator process: the recreated Valkey
	// is a new group, whichever pod led the old one.
	f.Spec.HighAvailability.Enabled = false
	report(env)
	f.Spec.HighAvailability.Enabled = true
	objs = append([]client.Object{valkeyStatefulSet(f), secretsWithValkeyPassword(f, testValkeyPassword)}, podsOf("c")...)
	env.rc.Client = newTestEnv(t, f, objs...).rc.Client
	setMaster(0)
	if evs := report(env); hasEvent(evs, "Warning ValkeyFailedOver") {
		t.Fatalf("turning HA off and on reported as a failover: %v", evs)
	}
}

func TestFailedOver(t *testing.T) {
	f := haInstance()
	pods := []corev1.Pod{*uidPod(f, 0, "a0"), *uidPod(f, 1, "a1"), *uidPod(f, 2, "b2")}
	group := map[string]types.UID{testValkeyPod0: "a0", testValkeyPod1: "a1", testValkeyPod2: "a2"}
	for _, tt := range []struct {
		name         string
		prev, master string
		group        map[string]types.UID
		want         bool
	}{
		{"first look", "", testValkeyPod0, group, false},
		{"unchanged", testValkeyPod0, testValkeyPod0, group, false},
		{"to a member", testValkeyPod0, testValkeyPod1, group, true},
		{"to a pod that replaced a member", testValkeyPod0, testValkeyPod2, group, false},
		{"to a pod that was not there", testValkeyPod0, testValkeyPod1, map[string]types.UID{testValkeyPod0: "a0"}, false},
		{"after an operator restart", testValkeyPod0, testValkeyPod1, nil, true},
	} {
		if got := failedOver(tt.prev, tt.group, tt.master, pods); got != tt.want {
			t.Errorf("%s: failedOver = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// --- Valkey is emptied after a restore and when its password changes ---

// valkeyResetEnv is an HA instance whose Valkey StatefulSet the HA step has created, with
// three running pods.
func valkeyResetEnv(t *testing.T) (testEnv, *forgejov1.Forgejo) {
	t.Helper()
	f := haInstance()
	env := newTestEnv(t, f, secretsWithValkeyPassword(f, testValkeyPassword))
	if _, err := ReconcileHA(context.Background(), env.rc); err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	for i := range 3 {
		p := valkeyPod(f, i, true)
		p.OwnerReferences[0].UID = sts.UID
		if err := env.rc.Client.Create(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	setValkeyStatus(t, env, f, 3, 3)
	env.events()
	return env, f
}

func valkeySTS(t *testing.T, env testEnv, f *forgejov1.Forgejo) *appsv1.StatefulSet {
	t.Helper()
	sts := &appsv1.StatefulSet{}
	if !found(t, env.rc.Client, f, forgejo.ValkeyName(f), sts) {
		t.Fatal("no Valkey StatefulSet")
	}
	return sts
}

func setValkeyStatus(t *testing.T, env testEnv, f *forgejov1.Forgejo, replicas, ready int32) {
	t.Helper()
	sts := valkeySTS(t, env, f)
	sts.Status.ObservedGeneration, sts.Status.Replicas, sts.Status.ReadyReplicas = sts.Generation, replicas, ready
	if err := env.rc.Client.Status().Update(context.Background(), sts); err != nil {
		t.Fatal(err)
	}
}

func deleteValkeyPods(t *testing.T, env testEnv, f *forgejov1.Forgejo) {
	t.Helper()
	if err := env.rc.Client.DeleteAllOf(context.Background(), &corev1.Pod{}, client.InNamespace(f.Namespace),
		client.MatchingLabels(valkeySelector(f))); err != nil {
		t.Fatal(err)
	}
}

func scalingUpRestore(name, uid string) *forgejov1.ForgejoRestore {
	rs := &forgejov1.ForgejoRestore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", UID: types.UID(uid)}}
	rs.Status.Phase = forgejov1.RestorePhaseScalingUp
	return rs
}

func TestValkeyEmptiedAfterRestore(t *testing.T) {
	ctx := context.Background()
	env, f := valkeyResetEnv(t)
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r1", "uid-r1")

	// The restore reached ScalingUp: Valkey goes to zero before any Forgejo pod starts, and the
	// steps that roll Forgejo wait.
	res, err := ReconcileHA(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	if !res.Stop || *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetRestore+"uid-r1" {
		t.Fatalf("stop %v, replicas %d, annotations %v", res.Stop, *sts.Spec.Replicas, sts.Annotations)
	}
	if evs := env.events(); !hasEvent(evs, "Normal ValkeyReset") || !strings.Contains(strings.Join(evs, "\n"), "ForgejoRestore r1") {
		t.Fatalf("events %v", evs)
	}

	// Pods are left (terminating): it stays at zero, also once the restore has completed.
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = false, nil
	setValkeyStatus(t, env, f, 0, 0)
	if res, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if sts = valkeySTS(t, env, f); !res.Stop || *sts.Spec.Replicas != 0 {
		t.Fatalf("pods left: stop %v replicas %d", res.Stop, *sts.Spec.Replicas)
	}
	if evs := env.events(); hasEvent(evs, "Normal ValkeyReset") {
		t.Fatalf("reset announced twice: %v", evs)
	}

	// Every pod is gone: three new pods start without data; the step waits for one to be Ready.
	deleteValkeyPods(t, env, f)
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r1", "uid-r1")
	if res, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	sts = valkeySTS(t, env, f)
	if *sts.Spec.Replicas != valkeyMembers || sts.Annotations[annotationValkeyReset] != "" ||
		sts.Annotations[annotationValkeyResetDone] != valkeyResetRestore+"uid-r1" || !res.Stop {
		t.Fatalf("after the pods are gone: replicas %d, annotations %v, stop %v", *sts.Spec.Replicas, sts.Annotations, res.Stop)
	}

	// Once per restore: the same restore, still ScalingUp, does not empty it again.
	setValkeyStatus(t, env, f, 3, 3)
	if res, err = ReconcileHA(ctx, env.rc); err != nil || res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if sts = valkeySTS(t, env, f); *sts.Spec.Replicas != valkeyMembers {
		t.Fatalf("emptied again for the same restore: replicas %d", *sts.Spec.Replicas)
	}

	// A later restore does.
	env.rc.ScalingUpRestore = scalingUpRestore("r2", "uid-r2")
	if res, err = ReconcileHA(ctx, env.rc); err != nil || !res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
	if sts = valkeySTS(t, env, f); *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetRestore+"uid-r2" {
		t.Fatalf("second restore: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
}

// In maintenance (spec.restoreFrom holds the instance while the restore is ScalingUp) the
// workload stays at zero anyway, so the step does not stop the steps after it.
func TestValkeyEmptiedInMaintenanceDoesNotStop(t *testing.T) {
	env, f := valkeyResetEnv(t)
	env.rc.Maintenance = true
	env.rc.RestoreScalingUp, env.rc.ScalingUpRestore = true, scalingUpRestore("r", "uid-r")
	res, err := ReconcileHA(context.Background(), env.rc)
	if err != nil {
		t.Fatal(err)
	}
	if sts := valkeySTS(t, env, f); res.Stop || *sts.Spec.Replicas != 0 {
		t.Fatalf("stop %v replicas %d", res.Stop, *sts.Spec.Replicas)
	}
}

// A changed valkey-password replaces every Valkey pod at once with the new password (a
// rolling update would stall at its first pod); app.ini follows once Valkey is back.
func TestValkeyEmptiedWhenPasswordChanges(t *testing.T) {
	ctx := context.Background()
	env, f := valkeyResetEnv(t)
	sec := &corev1.Secret{}
	if !found(t, env.rc.Client, f, forgejo.SecretsName(f), sec) {
		t.Fatal("no secrets")
	}
	sec.Data[valkeyPasswordKey] = []byte("n3wpassword")
	if err := env.rc.Client.Update(ctx, sec); err != nil {
		t.Fatal(err)
	}

	res, err := ReconcileHA(ctx, env.rc)
	if err != nil {
		t.Fatal(err)
	}
	sts := valkeySTS(t, env, f)
	if !res.Stop || *sts.Spec.Replicas != 0 || sts.Annotations[annotationValkeyReset] != valkeyResetPassword ||
		sts.Spec.Template.Annotations[annotationValkeyPassword] != passwordDigest("n3wpassword") {
		t.Fatalf("stop %v, replicas %d, annotations %v, template %v", res.Stop, *sts.Spec.Replicas, sts.Annotations, sts.Spec.Template.Annotations)
	}
	if !strings.Contains(env.rc.ValkeyURL, "n3wpassword") {
		t.Fatal("rc.ValkeyURL does not carry the new password")
	}
	evs := env.events()
	if !hasEvent(evs, "Normal ValkeyReset") || strings.Contains(strings.Join(evs, "\n"), "n3wpassword") ||
		strings.Contains(strings.Join(evs, "\n"), testValkeyPassword) {
		t.Fatalf("events %v", evs)
	}

	setValkeyStatus(t, env, f, 0, 0)
	deleteValkeyPods(t, env, f)
	if _, err = ReconcileHA(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	sts = valkeySTS(t, env, f)
	if *sts.Spec.Replicas != valkeyMembers || sts.Annotations[annotationValkeyReset] != "" || sts.Annotations[annotationValkeyResetDone] != "" {
		t.Fatalf("after the pods are gone: replicas %d, annotations %v", *sts.Spec.Replicas, sts.Annotations)
	}
	setValkeyStatus(t, env, f, 3, 3)
	if res, err = ReconcileHA(ctx, env.rc); err != nil || res.Stop {
		t.Fatalf("res %+v, err %v", res, err)
	}
}

// --- a cron runner pod that cannot start ---

// pendingCronPod is a cron runner pod that the scheduler could not place for age.
func pendingCronPod(f *forgejov1.Forgejo, age time.Duration) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: forgejo.CronRunnerName(f) + "-0", Namespace: f.Namespace, UID: "uid-cron",
			Labels: forgejo.CronSelectorLabels(f), CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			Annotations: map[string]string{forgejo.AnnotationConfigHash: testHash}},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable}}},
	}
	return p
}

// The Deployment restart is decided by the server pods, which the probes reach: a cron runner
// pod that cannot be scheduled does not suspend it.
func TestRestartUnhealthyIgnoresPendingCronRunner(t *testing.T) {
	ctx := context.Background()
	f := haInstance()
	now := metav1.Now()
	f.Status.Health = &forgejov1.HealthStatus{LastCheckTime: &now, ConsecutiveFailures: 5, Message: "status fail: cache=fail",
		Checks: map[string]string{"cache": testCheckFail}}
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, readyPod(f, "fj-a", testHash, time.Hour), pendingCronPod(f, time.Hour))
	pods, err := forgejoPods(ctx, env.rc)
	if err != nil || len(pods) != 2 {
		t.Fatalf("pods %d, err %v", len(pods), err)
	}
	if err := restartUnhealthy(ctx, env.rc, dep, pods); err != nil {
		t.Fatal(err)
	}
	got := &appsv1.Deployment{}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; !ok {
		t.Fatal("no restart while the cron runner's pod was Pending")
	}

	// Without a server pod there is nothing to restart, whatever the cron runner does.
	f2 := haInstance()
	f2.Status.Health = f.Status.Health.DeepCopy()
	dep2 := testDeployment(f2, 1)
	env2 := newTestEnv(t, f2, dep2, cronPod(f2, "fj-cron-0", time.Hour))
	pods, _ = forgejoPods(ctx, env2.rc)
	if err := restartUnhealthy(ctx, env2.rc, dep2, pods); err != nil {
		t.Fatal(err)
	}
	if err := env2.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep2), got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok {
		t.Fatal("restarted without a server pod")
	}
}

// A cron runner pod that has not started any container for cronStartTimeout no longer holds
// back the last known good config; one that started (and fails) or is merely new still does.
func TestLastKnownGoodWithCronRunnerThatCannotStart(t *testing.T) {
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
		name     string
		pod      func(*forgejov1.Forgejo) *corev1.Pod
		recorded bool
	}{
		{"unschedulable for an hour", func(f *forgejov1.Forgejo) *corev1.Pod { return pendingCronPod(f, time.Hour) }, true},
		{"waiting for its volume for an hour", multiAttach, true},
		{"unschedulable for a minute", func(f *forgejov1.Forgejo) *corev1.Pod { return pendingCronPod(f, time.Minute) }, false},
		{"crash-looping", crashing, false},
		{"of an older template", func(f *forgejov1.Forgejo) *corev1.Pod {
			p := pendingCronPod(f, time.Hour)
			p.Annotations[forgejo.AnnotationConfigHash] = testOldHash
			return p
		}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			f.Status.Health = &forgejov1.HealthStatus{Healthy: true}
			dep := testDeployment(f, 1)
			env := newTestEnv(t, f, dep, cronRunnerFor(f, false), configSecret(f), readyPod(f, "fj-a", testHash, time.Hour), tt.pod(f))
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
			if got := found(t, env.rc.Client, f, forgejo.LKGConfigSecretName(f), &corev1.Secret{}); got != tt.recorded {
				t.Fatalf("LKG recorded = %v, want %v", got, tt.recorded)
			}
			if evs := env.events(); tt.recorded && !strings.Contains(strings.Join(evs, "\n"), "by the server pods") {
				t.Fatalf("events %v", evs)
			}
		})
	}
}
