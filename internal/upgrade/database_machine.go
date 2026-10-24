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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// dbSettle is how long a recreated cluster must have been reported ready (DatabaseReady) before
// the rollback restores into it: right after CloudNativePG reports a new cluster healthy, its
// Service may not route to the primary yet, and a restore that cannot connect fails.
const dbSettle = 15 * time.Second

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
	// stopped keeps Forgejo at zero replicas while rolling back, until the restore has
	// replaced the database.
	stopped bool
	// stop makes the step return Stop: the database step must not touch the Cluster while a
	// rollback deletes it (it would update the old one or create the new one too early).
	stop bool
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
	case forgejov1.DatabaseUpgradeRollingBack:
		err = m.rollingBack()
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
	case forgejov1.DatabaseUpgradeRollingBack:
		rc.DatabaseImage = u.FromImage
		rc.DatabaseStorageFloor = u.StorageSize
		if m.stopped {
			rc.ReplicasOverride = zero
		}
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
// data a restore is replacing), during the verification (Forgejo is stopped), and before the
// rollback's restore. While CloudNativePG upgrades or reverts the cluster nothing waits:
// restores do not start then (the restore controller waits for the upgrade).
func (m *dbMachine) heldByMaintenance() bool {
	rc, u := m.rc, m.u
	if !rc.Maintenance {
		return false
	}
	switch u.Phase {
	case forgejov1.DatabaseUpgradeUpgrading, forgejov1.DatabaseUpgradeReverting:
		return false
	case forgejov1.DatabaseUpgradeRollingBack:
		if b := rc.Instance.Status.Backup; u.RestoreName != "" && b != nil && b.ActiveRestore == u.RestoreName {
			return false
		}
		m.stopped = true
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
// never) run to the end, and the old image is written back.
func (m *dbMachine) upgrading() error {
	rc, u, c := m.rc, m.u, m.c
	if c == nil {
		m.toRollingBack(fmt.Sprintf("CNPG cluster %s disappeared during the upgrade", forgejo.DatabaseClusterName(rc.Instance)))
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
		m.toRollingBack(fmt.Sprintf("CNPG cluster %s disappeared after the upgrade", forgejo.DatabaseClusterName(f)))
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

// verifyFailed rolls back from the pre-upgrade backup, or keeps the new major when
// spec.upgrade.autoRollback is false. The old data directory is gone after a successful
// pg_upgrade, so going back means recreating the cluster.
func (m *dbMachine) verifyFailed(reason string) {
	rc, u, f := m.rc, m.u, m.rc.Instance
	rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeFailed", dbEventAction, "Upgrade of PostgreSQL from %s to %s failed: %s",
		u.FromImage, u.ToImage, reason)
	if forgejo.BoolOr(f.Spec.Upgrade.AutoRollback, true) {
		m.toRollingBack(reason)
		return
	}
	u.FailureReason = reason
	m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
		"Upgrade of PostgreSQL to %s failed: %s. It was not rolled back because spec.upgrade.autoRollback is false: the cluster stays on "+
			"PostgreSQL %d. To go back, restore pre-upgrade backup %s into a cluster of PostgreSQL %d (docs/upgrades.md).",
		u.ToImage, reason, u.ToMajor, u.BackupName, u.FromMajor))
}

// dbRevertTimeout is the least time a revert waits for the old data to come back: CloudNativePG
// has to delete the upgrade Job and start every instance again, which a short
// spec.upgrade.timeout (shared with Forgejo upgrades) does not allow for.
const dbRevertTimeout = 15 * time.Minute

// reverting waits for the old data to come back after the old image was written: the old
// major with the same system identifier, served by the primary (replicas may still be
// starting; CloudNativePG brings them back by itself). When it does not within
// spec.upgrade.timeout, and at least dbRevertTimeout, the cluster is rolled back from the
// backup only if pg_upgrade had started: a pg_upgrade --link that failed after linking can
// leave the old data directory unusable. If it never started (an image that cannot be pulled),
// the old data is untouched, and deleting the Cluster would only lose it: the upgrade fails
// and the Cluster is kept as it is.
func (m *dbMachine) reverting() error {
	rc, u, c, f := m.rc, m.u, m.c, m.rc.Instance
	switch {
	case c == nil:
		m.toRollingBack(fmt.Sprintf("CNPG cluster %s disappeared while it was reverted", forgejo.DatabaseClusterName(f)))
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
		switch {
		case !u.PGUpgradeStarted:
			u.FailureReason = reason
			m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
				"Upgrade of PostgreSQL to %s failed: %s. pg_upgrade never started, so the old data is untouched: CNPG cluster %s is "+
					"kept as it is and not rolled back from the backup, and Forgejo starts once the cluster serves again. Check its pods.",
				u.ToImage, reason, c.Name))
			rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRollbackFailed", dbEventAction, "%s", u.Message)
		case forgejo.BoolOr(f.Spec.Upgrade.AutoRollback, true):
			m.toRollingBack(reason)
		default:
			u.FailureReason = reason
			m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
				"Upgrade of PostgreSQL to %s failed: %s. It was not rolled back from the backup because spec.upgrade.autoRollback is "+
					"false: restore pre-upgrade backup %s (database only) into a cluster of PostgreSQL %d (docs/upgrades.md).",
				u.ToImage, reason, u.BackupName, u.FromMajor))
			rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRollbackFailed", dbEventAction, "%s", u.Message)
		}
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

// toRollingBack starts the rollback from the pre-upgrade backup.
func (m *dbMachine) toRollingBack(reason string) {
	u, f := m.u, m.rc.Instance
	u.FailureReason = reason
	u.RestoreName = objectName(f, "pg-rollback", *u.StartTime)
	if m.c != nil && quantityLess(u.StorageSize, m.c.StorageSize) {
		// Also recorded when the Cluster is deleted; recorded here already, in case that pass's
		// status write is lost.
		u.StorageSize = m.c.StorageSize
	}
	m.stopped = true
	m.transition(forgejov1.DatabaseUpgradeRollingBack, fmt.Sprintf(
		"Rolling back to %s: %s. CNPG cluster %s is recreated on %s and pre-upgrade backup %s is restored into it",
		u.FromImage, reason, forgejo.DatabaseClusterName(f), u.FromImage, u.BackupName))
	m.rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRollingBack", dbEventAction, "%s", u.Message)
}

// rollingBack recreates the cluster on the old image and restores the pre-upgrade backup into
// it: Forgejo is stopped, the Cluster is deleted, and once it and its pods, volumes and Secrets
// are gone, the database step creates it again (this step stops meanwhile, so that the database
// step neither updates the old Cluster nor creates the new one too early). The restore is
// database only: Forgejo was stopped from the backup on, so its data volume matches the dump.
func (m *dbMachine) rollingBack() error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	rs := &forgejov1.ForgejoRestore{}
	err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.RestoreName}, rs)
	if err == nil {
		m.followRestore(rs)
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	m.stopped = true
	rc.Requeue(5 * time.Second)
	name := forgejo.DatabaseClusterName(f)
	c := m.c
	switch {
	case c != nil && c.Deleting:
		m.stop = true
		u.Message = fmt.Sprintf("Rolling back to %s: waiting for CNPG cluster %s to be deleted", u.FromImage, name)
		return nil
	case c != nil && u.StartTime != nil && c.Created.Before(u.StartTime):
		// The cluster of the failed upgrade. It holds the only copy of the data until the
		// backup is restored, so it goes only while that backup can restore it (read again in
		// every pass up to the deletion). Forgejo stops first (the workload step scales it to
		// zero in the passes that wait here); a pod that does not stop within stopTimeout (an
		// unreachable node) cannot write to a deleted database either.
		if why, err := m.rollbackBackupUnusable(); err != nil {
			return err
		} else if why != "" {
			m.keepCluster(c, why)
			return nil
		}
		n, err := backup.ServerPodsRunning(m.ctx, rc.Client, f)
		if err != nil {
			return err
		}
		if n > 0 && (u.PhaseStartTime == nil || time.Since(u.PhaseStartTime.Time) < stopTimeout) {
			u.Message = fmt.Sprintf("Rolling back to %s: waiting for %d Forgejo pod(s) to stop before CNPG cluster %s is recreated", u.FromImage, n, name)
			return nil
		}
		if quantityLess(u.StorageSize, c.StorageSize) {
			u.StorageSize = c.StorageSize
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(forgejo.CNPGClusterGVK)
		obj.SetNamespace(f.Namespace)
		obj.SetName(name)
		if err := rc.Client.Delete(m.ctx, obj, client.Preconditions{UID: &c.UID},
			client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("deleting CNPG cluster %s: %w", name, err)
		}
		m.stop = true
		u.Message = fmt.Sprintf("Rolling back to %s: deleted CNPG cluster %s (PostgreSQL %d) to recreate it on %s", u.FromImage, name,
			c.DataMajor, u.FromImage)
		rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeRollingBack", dbEventAction,
			"Deleted CNPG cluster %s to recreate it on %s and restore pre-upgrade backup %s into it", name, u.FromImage, u.BackupName)
		return nil
	case c == nil:
		left, err := m.clusterLeftovers()
		if err != nil {
			return err
		}
		if len(left) > 0 {
			// A new cluster of the same name would pick up the old volumes.
			m.stop = true
			u.Message = fmt.Sprintf("Rolling back to %s: waiting for %s of the deleted CNPG cluster %s to go", u.FromImage, describe(left), name)
			return nil
		}
		// The database step creates the Cluster in this pass (rc.DatabaseImage is the old image).
		u.Message = fmt.Sprintf("Rolling back to %s: creating CNPG cluster %s again on %s", u.FromImage, name, u.FromImage)
		return nil
	case c.DataMajor != int(u.FromMajor) || !c.Healthy():
		u.Message = fmt.Sprintf("Rolling back to %s: waiting for the recreated CNPG cluster %s (%q, %d of %d instances ready)",
			u.FromImage, name, c.Phase, c.ReadyInstances, c.SpecInstances)
		return nil
	}
	if ready := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseReady); ready == nil ||
		ready.Status != metav1.ConditionTrue || (u.PhaseStartTime != nil && ready.LastTransitionTime.Before(u.PhaseStartTime)) ||
		time.Since(ready.LastTransitionTime.Time) < dbSettle {
		u.Message = fmt.Sprintf("Rolling back to %s: waiting for the recreated CNPG cluster %s to serve (DatabaseReady)", u.FromImage, name)
		return nil
	}
	if err := m.createRestore(); err != nil {
		return err
	}
	u.Message = fmt.Sprintf("Rolling back to %s: restoring pre-upgrade backup %s into the recreated cluster (ForgejoRestore %s)",
		u.FromImage, u.BackupName, u.RestoreName)
	return nil
}

// rollbackBackupUnusable explains why the pre-upgrade backup cannot restore the database into a
// cluster of the old major, or returns "": it exists and is not being deleted, is Completed with
// a database dump that pg_dump of the old major wrote, and its volume exists. Everything is read
// live: it decides whether the Cluster that holds the data may be deleted. (That the files are
// intact only the restore can tell.)
func (m *dbMachine) rollbackBackupUnusable() (string, error) {
	rc, u, f := m.rc, m.u, m.rc.Instance
	b := &forgejov1.ForgejoBackup{}
	if err := rc.APIReader.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.BackupName}, b); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("pre-upgrade backup %s does not exist", u.BackupName), nil
		}
		return "", fmt.Errorf("reading pre-upgrade backup %s: %w", u.BackupName, err)
	}
	name := "pre-upgrade backup " + b.Name
	dump := forgejo.ParsePostgresImage(b.Status.PostgresImage)
	switch {
	case !b.DeletionTimestamp.IsZero():
		return name + " is being deleted", nil
	case b.Spec.ForgejoRef.Name != f.Name:
		return fmt.Sprintf("%s belongs to Forgejo %s", name, b.Spec.ForgejoRef.Name), nil
	case b.Status.Phase != forgejov1.BackupPhaseCompleted:
		return fmt.Sprintf("%s is %q, not Completed", name, b.Status.Phase), nil
	case b.Status.DatabaseSizeBytes <= 0:
		return name + " records no database dump (status.databaseSizeBytes)", nil
	case b.Status.PostgresImage != u.FromImage && (!dump.OK || dump.Major != int(u.FromMajor)):
		return fmt.Sprintf("the dump of %s was written by %q, not by PostgreSQL %d", name, b.Status.PostgresImage, u.FromMajor), nil
	case b.Status.PVCName == "":
		return name + " records no volume", nil
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := rc.APIReader.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: b.Status.PVCName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("the volume of %s (PVC %s) does not exist", name, b.Status.PVCName), nil
		}
		return "", fmt.Errorf("reading backup volume %s: %w", b.Status.PVCName, err)
	}
	if !pvc.DeletionTimestamp.IsZero() {
		return fmt.Sprintf("the volume of %s (PVC %s) is being deleted", name, b.Status.PVCName), nil
	}
	return "", nil
}

// keepCluster ends a rollback that cannot restore the database without deleting anything, as
// if spec.upgrade.autoRollback were false: the Cluster keeps the data it has.
func (m *dbMachine) keepCluster(c *forgejo.CNPGCluster, why string) {
	u := m.u
	u.RestoreName, u.StorageSize = "", ""
	m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
		"Upgrade of PostgreSQL to %s failed (%s) and could not be rolled back: %s. CNPG cluster %s was not deleted and keeps its data "+
			"(PostgreSQL %d, %s); restore a backup by hand (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").",
		u.ToImage, u.FailureReason, why, c.Name, c.DataMajor, c.SpecImage))
	m.rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRollbackFailed", dbEventAction, "%s", u.Message)
}

// clusterLeftovers names the pods, volumes, Secrets and Services of the deleted Cluster that
// still exist: garbage collection removes them after the Cluster, and a new Cluster of the same
// name would take them over (its first instance would start on the old volume). Pods and
// volumes carry CNPG's cluster label; Secrets and Services (some without it) are found by their
// owner reference, read as metadata only. Their labels are not the operator's, so they are read
// live.
func (m *dbMachine) clusterLeftovers() ([]string, error) {
	rc, f := m.rc, m.rc.Instance
	name := forgejo.DatabaseClusterName(f)
	sel := client.MatchingLabels{forgejo.CNPGClusterLabel: name}
	var left []string
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := rc.Client.List(m.ctx, pvcs, client.InNamespace(f.Namespace), sel); err != nil {
		return nil, err
	}
	for _, p := range pvcs.Items {
		left = append(left, "PVC "+p.Name)
	}
	pods := &corev1.PodList{}
	if err := rc.Client.List(m.ctx, pods, client.InNamespace(f.Namespace), sel); err != nil {
		return nil, err
	}
	for _, p := range pods.Items {
		left = append(left, "pod "+p.Name)
	}
	for _, kind := range []string{"Secret", "Service"} {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(kind + "List"))
		if err := rc.Client.List(m.ctx, list, client.InNamespace(f.Namespace)); err != nil {
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

// createRestore creates the database-only restore of the pre-upgrade backup.
func (m *dbMachine) createRestore() error {
	rc, f, u := m.rc, m.rc.Instance, m.u
	rs := &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      u.RestoreName,
			Namespace: f.Namespace,
			Labels:    map[string]string{forgejo.LabelForgejo: f.Name},
		},
		Spec: forgejov1.ForgejoRestoreSpec{
			ForgejoRef:      forgejov1.ForgejoRef{Name: f.Name},
			BackupName:      u.BackupName,
			RestoreDatabase: ptr.To(true),
			RestoreData:     ptr.To(false),
		},
	}
	if err := controllerutil.SetOwnerReference(f, rs, rc.Scheme); err != nil {
		return err
	}
	if err := rc.Client.Create(m.ctx, rs); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating rollback restore %s: %w", rs.Name, err)
	}
	rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeRollingBack", dbEventAction,
		"Restoring pre-upgrade backup %s into the recreated CNPG cluster with ForgejoRestore %s", u.BackupName, rs.Name)
	return nil
}

// followRestore ends the rollback with the outcome of its restore.
func (m *dbMachine) followRestore(rs *forgejov1.ForgejoRestore) {
	rc, u := m.rc, m.u
	switch rs.Status.Phase {
	case forgejov1.RestorePhaseFailed:
		u.RollbackRestoreFailed = true
		m.finish(forgejov1.DatabaseUpgradeFailed, "failed", fmt.Sprintf(
			"Upgrade of PostgreSQL to %s failed (%s) and its rollback failed too: ForgejoRestore %s of pre-upgrade backup %s into the "+
				"recreated cluster failed: %s.", u.ToImage, u.FailureReason, rs.Name, u.BackupName, rs.Status.Message))
		rc.Event(corev1.EventTypeWarning, "DatabaseUpgradeRollbackFailed", dbEventAction, "%s", u.Message)
	case forgejov1.RestorePhaseScalingUp, forgejov1.RestorePhaseCompleted:
		m.finish(forgejov1.DatabaseUpgradeRolledBack, "rolledback", fmt.Sprintf(
			"Upgrade of PostgreSQL to %s failed (%s) and was rolled back: CNPG cluster %s was recreated on %s and pre-upgrade backup %s "+
				"was restored into it.", u.ToImage, u.FailureReason, forgejo.DatabaseClusterName(rc.Instance), u.FromImage, u.BackupName))
		rc.RecordAutoPilotAction("DatabaseUpgradeRolledBack", u.Message)
	default:
		m.stopped = true
		u.Message = fmt.Sprintf("Rolling back to %s: restoring pre-upgrade backup %s into the recreated cluster (ForgejoRestore %s %s)",
			u.FromImage, u.BackupName, rs.Name, phaseOrPending(rs.Status.Phase))
		if rs.Status.Message != "" {
			u.Message += ": " + rs.Status.Message
		}
		rc.Requeue(5 * time.Second)
	}
}

func phaseOrPending(p forgejov1.RestorePhase) string {
	if p == "" {
		return string(forgejov1.RestorePhasePending)
	}
	return string(p)
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

// quantityLess reports a < b for two quantity strings; an unparsable or empty a is smaller, an
// unparsable or empty b is not larger.
func quantityLess(a, b string) bool {
	qb, err := resource.ParseQuantity(b)
	if err != nil {
		return false
	}
	qa, err := resource.ParseQuantity(a)
	if err != nil {
		return true
	}
	return qa.Cmp(qb) < 0
}
