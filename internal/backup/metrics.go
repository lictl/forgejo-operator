package backup

import (
	"sync"
	"time"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// lastSuccess holds the exported forgejo_operator_backup_last_success_timestamp_seconds per
// instance ("namespace/name" -> unix seconds). The gauge only moves forward, so a summary
// computed from a cache that has not seen the newest completion yet cannot lower it.
var lastSuccess = struct {
	sync.Mutex
	at map[string]int64
}{at: map[string]int64{}}

// ExportLastSuccess publishes forgejo.BackupLastSuccess for f from
// status.backup.lastSuccessfulBackupTime. It makes no API call (one map lookup under a
// mutex), so it can run on every reconcile, also when the backup-schedule step is skipped:
// the gauge is back right after an operator restart instead of only after the next
// successful backup (the staleness alerts depend on it). It only raises the gauge; an
// instance without a recorded success leaves it as it is. ReconcileSchedule removes the
// series once the instance has no successful backup left (ForgetLastSuccess).
func ExportLastSuccess(f *forgejov1.Forgejo) {
	if f == nil || f.Status.Backup == nil || f.Status.Backup.LastSuccessfulBackupTime == nil {
		return
	}
	RecordLastSuccess(f.Namespace, f.Name, f.Status.Backup.LastSuccessfulBackupTime.Time)
}

// ForgetLastSuccess removes the instance's last-success series (and what RecordLastSuccess
// remembers for it): the instance has no successful backup left, or it is gone.
func ForgetLastSuccess(namespace, name string) {
	lastSuccess.Lock()
	defer lastSuccess.Unlock()
	delete(lastSuccess.at, namespace+"/"+name)
	forgejo.BackupLastSuccess.DeleteLabelValues(namespace, name)
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
