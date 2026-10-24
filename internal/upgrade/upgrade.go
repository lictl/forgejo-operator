// Package upgrade orchestrates Forgejo image changes: version gating, pre-upgrade backup,
// rollout verification and automatic rollback.
//
// The state machine lives in status.upgrade so that it survives operator restarts. Each
// reconcile handles the phase read from the API server. Transitions are persisted with the
// status at the end of the reconcile, and objects (ForgejoBackup, ForgejoRestore) are only
// created by the handler of a phase that was already persisted, under names stored in
// status. A lost status write therefore repeats a step instead of skipping one.
package upgrade

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/backup"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejos,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejobackups;forgejorestores,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

const (
	// AnnotationRetry set to "true" on a Forgejo retries an image that failed to upgrade
	// (status.upgrade.failedImage). The operator removes it once it has been read.
	AnnotationRetry = "forgejo.lucas.icu/retry-upgrade"

	// Degraded reasons owned by this package.
	ReasonDowngradeRefused       = "DowngradeRefused"
	ReasonUpgradePathUnsupported = "UpgradePathUnsupported"
	ReasonRolledBack             = "UpgradeRolledBack"
	ReasonFailed                 = "UpgradeFailed"

	reasonSucceeded = "UpgradeSucceeded"
	reasonCancelled = "UpgradeCancelled"
	eventAction     = "Upgrade"
	defaultTimeout  = 15 * time.Minute
)

// Reconcile decides rc.TargetImage, rc.Upgrading and rc.ReplicasOverride, and drives
// status.upgrade and the Upgrading condition.
func Reconcile(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	desired, current := forgejo.Image(f), f.Status.Image
	u := f.Status.Upgrade
	busy := u != nil && inFlight(u.Phase)

	// The retry annotation is consumed only by an idle instance whose failed image is still
	// requested. Otherwise it is removed so that it cannot trigger a retry later. Removal waits
	// for the reconcile after the one that consumed it: patching the object bumps its
	// resourceVersion, which would make this reconcile's (optimistically locked) status write fail.
	retryValue, hasRetry := f.Annotations[AnnotationRetry]
	retry := hasRetry && retryValue == "true" && current != "" && !busy && u != nil &&
		u.FailedImage != "" && u.FailedImage == desired
	if hasRetry && !retry {
		if err := removeRetryAnnotation(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
	}

	var err error
	switch {
	case current == "":
		// First install: there is nothing to upgrade from or to roll back to.
		rc.TargetImage = desired
	case busy:
		err = (&machine{ctx: ctx, rc: rc, u: u}).run(desired)
	default:
		idle(rc, desired, current, retry)
	}
	if err == nil {
		err = stopAfterFailedRestore(ctx, rc, desired)
	}
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	return forgejo.StepResult{}, err
}

func inFlight(p forgejov1.UpgradePhase) bool {
	switch p {
	case forgejov1.UpgradePhasePending, forgejov1.UpgradePhaseBackingUp, forgejov1.UpgradePhaseRollingOut,
		forgejov1.UpgradePhaseVerifying, forgejov1.UpgradePhaseRollingBack:
		return true
	}
	return false
}

// idle handles an instance without an upgrade in flight.
func idle(rc *forgejo.ReconcileContext, desired, current string, retry bool) {
	f := rc.Instance
	u := f.Status.Upgrade
	rc.TargetImage = current

	if u != nil && u.FailedImage != "" && u.FailedImage != desired {
		// spec.image moved on, so the failed image may be tried again later.
		u.FailedImage = ""
	}

	switch {
	case desired == current:
		settle(rc, current)
		return
	case rc.DatabaseUpgrading || forgejo.DatabaseUpgradeHoldsWorkload(f):
		// One upgrade at a time: the PostgreSQL major upgrade stops Forgejo and verifies the
		// running version on the new major. A retry annotation waits as well.
		if u != nil && u.FailedImage == desired {
			hold(rc, u)
			return
		}
		rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, "WaitingForDatabaseUpgrade",
			fmt.Sprintf("The change to %s starts after the PostgreSQL major upgrade (status.database.upgrade)", desired))
		return
	case u != nil && u.FailedImage == desired && retry:
		rc.Event(corev1.EventTypeNormal, "UpgradeRetry", eventAction, "Retrying the upgrade to %s (annotation %s)", desired, AnnotationRetry)
		u.FailedImage = ""
		if keptFailure(u) {
			// The image is still deployed and may have migrated the database already, so a
			// backup now could not bring back the old version: just roll it out again.
			startRetry(rc, current, desired)
			return
		}
	case u != nil && u.FailedImage == desired:
		hold(rc, u)
		return
	}

	switch c := compare(current, f.Status.Version, desired, f.Spec.Upgrade.AllowDowngrade); {
	case c.kind == downgrade && !f.Spec.Upgrade.AllowDowngrade:
		refuse(rc, current, desired, c)
	case c.kind == sameVersion:
		// Same version under another tag, digest or registry: nothing to migrate.
		rollout(rc, desired, "Rollout", fmt.Sprintf("Rolling out %s (same version as %s) without upgrade orchestration", desired, current))
	case c.kind == downgrade:
		// An orchestrated downgrade could never verify: the old version refuses a database
		// migrated by a newer one, and the rollback would restore the newer state. Allowed
		// downgrades go together with a ForgejoRestore of a backup the old version wrote, so the
		// image is deployed as is (also during the restore's maintenance mode).
		msg := fmt.Sprintf("Downgrading from %s to %s without upgrade orchestration (spec.upgrade.allowDowngrade); "+
			"%s only starts on a database restored from a backup it wrote", current, desired, c.to)
		if prev := meta.FindStatusCondition(f.Status.Conditions, forgejov1.ConditionUpgrading); prev == nil || prev.Message != msg {
			rc.Event(corev1.EventTypeWarning, "Downgrade", eventAction, "%s", msg)
		}
		rollout(rc, desired, "Downgrade", msg)
	case c.kind == upgradeVersion && !directUpgrade(c):
		refusePath(rc, current, desired, c)
	case rc.Maintenance:
		// A restore owns the instance; the upgrade starts once it is done.
		rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reasonWaitingForRestore,
			fmt.Sprintf("The upgrade to %s starts after the running restore: %s", desired, rc.MaintenanceReason))
	case rc.SSHHostKeysUnavailable:
		// The pods of the new image could not start (docs/ssh.md): the cold backup would
		// scale Forgejo down for nothing.
		rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reasonWaitingForSSHHostKeys,
			fmt.Sprintf("The upgrade to %s starts once the Secret of SSH host keys is usable again (Degraded/SSHHostKeysUnavailable)", desired))
	case rc.ObjectStorageUnavailable:
		// Forgejo does not start without its bucket: the new version could not be verified,
		// and the rollback would restore the database for nothing.
		rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, "WaitingForObjectStorage",
			fmt.Sprintf("The upgrade to %s starts once the bucket of spec.objectStorage is usable (ObjectStorageReady)", desired))
	default:
		start(rc, current, desired, c)
	}
}

// rollout deploys an image change that needs no orchestration.
func rollout(rc *forgejo.ReconcileContext, image, reason, msg string) {
	rc.TargetImage = image
	clearDegraded(rc)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reason, msg)
}

// settle reports an instance that runs the requested image.
func settle(rc *forgejo.ReconcileContext, current string) {
	u := rc.Instance.Status.Upgrade
	if u != nil && u.Phase == forgejov1.UpgradePhaseFailed && u.ToImage == current {
		// A failed upgrade that left the new image deployed became available after all
		// (e.g. a slow migration that outlived the timeout, or a fixed configuration).
		u.Phase = forgejov1.UpgradePhaseSucceeded
		u.FailedImage = ""
		u.Message = fmt.Sprintf("%s became available after the upgrade had been reported as failed", current)
		rc.Event(corev1.EventTypeNormal, "UpgradeRecovered", eventAction, "%s", u.Message)
	}
	clearDegraded(rc)
	reason, msg := "UpToDate", "Running "+current
	switch {
	case u == nil:
	case u.Phase == forgejov1.UpgradePhaseSucceeded && u.ToImage == current:
		reason, msg = reasonSucceeded, u.Message
	case u.Phase == forgejov1.UpgradePhaseCancelled && u.FromImage == current:
		reason, msg = reasonCancelled, u.Message
	}
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reason, msg)
}

// hold keeps a failed upgrade's outcome in place until spec.image changes or a retry is asked for.
func hold(rc *forgejo.ReconcileContext, u *forgejov1.UpgradeStatus) {
	rc.TargetImage = keptImage(u)
	if forgejo.UpgradeHoldsWorkload(rc.Instance) {
		// The kept image never became available, so its migration may still be running or
		// retrying: it must stay the only pod, as during the upgrade. Once it is available
		// core verifies it, settle turns the upgrade into Succeeded, and the hold ends.
		rc.ReplicasOverride = ptr.To[int32](1)
	}
	reason := ReasonFailed
	if u.Phase == forgejov1.UpgradePhaseRolledBack {
		reason = ReasonRolledBack
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, reason, u.Message)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, reason,
		fmt.Sprintf("%s Choose another image, or set the annotation %s=\"true\" to retry %s.", u.Message, AnnotationRetry, u.ToImage))
}

// keptImage is the image deployed after a terminal phase.
func keptImage(u *forgejov1.UpgradeStatus) string {
	if u.Phase == forgejov1.UpgradePhaseSucceeded || keptFailure(u) {
		return u.ToImage
	}
	return u.FromImage
}

// keptFailure reports a failed upgrade that left the new image deployed: it ran without a
// rollback, and its migrations may have changed the database, which the old version refuses
// to start on.
func keptFailure(u *forgejov1.UpgradeStatus) bool {
	return u.Phase == forgejov1.UpgradePhaseFailed && u.RolloutStartTime != nil && u.RestoreName == ""
}

// refuse blocks a downgrade. The Warning event is emitted once per refused image.
func refuse(rc *forgejo.ReconcileContext, current, desired string, c comparison) {
	msg := fmt.Sprintf("Refusing to downgrade from %s (%s) to %s (%s): Forgejo cannot migrate a database backwards. "+
		"Choose version %s or newer, or set spec.upgrade.allowDowngrade and restore a backup taken with the older version.",
		current, c.from, desired, c.to, c.from)
	if prev := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionUpgrading); prev == nil ||
		prev.Reason != ReasonDowngradeRefused || prev.Message != msg {
		rc.Event(corev1.EventTypeWarning, ReasonDowngradeRefused, eventAction, "%s", msg)
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonDowngradeRefused, msg)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, ReasonDowngradeRefused, msg)
}

// directUpgrade reports whether the version table allows upgrading between the majors of c
// in one step (forgejo.VersionTable.DirectUpgrade).
func directUpgrade(c comparison) bool {
	ok, _ := forgejo.Versions.DirectUpgrade(c.from.Major, c.to.Major)
	return ok
}

// refusePath blocks an upgrade that skips more majors than the version table allows, like
// a downgrade: the current image keeps running and one Warning event is emitted.
func refusePath(rc *forgejo.ReconcileContext, current, desired string, c comparison) {
	_, via := forgejo.Versions.DirectUpgrade(c.from.Major, c.to.Major)
	msg := fmt.Sprintf("Refusing to upgrade from %s (%s) to %s (%s) in one step: the operator upgrades to Forgejo %d "+
		"only from %d or newer. Upgrade to Forgejo %d first (its latest patch release), then to %s. See docs/versions.md.",
		current, c.from, desired, c.to, c.to.Major, via, via, desired)
	if prev := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionUpgrading); prev == nil ||
		prev.Reason != ReasonUpgradePathUnsupported || prev.Message != msg {
		rc.Event(corev1.EventTypeWarning, ReasonUpgradePathUnsupported, eventAction, "%s", msg)
	}
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonUpgradePathUnsupported, msg)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, ReasonUpgradePathUnsupported, msg)
}

// warnTarget emits Warning events about the version an upgrade goes to: one outside the
// support policy (the upgrade still proceeds), and app.ini values that the new major refuses
// to start with (forgejo.Traits.StrictDurations), which would make the upgrade fail.
func warnTarget(rc *forgejo.ReconcileContext, to string, c comparison) {
	switch s := forgejo.Versions.Classify(c.to, c.toOK, time.Now()); s.Level {
	case forgejo.SupportDeprecated, forgejo.SupportEndOfLife, forgejo.SupportFuture:
		rc.Event(corev1.EventTypeWarning, reasonUnsupportedVersion, eventAction, "Upgrading to %s anyway: %s", to, s.Message)
	}
	// An unknown running version (a digest before Forgejo reported one) counts as lenient: it
	// may be 15 or 16, and a warning that turns out unnecessary is cheaper than a failed upgrade.
	fromStrict := c.fromOK && forgejo.Versions.Traits(c.from, true).StrictDurations
	if forgejo.Versions.Traits(c.to, c.toOK).StrictDurations && !fromStrict {
		if bad := forgejo.InvalidDurations(rc.Instance.Spec.Config); len(bad) > 0 {
			rc.Event(corev1.EventTypeWarning, reasonConfigIncompatible, eventAction,
				"%s refuses to start with %s in spec.config: these must be durations with a unit (e.g. 30s, -1s). "+
					"Fix them, or the upgrade fails and is rolled back.", to, strings.Join(bad, ", "))
		}
	}
}

// reasonConfigIncompatible: spec.config has values the target version refuses.
const reasonConfigIncompatible = "ConfigIncompatible"

// start records a new upgrade in phase Pending. Nothing changes in the cluster until the
// Pending phase has been persisted: the next reconcile stops Forgejo for the backup or, without
// a backup, deploys the new image together with rolloutStartTime.
func start(rc *forgejo.ReconcileContext, from, to string, c comparison) {
	u := begin(rc, from, to, forgejo.BoolOr(rc.Instance.Spec.Upgrade.BackupBeforeUpgrade, true))

	switch c.kind {
	case unknownVersion:
		if c.fromReported && c.toOK {
			rc.Event(corev1.EventTypeWarning, "UnverifiedVersion", eventAction,
				"The version of %s is only known from what Forgejo reports (%s), which can lag behind an image change; "+
					"upgrading to %s (%s) with orchestration instead of a plain rollout", from, c.from, to, c.to)
		} else {
			rc.Event(corev1.EventTypeWarning, "UnverifiedVersion", eventAction,
				"Cannot compare the versions of %s and %s (digest or non-version tag); upgrading without the downgrade check", from, to)
		}
	case upgradeVersion:
		switch {
		case c.to.Major > c.from.Major+1:
			skipped := strconv.Itoa(c.from.Major + 1)
			if c.to.Major > c.from.Major+2 {
				skipped += "-" + strconv.Itoa(c.to.Major-1)
			}
			rc.Event(corev1.EventTypeNormal, "MajorUpgrade", eventAction,
				"Upgrading across major versions from %d to %d, skipping %s; Forgejo migrates the database forward "+
					"through every version in one step", c.from.Major, c.to.Major, skipped)
		case c.to.Major > c.from.Major:
			rc.Event(corev1.EventTypeNormal, "MajorUpgrade", eventAction,
				"Upgrading across major versions from %d to %d; Forgejo migrates the database forward", c.from.Major, c.to.Major)
		}
	}
	warnTarget(rc, to, c)
	rc.Event(corev1.EventTypeNormal, "UpgradeStarted", eventAction, "Upgrading from %s to %s", from, to)
	(&machine{rc: rc, u: u}).apply(to)
}

// startRetry rolls out a failed image that is still deployed once more, without a backup.
func startRetry(rc *forgejo.ReconcileContext, from, to string) {
	u := begin(rc, from, to, false)
	u.Retry = true
	u.Message = fmt.Sprintf("Retrying the rollout of %s without a pre-upgrade backup: it was left deployed after it failed", to)
	(&machine{rc: rc, u: u}).apply(to)
}

// begin records a new upgrade in phase Pending.
func begin(rc *forgejo.ReconcileContext, from, to string, withBackup bool) *forgejov1.UpgradeStatus {
	f := rc.Instance
	now := metav1.Now()
	u := &forgejov1.UpgradeStatus{
		Phase:     forgejov1.UpgradePhasePending,
		FromImage: from,
		ToImage:   to,
		StartTime: &now,
		Message:   fmt.Sprintf("Upgrade from %s to %s is starting", from, to),
	}
	if withBackup {
		u.BackupName = objectName(f, "pre-upgrade", now)
	}
	f.Status.Upgrade = u
	clearDegraded(rc)
	rc.Requeue(time.Second)
	return u
}

// stopAfterFailedRestore keeps Forgejo at zero replicas after a rollback whose restore failed,
// while the failed image is still requested: the database may hold the new version's
// (possibly half-migrated) schema, or the data volume a half-extracted archive, and the old
// version must not serve users on that. A ForgejoRestore created after the failure lifts the
// hold for good once it has restored the data (ScalingUp, when it waits for Forgejo to come
// back). Changing spec.image lifts it too: setting it back to status.image starts the old
// version without a restore.
func stopAfterFailedRestore(ctx context.Context, rc *forgejo.ReconcileContext, desired string) error {
	f := rc.Instance
	u := f.Status.Upgrade
	if u == nil || u.Phase != forgejov1.UpgradePhaseFailed || !u.RollbackRestoreFailed || u.RestoreName == "" ||
		u.CompletionTime == nil || desired != u.ToImage {
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
		u.Message = fmt.Sprintf("Upgrade to %s failed and its automatic rollback failed; ForgejoRestore %s then restored backup %s, so %s runs again.",
			u.ToImage, r.Name, r.Spec.BackupName, u.FromImage)
		rc.Event(corev1.EventTypeNormal, "UpgradeRollbackRestored", eventAction, "%s", u.Message)
		hold(rc, u)
		return nil
	}
	rc.ReplicasOverride = ptr.To[int32](0)
	msg := fmt.Sprintf("%s Forgejo is kept stopped until a ForgejoRestore has restored a backup taken with %s, "+
		"or spec.image is changed (setting it back to %s starts it without a restore).", u.Message, u.FromImage, u.FromImage)
	rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionTrue, ReasonFailed, msg)
	rc.SetCondition(forgejov1.ConditionUpgrading, metav1.ConditionFalse, ReasonFailed, msg)
	return nil
}

// objectName derives the pre-upgrade backup and rollback restore names from the upgrade's
// start time (second precision, as persisted), so they are stable across reconciles.
func objectName(f *forgejov1.Forgejo, kind string, startedAt metav1.Time) string {
	return fmt.Sprintf("%s-%s-%s", f.Name, kind, startedAt.UTC().Format("20060102150405"))
}

// clearDegraded resets Degraded only when this package set it.
func clearDegraded(rc *forgejo.ReconcileContext) {
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionDegraded)
	if c == nil || c.Status != metav1.ConditionTrue {
		return
	}
	switch c.Reason {
	case ReasonDowngradeRefused, ReasonUpgradePathUnsupported, ReasonRolledBack, ReasonFailed:
		rc.SetCondition(forgejov1.ConditionDegraded, metav1.ConditionFalse, "AsExpected", "")
	}
}

func removeRetryAnnotation(ctx context.Context, rc *forgejo.ReconcileContext) error {
	return removeAnnotation(ctx, rc, AnnotationRetry)
}

type changeKind int

const (
	upgradeVersion changeKind = iota
	sameVersion
	downgrade
	unknownVersion
)

type comparison struct {
	kind     changeKind
	from, to forgejo.Version
	// fromOK and toOK report whether from and to are known.
	fromOK, toOK bool
	// fromReported: from is the version Forgejo reports (status.version), since the running
	// image's reference carries none (a digest or a custom tag).
	fromReported bool
}

// compare classifies an image change by version. Floating tags compare equal to every
// version they cover ("16" == "16.0.6"). The target's version comes from its tag. The running
// one comes from the tag of status.image, or, when that carries no version (a digest or a
// custom tag such as "prod"), from the version Forgejo reports (status.version), so that the
// downgrade and upgrade-path checks also apply to such images.
//
// A reported version can lag behind an image change until the next health check reads it
// again, so it only ever refuses: a change that it alone would let through without
// orchestration (the same version, or an allowed downgrade) is orchestrated like an
// unknown version, as it was before Forgejo reported anything.
func compare(from, reported, to string, allowDowngrade bool) comparison {
	fv, okFrom := forgejo.ImageVersion(from)
	fromReported := false
	if !okFrom {
		fv, okFrom = forgejo.InstanceVersion(from, reported)
		fromReported = okFrom
	}
	tv, okTo := forgejo.ImageVersion(to)
	c := comparison{from: fv, to: tv, fromOK: okFrom, toOK: okTo, fromReported: fromReported}
	switch {
	case !okFrom || !okTo:
		c.kind = unknownVersion
	case tv.Compare(fv) < 0:
		c.kind = downgrade
	case tv.Compare(fv) == 0:
		c.kind = sameVersion
	default:
		c.kind = upgradeVersion
	}
	if fromReported && (c.kind == sameVersion || c.kind == downgrade && allowDowngrade) {
		c.kind = unknownVersion
	}
	return c
}
