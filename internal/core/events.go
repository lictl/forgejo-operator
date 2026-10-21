package core

import (
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// warnInterval is how often a persisting problem is repeated as a Warning event.
const warnInterval = time.Hour

// eventLimiter remembers when a Warning was last emitted per instance and key. It lives
// only in memory: after an operator restart a persisting problem is reported once more.
type eventLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

var warnings = &eventLimiter{last: map[string]time.Time{}}

func (l *eventLimiter) allow(key string, every time.Duration, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t, ok := l.last[key]; ok && now.Sub(t) < every {
		return false
	}
	if len(l.last) > 4096 {
		for k, t := range l.last {
			if now.Sub(t) >= 24*time.Hour {
				delete(l.last, k)
			}
		}
	}
	l.last[key] = now
	return true
}

// warnEvery emits a Warning event for the instance at most once per interval for key. Put
// what identifies the problem (e.g. a requested size) into key, so that a different problem
// is reported at once.
func warnEvery(rc *forgejo.ReconcileContext, key string, every time.Duration, reason, action, note string, args ...any) {
	if warnings.allow(string(rc.Instance.UID)+"/"+key, every, time.Now()) {
		rc.Event(corev1.EventTypeWarning, reason, action, note, args...)
	}
}

// reasonReconcileError is the controller's Degraded reason for a failed step.
const reasonReconcileError = "ReconcileError"

// setDegraded sets Degraded for one of core's reasons without hiding another feature's
// reason (Degraded is shared by several steps).
func setDegraded(rc *forgejo.ReconcileContext, reason, message string) {
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason == reason || c.Reason == reasonReconcileError {
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, reason, message)
	}
}

// clearDegraded resets Degraded only when it was set for reason.
func clearDegraded(rc *forgejo.ReconcileContext, reason string) {
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
	if c != nil && c.Status == metav1.ConditionTrue && c.Reason == reason {
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}
