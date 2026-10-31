// Package backup implements ForgejoBackup/ForgejoRestore execution, scheduled backups with
// retention, and the maintenance mode used while a restore is running.
package backup

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=persistentvolumeclaims,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups,verbs=get;list;watch;create;delete

// ReconcileMaintenance sets rc.Maintenance while a ForgejoRestore targets this instance and
// records it in status.backup.activeRestore. The restore controller waits for that
// acknowledgement and for the server pods to be gone before it touches any data. A failed
// restore that already changed the database or the data volume keeps the instance in
// maintenance until it is released (see HoldsMaintenance), as does a failed bootstrap of
// spec.restoreFrom; both set rc.MaintenanceHeld as well.
func ReconcileMaintenance(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if Bootstrapping(f) {
		if err := recoverBootstrap(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	}
	restores, err := ListRestores(ctx, rc.Client, f.Namespace, f.Name)
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("listing restores: %w", err)
	}
	active, held := MaintenanceRestore(restores)
	rc.RestoreMigrating = migratingRestore(restores) != nil
	rc.RestoreScalingUp = slices.ContainsFunc(restores, func(r forgejov1.ForgejoRestore) bool {
		return r.Status.Phase == forgejov1.RestorePhaseScalingUp
	})
	rc.ScalingUpRestore = nil
	for i := range restores { // oldest first
		if restores[i].Status.Phase == forgejov1.RestorePhaseScalingUp {
			rc.ScalingUpRestore = &restores[i]
		}
	}

	previous := ""
	if f.Status.Backup != nil {
		previous = f.Status.Backup.ActiveRestore
	}
	if active == nil {
		if previous != "" {
			f.Status.Backup.ActiveRestore = ""
			rc.Event(corev1.EventTypeNormal, "MaintenanceEnded", "Restore", "ForgejoRestore %s no longer needs maintenance mode", previous)
		}
		// spec.restoreFrom: no pod starts before the backup is restored, also before its
		// ForgejoRestore exists and after it failed.
		if reason, failed := bootstrapHold(f, restores); reason != "" {
			rc.Maintenance, rc.MaintenanceHeld = true, failed
			rc.MaintenanceReason = reason
		}
		return forgejo.StepResult{}, nil
	}

	rc.Maintenance, rc.MaintenanceHeld = true, held
	summary(f).ActiveRestore = active.Name
	if held {
		rc.MaintenanceReason = fmt.Sprintf("ForgejoRestore %s failed after it changed the instance; fix the cause and run a new restore, "+
			"or annotate the restore with %s=true (or delete it) to start Forgejo as it is", active.Name, AnnotationReleaseMaintenance)
		if previous != active.Name {
			rc.Event(corev1.EventTypeWarning, "MaintenanceHeld", "Restore", "%s", rc.MaintenanceReason)
		}
		return forgejo.StepResult{}, nil
	}
	rc.MaintenanceReason = fmt.Sprintf("ForgejoRestore %s is restoring %s", active.Name, RestoreSource(active))
	if previous != active.Name {
		rc.Event(corev1.EventTypeNormal, "MaintenanceStarted", "Restore", "Scaling down for ForgejoRestore %s", active.Name)
	}
	return forgejo.StepResult{}, nil
}

// MaintenanceRestore picks the restore that keeps the instance scaled to zero from restores
// (oldest first): the oldest running one, otherwise the newest failed restore that holds
// maintenance (held=true) and has not been superseded by a later restore that succeeded.
// Running restores win so that a new restore can take over from a held one.
func MaintenanceRestore(restores []forgejov1.ForgejoRestore) (*forgejov1.ForgejoRestore, bool) {
	var held *forgejov1.ForgejoRestore
	for i := range restores {
		r := &restores[i]
		if NeedsMaintenance(r) {
			return r, false
		}
		if HoldsMaintenance(r) && !restoredAfter(restores[i+1:]) {
			held = r
		}
	}
	return held, held != nil
}

// migratingRestore returns a restore that replaced the database and waits for Forgejo to
// come back (ScalingUp): the restored schema may be older than the running image, so the
// first pod migrates it and must run alone.
func migratingRestore(restores []forgejov1.ForgejoRestore) *forgejov1.ForgejoRestore {
	for i := range restores {
		r := &restores[i]
		if r.Status.Phase == forgejov1.RestorePhaseScalingUp && forgejo.BoolOr(r.Spec.RestoreDatabase, true) {
			return r
		}
	}
	return nil
}

// restoredAfter reports whether one of the (later) restores has finished restoring.
func restoredAfter(later []forgejov1.ForgejoRestore) bool {
	for i := range later {
		if p := later[i].Status.Phase; p == forgejov1.RestorePhaseScalingUp || p == forgejov1.RestorePhaseCompleted {
			return true
		}
	}
	return false
}

// ReconcileSchedule creates scheduled ForgejoBackups, prunes old ones and maintains
// status.backup and the BackupHealthy condition.
func ReconcileSchedule(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	warn := func(reason, msg string) { rc.Event(corev1.EventTypeWarning, reason, "ResizeVolume", "%s", msg) }
	if grown, err := GrowBackupPVC(ctx, rc.Client, f, warn); err != nil {
		return forgejo.StepResult{}, err
	} else if grown {
		size := BackupClaimSize(f)
		rc.Event(corev1.EventTypeNormal, "BackupStorageResized", "ResizeVolume", "Requested %s for backup PVC %s",
			size.String(), forgejo.BackupPVCName(f))
	}

	backups, err := ListBackups(ctx, rc.Client, f.Namespace, f.Name)
	if err != nil {
		return forgejo.StepResult{}, fmt.Errorf("listing backups: %w", err)
	}
	summarize(f, backups)
	if f.Status.Backup == nil || f.Status.Backup.LastSuccessfulBackupTime == nil {
		ForgetLastSuccess(f.Namespace, f.Name)
	} else {
		ExportLastSuccess(f)
	}
	key := summarizeRemote(ctx, rc, backups)

	if !f.Spec.Backup.Enabled {
		forgejo.BackupRetentionHeld.DeleteLabelValues(f.Namespace, f.Name)
		forgetRemoteRetention(f.Namespace, f.Name)
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionBackupHealthy)
		if f.Status.Backup != nil {
			f.Status.Backup.NextScheduleTime = nil
		}
		return forgejo.StepResult{}, nil
	}

	spec := f.Spec.Backup.Schedule
	if spec == "" {
		spec = forgejov1.DefaultBackupSchedule
	}
	sched, err := cron.ParseStandard(spec)
	if err != nil {
		rc.SetCondition(forgejov1.ConditionBackupHealthy, metav1.ConditionFalse, "InvalidSchedule",
			fmt.Sprintf("spec.backup.schedule %q: %v", spec, err))
		return forgejo.StepResult{}, nil
	}

	st := summary(f)
	now := time.Now()
	due := nextRun(sched, st, now)
	var deferredSince *time.Time
	if !now.Before(due) {
		if busy := firstActive(backups); upgradeInProgress(rc) || Bootstrapping(f) {
			// A hot dump taken while an upgrade migrates the database would be labelled with
			// the old image. The run starts once the upgrade has settled; it is not a missed
			// run for BackupHealthy.
			deferredSince = &due
			rc.Requeue(30 * time.Second)
		} else if busy != "" {
			// Backups never overlap; the due run starts once the other one finishes.
			rc.Requeue(15 * time.Second)
		} else {
			runAt := latestRun(sched, due, now)
			b, err := createScheduled(ctx, rc, runAt)
			if err != nil {
				return forgejo.StepResult{}, err
			}
			backups = append(backups, *b)
			st.LastScheduleTime = &metav1.Time{Time: runAt}
			st.LastBackupName = b.Name
			due = sched.Next(runAt)
		}
	}
	st.NextScheduleTime = &metav1.Time{Time: due}
	rc.Requeue(time.Until(due) + time.Second)

	scheduled := scheduledBackups(backups)
	drop, err := prune(ctx, rc, scheduled, backups, now)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	pruneRemote(ctx, rc, backups, now)
	if drop == "" {
		drop = remoteSizeDrop(f)
	}
	held := 0.0
	if drop != "" {
		held = 1
	}
	forgejo.BackupRetentionHeld.WithLabelValues(f.Namespace, f.Name).Set(held)
	setHealth(rc, sched, scheduled, now, deferredSince, key, drop)
	return forgejo.StepResult{}, nil
}

// upgradeInProgress reports whether the upgrade step is changing the image of Forgejo or
// PostgreSQL, a failed upgrade still holds its new image's migration (see
// forgejo.UpgradeHoldsWorkload), or a failed PostgreSQL upgrade left a database that may be
// empty (forgejo.DatabaseUpgradeHoldsWorkload): a scheduled backup of it would count as a good
// one and let retention prune the backups taken before.
func upgradeInProgress(rc *forgejo.ReconcileContext) bool {
	return rc.Upgrading || forgejo.UpgradeHoldsWorkload(rc.Instance) || forgejo.DatabaseUpgradeHoldsWorkload(rc.Instance)
}

func summary(f *forgejov1.Forgejo) *forgejov1.BackupStatusSummary {
	if f.Status.Backup == nil {
		f.Status.Backup = &forgejov1.BackupStatusSummary{}
	}
	return f.Status.Backup
}

// summarize records the newest backup and the newest successful backup of any reason.
func summarize(f *forgejov1.Forgejo, backups []forgejov1.ForgejoBackup) {
	if len(backups) == 0 && f.Status.Backup == nil {
		return
	}
	st := summary(f)
	st.LastBackupName = ""
	if n := len(backups); n > 0 {
		st.LastBackupName = backups[n-1].Name
	}
	st.LastSuccessfulBackupName, st.LastSuccessfulBackupTime = "", nil
	for i := range backups {
		b := &backups[i]
		if b.Status.Phase != forgejov1.BackupPhaseCompleted || b.DeletionTimestamp != nil || b.Status.CompletionTime == nil {
			continue
		}
		if st.LastSuccessfulBackupTime == nil || b.Status.CompletionTime.After(st.LastSuccessfulBackupTime.Time) {
			st.LastSuccessfulBackupName = b.Name
			st.LastSuccessfulBackupTime = b.Status.CompletionTime.DeepCopy()
		}
	}
}

// nextRun is the next due time. Without a previous run, the first run is the next tick after
// backups were enabled (remembered in nextScheduleTime); missed runs are not backfilled.
func nextRun(sched cron.Schedule, st *forgejov1.BackupStatusSummary, now time.Time) time.Time {
	if st.LastScheduleTime != nil {
		return sched.Next(st.LastScheduleTime.Time)
	}
	next := sched.Next(now)
	if st.NextScheduleTime != nil && !st.NextScheduleTime.After(next) {
		return st.NextScheduleTime.Time
	}
	return next
}

// latestRun is the most recent scheduled time at or before now, starting from due <= now.
func latestRun(sched cron.Schedule, due, now time.Time) time.Time {
	t := due
	for range 100000 {
		n := sched.Next(t)
		if n.After(now) {
			return t
		}
		t = n
	}
	return now
}

func firstActive(backups []forgejov1.ForgejoBackup) string {
	for i := range backups {
		if IsBackupActive(&backups[i]) && backups[i].DeletionTimestamp == nil {
			return backups[i].Name
		}
	}
	return ""
}

func createScheduled(ctx context.Context, rc *forgejo.ReconcileContext, runAt time.Time) (*forgejov1.ForgejoBackup, error) {
	f := rc.Instance
	b := &forgejov1.ForgejoBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-scheduled-%s", f.Name, runAt.UTC().Format("20060102150405")),
			Namespace: f.Namespace,
			Labels: map[string]string{
				forgejo.LabelForgejo:      f.Name,
				forgejo.LabelBackupReason: string(forgejov1.BackupReasonScheduled),
			},
		},
		Spec: forgejov1.ForgejoBackupSpec{
			ForgejoRef: forgejov1.ForgejoRef{Name: f.Name},
			Reason:     forgejov1.BackupReasonScheduled,
		},
	}
	// Under the Retain policy backups outlive the instance, like the data they protect.
	if !forgejo.RetainData(f) {
		if err := controllerutil.SetOwnerReference(f, b, rc.Scheme); err != nil {
			return nil, err
		}
	}
	if err := rc.Client.Create(ctx, b); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("creating scheduled backup %s: %w", b.Name, err)
		}
		return b, nil
	}
	rc.Event(corev1.EventTypeNormal, "BackupScheduled", "Backup", "Created scheduled ForgejoBackup %s", b.Name)
	return b, nil
}

// scheduledBackups returns scheduled backups that are not being deleted, newest first.
func scheduledBackups(backups []forgejov1.ForgejoBackup) []*forgejov1.ForgejoBackup {
	var out []*forgejov1.ForgejoBackup
	for _, v := range slices.Backward(backups) {
		b := &v
		if Reason(b) == forgejov1.BackupReasonScheduled && b.DeletionTimestamp == nil {
			out = append(out, b)
		}
	}
	return out
}

// prune removes the scheduled backups that pruneCandidates selects among those of the
// instance's identity (see retentionguard.go). Manual and pre-upgrade backups are never pruned.
// While the size-drop guard holds (the returned message), no completed backup is pruned;
// failed ones, which hold no files, still are. Deleting a ForgejoBackup removes its files
// (finalizer).
func prune(ctx context.Context, rc *forgejo.ReconcileContext, scheduled []*forgejov1.ForgejoBackup, backups []forgejov1.ForgejoBackup,
	now time.Time) (string, error) {
	f := rc.Instance
	keep := int(f.Spec.Backup.Retention)
	if keep < 1 {
		keep = 7
	}
	own := ofInstanceIdentity(scheduled, rc.IdentityFingerprint)
	sizes := localSizes(own)
	drop := ""
	if found := checkSizeDrop(sizes, acceptedSince(f, sizes, backups)); found != "" {
		drop = fmt.Sprintf("Scheduled backup %s is less than half the size of an earlier one (%s): the instance may have lost data. "+
			"Retention keeps every completed scheduled backup until the sizes recover; if the drop is expected (e.g. repositories "+
			"were deleted), annotate the Forgejo with %s=%s", sizes[0].name, found, AnnotationBackupSizeAccepted, sizes[0].name)
	}
	for _, b := range pruneCandidates(own, keep, now) {
		if drop != "" && b.Status.Phase == forgejov1.BackupPhaseCompleted {
			continue
		}
		if inUse, err := RestoreUsing(ctx, rc.Client, b); err != nil || inUse != "" {
			if err != nil {
				return "", err
			}
			continue
		}
		if err := rc.Client.Delete(ctx, b); client.IgnoreNotFound(err) != nil {
			return "", fmt.Errorf("pruning backup %s: %w", b.Name, err)
		}
		rc.Event(corev1.EventTypeNormal, "BackupPruned", "Backup", "Deleted %s scheduled backup %s (retention %d)", b.Status.Phase, b.Name, keep)
	}
	return drop, nil
}

// pruneCandidates selects from scheduled (newest first) the completed backups beyond the
// newest keep, and every failed one except the newest failed backup, which is kept (so
// BackupHealthy can report it) only while no completed backup is newer. Failed backups are
// removed whether or not a newer success exists, so failed runs cannot pile up on a full
// volume; completed backups within retention are never removed to make room.
//
// A completed backup beyond keep whose upload has not settled yet (uploadUnsettled) is kept
// until it has: deleting it would stop its upload Job (finalizer), so with a short retention and
// uploads slower than the schedule no off-cluster copy would ever complete. The next pass after
// the upload ends prunes it.
func pruneCandidates(scheduled []*forgejov1.ForgejoBackup, keep int, now time.Time) []*forgejov1.ForgejoBackup {
	var completed, failed []*forgejov1.ForgejoBackup
	for _, b := range scheduled {
		switch b.Status.Phase {
		case forgejov1.BackupPhaseCompleted:
			completed = append(completed, b)
		case forgejov1.BackupPhaseFailed:
			failed = append(failed, b)
		}
	}
	slices.SortStableFunc(completed, func(a, b *forgejov1.ForgejoBackup) int { return finishedAt(b).Compare(finishedAt(a)) })

	var doomed []*forgejov1.ForgejoBackup
	if len(completed) > keep {
		for _, b := range completed[keep:] {
			if !uploadUnsettled(b, now) {
				doomed = append(doomed, b)
			}
		}
	}
	for i, b := range failed {
		if i == 0 && (len(completed) == 0 || !b.CreationTimestamp.Before(&completed[0].CreationTimestamp)) {
			continue
		}
		doomed = append(doomed, b)
	}
	return doomed
}

// uploadUnsettled reports whether b's upload is Pending or Uploading and b finished less than
// RemoteIncompleteAge ago. An upload Job runs at most jobActiveDeadline, so an older one is
// stuck (e.g. Pending on a missing Secret) and must not pin backup-volume space forever.
func uploadUnsettled(b *forgejov1.ForgejoBackup, now time.Time) bool {
	r := b.Status.Remote
	if r == nil || (r.Phase != forgejov1.RemoteBackupPending && r.Phase != forgejov1.RemoteBackupUploading) {
		return false
	}
	return now.Sub(finishedAt(b)) < RemoteIncompleteAge
}

func finishedAt(b *forgejov1.ForgejoBackup) time.Time {
	if b.Status.CompletionTime != nil {
		return b.Status.CompletionTime.Time
	}
	return b.CreationTimestamp.Time
}

// setHealth reports BackupHealthy from the newest finished scheduled backup. A successful
// backup turns stale once two further scheduled runs have passed without a newer success.
// While a due run is deferred (deferredSince), staleness is judged as of the time the run was
// due, so the deferral itself never makes the backups stale. A finding of the size-drop guard
// (drop) comes first, with a Warning event when its message is new: it holds retention until
// the sizes recover or a person accepts them.
func setHealth(rc *forgejo.ReconcileContext, sched cron.Schedule, scheduled []*forgejov1.ForgejoBackup, now time.Time, deferredSince *time.Time,
	key keyProblem, drop string) {
	if drop != "" {
		if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionBackupHealthy); c == nil || c.Message != drop {
			rc.Event(corev1.EventTypeWarning, reasonBackupSizeDropped, "Backup", "%s", drop)
		}
		rc.SetCondition(forgejov1.ConditionBackupHealthy, metav1.ConditionFalse, reasonBackupSizeDropped, drop)
		return
	}
	var latest *forgejov1.ForgejoBackup
	running := false
	for _, b := range scheduled {
		if IsBackupActive(b) {
			running = true
			continue
		}
		latest = b
		break
	}
	set := func(status metav1.ConditionStatus, reason, msg string, args ...any) {
		// A backup key that is missing, unusable or not copied out of the cluster makes the
		// remote backups worthless for a disaster recovery; only failed backups say more.
		if key.reason != "" && status != metav1.ConditionFalse {
			rc.SetCondition(forgejov1.ConditionBackupHealthy, metav1.ConditionFalse, key.reason, key.message)
			return
		}
		rc.SetCondition(forgejov1.ConditionBackupHealthy, status, reason, fmt.Sprintf(msg, args...))
	}
	switch {
	case latest == nil && running:
		set(metav1.ConditionUnknown, "InProgress", "The first scheduled backup is running")
	case latest == nil:
		set(metav1.ConditionUnknown, "AwaitingFirstBackup", "No scheduled backup has run yet")
	case latest.Status.Phase == forgejov1.BackupPhaseFailed:
		set(metav1.ConditionFalse, "BackupFailed", "Scheduled backup %s failed: %s", latest.Name, latest.Status.Message)
	default:
		done := finishedAt(latest)
		n1 := sched.Next(done)
		n2 := sched.Next(n1)
		grace := min(n2.Sub(n1), 10*time.Minute)
		at, note := now, ""
		if deferredSince != nil && deferredSince.Before(now) {
			at, note = *deferredSince, "; the next scheduled backup waits for the upgrade to finish"
		}
		if at.After(n2.Add(grace)) {
			set(metav1.ConditionFalse, "BackupStale", "Last successful scheduled backup %s completed at %s; later runs did not succeed%s",
				latest.Name, done.UTC().Format(time.RFC3339), note)
			return
		}
		if r := latest.Status.Remote; r != nil && RemoteEnabled(rc.Instance) {
			switch r.Phase {
			case forgejov1.RemoteBackupFailed:
				set(metav1.ConditionFalse, "UploadFailed", "Scheduled backup %s completed at %s, but its upload to %s failed: %s",
					latest.Name, done.UTC().Format(time.RFC3339), r.Location, r.Message)
				return
			case forgejov1.RemoteBackupUploaded:
				note = "; uploaded to " + r.Location + note
			default:
				note = "; uploading to " + r.Location + note
			}
		}
		set(metav1.ConditionTrue, "BackupSucceeded", "Scheduled backup %s completed at %s%s", latest.Name, done.UTC().Format(time.RFC3339), note)
	}
}
