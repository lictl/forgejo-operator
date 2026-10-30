package upgrade

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// dbAcceptGrace is how long the CNPG Cluster may take to show a new image the database step
// wrote. A Cluster that still does not ask for it (the write failed, e.g. refused by CNPG's
// webhook, or the database step did not run) means that nothing changed, and the upgrade is
// aborted instead of keeping Forgejo stopped.
const dbAcceptGrace = 2 * time.Minute

// dbQueued, dbPaused and dbSlow remember, per instance, an image change reported while an
// upgrade is in flight, a pause reported for the current maintenance window, and a slow Job
// reported once, so that their events are not repeated on every pass.
var dbQueued, dbPaused, dbSlow sync.Map

// dbMachine advances a PostgreSQL major upgrade that is in flight by one phase per pass.
type dbMachine struct {
	ctx context.Context
	rc  *forgejo.ReconcileContext
	u   *forgejov1.DatabaseUpgradeStatus
	// c is the live CNPG Cluster, nil when it does not exist.
	c *forgejo.CNPGCluster
	// cancelling is set while a withdrawn upgrade waits for its pre-upgrade backup to finish.
	cancelling bool
}

func (m *dbMachine) run(desired string) error {
	if desired != m.u.ToImage {
		if m.cancellable() {
			err := m.cancel(desired)
			m.apply(desired)
			return err
		}
		m.reportQueued(desired)
	}
	if m.heldByMaintenance() {
		m.pause()
		m.apply(desired)
		return nil
	}
	m.resume()
	var err error
	switch m.u.Phase {
	case forgejov1.DatabaseUpgradePending:
		m.transition(forgejov1.DatabaseUpgradeBackingUp, "Stopping Forgejo for a cold backup of the database before the PostgreSQL upgrade")
	case forgejov1.DatabaseUpgradeBackingUp:
		err = m.backingUp()
	case forgejov1.DatabaseUpgradeUpgrading:
		err = m.upgrading()
	case forgejov1.DatabaseUpgradeVerifying:
		err = m.verifying()
	case forgejov1.DatabaseUpgradeReverting:
		err = m.reverting()
	}
	m.apply(desired)
	return err
}

// apply sets the pipeline outputs for the (possibly new) phase: the image the database step
// writes, Forgejo stopped while the database is being changed, and the Upgrading flags.
func (m *dbMachine) apply(desired string) {
	rc, u := m.rc, m.u
	zero := ptr.To[int32](0)
	switch u.Phase {
	case forgejov1.DatabaseUpgradePending:
		rc.DatabaseImage = u.FromImage
	case forgejov1.DatabaseUpgradeBackingUp, forgejov1.DatabaseUpgradeReverting:
		rc.DatabaseImage = u.FromImage
		rc.ReplicasOverride = zero
	case forgejov1.DatabaseUpgradeUpgrading:
		rc.DatabaseImage = u.ToImage
		rc.ReplicasOverride = zero
	case forgejov1.DatabaseUpgradeVerifying:
		// One Forgejo pod (rc.Upgrading), once the database step lets the workload through.
		rc.DatabaseImage = u.ToImage
	case forgejov1.DatabaseUpgradeSucceeded:
		rc.DatabaseImage = u.ToImage
		return
	case forgejov1.DatabaseUpgradeCancelled:
		rc.DatabaseImage = keptDatabaseImage(u, m.c)
		return
	default:
		holdDatabase(rc, u, m.c)
		return
	}
	rc.Upgrading, rc.DatabaseUpgrading = true, true
	msg := u.Message
	if desired != u.ToImage && !m.cancelling {
		msg += fmt.Sprintf(" (spec.database.cnpg.imageName is now %s; it is handled once this upgrade has finished)", desired)
	}
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionTrue, string(u.Phase), msg)
}

func (m *dbMachine) key() string { return m.rc.Instance.Namespace + "/" + m.rc.Instance.Name }

func (m *dbMachine) transition(p forgejov1.DatabaseUpgradePhase, msg string) {
	now := metav1.Now()
	m.u.Phase, m.u.Message, m.u.PhaseStartTime = p, msg, &now
	m.rc.Requeue(time.Second)
}

func (m *dbMachine) reportQueued(desired string) {
	if prev, ok := dbQueued.Load(m.key()); ok && prev == desired {
		return
	}
	dbQueued.Store(m.key(), desired)
	m.rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeQueued", dbEventAction,
		"spec.database.cnpg.imageName changed to %s while the upgrade to %s is in progress; it is handled once this upgrade has finished",
		desired, m.u.ToImage)
}

// heldByMaintenance reports whether a restore that is not this upgrade's own keeps the instance
// in maintenance in a phase that must wait for it: before the backup (which would be taken of
// data a restore is replacing) and during the verification (Forgejo is stopped). While
// CloudNativePG upgrades or reverts the cluster nothing waits: restores do not start then (the
// restore controller waits for the upgrade).
func (m *dbMachine) heldByMaintenance() bool {
	rc, u := m.rc, m.u
	if !rc.Maintenance {
		return false
	}
	switch u.Phase {
	case forgejov1.DatabaseUpgradeUpgrading, forgejov1.DatabaseUpgradeReverting:
		return false
	}
	return true
}

// pause holds the upgrade in its phase while maintenance stops Forgejo. The verification's
// timeout counts from the end of maintenance.
func (m *dbMachine) pause() {
	rc, u := m.rc, m.u
	if u.Phase == forgejov1.DatabaseUpgradeVerifying {
		now := metav1.Now()
		u.PhaseStartTime = &now
	}
	u.Message = fmt.Sprintf("Paused in phase %s while the instance is in maintenance (%s); the upgrade to %s resumes once maintenance has ended",
		u.Phase, rc.MaintenanceReason, u.ToImage)
	if _, loaded := dbPaused.LoadOrStore(m.key(), true); !loaded {
		rc.Event(corev1.EventTypeNormal, "DatabaseUpgradePaused", dbEventAction, "%s", u.Message)
	}
	rc.Requeue(10 * time.Second)
}

func (m *dbMachine) resume() {
	if _, ok := dbPaused.LoadAndDelete(m.key()); ok {
		m.rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeResumed", dbEventAction,
			"Maintenance has ended; resuming the upgrade to %s in phase %s", m.u.ToImage, m.u.Phase)
	}
}

// cancellable reports whether the upgrade can still be withdrawn: the new image has not been
// written to the Cluster. A pass whose status write was lost may have written it while
// BackingUp is still persisted, so the live Cluster is checked.
func (m *dbMachine) cancellable() bool {
	u := m.u
	if u.RolloutStartTime != nil {
		return false
	}
	switch u.Phase {
	case forgejov1.DatabaseUpgradePending:
		return true
	case forgejov1.DatabaseUpgradeBackingUp:
		return m.c == nil || (m.c.SpecImage != u.ToImage && m.c.Image != u.ToImage)
	}
	return false
}

// cancel ends an upgrade withdrawn before its image was written. A pre-upgrade backup that is
// already running finishes first, with Forgejo stopped. One that has not started (Pending) is
// not waited for: nothing writes yet, and it may itself be waiting, for example for a restore.
// It runs later like a manual backup.
func (m *dbMachine) cancel(desired string) error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	if u.Phase == forgejov1.DatabaseUpgradeBackingUp {
		b := &forgejov1.ForgejoBackup{}
		err := rc.APIReader.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.BackupName}, b)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && b.DeletionTimestamp.IsZero() && b.Status.Phase == forgejov1.BackupPhaseRunning {
			m.cancelling = true
			u.Message = fmt.Sprintf("spec.database.cnpg.imageName changed to %s: cancelling the upgrade to %s once pre-upgrade backup %s has finished",
				desired, u.ToImage, b.Name)
			rc.Requeue(10 * time.Second)
			return nil
		}
	}
	m.finish(forgejov1.DatabaseUpgradeCancelled, "", fmt.Sprintf(
		"Upgrade of PostgreSQL from %s to %s was cancelled before the new image was written: spec.database.cnpg.imageName changed to %s",
		u.FromImage, u.ToImage, desired))
	rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeCancelled", dbEventAction, "%s", u.Message)
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reasonPostgresUpgradeCancelled, u.Message)
	return nil
}

// backingUp stops Forgejo and everything else that uses the database, then takes the cold
// pre-upgrade backup (with the running, old PostgreSQL image) and waits for it.
func (m *dbMachine) backingUp() error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	b := &forgejov1.ForgejoBackup{}
	err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.BackupName}, b)
	switch {
	case apierrors.IsNotFound(err):
		busy, err := m.databaseUsers()
		if err != nil {
			return err
		}
		if len(busy) > 0 {
			// A pod that cannot be confirmed stopped may still write to the database.
			if u.StartTime != nil && time.Since(u.StartTime.Time) > stopTimeout {
				m.abort(fmt.Sprintf("%s did not stop within %s for the pre-upgrade backup", describe(busy), stopTimeout))
				return nil
			}
			u.Message = fmt.Sprintf("Waiting for %s to stop before the pre-upgrade backup", describe(busy))
			rc.Requeue(3 * time.Second)
			return nil
		}
		if err := m.createBackup(); err != nil {
			return err
		}
		u.Message = fmt.Sprintf("Taking pre-upgrade backup %s of the database with %s", u.BackupName, u.FromImage)
	case err != nil:
		return err
	case !b.DeletionTimestamp.IsZero():
		m.abort(fmt.Sprintf("pre-upgrade backup %s was deleted", b.Name))
		return nil
	case b.Status.Phase == forgejov1.BackupPhaseCompleted:
		rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeBackupCompleted", dbEventAction, "Pre-upgrade backup %s completed", b.Name)
		m.startUpgrade()
		return nil
	case b.Status.Phase == forgejov1.BackupPhaseFailed:
		m.abort(fmt.Sprintf("pre-upgrade backup %s failed: %s", b.Name, b.Status.Message))
		return nil
	default:
		u.Message = fmt.Sprintf("Taking pre-upgrade backup %s of the database with %s", b.Name, u.FromImage)
		if b.Status.Message != "" {
			u.Message += ": " + b.Status.Message
		}
	}
	rc.Requeue(10 * time.Second)
	return nil
}

// databaseUsers names what may still read or write the database: Forgejo's server and cron
// runner pods, runner registration Jobs (forgejo-cli writes to the database), and backup and
// restore Jobs of the instance. Upload Jobs only read the backup volume.
func (m *dbMachine) databaseUsers() ([]string, error) {
	rc, f := m.rc, m.rc.Instance
	var busy []string
	n, err := backup.ServerPodsRunning(m.ctx, rc.Client, f)
	if err != nil {
		return nil, err
	}
	if n > 0 {
		busy = append(busy, fmt.Sprintf("%d Forgejo pod(s)", n))
	}
	pods := &corev1.PodList{}
	if err := rc.Client.List(m.ctx, pods, client.InNamespace(f.Namespace), client.MatchingLabels{forgejo.LabelName: forgejo.AppName}); err != nil {
		return nil, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if backup.PodFinished(p) {
			continue
		}
		l := p.Labels
		switch {
		case l[forgejo.LabelComponent] == forgejo.ComponentRunnerRegistration && l[forgejo.LabelForgejo] == f.Name:
			busy = append(busy, "runner registration pod "+p.Name)
		case l[forgejo.LabelComponent] == forgejo.ComponentRestore && l[forgejo.LabelInstance] == f.Name:
			busy = append(busy, "restore pod "+p.Name)
		case l[forgejo.LabelComponent] == forgejo.ComponentBackup && l[forgejo.LabelInstance] == f.Name &&
			strings.HasSuffix(l[batchv1.JobNameLabel], "-backup"):
			busy = append(busy, "backup pod "+p.Name)
		}
	}
	return busy, nil
}

// createBackup creates the database-only pre-upgrade ForgejoBackup. Its Job dumps the
// database with the running PostgreSQL image (status.database.image), so that the dump can be
// restored into a cluster of the old major.
func (m *dbMachine) createBackup() error {
	rc, f, u := m.rc, m.rc.Instance, m.u
	b := &forgejov1.ForgejoBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      u.BackupName,
			Namespace: f.Namespace,
			Labels: map[string]string{
				forgejo.LabelForgejo:      f.Name,
				forgejo.LabelBackupReason: string(forgejov1.BackupReasonPreUpgrade),
			},
		},
		Spec: forgejov1.ForgejoBackupSpec{
			ForgejoRef:  forgejov1.ForgejoRef{Name: f.Name},
			Reason:      forgejov1.BackupReasonPreUpgrade,
			IncludeData: ptr.To(false),
		},
	}
	if !forgejo.RetainData(f) {
		if err := controllerutil.SetOwnerReference(f, b, rc.Scheme); err != nil {
			return err
		}
	}
	if err := rc.Client.Create(m.ctx, b); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating pre-upgrade backup %s: %w", b.Name, err)
	}
	rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeBackup", dbEventAction,
		"Created ForgejoBackup %s (database only, with %s) before upgrading PostgreSQL to %s", b.Name, u.FromImage, u.ToImage)
	return nil
}

// startUpgrade writes the new image (apply sets rc.DatabaseImage) in the pass that records
// rolloutStartTime.
func (m *dbMachine) startUpgrade() {
	u := m.u
	if u.RolloutStartTime == nil {
		now := metav1.Now()
		u.RolloutStartTime = &now
	}
	m.transition(forgejov1.DatabaseUpgradeUpgrading, fmt.Sprintf(
		"Upgrading PostgreSQL from %d to %d: CloudNativePG stops the cluster and runs pg_upgrade; Forgejo stays stopped", u.FromMajor, u.ToMajor))
	m.rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeApplying", dbEventAction,
		"Writing %s to CNPG cluster %s; CloudNativePG stops PostgreSQL and runs pg_upgrade in Job <primary>-%s",
		u.ToImage, forgejo.DatabaseClusterName(m.rc.Instance), forgejo.CNPGMajorUpgradeJobRole)
}

// abort ends an upgrade that failed before the new image reached the Cluster: nothing changed.
func (m *dbMachine) abort(reason string) {
	m.u.FailureReason = reason
	m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
		"Upgrade of PostgreSQL to %s aborted before the new image was written: %s. %s keeps running.", m.u.ToImage, reason, m.u.FromImage))
	m.rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeFailed", dbEventAction, "%s", m.u.Message)
}

// upgrading watches CloudNativePG's in-place upgrade: the data on the new major means success;
// a failed upgrade Job, or one whose pod cannot pull its images, means pg_upgrade did not (or
// never) run to the end, and the old image is written back. So does a timeout before pg_upgrade
// started (a Job pod that stays Pending, a Job CloudNativePG never creates); once it has
// started, pg_upgrade is never interrupted.
func (m *dbMachine) upgrading() error {
	rc, u, c := m.rc, m.u, m.c
	if c == nil {
		m.failHeld(fmt.Sprintf("CNPG cluster %s disappeared during the upgrade", forgejo.DatabaseClusterName(rc.Instance)),
			"Recreate it from the pre-upgrade backup (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").")
		return nil
	}
	if c.DataMajor == int(u.ToMajor) {
		u.PGUpgradeStarted = true
		m.toVerifying(fmt.Sprintf("pg_upgrade moved the data to PostgreSQL %d", u.ToMajor))
		return nil
	}
	job, pods, err := m.majorUpgradeJob()
	if err != nil {
		return err
	}
	if pgUpgradeStarted(pods) {
		u.PGUpgradeStarted = true
	}
	now := time.Now()
	if job != nil {
		if state, why := backup.StateOf(job); state == backup.JobFailed {
			if len(pods) == 0 {
				// Nobody can tell any more whether pg_upgrade ran: assume it did.
				u.PGUpgradeStarted = true
			}
			// CloudNativePG deletes the Job (and its log) once the old image is written back.
			msg := fmt.Sprintf("pg_upgrade failed in CloudNativePG's upgrade Job %s (%s)", job.Name, why)
			if detail := backup.FailureMessage(pods); detail != "" {
				msg += ": " + detail
			}
			m.toReverting(msg)
			return nil
		}
		if why := upgradePodBlocked(pods, now); why != "" {
			m.toReverting(why)
			return nil
		}
	}
	if since := u.RolloutStartTime; since != nil && now.Sub(since.Time) > dbAcceptGrace && c.SpecImage != u.ToImage && c.Image != u.ToImage {
		m.abort(fmt.Sprintf("CNPG cluster %s still asks for %s %s after the new image was to be written (see the Degraded and "+
			"DatabaseReady conditions)", c.Name, c.SpecImage, dbAcceptGrace))
		return nil
	}
	timeout := forgejo.DurationOr(rc.Instance.Spec.Upgrade.Timeout, defaultTimeout)
	elapsed := time.Duration(0)
	if u.RolloutStartTime != nil {
		elapsed = now.Sub(u.RolloutStartTime.Time).Round(time.Second)
	}
	u.Message = fmt.Sprintf("Upgrading PostgreSQL from %d to %d for %s: CloudNativePG %q", u.FromMajor, u.ToMajor, elapsed, c.Phase)
	if job != nil {
		u.Message += fmt.Sprintf(", Job %s running", job.Name)
	}
	// Like a revert, at least dbRevertTimeout: spec.upgrade.timeout is shared with Forgejo
	// upgrades and may be short, while CloudNativePG may take minutes to pull the new image.
	if startBy := max(timeout, dbRevertTimeout); elapsed > startBy && !u.PGUpgradeStarted {
		why := "CloudNativePG created no upgrade Job"
		if job != nil {
			why = fmt.Sprintf("the container of upgrade Job %s that runs pg_upgrade did not start", job.Name)
		}
		m.toReverting(fmt.Sprintf("%s within %s of writing the new image", why, startBy))
		return nil
	}
	if elapsed > timeout {
		u.Message += fmt.Sprintf("; this takes longer than spec.upgrade.timeout (%s), but pg_upgrade is never interrupted", timeout)
		if _, loaded := dbSlow.LoadOrStore(m.key()+"/"+u.StartTime.UTC().String(), true); !loaded {
			rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeSlow", dbEventAction,
				"The upgrade of PostgreSQL to %s has run for %s, longer than spec.upgrade.timeout (%s); pg_upgrade is not interrupted, "+
					"since aborting it can leave the old data unusable. Check the upgrade Job's log.", u.ToImage, elapsed, timeout)
		}
	}
	rc.Requeue(10 * time.Second)
	return nil
}

// majorUpgradeJob returns CloudNativePG's major upgrade Job of the cluster (the newest) and its
// pods, or nil. Its labels are not the operator's, so it is read live.
func (m *dbMachine) majorUpgradeJob() (*batchv1.Job, []corev1.Pod, error) {
	rc, f := m.rc, m.rc.Instance
	jobs := &batchv1.JobList{}
	if err := rc.Client.List(m.ctx, jobs, client.InNamespace(f.Namespace), client.MatchingLabels{
		forgejo.CNPGClusterLabel: forgejo.DatabaseClusterName(f), forgejo.CNPGJobRoleLabel: forgejo.CNPGMajorUpgradeJobRole,
	}); err != nil {
		return nil, nil, fmt.Errorf("listing CNPG's upgrade Jobs: %w", err)
	}
	var job *batchv1.Job
	for i := range jobs.Items {
		if j := &jobs.Items[i]; j.DeletionTimestamp.IsZero() && (job == nil || job.CreationTimestamp.Before(&j.CreationTimestamp)) {
			job = j
		}
	}
	if job == nil {
		return nil, nil, nil
	}
	pods, err := backup.JobPods(m.ctx, rc.Client, job)
	return job, pods, err
}

// pgUpgradeStarted reports whether the container that runs pg_upgrade has started in one of the
// upgrade Job's pods: from then on pg_upgrade --link may have changed the old data directory.
func pgUpgradeStarted(pods []corev1.Pod) bool {
	for i := range pods {
		for _, cs := range pods[i].Status.ContainerStatuses {
			if cs.Name == forgejo.CNPGMajorUpgradeContainer && (cs.State.Running != nil || cs.State.Terminated != nil ||
				cs.LastTerminationState.Terminated != nil || cs.RestartCount > 0) {
				return true
			}
		}
	}
	return false
}

// upgradePodBlocked reports why the upgrade Job's newest pod cannot run pg_upgrade at all: an
// image that cannot be pulled for pullGrace, or an invalid image name. pg_upgrade has not started
// then, so writing the old image back is safe.
func upgradePodBlocked(pods []corev1.Pod, now time.Time) string {
	if len(pods) == 0 {
		return ""
	}
	p := &pods[0]
	if p.DeletionTimestamp != nil {
		return ""
	}
	for _, cs := range statuses(p) {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			if cs.Name == forgejo.CNPGMajorUpgradeContainer {
				return "" // pg_upgrade runs (or ran): only the Job's outcome counts
			}
			continue
		}
		w := cs.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "InvalidImageName", "ErrImageNeverPull":
			return fmt.Sprintf("the image of container %s of CloudNativePG's upgrade pod %s cannot be used: %s", cs.Name, p.Name,
				strings.TrimSpace(w.Reason+" "+w.Message))
		case waitingErrImagePull, waitingImagePullBackOff:
			if now.Sub(p.CreationTimestamp.Time) > pullGrace {
				return fmt.Sprintf("the image of container %s of CloudNativePG's upgrade pod %s could not be pulled for %s: %s", cs.Name, p.Name,
					pullGrace, truncate(strings.TrimSpace(w.Reason+" "+w.Message), 300))
			}
		}
	}
	return ""
}

// toReverting writes the old image back: CloudNativePG then deletes the upgrade Job and starts
// the untouched old data again.
func (m *dbMachine) toReverting(reason string) {
	u := m.u
	u.FailureReason = reason
	m.rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeFailed", dbEventAction,
		"Upgrade of PostgreSQL from %s to %s failed: %s; writing %s back", u.FromImage, u.ToImage, reason, u.FromImage)
	m.transition(forgejov1.DatabaseUpgradeReverting, fmt.Sprintf("Reverting to %s: %s", u.FromImage, reason))
}

// toVerifying starts Forgejo on the new major.
func (m *dbMachine) toVerifying(why string) {
	u := m.u
	m.transition(forgejov1.DatabaseUpgradeVerifying, fmt.Sprintf("%s; starting Forgejo on it", why))
	m.rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeVerifying", dbEventAction,
		"%s (%s); starting Forgejo to verify it", why, u.ToImage)
}

// verifying waits for Forgejo to become available on the new major (a new system identifier,
// every replica of the Deployment available) and fails on a crashing pod or after
// spec.upgrade.timeout. Only pods started for the verification are judged (see leftoverPod).
func (m *dbMachine) verifying() error {
	rc, u, c, f := m.rc, m.u, m.c, m.rc.Instance
	if c == nil {
		m.failHeld(fmt.Sprintf("CNPG cluster %s disappeared after the upgrade", forgejo.DatabaseClusterName(f)),
			"Recreate it from the pre-upgrade backup (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").")
		return nil
	}
	newSystem := c.DataMajor == int(u.ToMajor) && c.SystemID != "" && (u.FromSystemID == "" || c.SystemID != u.FromSystemID)
	dep := &appsv1.Deployment{}
	if err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		dep = nil
	}
	image := f.Status.Image
	if newSystem && dep != nil && rolledOut(dep, image) {
		m.succeed(c)
		return nil
	}
	now := time.Now()
	pods, err := serverPods(m.ctx, rc)
	if err != nil {
		return err
	}
	if reason := newImageFailure(pods, image, now, m.leftoverPod); reason != "" {
		m.verifyFailed(fmt.Sprintf("Forgejo fails on PostgreSQL %d: %s", u.ToMajor, reason))
		return nil
	}
	timeout := forgejo.DurationOr(f.Spec.Upgrade.Timeout, defaultTimeout)
	if u.PhaseStartTime != nil && now.Sub(u.PhaseStartTime.Time) > timeout {
		m.verifyFailed(fmt.Sprintf("Forgejo did not become available on PostgreSQL %d within %s", u.ToMajor, timeout))
		return nil
	}
	switch {
	case !newSystem:
		u.Message = fmt.Sprintf("Waiting for PostgreSQL %d to start (CloudNativePG %q, %d of %d instances ready)",
			u.ToMajor, c.Phase, c.ReadyInstances, c.SpecInstances)
	default:
		msg, _ := rolloutProgress(pods, image)
		u.Message = fmt.Sprintf("PostgreSQL %d runs (system ID %s); verifying Forgejo on it: %s", u.ToMajor, c.SystemID, msg)
	}
	rc.Requeue(10 * time.Second)
	return nil
}

// leftoverPod reports a server pod the verification does not judge: one that has finished
// (phase Failed or Succeeded: evicted, or stopped by a node shutdown, which the scale-down does
// not delete), and one created before the new image was written. Forgejo is stopped from the
// backup on, so such a pod ran on the old major, whatever its restart count says; the image is
// the same, so it does not tell them apart.
func (m *dbMachine) leftoverPod(p *corev1.Pod) bool {
	if p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
		return true
	}
	since := m.u.RolloutStartTime
	if since == nil {
		since = m.u.PhaseStartTime
	}
	return since != nil && p.CreationTimestamp.Before(since)
}

func (m *dbMachine) succeed(c *forgejo.CNPGCluster) {
	u := m.u
	took := time.Since(u.StartTime.Time).Round(time.Second)
	m.finish(forgejov1.DatabaseUpgradeSucceeded, "succeeded", fmt.Sprintf(
		"Upgraded PostgreSQL from %d (%s) to %d (%s) in %s; system ID %s", u.FromMajor, u.FromImage, u.ToMajor, u.ToImage, took, c.SystemID))
	m.rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeSucceeded", dbEventAction, "%s", u.Message)
	m.rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reasonPostgresUpgradeSucceeded, u.Message)
	clearDatabaseDegraded(m.rc)
}

// verifyFailed keeps the new major and Forgejo stopped for a person: the old data directory is
// gone after a successful pg_upgrade, so going back would mean recreating the cluster from the
// backup, which the operator does not do by itself.
func (m *dbMachine) verifyFailed(reason string) {
	u := m.u
	m.failHeld(reason, fmt.Sprintf(
		"The data is on PostgreSQL %d. Fix the cause and set the annotation %s=true to start Forgejo on it again, or go back to "+
			"PostgreSQL %d with the pre-upgrade backup (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").",
		u.ToMajor, AnnotationRetryDatabase, u.FromMajor))
}

// failHeld ends an upgrade that failed in a way the operator does not repair. Nothing is rolled
// back or deleted: Forgejo stays stopped, and the instance is Broken, until a person acts
// (holdForDatabaseRestore). next says what they can do.
func (m *dbMachine) failHeld(reason, next string) {
	u := m.u
	u.FailureReason = reason
	u.HoldsForgejo = true
	m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
		"Upgrade of PostgreSQL from %s to %s failed: %s. Forgejo stays stopped; pre-upgrade backup %s (database only) holds the "+
			"database from before the upgrade. %s", u.FromImage, u.ToImage, reason, u.BackupName, next))
	// No event of its own: the hold reports the failure in this pass (rc.SetBroken).
}

// dbRevertTimeout is the least time a revert waits for the old data to come back: CloudNativePG
// has to delete the upgrade Job and start every instance again, which a short
// spec.upgrade.timeout (shared with Forgejo upgrades) does not allow for.
const dbRevertTimeout = 15 * time.Minute

// reverting waits for the old data to come back after the old image was written: the old
// major with the same system identifier, served by the primary. When it does not within
// spec.upgrade.timeout, and at least dbRevertTimeout, the upgrade fails. If pg_upgrade never
// started (an image that cannot be pulled), the old data is untouched and Forgejo starts once
// the cluster serves again. If it had started, a pg_upgrade --link that failed after linking can
// have left the old data directory unusable: Forgejo stays stopped for a person.
func (m *dbMachine) reverting() error {
	rc, u, c, f := m.rc, m.u, m.c, m.rc.Instance
	switch {
	case c == nil:
		m.failHeld(fmt.Sprintf("CNPG cluster %s disappeared while it was reverted", forgejo.DatabaseClusterName(f)),
			"Recreate it from the pre-upgrade backup (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").")
		return nil
	case c.DataMajor == int(u.ToMajor):
		// The Job succeeded between the failure report and the revert (CNPG then refuses the
		// old image, see mutateCNPGCluster): the old data is gone, the new major is verified.
		u.PGUpgradeStarted = true
		m.toVerifying(fmt.Sprintf("pg_upgrade completed after all and moved the data to PostgreSQL %d", u.ToMajor))
		return nil
	}
	serving := revertedData(u, c)
	if serving == "" {
		why, err := forgejo.CNPGPrimaryUnavailable(m.ctx, rc.Client, f.Namespace, c.Phase, c.CurrentPrimary, c.TargetPrimary, c.ReadyInstances)
		if err != nil {
			return err
		}
		if why == "" {
			m.finish(forgejov1.DatabaseUpgradeRolledBack, "rolledback", fmt.Sprintf(
				"Upgrade of PostgreSQL to %s failed (%s) and was reverted to %s: CloudNativePG started the untouched old data again "+
					"(system ID %s), so nothing had to be restored.", u.ToImage, u.FailureReason, u.FromImage, c.SystemID))
			rc.RecordAutoPilotAction("DatabaseUpgradeRolledBack", u.Message)
			return nil
		}
		serving = why
	}
	timeout := max(forgejo.DurationOr(f.Spec.Upgrade.Timeout, defaultTimeout), dbRevertTimeout)
	if u.PhaseStartTime != nil && time.Since(u.PhaseStartTime.Time) > timeout {
		reason := fmt.Sprintf("%s; the old data did not come back within %s after %s was written back (CloudNativePG %q: %s)",
			u.FailureReason, timeout, u.FromImage, c.Phase, serving)
		if !u.PGUpgradeStarted {
			u.FailureReason = reason
			m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
				"Upgrade of PostgreSQL to %s failed: %s. pg_upgrade never started, so the old data is untouched: CNPG cluster %s is "+
					"kept as it is, and Forgejo starts once the cluster serves again. Check its pods.",
				u.ToImage, reason, c.Name))
			rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRevertFailed", dbEventAction, "%s", u.Message)
			return nil
		}
		m.failHeld(reason, fmt.Sprintf(
			"pg_upgrade had started, so the data of CNPG cluster %s may be unusable. Check its pods; if PostgreSQL %d does not come "+
				"back, recreate the cluster from the pre-upgrade backup (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").",
			c.Name, u.FromMajor))
		return nil
	}
	u.Message = fmt.Sprintf("Reverting to %s: CloudNativePG deletes the upgrade Job and starts the old data (%q, %d of %d instances ready)",
		u.FromImage, c.Phase, c.ReadyInstances, c.SpecInstances)
	rc.Requeue(5 * time.Second)
	return nil
}

// revertedData explains why the Cluster does not hold the old data after a revert, or returns
// "" when it does: the old major on the old image with the old system identifier.
func revertedData(u *forgejov1.DatabaseUpgradeStatus, c *forgejo.CNPGCluster) string {
	switch {
	case c.Deleting:
		return "the cluster is being deleted"
	case c.DataMajor != int(u.FromMajor):
		return fmt.Sprintf("its data reports PostgreSQL %d", c.DataMajor)
	case c.Image != u.FromImage:
		return "it still reports image " + c.Image
	case u.FromSystemID != "" && c.SystemID != u.FromSystemID:
		return fmt.Sprintf("system ID %s instead of %s", c.SystemID, u.FromSystemID)
	}
	return ""
}

// clusterLeftovers names the pods, volumes, Secrets and Services of the deleted Cluster that
// still exist: garbage collection removes them after the Cluster, and a new Cluster of the same
// name would take them over (its first instance would start on the old volume). Pods and
// volumes carry CNPG's cluster label; Secrets and Services (some without it) are found by their
// owner reference, read as metadata only. Their labels are not the operator's, so they are read
// live.
func clusterLeftovers(ctx context.Context, rc *forgejo.ReconcileContext) ([]string, error) {
	f := rc.Instance
	name := forgejo.DatabaseClusterName(f)
	sel := client.MatchingLabels{forgejo.CNPGClusterLabel: name}
	var left []string
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := rc.Client.List(ctx, pvcs, client.InNamespace(f.Namespace), sel); err != nil {
		return nil, err
	}
	for _, p := range pvcs.Items {
		left = append(left, "PVC "+p.Name)
	}
	pods := &corev1.PodList{}
	if err := rc.Client.List(ctx, pods, client.InNamespace(f.Namespace), sel); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		left = append(left, "pod "+p.Name)
	}
	for _, kind := range []string{"Secret", "Service"} {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind + "List"))
		if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace)); err != nil {
			return nil, err
		}
		for _, o := range list.Items {
			if ownedByCluster(o.OwnerReferences, name) {
				left = append(left, kind+" "+o.Name)
			}
		}
	}
	slices.Sort(left)
	return left, nil
}

// ownedByCluster reports an owner reference to the CNPG Cluster name.
func ownedByCluster(refs []metav1.OwnerReference, name string) bool {
	return slices.ContainsFunc(refs, func(r metav1.OwnerReference) bool {
		return r.Kind == forgejo.CNPGClusterGVK.Kind && r.Name == name && strings.HasPrefix(r.APIVersion, forgejo.CNPGClusterGVK.Group+"/")
	})
}

// finish moves the upgrade to a terminal phase and counts it (result "" is not counted). A
// failed image is held until spec.database.cnpg.imageName changes or a retry is requested.
func (m *dbMachine) finish(p forgejov1.DatabaseUpgradePhase, result, msg string) {
	u, f := m.u, m.rc.Instance
	now := metav1.Now()
	u.Phase, u.Message, u.CompletionTime = p, msg, &now
	if p == forgejov1.DatabaseUpgradeFailed || p == forgejov1.DatabaseUpgradeRolledBack {
		u.FailedImage = u.ToImage
	}
	dbQueued.Delete(m.key())
	dbPaused.Delete(m.key())
	if result != "" {
		forgejo.DatabaseUpgradeTotal.WithLabelValues(f.Namespace, f.Name, result).Inc()
	}
	m.rc.Requeue(time.Second)
}
