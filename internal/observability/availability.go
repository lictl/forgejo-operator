package observability

import (
	"context"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// The availability SLI (docs/observability.md, "Service level objectives") counts one
// sample per health-check interval of time in forgejo_operator_availability_probes_total.
// Unlike the health probe itself, it is also taken when there is nothing to probe: no Ready
// pod while replicas are desired is an outage, not a pause of the measurement.
const (
	resultUp   = "up"
	resultDown = "down"
)

// availabilitySamples remembers, per instance, how far this process has counted samples.
var availabilitySamples sync.Map // types.NamespacedName -> sampled

type sampled struct {
	uid types.UID
	// until is the end of the last health-check interval counted.
	until time.Time
}

// maxSampleSpan bounds the time that the samples of one pass stand for. The controller
// reconciles every instance at least once a minute, also while a step fails (its resync caps
// the error backoff as well), so a sample comes at most about a minute after it is due, plus
// a slow pass (probe timeouts) and the wait for the work queue. A longer gap means that the
// operator did not watch the instance, and is not counted with the result seen at its end.
const maxSampleSpan = 3 * time.Minute

// exportAvailability exports the instance's four sample series at 0, so that increase()
// and rate() also see their first increment.
func exportAvailability(f *forgejov1.Forgejo) {
	for _, result := range []string{resultUp, resultDown} {
		for _, planned := range []bool{true, false} {
			forgejo.AvailabilityProbes.WithLabelValues(f.Namespace, f.Name, result, strconv.FormatBool(planned)).Add(0)
		}
	}
}

// recordAvailability counts the samples that are due, all with the result seen now: one for
// each health-check interval since the last one counted, so that the SLI measures time, not
// passes. Passes do not come once per interval: a failing step backs the instance off for up
// to a minute, and probes that time out slow every pass down, both typically during an
// outage, which would otherwise weigh less than the same time of normal operation. One pass
// counts at most the intervals of maxSampleSpan, or one when the interval is longer. It
// returns how long until the next sample is due.
func recordAvailability(rc *forgejo.ReconcileContext, up bool, now time.Time, interval time.Duration) time.Duration {
	f := rc.Instance
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	count, until := 1, now
	if v, ok := availabilitySamples.Load(key); ok {
		if s := v.(sampled); s.uid == f.UID {
			// One second of tolerance for timer jitter, as for the probes.
			elapsed := now.Sub(s.until) + time.Second
			if elapsed < interval {
				return interval - elapsed
			}
			limit := max(1, int(maxSampleSpan/interval))
			if count = int(elapsed / interval); count <= limit {
				until = s.until.Add(time.Duration(count) * interval)
			} else {
				// After a gap, count from now on.
				count = limit
			}
		}
	}
	availabilitySamples.Store(key, sampled{uid: f.UID, until: until})
	result := resultDown
	if up {
		result = resultUp
	}
	forgejo.AvailabilityProbes.WithLabelValues(f.Namespace, f.Name, result, strconv.FormatBool(plannedDowntime(rc))).
		Add(float64(count))
	return until.Add(interval).Sub(now)
}

// plannedDowntime reports whether Forgejo may be unavailable by design right now, so that
// a down sample does not count against the SLO: the instance is paused or scaled to zero, a
// restore holds the instance in maintenance or brings it back up (ScalingUp), an
// orchestrated upgrade is in flight, or it has never been available yet (the first
// installation, also of spec.restoreFrom). A hold that only a person ends
// (rc.MaintenanceHeld: a failed restore that changed the instance, a failed bootstrap of
// spec.restoreFrom) is unplanned, also during an upgrade's rollback and before the first
// healthy probe.
func plannedDowntime(rc *forgejo.ReconcileContext) bool {
	f := rc.Instance
	switch {
	case f.Spec.Paused, forgejo.DesiredReplicas(f) == 0 && !forgejo.HorizontalAutoscalingEnabled(f):
		return true
	case rc.MaintenanceHeld:
		return false
	}
	return rc.Maintenance || rc.RestoreScalingUp || rc.Upgrading ||
		f.Status.Health == nil || f.Status.Health.LastHealthyTime == nil
}

// SampleWhilePaused takes the availability samples of a paused instance, for which no
// step runs. It does not probe Forgejo: a sample is up while a pod is Ready (the pods'
// readiness probe is /api/healthz as well). Paused samples are planned.
func SampleWhilePaused(ctx context.Context, rc *forgejo.ReconcileContext) {
	f := rc.Instance
	exportAvailability(f)
	now := time.Now()
	ready, _, err := servingPods(ctx, rc, now)
	if err != nil {
		logf.FromContext(ctx).Info("Could not list Forgejo pods for the availability sample", "error", err.Error())
		return
	}
	rc.Requeue(recordAvailability(rc, ready > 0, now, healthInterval(f)))
}

func healthInterval(f *forgejov1.Forgejo) time.Duration {
	return forgejo.DurationOr(f.Spec.AutoPilot.SelfHealing.HealthCheckInterval, defaultHealthInterval)
}
