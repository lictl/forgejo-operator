package controller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

const (
	// refusalDelay is the first pause before a Job create that the API server refused is
	// tried again; it doubles up to refusalMaxDelay while the refusal lasts.
	refusalDelay    = 5 * time.Second
	refusalMaxDelay = 5 * time.Minute
)

// refusalKey is one execution of a ForgejoBackup: its dump or its upload.
type refusalKey struct {
	uid    types.UID
	upload bool
}

type refusal struct {
	until time.Time
	delay time.Duration
}

// refusalBackoff paces the retries of a Job create that was refused (a quota, an admission
// webhook, RBAC). A start journals its execution before it creates the Job and discards the
// journal when the create is refused. Both are status writes, and every status write requeues
// the object at once, which bypasses the error backoff of the work queue: without this pause
// a lasting refusal would be retried about ten times a second. The state is only a delay and
// not worth persisting; after a restart the next attempt simply comes at once.
type refusalBackoff struct {
	mu      sync.Mutex
	pending map[refusalKey]refusal
}

// remaining is how long k must still wait, or zero.
func (b *refusalBackoff) remaining(k refusalKey, now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return max(b.pending[k].until.Sub(now), 0)
}

// refuse records a refusal at now and doubles the pause of k.
func (b *refusalBackoff) refuse(k refusalKey, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending == nil {
		b.pending = map[refusalKey]refusal{}
	}
	for other, r := range b.pending { // entries of backups that were deleted meanwhile
		if now.Sub(r.until) > time.Hour {
			delete(b.pending, other)
		}
	}
	delay := refusalDelay
	if prev, ok := b.pending[k]; ok {
		delay = min(prev.delay*2, refusalMaxDelay)
	}
	b.pending[k] = refusal{until: now.Add(delay), delay: delay}
}

// clear forgets k, once its create was not refused.
func (b *refusalBackoff) clear(k refusalKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, k)
}
