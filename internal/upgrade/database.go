package upgrade

// PostgreSQL major upgrades of the operator-managed CloudNativePG cluster (user documentation in
// docs/upgrades.md, "PostgreSQL major upgrades").
//
// CloudNativePG upgrades a major in place and offline when spec.imageName names a newer major
// than its data (status.pgDataImageInfo): it stops every instance, runs pg_upgrade --link in Job
// "<primary>-major-upgrade" and clones the replicas again. The database-upgrade step runs before
// the database step and chooses the image that step writes (rc.DatabaseImage): spec's image
// when it may be applied as it is (a new cluster, the same major), the current one while an
// upgrade is refused or waits, and a newer major only once Forgejo is stopped and a cold backup
// of the old major exists. The state machine is persisted in status.database.upgrade:
//
//	Pending → BackingUp → Upgrading ──────────────────► Verifying → Succeeded
//	   │          │           │ Job failed or               │ Forgejo not available
//	   ▼          ▼           ▼ image not pulled             ▼ on the new major
//	Cancelled   Failed     Reverting ──────────────────► RollingBack → RolledBack
//	          (nothing        │ old data back  (not back,     │ backup unusable: Failed (Cluster kept)
//	           changed)       ▼ (same system  pg_upgrade had  │ restore failed: Failed (Forgejo stays
//	                       RolledBack  ID)    started)        ▼ stopped)
//	                       (not back, pg_upgrade never started: Failed, Cluster kept)
//
// Reverting writes the old image back, which CNPG answers by deleting the upgrade Job and
// starting the untouched old data. Nothing interrupts a running pg_upgrade: a Job that runs
// longer than spec.upgrade.timeout is only reported. RollingBack, for an old data directory that
// does not come back after pg_upgrade had started and for a new major Forgejo does not run on,
// deletes the Cluster (only while the pre-upgrade backup, read again right before, can restore
// it), lets the database step create it again on the old image, and restores the pre-upgrade
// backup into it.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=postgresql.cnpg.io,resources=clusters,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=pods;persistentvolumeclaims;secrets,verbs=get;list;watch

const (
	// AnnotationRetryDatabase set to "true" on a Forgejo retries a PostgreSQL image whose upgrade
	// failed (status.database.upgrade.failedImage). The operator removes it once it has been read.
	AnnotationRetryDatabase = "forgejo.lucas.icu/retry-database-upgrade"

	// Degraded reasons owned by the database upgrade.
	ReasonPostgresDowngradeRefused       = "PostgresDowngradeRefused"
	ReasonPostgresUpgradePathUnsupported = "PostgresUpgradePathUnsupported"
	ReasonPostgresVersionUnsupported     = "PostgresVersionUnsupported"
	ReasonPostgresUpgradeRolledBack      = "PostgresUpgradeRolledBack"
	ReasonPostgresUpgradeFailed          = "PostgresUpgradeFailed"

	reasonPostgresUpgradeSucceeded = "PostgresUpgradeSucceeded"
	reasonPostgresUpgradeCancelled = "PostgresUpgradeCancelled"
	reasonDatabaseUpToDate         = "UpToDate"
	reasonDatabaseRollout          = "Rollout"
	dbEventAction                  = "DatabaseUpgrade"

	// Reasons of the DatabaseUpgrading condition while an upgrade waits to start.
	reasonWaitingForRestore        = "WaitingForRestore"
	reasonWaitingForForgejoUpgrade = "WaitingForForgejoUpgrade"
	reasonWaitingForSSHHostKeys    = "WaitingForSSHHostKeys"
)

// ReconcileDatabase is the database-upgrade step. It decides rc.DatabaseImage, sets
// rc.Upgrading and rc.DatabaseUpgrading while a PostgreSQL major upgrade is in flight (and
// rc.ReplicasOverride while it keeps Forgejo stopped), and drives status.database.upgrade and the
// DatabaseUpgrading condition. It stops (so that the database step does not touch the Cluster)
// while a rollback deletes the Cluster, and while a new instance asks for a major the operator
// does not run.
func ReconcileDatabase(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	desired := forgejo.PostgresImage(f)
	rc.DatabaseImage = desired
	if forgejo.DatabaseType(f) != forgejov1.DatabaseTypeCNPG {
		// An external database belongs to its owner: its image only runs backup and restore Jobs.
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionDatabaseUpgrading)
		clearDatabaseDegraded(rc)
		return forgejo.StepResult{}, nil
	}
	if !rc.HasAPI(forgejo.CNPGClusterGVK) {
		// The database step reports CNPGNotInstalled.
		return forgejo.StepResult{}, nil
	}
	cluster, err := readCluster(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	u := forgejo.DatabaseUpgrade(f)
	busy := u != nil && forgejo.DatabaseUpgradeInFlightPhase(u.Phase)

	// The retry annotation is consumed like the Forgejo upgrade's (see Reconcile).
	retryValue, hasRetry := f.Annotations[AnnotationRetryDatabase]
	retry := hasRetry && retryValue == valueTrue && cluster != nil && !busy && u != nil &&
		u.FailedImage != "" && u.FailedImage == desired
	if hasRetry && !retry {
		if err := removeAnnotation(ctx, rc, AnnotationRetryDatabase); err != nil {
			return forgejo.StepResult{}, err
		}
	}

	var res forgejo.StepResult
	if busy {
		m := &dbMachine{ctx: ctx, rc: rc, u: u, c: cluster}
		err = m.run(desired)
		res.Stop = m.stop
	} else {
		res, err = idleDatabase(ctx, rc, cluster, desired, retry)
	}
	if err == nil {
		err = holdAfterFailedDatabaseRestore(ctx, rc)
	}
	return res, err
}

const valueTrue = "true"

func readCluster(ctx context.Context, rc *forgejo.ReconcileContext) (*forgejo.CNPGCluster, error) {
	f := rc.Instance
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(forgejo.CNPGClusterGVK)
	err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DatabaseClusterName(f)}, obj)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading CNPG cluster %s: %w", forgejo.DatabaseClusterName(f), err)
	}
	return forgejo.ReadCNPGCluster(obj), nil
}

// databaseStatus returns status.database, creating it.
func databaseStatus(f *forgejov1.Forgejo) *forgejov1.DatabaseStatus {
	if f.Status.Database == nil {
		f.Status.Database = &forgejov1.DatabaseStatus{Type: forgejov1.DatabaseTypeCNPG, ClusterName: forgejo.DatabaseClusterName(f)}
	}
	return f.Status.Database
}

// idleDatabase handles an instance without a PostgreSQL upgrade in flight: the version gate.
func idleDatabase(ctx context.Context, rc *forgejo.ReconcileContext, c *forgejo.CNPGCluster, desired string, retry bool) (forgejo.StepResult, error) {
	f := rc.Instance
	u := forgejo.DatabaseUpgrade(f)
	// After a rollback whose restore failed, the record stays as it is until a restore has
	// replaced the database that may be empty (holdAfterFailedDatabaseRestore): neither another
	// image nor the retry annotation clears it, and no upgrade starts (databaseUpgradeBlocked).
	held := forgejo.DatabaseUpgradeHoldsWorkload(f)
	if u != nil && u.FailedImage != "" && u.FailedImage != desired && !held {
		// spec.database.cnpg.imageName moved on, so the failed image may be tried again later.
		u.FailedImage = ""
	}
	ref := forgejo.ParsePostgresImage(desired)
	if c == nil {
		return newDatabaseCluster(rc, desired, ref), nil
	}

	current := c.RunningImage()
	// Unless the gate lets the requested image through, what the Cluster asks for stays.
	if c.SpecImage != "" {
		rc.DatabaseImage = c.SpecImage
	}
	if desired == c.SpecImage || desired == current {
		rc.DatabaseImage = desired
		settleDatabase(rc, current)
		return forgejo.StepResult{}, nil
	}
	if keepFailedImage(rc, u, c, desired, retry, held) {
		return forgejo.StepResult{}, nil
	}

	cur := forgejo.ParsePostgresImage(current)
	switch {
	case !ref.OK || c.DataMajor == 0:
		// No major to compare: a tag without a version (the API server refuses it for CNPG, so
		// only objects that bypassed validation get here), or a cluster CNPG has not initialised
		// yet, which has no data to upgrade. CloudNativePG decides.
		if !ref.OK {
			warnOnce(rc, "unparseable/"+desired, "UnverifiedVersion",
				"Cannot read the PostgreSQL major version from %s; it is written to the CNPG cluster without the upgrade checks", desired)
		}
		rollOutDatabase(rc, desired, "Rolling out "+desired+" without upgrade orchestration")
	case !forgejo.Postgres.InRange(ref.Major):
		refuseVersion(rc, current, desired, ref)
	case ref.Major < c.DataMajor:
		refuseDatabase(rc, ReasonPostgresDowngradeRefused, fmt.Sprintf(
			"Refusing to downgrade PostgreSQL from %d (%s) to %d (%s): the data belongs to PostgreSQL %d, and PostgreSQL does not "+
				"move data to an older major in place. Keep %d or newer; to return to %d, restore a backup taken with it into a new "+
				"cluster (docs/upgrades.md, \"Rolling back a PostgreSQL upgrade\").",
			c.DataMajor, current, ref.Major, desired, c.DataMajor, c.DataMajor, ref.Major))
	case ref.Major == c.DataMajor:
		rollOutDatabase(rc, desired, fmt.Sprintf("Rolling out %s (PostgreSQL %d like %s): CloudNativePG replaces the instances one by one",
			desired, ref.Major, current))
	case cur.OS != "" && ref.OS != "" && cur.OS != ref.OS:
		refuseDatabase(rc, ReasonPostgresUpgradePathUnsupported, fmt.Sprintf(
			"Refusing to upgrade PostgreSQL from %s (%d on Debian %s) to %s (%d on Debian %s): CloudNativePG upgrades a major in place "+
				"only between images of the same Debian release, because pg_upgrade runs the old binaries with the new image's "+
				"libraries. Choose a PostgreSQL %d image on %s, or first move PostgreSQL %d to %s (a change within the major, which "+
				"CloudNativePG rolls out), then upgrade.",
			current, c.DataMajor, cur.OS, desired, ref.Major, ref.OS, ref.Major, cur.OS, c.DataMajor, ref.OS))
	default:
		if cur.OS == "" || ref.OS == "" {
			warnOnce(rc, "os/"+current+"/"+desired, "UnverifiedVersion",
				"Cannot tell whether %s and %s are built on the same Debian release, which CloudNativePG's in-place upgrade "+
					"needs; upgrading anyway", current, desired)
		}
		reason, msg, err := databaseUpgradeBlocked(ctx, rc, c, desired)
		if err != nil {
			return forgejo.StepResult{}, err
		}
		if reason != "" {
			rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reason, msg)
			return forgejo.StepResult{}, nil
		}
		startDatabaseUpgrade(rc, c, desired, ref)
	}
	return forgejo.StepResult{}, nil
}

// newDatabaseCluster lets a new cluster (also the one a rollback recreates, see
// dbMachine.rollingBack) start on the requested image, if Forgejo can run on its major.
func newDatabaseCluster(rc *forgejo.ReconcileContext, desired string, ref forgejo.PostgresImageRef) forgejo.StepResult {
	if ref.OK && !forgejo.Postgres.InRange(ref.Major) {
		msg := refuseVersion(rc, "", desired, ref)
		rc.SetCondition(forgejov1.ConditionDatabaseReady, metav1.ConditionFalse, ReasonPostgresVersionUnsupported, msg)
		return forgejo.StepResult{Stop: true}
	}
	warnUnverified(rc, desired, ref)
	clearDatabaseDegraded(rc)
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reasonDatabaseUpToDate,
		"Creating CNPG cluster "+forgejo.DatabaseClusterName(rc.Instance)+" with "+desired)
	return forgejo.StepResult{}
}

// keepFailedImage holds the image of a failed upgrade (the cluster keeps the one it runs) and
// reports whether it did. The retry annotation releases it, except while a failed rollback
// restore keeps Forgejo stopped (held): the retry then waits, and the annotation stays until a
// restore has lifted the hold.
func keepFailedImage(rc *forgejo.ReconcileContext, u *forgejov1.DatabaseUpgradeStatus, c *forgejo.CNPGCluster, desired string, retry, held bool) bool {
	if u == nil || u.FailedImage != desired {
		return false
	}
	switch {
	case retry && held:
		holdDatabase(rc, u, c)
		rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reasonWaitingForRestore, fmt.Sprintf(
			"The retry of the upgrade to %s (annotation %s) waits: %s", desired, AnnotationRetryDatabase, heldMessage(u)))
		return true
	case retry:
		rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeRetry", dbEventAction, "Retrying the upgrade to %s (annotation %s)", desired, AnnotationRetryDatabase)
		u.FailedImage = ""
		return false
	}
	holdDatabase(rc, u, c)
	return true
}

// rollOutDatabase lets an image change through that CloudNativePG applies by itself.
func rollOutDatabase(rc *forgejo.ReconcileContext, image, msg string) {
	rc.DatabaseImage = image
	clearDatabaseDegraded(rc)
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reasonDatabaseRollout, msg)
}

// settleDatabase reports a cluster that runs the requested image.
func settleDatabase(rc *forgejo.ReconcileContext, current string) {
	u := forgejo.DatabaseUpgrade(rc.Instance)
	clearDatabaseDegraded(rc)
	reason, msg := reasonDatabaseUpToDate, "Running "+current
	switch {
	case u == nil:
	case u.Phase == forgejov1.DatabaseUpgradeSucceeded && u.ToImage == current:
		reason, msg = reasonPostgresUpgradeSucceeded, u.Message
	case u.Phase == forgejov1.DatabaseUpgradeCancelled && u.FromImage == current:
		reason, msg = reasonPostgresUpgradeCancelled, u.Message
	}
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reason, msg)
}

// holdDatabase keeps a failed upgrade's outcome until spec.database.cnpg.imageName changes or a
// retry is asked for: the cluster keeps the image it runs.
func holdDatabase(rc *forgejo.ReconcileContext, u *forgejov1.DatabaseUpgradeStatus, c *forgejo.CNPGCluster) {
	rc.DatabaseImage = keptDatabaseImage(u, c)
	reason := ReasonPostgresUpgradeFailed
	if u.Phase == forgejov1.DatabaseUpgradeRolledBack {
		reason = ReasonPostgresUpgradeRolledBack
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, reason, u.Message)
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reason,
		fmt.Sprintf("%s Choose another image, or set the annotation %s=\"true\" to retry %s.", u.Message, AnnotationRetryDatabase, u.ToImage))
}

// keptDatabaseImage is the image the cluster keeps after a terminal phase: the one it runs.
func keptDatabaseImage(u *forgejov1.DatabaseUpgradeStatus, c *forgejo.CNPGCluster) string {
	switch {
	case c != nil && c.SpecImage != "":
		return c.SpecImage
	case u.Phase == forgejov1.DatabaseUpgradeSucceeded:
		return u.ToImage
	}
	return u.FromImage
}

// refuseDatabase keeps the current image and reports why the requested one is not applied.
// The Warning event is emitted once per refusal.
func refuseDatabase(rc *forgejo.ReconcileContext, reason, msg string) {
	if prev := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDatabaseUpgrading); prev == nil ||
		prev.Reason != reason || prev.Message != msg {
		rc.Event(corev1.EventTypeWarning, reason, dbEventAction, "%s", msg)
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, reason, msg)
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, reason, msg)
}

// refuseVersion refuses a major outside forgejo.Postgres and returns the message.
func refuseVersion(rc *forgejo.ReconcileContext, current, desired string, ref forgejo.PostgresImageRef) string {
	why := fmt.Sprintf("older than %d, the oldest major Forgejo supports", forgejo.Postgres.Min)
	if ref.Major > forgejo.Postgres.Max {
		why = fmt.Sprintf("newer than %d, the newest major the supported CloudNativePG release (1.30) supports", forgejo.Postgres.Max)
	}
	msg := fmt.Sprintf("Refusing PostgreSQL %d (%s): it is %s. The operator runs PostgreSQL %d to %d (verified: %s).",
		ref.Major, desired, why, forgejo.Postgres.Min, forgejo.Postgres.Max, forgejo.Postgres.VerifiedList())
	if current != "" {
		msg += " " + current + " keeps running."
	} else {
		msg += " The CNPG cluster is not created."
	}
	refuseDatabase(rc, ReasonPostgresVersionUnsupported, msg)
	return msg
}

// databaseUpgradeBlocked names what a PostgreSQL major upgrade waits for before it starts, as the
// reason and message of the DatabaseUpgrading condition, or returns "" when it may start. It
// never starts after a rollback whose restore failed (until a restore has lifted that hold), and
// never together with a restore, a bootstrap of spec.restoreFrom or a Forgejo upgrade, and it
// waits for what the verification of the new major needs: Forgejo running, a healthy cluster,
// the SSH host keys and the bucket of spec.objectStorage (their steps run later in the pass,
// so their last reports in status are used).
func databaseUpgradeBlocked(ctx context.Context, rc *forgejo.ReconcileContext, c *forgejo.CNPGCluster, desired string) (string, string, error) {
	f := rc.Instance
	to := fmt.Sprintf("The upgrade to %s", desired)
	switch {
	case forgejo.DatabaseUpgradeHoldsWorkload(f):
		return reasonWaitingForRestore, fmt.Sprintf("%s waits: %s", to, heldMessage(forgejo.DatabaseUpgrade(f))), nil
	case rc.Maintenance:
		return reasonWaitingForRestore, fmt.Sprintf("%s starts after the running restore: %s", to, rc.MaintenanceReason), nil
	case backup.Bootstrapping(f):
		return reasonWaitingForRestore, to + " starts once spec.restoreFrom has been restored", nil
	}
	// A restore asked for earlier goes first, also one that still waits (Pending; once the
	// upgrade is in flight it would wait for the whole upgrade) and one whose first pod migrates
	// the restored database (ScalingUp), which the backup phase would stop half-way.
	restores, err := backup.ListRestores(ctx, rc.Client, f.Namespace, f.Name)
	if err != nil {
		return "", "", fmt.Errorf("listing restores: %w", err)
	}
	for i := range restores {
		if r := &restores[i]; backup.IsRestoreActive(r) {
			return reasonWaitingForRestore, fmt.Sprintf("%s starts once ForgejoRestore %s has finished (phase %s)",
				to, r.Name, phaseOrPending(r.Status.Phase)), nil
		}
	}
	switch {
	case forgejo.UpgradeInFlight(f) || forgejo.UpgradeHoldsWorkload(f) || forgejoRollbackRestoreFailed(f):
		return reasonWaitingForForgejoUpgrade, fmt.Sprintf("%s starts after the upgrade of Forgejo to %s (status.upgrade)", to, f.Status.Upgrade.ToImage), nil
	case f.Status.Image == "":
		return "WaitingForFirstRollout", to + " starts once Forgejo has been rolled out (status.image)", nil
	case !c.Healthy():
		return "WaitingForDatabase", fmt.Sprintf("%s starts once CNPG cluster %s is healthy (phase %q, %d of %d instances ready)",
			to, c.Name, c.Phase, c.ReadyInstances, c.SpecInstances), nil
	case f.Status.SSH != nil && f.Status.SSH.Message != "":
		return reasonWaitingForSSHHostKeys, to + " starts once the Secret of SSH host keys is usable again: " + f.Status.SSH.Message, nil
	}
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionObjectStorageReady); forgejo.ObjectStorageEnabled(f) &&
		cond != nil && cond.Status == metav1.ConditionFalse && cond.Reason != "UnreachableFromOperator" {
		return "WaitingForObjectStorage", to + " starts once the bucket of spec.objectStorage is usable (ObjectStorageReady)", nil
	}
	return "", "", nil
}

// heldMessage says why an upgrade (or its retry) waits after a rollback whose restore failed.
func heldMessage(u *forgejov1.DatabaseUpgradeStatus) string {
	return fmt.Sprintf("the rollback of the upgrade to %s failed and the recreated database may be empty, so Forgejo is kept stopped "+
		"and no upgrade starts until a ForgejoRestore has restored a backup (for example %s, database only)", u.ToImage, u.BackupName)
}

// forgejoRollbackRestoreFailed reports a Forgejo upgrade whose rollback restore failed, which
// keeps Forgejo stopped (see stopAfterFailedRestore).
func forgejoRollbackRestoreFailed(f *forgejov1.Forgejo) bool {
	u := f.Status.Upgrade
	return u != nil && u.Phase == forgejov1.UpgradePhaseFailed && u.RollbackRestoreFailed
}

// startDatabaseUpgrade records a new PostgreSQL major upgrade in phase Pending. Nothing changes
// in the cluster until that is persisted: the next pass stops Forgejo for the backup.
func startDatabaseUpgrade(rc *forgejo.ReconcileContext, c *forgejo.CNPGCluster, desired string, ref forgejo.PostgresImageRef) {
	f := rc.Instance
	now := metav1.Now()
	from := c.RunningImage()
	u := &forgejov1.DatabaseUpgradeStatus{
		Phase:          forgejov1.DatabaseUpgradePending,
		FromImage:      from,
		ToImage:        desired,
		FromMajor:      int32(c.DataMajor),
		ToMajor:        int32(ref.Major),
		FromSystemID:   c.SystemID,
		BackupName:     objectName(f, fmt.Sprintf("pre-pg%d", ref.Major), now),
		StartTime:      &now,
		PhaseStartTime: &now,
		Message:        fmt.Sprintf("Upgrade of PostgreSQL from %d (%s) to %d (%s) is starting", c.DataMajor, from, ref.Major, desired),
	}
	databaseStatus(f).Upgrade = u
	clearDatabaseDegraded(rc)
	warnUnverified(rc, desired, ref)
	rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeStarted", dbEventAction,
		"Upgrading PostgreSQL from %d (%s) to %d (%s): Forgejo is stopped for a cold backup of the database, then CloudNativePG "+
			"runs pg_upgrade", c.DataMajor, from, ref.Major, desired)
	(&dbMachine{rc: rc, u: u, c: c}).apply(desired)
	rc.Requeue(time.Second)
}

// holdAfterFailedDatabaseRestore keeps Forgejo at zero replicas after a rollback whose restore
// into the recreated cluster failed: the database may be empty or half-restored, and Forgejo
// must not start on it (it would migrate an empty database and serve an empty instance). A
// ForgejoRestore created after the failure lifts the hold once it has restored a backup.
func holdAfterFailedDatabaseRestore(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	u := forgejo.DatabaseUpgrade(f)
	if !forgejo.DatabaseUpgradeHoldsWorkload(f) || u.CompletionTime == nil {
		return nil
	}
	restores, err := backup.ListRestores(ctx, rc.Client, f.Namespace, f.Name)
	if err != nil {
		return fmt.Errorf("listing restores: %w", err)
	}
	for _, r := range restores {
		if r.Name == u.RestoreName || r.CreationTimestamp.Before(u.CompletionTime) ||
			(r.Status.Phase != forgejov1.RestorePhaseScalingUp && r.Status.Phase != forgejov1.RestorePhaseCompleted) {
			continue
		}
		u.RollbackRestoreFailed = false
		u.Message = fmt.Sprintf("Upgrade of PostgreSQL to %s failed and its rollback restore failed; ForgejoRestore %s then restored %s, "+
			"so Forgejo runs again.", u.ToImage, r.Name, backup.RestoreSource(&r))
		rc.Event(corev1.EventTypeNormal, "DatabaseUpgradeRollbackRestored", dbEventAction, "%s", u.Message)
		return nil
	}
	zero := int32(0)
	rc.ReplicasOverride = &zero
	msg := fmt.Sprintf("%s Forgejo is kept stopped until a ForgejoRestore has restored a backup (for example %s, database only).",
		u.Message, u.BackupName)
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonPostgresUpgradeFailed, msg)
	if cond := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionDatabaseUpgrading); cond != nil {
		switch cond.Reason {
		case reasonWaitingForRestore, ReasonPostgresDowngradeRefused, ReasonPostgresUpgradePathUnsupported, ReasonPostgresVersionUnsupported:
			// What the version gate reported in this pass about the image asked for meanwhile
			// stays: an upgrade (or its retry) that waits for that restore, or a refusal, whose
			// Warning event is emitted when this condition changes.
			return nil
		}
	}
	rc.SetCondition(forgejov1.ConditionDatabaseUpgrading, metav1.ConditionFalse, ReasonPostgresUpgradeFailed, msg)
	return nil
}

// clearDatabaseDegraded resets Degraded only when the database upgrade set it.
func clearDatabaseDegraded(rc *forgejo.ReconcileContext) {
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue {
		return
	}
	switch c.Reason {
	case ReasonPostgresDowngradeRefused, ReasonPostgresUpgradePathUnsupported, ReasonPostgresVersionUnsupported,
		ReasonPostgresUpgradeRolledBack, ReasonPostgresUpgradeFailed:
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}

// warnUnverified warns once per instance and image about a major in range that the operator
// has not verified.
func warnUnverified(rc *forgejo.ReconcileContext, image string, ref forgejo.PostgresImageRef) {
	if !ref.OK || !forgejo.Postgres.InRange(ref.Major) || forgejo.Postgres.IsVerified(ref.Major) {
		return
	}
	warnOnce(rc, "unverified/"+image, reasonUnsupportedVersion,
		"Using PostgreSQL %d (%s) anyway: the operator is verified with PostgreSQL %s", ref.Major, image, forgejo.Postgres.VerifiedList())
}

// dbWarned remembers the once-only Warning events per instance (UID) and key; it lives only in
// memory, so an operator restart reports a persisting case once more.
var dbWarned sync.Map

func warnOnce(rc *forgejo.ReconcileContext, key, reason, note string, args ...any) {
	if _, loaded := dbWarned.LoadOrStore(string(rc.Instance.UID)+"/"+key, true); !loaded {
		rc.Event(corev1.EventTypeWarning, reason, dbEventAction, note, args...)
	}
}

// removeAnnotation removes an annotation of the Forgejo (see removeRetryAnnotation).
func removeAnnotation(ctx context.Context, rc *forgejo.ReconcileContext, key string) error {
	patch, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{key: nil}},
	})
	if err != nil {
		return err
	}
	obj := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: rc.Instance.Namespace, Name: rc.Instance.Name}}
	if err := rc.Client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
		return client.IgnoreNotFound(fmt.Errorf("removing annotation %s: %w", key, err))
	}
	return nil
}

// describe joins what is still running for a message.
func describe(items []string) string {
	if len(items) > 3 {
		items = append(items[:3:3], fmt.Sprintf("%d more", len(items)-3))
	}
	return strings.Join(items, ", ")
}
