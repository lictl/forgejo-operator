package upgrade

import (
	"context"
	"fmt"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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

// stopTimeout bounds the wait for Forgejo's pods to stop before the pre-upgrade backup, like
// the restore's scale-down timeout. A pod stuck terminating (unreachable node, finalizer)
// would otherwise keep Forgejo down forever.
const stopTimeout = 15 * time.Minute

// queued remembers, per instance, the spec.image change already reported while an upgrade
// was in flight, so the event is not repeated on every reconcile.
var queued sync.Map

// paused remembers, per instance, that an UpgradePaused event was emitted for the current
// maintenance window, so that it is not repeated and the end of the window is reported.
var paused sync.Map

// machine advances an upgrade that is in flight by one phase per reconcile.
type machine struct {
	ctx context.Context
	rc  *forgejo.ReconcileContext
	u   *forgejov1.UpgradeStatus
	// stopped keeps Forgejo at zero replicas while rolling back, until the restore has
	// replaced the database the new version may have migrated.
	stopped bool
	// cancelling is set while a withdrawn upgrade waits for its pre-upgrade backup to finish.
	cancelling bool
}

func (m *machine) run(desired string) error {
	if desired != m.u.ToImage {
		ok, err := m.cancellable()
		if err != nil {
			return err
		}
		if ok {
			err = m.cancel(desired)
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
	case forgejov1.UpgradePhasePending:
		m.pending()
	case forgejov1.UpgradePhaseBackingUp:
		err = m.backingUp()
	case forgejov1.UpgradePhaseRollingOut, forgejov1.UpgradePhaseVerifying:
		err = m.rollingOut()
	case forgejov1.UpgradePhaseRollingBack:
		err = m.rollingBack()
	}
	m.apply(desired)
	return err
}

// apply sets the pipeline outputs for the (possibly new) phase.
func (m *machine) apply(desired string) {
	rc, u := m.rc, m.u
	switch u.Phase {
	case forgejov1.UpgradePhasePending:
		// The new image is first deployed together with rolloutStartTime (startRollout), except
		// for a retry, whose image is still deployed from the failed attempt.
		rc.TargetImage = u.FromImage
		if u.Retry {
			rc.TargetImage = u.ToImage
		}
	case forgejov1.UpgradePhaseBackingUp:
		// A cold backup: nothing writes to the database or the volume while it runs.
		rc.TargetImage = u.FromImage
		rc.ReplicasOverride = ptr.To[int32](0)
	case forgejov1.UpgradePhaseRollingOut, forgejov1.UpgradePhaseVerifying:
		rc.TargetImage = u.ToImage
	case forgejov1.UpgradePhaseRollingBack:
		rc.TargetImage = u.FromImage
		if m.stopped {
			rc.ReplicasOverride = ptr.To[int32](0)
		}
	case forgejov1.UpgradePhaseSucceeded:
		rc.TargetImage = u.ToImage
		return
	case forgejov1.UpgradePhaseCancelled:
		rc.TargetImage = u.FromImage
		return
	default:
		hold(rc, u)
		return
	}
	rc.Upgrading = true
	msg := u.Message
	if desired != u.ToImage && !m.cancelling {
		msg += fmt.Sprintf(" (spec.image is now %s; it is handled once this upgrade has finished)", desired)
	}
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionTrue, string(u.Phase), msg)
}

func (m *machine) reportQueued(desired string) {
	key := instanceKey(m.rc)
	if prev, ok := queued.Load(key); ok && prev == desired {
		return
	}
	queued.Store(key, desired)
	m.rc.Event(corev1.EventTypeNormal, "UpgradeQueued", eventAction,
		"spec.image changed to %s while the upgrade to %s is in progress; it is handled once this upgrade has finished", desired, m.u.ToImage)
}

// heldByMaintenance reports whether maintenance mode (rc.Maintenance: a ForgejoRestore, or a
// restore Job that still runs) keeps the instance at zero replicas on behalf of something
// else than this upgrade. The rollback's own restore is what RollingBack waits for, so its
// maintenance (including a failed restore that holds it) does not pause the upgrade.
func (m *machine) heldByMaintenance() bool {
	rc, u := m.rc, m.u
	if !rc.Maintenance {
		return false
	}
	if b := rc.Instance.Status.Backup; u.Phase == forgejov1.UpgradePhaseRollingBack && u.RestoreName != "" &&
		b != nil && b.ActiveRestore == u.RestoreName {
		return false
	}
	return true
}

// pause holds the upgrade in its phase while maintenance mode stops Forgejo: the phase
// handlers would otherwise take the stopped pods for a finished cold stop, start a rollout no
// pod can run, or let spec.upgrade.timeout expire. No phase advances, no backup or restore is
// created and no timeout runs. A cancellation (spec.image changed before the new image was
// deployed) still applies, since it deploys nothing.
func (m *machine) pause() {
	rc, u := m.rc, m.u
	if u.Phase == forgejov1.UpgradePhaseVerifying {
		// The verified pod is gone: after maintenance the new image starts again and migrates
		// the database (which a restore may have replaced) once more.
		u.Phase = forgejov1.UpgradePhaseRollingOut
	}
	if u.RolloutStartTime != nil &&
		(u.Phase == forgejov1.UpgradePhaseRollingOut || u.Phase == forgejov1.UpgradePhaseRollingBack) {
		// The timeouts of these phases count from rolloutStartTime (for RollingBack: from the
		// later of it and the restore's completion). Keeping it at the current time while paused
		// makes them count from the end of maintenance, also across operator restarts.
		now := metav1.Now()
		u.RolloutStartTime = &now
	}
	u.Message = fmt.Sprintf("Paused in phase %s while the instance is in maintenance (%s); the upgrade to %s resumes once maintenance has ended",
		u.Phase, rc.MaintenanceReason, u.ToImage)
	if _, loaded := paused.LoadOrStore(instanceKey(rc), true); !loaded {
		rc.Event(corev1.EventTypeNormal, "UpgradePaused", eventAction, "%s", u.Message)
	}
	rc.Requeue(10 * time.Second)
}

// resume reports the end of a pause seen by this operator process.
func (m *machine) resume() {
	if _, ok := paused.LoadAndDelete(instanceKey(m.rc)); ok {
		m.rc.Event(corev1.EventTypeNormal, "UpgradeResumed", eventAction,
			"Maintenance has ended; resuming the upgrade to %s in phase %s", m.u.ToImage, m.u.Phase)
	}
}

func instanceKey(rc *forgejo.ReconcileContext) string {
	return rc.Instance.Namespace + "/" + rc.Instance.Name
}

func (m *machine) transition(p forgejov1.UpgradePhase, msg string) {
	m.u.Phase, m.u.Message = p, msg
	m.rc.Requeue(time.Second)
}

func (m *machine) pending() {
	if m.u.BackupName == "" {
		m.startRollout()
		return
	}
	m.transition(forgejov1.UpgradePhaseBackingUp, "Stopping Forgejo for a consistent pre-upgrade backup")
}

// startRollout switches the Deployment to the new image in this reconcile. When the status
// write of an earlier attempt was lost, the rollout may already be running; that only moves
// the start of the timeout.
func (m *machine) startRollout() {
	if m.u.RolloutStartTime == nil {
		now := metav1.Now()
		m.u.RolloutStartTime = &now
	}
	m.transition(forgejov1.UpgradePhaseRollingOut, fmt.Sprintf("Rolling out %s", m.u.ToImage))
	m.rc.Event(corev1.EventTypeNormal, "UpgradeRollingOut", eventAction, "Deploying %s; its bootstrap init container migrates the database", m.u.ToImage)
}

// cancellable reports whether the upgrade can still be withdrawn because nothing of the new
// image has been deployed: it is waiting for its pre-upgrade backup. Upgrades without a backup
// deploy the new image right away (a retry even keeps it deployed) and are never cancelled.
func (m *machine) cancellable() (bool, error) {
	u, f := m.u, m.rc.Instance
	if u.BackupName == "" || u.Retry || u.RolloutStartTime != nil {
		return false, nil
	}
	switch u.Phase {
	case forgejov1.UpgradePhasePending:
		return true, nil
	case forgejov1.UpgradePhaseBackingUp:
		// The rollout may have started in a reconcile whose status write was lost (BackingUp is
		// still persisted, but the Deployment runs the new image), so check the live Deployment.
		dep := &appsv1.Deployment{}
		err := m.rc.APIReader.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return deploymentImage(dep) != u.ToImage, nil
	}
	return false, nil
}

// cancel ends an upgrade withdrawn before its image was deployed. A pre-upgrade backup that
// is already running finishes first, with Forgejo stopped, so that it stays a cold backup.
func (m *machine) cancel(desired string) error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	if u.Phase == forgejov1.UpgradePhaseBackingUp {
		b := &forgejov1.ForgejoBackup{}
		err := rc.APIReader.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.BackupName}, b)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err == nil && b.DeletionTimestamp.IsZero() && backup.IsBackupActive(b) {
			m.cancelling = true
			u.Message = fmt.Sprintf("spec.image changed to %s: cancelling the upgrade to %s once pre-upgrade backup %s has finished",
				desired, u.ToImage, b.Name)
			rc.Requeue(10 * time.Second)
			return nil
		}
	}
	m.finish(forgejov1.UpgradePhaseCancelled, "", fmt.Sprintf(
		"Upgrade from %s to %s was cancelled before it was deployed: spec.image changed to %s", u.FromImage, u.ToImage, desired))
	rc.Event(corev1.EventTypeNormal, "UpgradeCancelled", eventAction, "%s", u.Message)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reasonCancelled, u.Message)
	return nil
}

// backingUp stops Forgejo, then takes the pre-upgrade backup and waits for it.
func (m *machine) backingUp() error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	b := &forgejov1.ForgejoBackup{}
	err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.BackupName}, b)
	switch {
	case apierrors.IsNotFound(err):
		n, err := backup.ServerPodsRunning(m.ctx, rc.Client, f)
		if err != nil {
			return err
		}
		if n > 0 {
			// A pod that cannot be confirmed stopped (e.g. on a partitioned node) may still write
			// to the database, so the upgrade is aborted rather than backed up hot.
			if u.StartTime != nil && time.Since(u.StartTime.Time) > stopTimeout {
				m.abort(fmt.Sprintf("%d Forgejo pod(s) did not stop within %s for the pre-upgrade backup", n, stopTimeout))
				return nil
			}
			u.Message = fmt.Sprintf("Waiting for %d Forgejo pod(s) to stop before the pre-upgrade backup", n)
			rc.Requeue(3 * time.Second)
			return nil
		}
		if err := m.createBackup(); err != nil {
			return err
		}
		u.Message = fmt.Sprintf("Taking pre-upgrade backup %s", u.BackupName)
	case err != nil:
		return err
	case !b.DeletionTimestamp.IsZero():
		m.abort(fmt.Sprintf("Pre-upgrade backup %s was deleted", b.Name))
		return nil
	case b.Status.Phase == forgejov1.BackupPhaseCompleted:
		rc.Event(corev1.EventTypeNormal, "PreUpgradeBackupCompleted", eventAction, "Pre-upgrade backup %s completed", b.Name)
		m.startRollout()
		return nil
	case b.Status.Phase == forgejov1.BackupPhaseFailed:
		m.abort(fmt.Sprintf("Pre-upgrade backup %s failed: %s", b.Name, b.Status.Message))
		return nil
	default:
		u.Message = fmt.Sprintf("Taking pre-upgrade backup %s", b.Name)
		if b.Status.Message != "" {
			u.Message += ": " + b.Status.Message
		}
	}
	rc.Requeue(10 * time.Second)
	return nil
}

func (m *machine) createBackup() error {
	rc, f := m.rc, m.rc.Instance
	b := &forgejov1.ForgejoBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.u.BackupName,
			Namespace: f.Namespace,
			Labels: map[string]string{
				forgejo.LabelForgejo:      f.Name,
				forgejo.LabelBackupReason: string(forgejov1.BackupReasonPreUpgrade),
			},
		},
		Spec: forgejov1.ForgejoBackupSpec{
			ForgejoRef:  forgejov1.ForgejoRef{Name: f.Name},
			Reason:      forgejov1.BackupReasonPreUpgrade,
			IncludeData: ptr.To(forgejo.BackupIncludeData(f)),
		},
	}
	// Like the data they protect, backups outlive the instance under the Retain policy.
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
	rc.Event(corev1.EventTypeNormal, "PreUpgradeBackup", eventAction, "Created ForgejoBackup %s before upgrading to %s", b.Name, m.u.ToImage)
	return nil
}

// abort ends an upgrade that failed before the new image was deployed.
func (m *machine) abort(reason string) {
	m.u.FailureReason = reason
	m.finish(forgejov1.UpgradePhaseFailed, "failed",
		fmt.Sprintf("Upgrade to %s aborted before it was deployed: %s. %s keeps running.", m.u.ToImage, reason, m.u.FromImage))
	m.rc.Event(corev1.EventTypeWarning, ReasonFailed, eventAction, "%s", m.u.Message)
}

// rollingOut deploys the new image and watches it until it is verified, a failure is
// detected, or the timeout expires.
func (m *machine) rollingOut() error {
	rc, u := m.rc, m.u
	if ok, err := m.verified(); err != nil || ok {
		if ok {
			m.succeed()
		}
		return err
	}
	now := time.Now()
	if u.RolloutStartTime == nil {
		u.RolloutStartTime = &metav1.Time{Time: now}
	}
	pods, err := m.currentPods()
	if err != nil {
		return err
	}
	if reason := newImageFailure(pods, u.ToImage, now, m.leftover); reason != "" {
		m.rolloutFailed(reason)
		return nil
	}
	timeout := forgejo.DurationOr(rc.Instance.Spec.Upgrade.Timeout, defaultTimeout)
	left := timeout - now.Sub(u.RolloutStartTime.Time)
	if left <= 0 {
		m.rolloutFailed(fmt.Sprintf("%s did not become available within %s", u.ToImage, timeout))
		return nil
	}

	msg, running := rolloutProgress(pods, u.ToImage)
	if running && u.Phase == forgejov1.UpgradePhaseRollingOut {
		u.Phase = forgejov1.UpgradePhaseVerifying
	}
	u.Message = msg
	rc.Requeue(min(left+time.Second, 10*time.Second))
	return nil
}

// verified reports whether the new image is verified: core reports it as the running image
// (status.image), and the Deployment has rolled it out with every replica available. The
// second check keeps the machine from relying on when core updates status.image.
func (m *machine) verified() (bool, error) {
	rc, u, f := m.rc, m.u, m.rc.Instance
	if f.Status.Image != u.ToImage {
		return false, nil
	}
	dep := &appsv1.Deployment{}
	err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return rolledOut(dep, u.ToImage), nil
}

// leftover reports a pod of the failed attempt that a retry started with.
func (m *machine) leftover(p *corev1.Pod) bool {
	return m.u.Retry && m.u.StartTime != nil && p.CreationTimestamp.Before(m.u.StartTime)
}

// currentPods lists the server pods. A retry starts with the pods of the failed attempt: one
// that is failing (crash loop, pull error, exited container) is deleted so that a fresh pod
// gets a clean start, but one that is still migrating or serving is kept, since interrupting
// "forgejo migrate" can leave the schema half-migrated. A kept pod that fails again later is
// deleted then.
func (m *machine) currentPods() ([]corev1.Pod, error) {
	pods, err := serverPods(m.ctx, m.rc)
	if err != nil || !m.u.Retry {
		return pods, err
	}
	var current []corev1.Pod
	for i := range pods {
		p := &pods[i]
		if podImage(p) == m.u.ToImage && m.leftover(p) {
			if p.DeletionTimestamp != nil {
				continue
			}
			if failing(p) {
				if err := m.rc.Client.Delete(m.ctx, p); client.IgnoreNotFound(err) != nil {
					return nil, fmt.Errorf("restarting pod %s: %w", p.Name, err)
				}
				continue
			}
		}
		current = append(current, *p)
	}
	return current, nil
}

func (m *machine) succeed() {
	u := m.u
	took := time.Since(u.StartTime.Time).Round(time.Second)
	m.finish(forgejov1.UpgradePhaseSucceeded, "succeeded", fmt.Sprintf("Upgraded from %s to %s in %s", u.FromImage, u.ToImage, took))
	m.rc.Event(corev1.EventTypeNormal, reasonSucceeded, eventAction, "%s", u.Message)
	m.rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reasonSucceeded, u.Message)
	clearDegraded(m.rc)
}

// rolloutFailed rolls back from the pre-upgrade backup, or leaves the new image in place
// when there is no backup or rollback is disabled.
func (m *machine) rolloutFailed(reason string) {
	rc, u, f := m.rc, m.u, m.rc.Instance
	rc.Event(corev1.EventTypeWarning, ReasonFailed, eventAction, "Upgrade from %s to %s failed: %s", u.FromImage, u.ToImage, reason)
	u.FailureReason = reason
	if forgejo.BoolOr(f.Spec.Upgrade.AutoRollback, true) && u.BackupName != "" {
		u.RestoreName = objectName(f, "rollback", *u.StartTime)
		m.stopped = true
		m.transition(forgejov1.UpgradePhaseRollingBack, fmt.Sprintf("Rolling back to %s: %s", u.FromImage, reason))
		return
	}
	why := "spec.upgrade.autoRollback is false"
	if u.BackupName == "" {
		why = "no pre-upgrade backup was taken (spec.upgrade.backupBeforeUpgrade is false)"
	}
	m.finish(forgejov1.UpgradePhaseFailed, "failed", fmt.Sprintf(
		"Upgrade to %s failed: %s. It was not rolled back because %s: the new version may already have migrated the database, "+
			"which %s cannot run on, so %s stays deployed with a single replica. Fix the cause, or restore a backup taken with %s and set spec.image back.",
		u.ToImage, reason, why, u.FromImage, u.ToImage, u.FromImage))
}

// rollingBack restores the pre-upgrade backup with Forgejo stopped, then brings the old
// image back and waits until it is available.
func (m *machine) rollingBack() error {
	rc, u, f := m.rc, m.u, m.rc.Instance
	rs := &forgejov1.ForgejoRestore{}
	err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: u.RestoreName}, rs)
	if apierrors.IsNotFound(err) {
		m.stopped = true
		if err := m.createRestore(); err != nil {
			return err
		}
		u.Message = fmt.Sprintf("Rolling back to %s: restoring pre-upgrade backup %s", u.FromImage, u.BackupName)
		rc.Requeue(5 * time.Second)
		return nil
	}
	if err != nil {
		return err
	}

	timeout := forgejo.DurationOr(f.Spec.Upgrade.Timeout, defaultTimeout)
	switch rs.Status.Phase {
	case forgejov1.RestorePhaseFailed:
		// The database may still hold the new version's (possibly half-migrated) schema, or
		// the volume a half-extracted archive: the old image must not start on it.
		u.RollbackRestoreFailed = true
		m.rollbackFailed(fmt.Sprintf("ForgejoRestore %s failed: %s", rs.Name, rs.Status.Message))
		return nil
	case forgejov1.RestorePhaseCompleted:
		dep := &appsv1.Deployment{}
		if err := rc.Client.Get(m.ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DeploymentName(f)}, dep); err != nil {
			return err
		}
		if rolledOut(dep, u.FromImage) {
			m.rolledBack()
			return nil
		}
		done := rs.Status.CompletionTime
		if done != nil && u.RolloutStartTime != nil && u.RolloutStartTime.After(done.Time) {
			// Maintenance after the restore paused the wait (see pause).
			done = u.RolloutStartTime
		}
		if done != nil && time.Since(done.Time) > timeout {
			// The data is consistent and the old version is only slow: it keeps running.
			m.rollbackFailed(fmt.Sprintf("%s did not become available within %s after backup %s was restored", u.FromImage, timeout, u.BackupName))
			return nil
		}
		u.Message = fmt.Sprintf("Backup %s restored; waiting for %s to become available", u.BackupName, u.FromImage)
	case forgejov1.RestorePhaseScalingUp:
		u.Message = fmt.Sprintf("Backup %s restored; waiting for %s to become available", u.BackupName, u.FromImage)
	default:
		m.stopped = true
		u.Message = fmt.Sprintf("Rolling back to %s: restoring pre-upgrade backup %s", u.FromImage, u.BackupName)
		if rs.Status.Message != "" {
			u.Message += " (" + rs.Status.Message + ")"
		}
	}
	rc.Requeue(5 * time.Second)
	return nil
}

func (m *machine) createRestore() error {
	rc, f := m.rc, m.rc.Instance
	rs := &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.u.RestoreName,
			Namespace: f.Namespace,
			Labels:    map[string]string{forgejo.LabelForgejo: f.Name},
		},
		Spec: forgejov1.ForgejoRestoreSpec{
			ForgejoRef: forgejov1.ForgejoRef{Name: f.Name},
			BackupName: m.u.BackupName,
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
	rc.Event(corev1.EventTypeNormal, "UpgradeRollingBack", eventAction,
		"Restoring pre-upgrade backup %s with ForgejoRestore %s, then redeploying %s", m.u.BackupName, rs.Name, m.u.FromImage)
	return nil
}

func (m *machine) rolledBack() {
	u := m.u
	m.finish(forgejov1.UpgradePhaseRolledBack, "rolledback", fmt.Sprintf(
		"Upgrade to %s failed (%s) and was rolled back to %s from pre-upgrade backup %s.", u.ToImage, u.FailureReason, u.FromImage, u.BackupName))
	m.rc.RecordAutoPilotAction("UpgradeRolledBack", u.Message)
}

func (m *machine) rollbackFailed(reason string) {
	u := m.u
	m.finish(forgejov1.UpgradePhaseFailed, "failed", fmt.Sprintf(
		"Upgrade to %s failed and the automatic rollback failed too: %s. Manual intervention is required: "+
			"restore ForgejoBackup %s with a new ForgejoRestore and check that %s starts.", u.ToImage, reason, u.BackupName, u.FromImage))
	m.rc.Event(corev1.EventTypeWarning, "UpgradeRollbackFailed", eventAction, "%s", u.Message)
}

// finish moves the upgrade to a terminal phase and counts it (result "" is not counted). A
// failed image is held (see hold) until spec.image changes or a retry is requested.
func (m *machine) finish(p forgejov1.UpgradePhase, result, msg string) {
	u, f := m.u, m.rc.Instance
	now := metav1.Now()
	u.Phase, u.Message, u.CompletionTime = p, msg, &now
	if p == forgejov1.UpgradePhaseFailed || p == forgejov1.UpgradePhaseRolledBack {
		u.FailedImage = u.ToImage
	}
	queued.Delete(instanceKey(m.rc))
	paused.Delete(instanceKey(m.rc))
	if result != "" {
		forgejo.UpgradeTotal.WithLabelValues(f.Namespace, f.Name, result).Inc()
	}
	// The next reconcile acts on a spec.image change that was queued meanwhile.
	m.rc.Requeue(time.Second)
}
