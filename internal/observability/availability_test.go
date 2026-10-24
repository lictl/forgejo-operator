package observability

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Keys of samples.
const (
	keyDownUnplanned = "down/false"
	keyDownPlanned   = "down/true"
)

// samples reads the instance's four availability counters, keyed "result/planned".
func samples(t *testing.T, f *forgejov1.Forgejo) map[string]float64 {
	t.Helper()
	out := map[string]float64{}
	for _, r := range []string{resultUp, resultDown} {
		for _, p := range []string{"true", "false"} {
			out[r+"/"+p] = counterValue(t, forgejo.AvailabilityProbes.WithLabelValues(f.Namespace, f.Name, r, p))
		}
	}
	return out
}

// expireSample pretends the last interval counted ended one interval ago, so one sample is due.
func expireSample(f *forgejov1.Forgejo) {
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	availabilitySamples.Store(key, sampled{uid: f.UID, until: time.Now().Add(-healthInterval(f) - 2*time.Second)})
}

// With no Ready pod the probe is skipped, but the SLI records the outage: one unplanned
// down sample per interval. Restores, upgrades, zero replicas and the first installation
// are planned, a held restore is not; a paused instance is sampled from its pods' readiness.
func TestAvailabilitySamplesDoNotHideOutages(t *testing.T) {
	ctx := context.Background()
	f := testInstance("avail")
	t.Cleanup(func() { ForgetInstance("ns", "avail"); forgejo.DeleteInstanceMetrics("ns", "avail") })
	was := metav1.NewTime(time.Now().Add(-time.Hour))
	f.Status.Health = &forgejov1.HealthStatus{LastHealthyTime: &was, LastCheckTime: &was}
	rc, _ := monitoringRC(t, f, interceptor.Funcs{})

	step := func(want map[string]float64) {
		t.Helper()
		before := samples(t, f)
		rc.RequeueAfter = 0
		if _, err := ReconcileHealth(ctx, rc); err != nil {
			t.Fatal(err)
		}
		after := samples(t, f)
		for k := range after {
			if got := after[k] - before[k]; got != want[k] {
				t.Fatalf("%s +%v, want +%v (all: %v)", k, got, want[k], after)
			}
		}
	}

	// The series exist from the first pass, so rate() sees the first down sample.
	step(map[string]float64{keyDownUnplanned: 1})
	if rc.RequeueAfter != defaultHealthInterval {
		t.Fatalf("requeue %s, want the health interval while nothing serves", rc.RequeueAfter)
	}
	// At most one sample per interval.
	step(map[string]float64{})
	if rc.RequeueAfter <= 0 || rc.RequeueAfter > defaultHealthInterval {
		t.Fatalf("requeue %s", rc.RequeueAfter)
	}

	for _, tt := range []struct {
		name  string
		setup func()
		undo  func()
	}{
		{"maintenance", func() { rc.Maintenance, rc.MaintenanceReason = true, "ForgejoRestore r is restoring" }, func() { rc.Maintenance = false }},
		{"restore scaling up", func() { rc.RestoreScalingUp = true }, func() { rc.RestoreScalingUp = false }},
		{"upgrade", func() { rc.Upgrading = true }, func() { rc.Upgrading = false }},
		{"zero replicas", func() { f.Spec.Replicas = new(int32(0)) }, func() { f.Spec.Replicas = nil }},
		{"never available", func() { f.Status.Health.LastHealthyTime = nil }, func() { f.Status.Health.LastHealthyTime = &was }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			defer tt.undo()
			expireSample(f)
			step(map[string]float64{keyDownPlanned: 1})
		})
	}

	// A hold that only a person ends (a failed restore that changed the instance, a failed
	// spec.restoreFrom) is an outage, also during an upgrade's rollback and before the first
	// healthy probe; replicas scaled to zero on purpose stay planned.
	rc.Maintenance, rc.MaintenanceHeld, rc.Upgrading = true, true, true
	f.Status.Health.LastHealthyTime = nil
	expireSample(f)
	step(map[string]float64{keyDownUnplanned: 1})
	f.Spec.Replicas = new(int32(0))
	expireSample(f)
	step(map[string]float64{keyDownPlanned: 1})
	f.Spec.Replicas = nil
	rc.Maintenance, rc.MaintenanceHeld, rc.Upgrading = false, false, false
	f.Status.Health.LastHealthyTime = &was

	// Unplanned again once the restore is over.
	expireSample(f)
	step(map[string]float64{keyDownUnplanned: 1})

	// A probe that passes is up; one that fails or does not reach Forgejo is down.
	expireSample(f)
	before := samples(t, f)
	recordAvailability(rc, true, time.Now(), healthInterval(f))
	if got := samples(t, f)["up/false"] - before["up/false"]; got != 1 {
		t.Fatalf("up/false +%v", got)
	}

	// Paused: no step runs, the sample follows the pods' readiness and is planned.
	f.Spec.Paused = true
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: f.Namespace, Name: "avail-0", Labels: forgejo.SelectorLabels(f)}}
	p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute))}}
	if err := rc.Client.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	expireSample(f)
	before = samples(t, f)
	rc.RequeueAfter = 0
	SampleWhilePaused(ctx, rc)
	if got := samples(t, f)["up/true"] - before["up/true"]; got != 1 {
		t.Fatalf("paused up/true +%v", got)
	}
	// Requeued for the next sample, due one interval after the interval just counted.
	if rc.RequeueAfter <= 0 || rc.RequeueAfter > defaultHealthInterval {
		t.Fatalf("paused requeue %s", rc.RequeueAfter)
	}

	// A re-created instance (new UID) is sampled right away.
	f.UID = "uid-avail-2"
	before = samples(t, f)
	SampleWhilePaused(ctx, rc)
	if got := samples(t, f)["up/true"] - before["up/true"]; got != 1 {
		t.Fatalf("re-created up/true +%v", got)
	}
}

// Samples measure time, not passes: a pass that comes late counts every health-check interval
// it missed, with the result it sees, so an outage during which a failing step backs the
// instance off for a minute weighs as much as the same time of normal operation. One pass
// counts at most maxSampleSpan.
func TestAvailabilitySamplesFollowTime(t *testing.T) {
	f := testInstance("avail-time")
	t.Cleanup(func() { ForgetInstance("ns", "avail-time"); forgejo.DeleteInstanceMetrics("ns", "avail-time") })
	was := metav1.NewTime(time.Now().Add(-time.Hour))
	f.Status.Health = &forgejov1.HealthStatus{LastHealthyTime: &was}
	rc := &forgejo.ReconcileContext{Instance: f}
	t0 := time.Now()
	s := time.Second

	for _, tt := range []struct {
		name     string
		at       time.Duration // since t0
		interval time.Duration
		up       bool
		want     float64       // samples counted
		wait     time.Duration // returned: until the next sample is due
	}{
		{"first sample", 0, 30 * s, true, 1, 30 * s},
		{"not due yet", 10 * s, 30 * s, true, 0, 19 * s},
		{"due within the tolerance", 29 * s, 30 * s, true, 1, 31 * s},
		// A failing step: the controller's backoff (at most a minute) drives the passes.
		{"backoff 1", 89 * s, 30 * s, false, 2, 31 * s},
		{"backoff 2", 149 * s, 30 * s, false, 2, 31 * s},
		// Slow passes, every 37s: one sample each, two once the delay adds up to an interval.
		{"slow 1", 186 * s, 30 * s, false, 1, 24 * s},
		{"slow 2", 223 * s, 30 * s, false, 1, 17 * s},
		{"slow 3", 260 * s, 30 * s, false, 1, 10 * s},
		{"slow 4", 297 * s, 30 * s, false, 1, 3 * s},
		{"slow 5", 334 * s, 30 * s, true, 2, 26 * s},
		// After a gap the operator did not watch: at most maxSampleSpan, then from now on.
		{"gap", 934 * s, 30 * s, true, 6, 30 * s},
		{"after the gap", 964 * s, 30 * s, true, 1, 30 * s},
		// An interval longer than maxSampleSpan: one sample per pass at most.
		{"long interval", 964*s + 25*time.Minute, 10 * time.Minute, true, 1, 10 * time.Minute},
	} {
		before := samples(t, f)
		wait := recordAvailability(rc, tt.up, t0.Add(tt.at), tt.interval)
		after := samples(t, f)
		key := keyDownUnplanned
		if tt.up {
			key = "up/false"
		}
		if got := after[key] - before[key]; got != tt.want || wait != tt.wait {
			t.Errorf("%s: %s +%v, next in %s; want +%v, next in %s", tt.name, key, got, wait, tt.want, tt.wait)
		}
		var total float64
		for k := range after {
			total += after[k] - before[k]
		}
		if total != tt.want {
			t.Errorf("%s: %v samples in all, want %v", tt.name, total, tt.want)
		}
	}
}
