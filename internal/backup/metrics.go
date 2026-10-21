package backup

import (
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// lastSuccess holds the exported forgejo_operator_backup_last_success_timestamp_seconds per
// instance ("namespace/name" -> unix seconds). The gauge only moves forward, so a summary
// computed from a cache that has not seen the newest completion yet cannot lower it.
var lastSuccess = struct {
	sync.Mutex
	at map[string]int64
}{at: map[string]int64{}}

// ExportLastSuccess publishes the instance's last successful backup time from its status. It
// runs on every reconcile, so the gauge is back right after an operator restart instead of
// only after the next successful backup (the staleness alerts depend on it). nil removes the
// series: the instance has no successful backup left.
func ExportLastSuccess(namespace, name string, t *metav1.Time) {
	if t == nil {
		lastSuccess.Lock()
		defer lastSuccess.Unlock()
		delete(lastSuccess.at, namespace+"/"+name)
		forgejo.BackupLastSuccess.DeleteLabelValues(namespace, name)
		return
	}
	RecordLastSuccess(namespace, name, t.Time)
}

// RecordLastSuccess raises the instance's last-success gauge to t; it never lowers it.
func RecordLastSuccess(namespace, name string, t time.Time) {
	lastSuccess.Lock()
	defer lastSuccess.Unlock()
	key := namespace + "/" + name
	v := max(lastSuccess.at[key], t.Unix())
	lastSuccess.at[key] = v
	forgejo.BackupLastSuccess.WithLabelValues(namespace, name).Set(float64(v))
}
