package autopilot

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/observability"
)

// unreachableHealth is status.health after probes that got no answer, as the health step
// reports them: either as Unreachable (streak reset) or, in older versions, as failures
// without Forgejo's checks.
func unreachableHealth(f *forgejov1.Forgejo, asFailures bool) {
	now := metav1.Now()
	msg := `GET /api/healthz: Get "http://fj-http.ns.svc.cluster.local:3000/api/healthz": ` +
		`dial tcp 10.0.0.1:3000: connect: connection refused`
	f.Status.Health = &forgejov1.HealthStatus{LastCheckTime: &now, Message: msg}
	if asFailures {
		f.Status.Health.ConsecutiveFailures = 12
		meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
			Type: forgejov1.ConditionHealthy, Status: metav1.ConditionFalse, Reason: "HealthCheckFailed", Message: msg,
		})
		return
	}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
		Type: forgejov1.ConditionHealthy, Status: metav1.ConditionUnknown, Reason: healthReasonUnreachable, Message: msg,
	})
}

// The instance-level restart after failed health checks, decided by the server pods.
func TestRestartUnhealthy(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name               string
		hung               bool // failures without checks (unreachableHealth); else checks Forgejo failed
		unreachable        bool // failures counted while the probes reached no Forgejo
		noServerPod        bool
		cronPod            func(*forgejov1.Forgejo) *corev1.Pod
		sshKeysUnavailable bool
		restart            bool
	}{
		// A healthz that Forgejo itself answers with failing checks, while the pod stays Ready.
		{name: "failures Forgejo reported itself", restart: true},
		// A probe that Forgejo accepted and then left hanging (or answered with an error page)
		// has no checks but is a real failure: the health step counts it, and the restart must
		// not mistake it for an unreachable Forgejo.
		{name: "probe accepted and left hanging", hung: true, restart: true},
		// Failures counted before the probes stopped reaching Forgejo do not restart it while
		// Healthy is Unreachable: a restart would not help an operator that cannot reach it.
		{name: "probes unanswered", unreachable: true},
		// While the Secret of SSH host keys is unusable, a restarted pod could not start.
		{name: "SSH host keys unusable", sshKeysUnavailable: true},
		// The probes reach the server pods only: a cron runner pod that cannot be scheduled does
		// not suspend the restart.
		{name: "cron runner pod pending", cronPod: func(f *forgejov1.Forgejo) *corev1.Pod { return pendingCronPod(f, time.Hour) },
			restart: true},
		// Without a server pod there is nothing to restart, whatever the cron runner does.
		{name: "no server pod", noServerPod: true, cronPod: func(f *forgejov1.Forgejo) *corev1.Pod { return cronPod(f, "fj-cron-0", time.Hour) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := haInstance()
			switch {
			case tt.hung:
				unreachableHealth(f, true)
			case tt.unreachable:
				unreachableHealth(f, false)
				f.Status.Health.ConsecutiveFailures = 5
			default:
				now := metav1.Now()
				f.Status.Health = &forgejov1.HealthStatus{LastCheckTime: &now, ConsecutiveFailures: 5, Message: "status fail: database=fail",
					Checks: map[string]string{"cache": testCheckPass, testCheckDatabase: testCheckFail}}
			}
			dep := testDeployment(f, 1)
			objs := []client.Object{dep}
			if !tt.noServerPod {
				objs = append(objs, readyPod(f, "fj-live", testHash, time.Hour))
			}
			if tt.cronPod != nil {
				objs = append(objs, tt.cronPod(f))
			}
			env := newTestEnv(t, f, objs...)
			env.rc.SSHHostKeysUnavailable = tt.sshKeysUnavailable
			pods, err := forgejoPods(ctx, env.rc)
			if err != nil || len(pods) != len(objs)-1 { // every object but the Deployment is a pod
				t.Fatalf("pods %d, err %v", len(pods), err)
			}
			if err := restartUnhealthy(ctx, env.rc, dep, pods); err != nil {
				t.Fatal(err)
			}
			got := &appsv1.Deployment{}
			if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
				t.Fatal(err)
			}
			if _, restarted := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; restarted != tt.restart {
				t.Fatalf("restarted = %v, want %v", restarted, tt.restart)
			}
		})
	}
}

// The operator cannot reach the Service (NetworkPolicy, mesh) while the kubelet's readiness
// probe on the same endpoint passes: no restart, Degraded explains it, LKG is still recorded.
func TestUnreachableProbeDoesNotRestart(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, false)
	dep := testDeployment(f, 1)
	env := newTestEnv(t, f, dep, configSecret(f), readyPod(f, "fj-live", testHash, time.Hour))
	ctx := context.Background()

	// The first reconcile only starts the clock.
	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionDegraded); c != nil && c.Status == metav1.ConditionTrue {
		t.Fatalf("Degraded set before %s: %+v", unreachableAfter, c)
	}
	stateFor(f).unansweredSince = time.Now().Add(-2 * unreachableAfter)
	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}

	got := &appsv1.Deployment{}
	if err := env.rc.Client.Get(ctx, client.ObjectKeyFromObject(dep), got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Spec.Template.Annotations[forgejo.AnnotationRestartedAt]; ok {
		t.Fatalf("Deployment restarted (restarted-at=%s) on probes Forgejo never answered", v)
	}
	if !lastAction(f, ActionPodRestarted).IsZero() {
		t.Fatalf("PodRestarted recorded: %+v", f.Status.AutoPilot.RecentActions)
	}
	c := condition(f, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != ReasonHealthProbeUnreachable {
		t.Fatalf("Degraded = %+v, want True/%s", c, ReasonHealthProbeUnreachable)
	}
	if ap := f.Status.AutoPilot; ap == nil || ap.LastKnownGoodConfigHash != testHash {
		t.Fatalf("LKG not recorded from pod readiness: %+v", f.Status.AutoPilot)
	}
	if evs := env.events(); !hasEvent(evs, "Warning "+ReasonHealthProbeUnreachable) {
		t.Fatalf("no %s warning event, got %q", ReasonHealthProbeUnreachable, evs)
	}

	// The warning is rate limited.
	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if evs := env.events(); hasEvent(evs, "Warning "+ReasonHealthProbeUnreachable) {
		t.Fatalf("warning repeated within %s: %q", warnInterval, evs)
	}

	// Once the probe gets through again, Degraded is cleared.
	now := metav1.Now()
	f.Status.Health = &forgejov1.HealthStatus{Healthy: true, LastCheckTime: &now, LastHealthyTime: &now, Checks: map[string]string{testCheckDatabase: testCheckPass}}
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
		Type: forgejov1.ConditionHealthy, Status: metav1.ConditionTrue, Reason: "HealthCheckPassed",
	})
	if _, err := Reconcile(ctx, env.rc); err != nil {
		t.Fatal(err)
	}
	if c := condition(f, forgejov1.ConditionDegraded); c == nil || c.Status != metav1.ConditionFalse {
		t.Fatalf("Degraded = %+v, want False", c)
	}
	if !stateFor(f).unansweredSince.IsZero() {
		t.Fatal("unanswered clock not reset after a healthy probe")
	}
}

// Degraded set by another feature is not overwritten.
func TestUnreachableKeepsOtherDegradedReason(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, false)
	meta.SetStatusCondition(&f.Status.Conditions, metav1.Condition{
		Type: forgejov1.ConditionDegraded, Status: metav1.ConditionTrue, Reason: ReasonConfigRolledBack, Message: "rolled back",
	})
	env := newTestEnv(t, f, readyPod(f, "fj-live", testHash, time.Hour))
	pods, _ := serverPods(context.Background(), env.rc)
	reportProbeUnreachable(env.rc, stateFor(f), pods, true)
	c := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDegraded)
	if c.Reason != ReasonConfigRolledBack {
		t.Fatalf("Degraded reason = %s, want %s", c.Reason, ReasonConfigRolledBack)
	}
}

// The unanswered clock only runs while a pod is Ready: a Forgejo that really stopped answering
// goes NotReady through the kubelet's probe on the same endpoint, which is not "unreachable".
func TestUnreachableNeedsReadyPods(t *testing.T) {
	f := testInstance()
	unreachableHealth(f, false)
	st := stateFor(f)
	pods := []corev1.Pod{*readyPod(f, "fj-live", testHash, time.Hour)}
	if trackUnreachable(st, f, pods) {
		t.Fatal("unreachable right after the first unanswered probe")
	}
	st.unansweredSince = time.Now().Add(-2 * unreachableAfter)
	if !trackUnreachable(st, f, pods) {
		t.Fatalf("not unreachable after %s", 2*unreachableAfter)
	}
	pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
	if trackUnreachable(st, f, pods) || !st.unansweredSince.IsZero() {
		t.Fatal("unreachable without a Ready pod")
	}
}

// healthReasonUnreachable must track the reason the health step sets.
func TestHealthReasonUnreachableMatchesObservability(t *testing.T) {
	if healthReasonUnreachable != observability.ReasonUnreachable {
		t.Fatalf("healthReasonUnreachable = %q, observability.ReasonUnreachable = %q", healthReasonUnreachable, observability.ReasonUnreachable)
	}
}
