package backup

// Disaster recovery bootstrap (spec.restoreFrom): a new instance is created from a remote
// backup before Forgejo starts for the first time.
//
//  1. maintenance: the instance is in maintenance from the first pass (no pod ever starts on
//     an empty database), and no backup is taken.
//  2. restore-identity (before secrets): the operator reads identity.json of the backup with
//     its own S3 client and creates "<name>-secrets", "<name>-admin" and
//     "<name>-ssh-host-keys" from it, so the secrets and ssh-host-keys steps find them and
//     generate nothing. Existing Secrets are never overwritten.
//  3. restore-from (after storage, so the database is ready): the operator creates
//     ForgejoRestore "<name>-restore-from" for the backup, which restores the database and the
//     data volume like any restore, and brings Forgejo up (the first pod migrates alone). It
//     refuses to when the database or the data volume existed before the instance
//     (TargetNotEmpty): a restore would replace data a previous instance left.
//  4. When that restore has restored everything (ScalingUp), the operator records it in the
//     annotation forgejo.lucas.icu/restored-from of "<name>-secrets" before Forgejo
//     starts, and status.restoredFrom.phase is Completed for good. The annotation outlives
//     status (and, with retainPolicy Retain, the Forgejo object): a re-created instance or a
//     lost status finds it and never restores the backup again over live data.

import (
	"bytes"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// +kubebuilder:rbac:groups=forgejo.lucas.icu,resources=forgejorestores,verbs=get;list;watch;create

const (
	// bootstrapRetry is how often a bootstrap step that waits is retried.
	bootstrapRetry = 30 * time.Second
	// remoteReadTimeout bounds the operator's reads of a remote backup in one pass.
	remoteReadTimeout = 30 * time.Second
	// identityRetryMax caps the backoff between reads of spec.restoreFrom after transient
	// failures; it starts at bootstrapRetry and doubles.
	identityRetryMax = 5 * time.Minute
)

// identityReadFailure is the last transient failure to read spec.restoreFrom of an instance.
// Until retryAt the step answers with it instead of reading the bucket again, so an endpoint
// that does not answer does not hold the Forgejo controller's worker on every pass.
type identityReadFailure struct {
	// source identifies spec.restoreFrom.remote as it was read; another source reads at once.
	source      string
	reason, msg string
	backoff     time.Duration
	retryAt     time.Time
}

// identityReadFailures maps types.NamespacedName to *identityReadFailure.
var identityReadFailures sync.Map

func forgetIdentityReadFailure(namespace, name string) {
	identityReadFailures.Delete(types.NamespacedName{Namespace: namespace, Name: name})
}

// restoreFromSource identifies src for identityReadFailure: every field that changes where or
// how the backup is read (endpoint, bucket, prefix, path, Secrets, CA).
func restoreFromSource(src *forgejov1.RemoteBackupSource) string {
	data, err := json.Marshal(src)
	if err != nil {
		return fmt.Sprintf("%+v", *src)
	}
	return string(data)
}

// Bootstrapping reports whether f is still being created from spec.restoreFrom. Until the
// bootstrap completes no backup of the instance is taken.
func Bootstrapping(f *forgejov1.Forgejo) bool { return forgejo.Bootstrapping(f) }

// BootstrapRestoreName is the ForgejoRestore that restores the database and the data volume of
// spec.restoreFrom.
func BootstrapRestoreName(f *forgejov1.Forgejo) string { return f.Name + "-restore-from" }

// RestoreSource names what a restore restores, for messages.
func RestoreSource(r *forgejov1.ForgejoRestore) string {
	if r.Spec.Remote != nil {
		return "remote backup " + RemoteLocation(&r.Spec.Remote.S3, r.Spec.Remote.Path)
	}
	return "backup " + r.Spec.BackupName
}

// restoreFromLocation is the location of spec.restoreFrom, as the restored-from annotation
// records it.
func restoreFromLocation(f *forgejov1.Forgejo) string {
	src := &f.Spec.RestoreFrom.Remote
	return RemoteLocation(&src.S3, src.Path)
}

// bootstrapHold explains why spec.restoreFrom keeps the instance in maintenance, or returns ""
// once the bootstrap is complete. Forgejo does not start before the restore is recorded on
// "<name>-secrets" (status.restoredFrom Completed), which the restore-from step does in the
// pass that sees the restore reach ScalingUp. failed is true when the bootstrap failed and
// only a person can end the hold (the restore failed, or the target is not empty).
func bootstrapHold(f *forgejov1.Forgejo, restores []forgejov1.ForgejoRestore) (reason string, failed bool) {
	if !Bootstrapping(f) {
		return "", false
	}
	where := restoreFromLocation(f)
	for i := range restores {
		r := &restores[i]
		if r.Name != BootstrapRestoreName(f) || !r.DeletionTimestamp.IsZero() {
			continue
		}
		switch r.Status.Phase {
		case forgejov1.RestorePhaseScalingUp, forgejov1.RestorePhaseCompleted:
			return fmt.Sprintf("ForgejoRestore %s restored %s; Forgejo starts once that is recorded on Secret %s",
				r.Name, where, forgejo.SecretsName(f)), false
		case forgejov1.RestorePhaseFailed:
			return fmt.Sprintf("ForgejoRestore %s of spec.restoreFrom failed: %s. Delete it to try again, or remove "+
				"spec.restoreFrom to start Forgejo without the backup", r.Name, r.Status.Message), true
		}
	}
	if st := f.Status.RestoredFrom; st != nil && st.Phase == forgejov1.RestoredFromFailed && st.Message != "" {
		return st.Message, true
	}
	return fmt.Sprintf("Restoring the instance from %s (spec.restoreFrom) before Forgejo starts", where), false
}

// restoredMarker reads the restored-from annotation of "<name>-secrets" ("" without one).
func restoredMarker(ctx context.Context, rc *forgejo.ReconcileContext) (string, error) {
	f := rc.Instance
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("reading Secret %s: %w", forgejo.SecretsName(f), err)
	}
	return sec.Annotations[forgejo.AnnotationRestoredFrom], nil
}

// recoverBootstrap completes the bootstrap of an instance whose "<name>-secrets" records that
// spec.restoreFrom was restored already, while its status does not say so (the status was
// lost, or the Forgejo was deleted and created again with retainPolicy Retain): the backup is
// never restored twice over the data Forgejo has written since.
func recoverBootstrap(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	marker, err := restoredMarker(ctx, rc)
	if err != nil || marker == "" || marker != restoreFromLocation(f) {
		return err
	}
	completeBootstrap(rc, fmt.Sprintf("Restored from %s earlier: Secret %s records it (annotation %s). spec.restoreFrom has no "+
		"further effect; remove it", marker, forgejo.SecretsName(f), forgejo.AnnotationRestoredFrom))
	return nil
}

// completeBootstrap ends the bootstrap: status.restoredFrom Completed, RestoredFromBackup True.
func completeBootstrap(rc *forgejo.ReconcileContext, msg string) {
	f := rc.Instance
	st := f.Status.RestoredFrom
	if st == nil {
		st = &forgejov1.RestoredFromStatus{Location: restoreFromLocation(f)}
		f.Status.RestoredFrom = st
	}
	now := metav1.Now()
	st.Phase, st.Message = forgejov1.RestoredFromCompleted, msg
	if st.CompletionTime == nil {
		st.CompletionTime = &now
	}
	rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionTrue, "Restored", msg)
	rc.Event(corev1.EventTypeNormal, "RestoredFromBackup", "RestoreFrom", "%s", msg)
}

// markRestored records on "<name>-secrets" that spec.restoreFrom was restored (see
// recoverBootstrap). It runs before Forgejo starts on the restored data. First the data volume
// and the CNPG cluster are stamped with the restored identity (forgejo.AnnotationIdentity), and
// what ran on them before is forgotten (forgejo.AnnotationRunningImage): they now hold the
// backup's data, also when they existed before the instance (restore-from-overwrite).
func markRestored(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec); err != nil {
		return fmt.Errorf("recording the restore of spec.restoreFrom on Secret %s: %w", forgejo.SecretsName(f), err)
	}
	where := restoreFromLocation(f)
	if sec.Annotations[forgejo.AnnotationRestoredFrom] == where {
		return nil
	}
	data, err := forgejo.ReadDataObjects(ctx, rc)
	if err != nil {
		return err
	}
	for _, o := range data.Present() {
		if err := forgejo.ResetIdentity(ctx, rc.Client, o.Object, forgejo.SecretKeyFingerprint(sec.Data)); err != nil {
			return fmt.Errorf("recording the restored identity on %s %s: %w", o.Kind, o.Object.GetName(), err)
		}
	}
	orig := sec.DeepCopy()
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	sec.Annotations[forgejo.AnnotationRestoredFrom] = where
	if err := rc.Client.Patch(ctx, sec, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return fmt.Errorf("recording the restore of spec.restoreFrom on Secret %s: %w", sec.Name, err)
	}
	return nil
}

// targetInUse explains why spec.restoreFrom must not be restored into this instance, or
// returns "": Secret "<name>-secrets" records an earlier restore of another backup, or the
// CNPG cluster or the data volume existed before this Forgejo (kept by retainPolicy Retain from
// an instance of the same name), so they may hold data the restore would replace. The
// annotation forgejo.lucas.icu/restore-from-overwrite=true on the Forgejo overrides it.
func targetInUse(ctx context.Context, rc *forgejo.ReconcileContext, marker string) (string, error) {
	f := rc.Instance
	if f.Annotations[forgejo.AnnotationRestoreFromOverwrite] == valueTrue {
		return "", nil
	}
	if marker != "" {
		return fmt.Sprintf("Secret %s records that this instance was restored from %s and has run since",
			forgejo.SecretsName(f), marker), nil
	}
	var older []string
	if forgejo.DatabaseType(f) != forgejov1.DatabaseTypeExternal && rc.HasAPI(forgejo.CNPGClusterGVK) {
		cluster := &unstructured.Unstructured{}
		cluster.SetGroupVersionKind(forgejo.CNPGClusterGVK)
		err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DatabaseClusterName(f)}, cluster)
		switch {
		case err == nil:
			if ts := cluster.GetCreationTimestamp(); ts.Before(&f.CreationTimestamp) {
				older = append(older, fmt.Sprintf("CNPG cluster %s (created %s)", cluster.GetName(), ts.UTC().Format(time.RFC3339)))
			}
		case !apierrors.IsNotFound(err):
			return "", fmt.Errorf("reading CNPG cluster %s: %w", forgejo.DatabaseClusterName(f), err)
		}
	}
	if f.Spec.Storage.ExistingClaim == "" {
		pvc := &corev1.PersistentVolumeClaim{}
		err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.DataPVCName(f)}, pvc)
		switch {
		case err == nil:
			if pvc.CreationTimestamp.Before(&f.CreationTimestamp) {
				older = append(older, fmt.Sprintf("PVC %s (created %s)", pvc.Name, pvc.CreationTimestamp.UTC().Format(time.RFC3339)))
			}
		case !apierrors.IsNotFound(err):
			return "", fmt.Errorf("reading PVC %s: %w", forgejo.DataPVCName(f), err)
		}
	}
	if len(older) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%s existed before this Forgejo (created %s) and may hold data that the restore would replace",
		strings.Join(older, " and "), f.CreationTimestamp.UTC().Format(time.RFC3339)), nil
}

// ReconcileRestoreIdentity restores the identity Secrets of spec.restoreFrom before the secrets
// step would generate new ones. It stops (and with it everything that needs the secrets) while
// the backup cannot be read or an existing Secret differs from the backup's.
func ReconcileRestoreIdentity(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if f.Spec.RestoreFrom == nil {
		forgetIdentityReadFailure(f.Namespace, f.Name)
		meta.RemoveStatusCondition(&f.Status.Conditions, forgejov1.ConditionRestoredFromBackup)
		return forgejo.StepResult{}, nil
	}
	st := f.Status.RestoredFrom
	if !Bootstrapping(f) || (st != nil && st.IdentityRestoredTime != nil) {
		forgetIdentityReadFailure(f.Namespace, f.Name)
		return forgejo.StepResult{}, nil
	}
	src := &f.Spec.RestoreFrom.Remote
	if st == nil {
		st = &forgejov1.RestoredFromStatus{}
		f.Status.RestoredFrom = st
	}
	st.Phase = forgejov1.RestoredFromRestoringIdentity
	st.Location = RemoteLocation(&src.S3, src.Path)

	wait := func(reason, msg string) forgejo.StepResult {
		if st.Message != msg {
			rc.Event(corev1.EventTypeWarning, reason, "RestoreFrom", "%s", msg)
		}
		st.Message = msg
		rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, reason, msg)
		return forgejo.StepResult{Stop: true, RequeueAfter: bootstrapRetry}
	}
	// After a transient failure the bucket is read again only once the backoff has run out
	// (30 s doubling to 5 min); meanwhile the step reports that failure unchanged.
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	source := restoreFromSource(src)
	readAt := time.Now()
	var last *identityReadFailure
	if v, ok := identityReadFailures.Load(key); ok && v.(*identityReadFailure).source == source {
		last = v.(*identityReadFailure)
		if readAt.Before(last.retryAt) {
			res := wait(last.reason, last.msg)
			res.RequeueAfter = last.retryAt.Sub(readAt)
			return res, nil
		}
	}
	failed := func(reason, msg string, err error) forgejo.StepResult {
		if !TransientRemoteError(err) {
			identityReadFailures.Delete(key)
			return wait(reason, msg)
		}
		backoff := bootstrapRetry
		if last != nil {
			backoff = min(2*last.backoff, identityRetryMax)
		}
		identityReadFailures.Store(key, &identityReadFailure{source: source, reason: reason, msg: msg,
			backoff: backoff, retryAt: readAt.Add(backoff)})
		res := wait(reason, msg)
		res.RequeueAfter = backoff
		return res
	}
	cctx, cancel := context.WithTimeout(ctx, remoteReadTimeout)
	defer cancel()
	store, err := OpenStore(cctx, rc.Client, f.Namespace, &src.S3, SourceKeyRef(f, src))
	if err != nil {
		return failed("RemoteBackupUnavailable", fmt.Sprintf("Cannot read the backup at %s: %v", st.Location, err), err), nil
	}
	m, err := store.ReadManifest(cctx, src.Path)
	if err != nil {
		return failed("RemoteBackupUnavailable", fmt.Sprintf("Cannot read the backup at %s: %v", st.Location, err), err), nil
	}
	id, err := store.ReadIdentity(cctx, src.Path, m)
	if err != nil {
		return failed("RemoteBackupUnavailable", fmt.Sprintf("Cannot read the identity of the backup at %s: %v", st.Location, err), err), nil
	}
	identityReadFailures.Delete(key)
	st.BackupName, st.ForgejoImage, st.InstanceUID = m.Backup, m.ForgejoImage, m.ForgejoUID
	if t := m.CompletedTime(); !t.IsZero() {
		st.BackupCompletionTime = &metav1.Time{Time: t}
	}
	restored, err := restoreIdentitySecrets(ctx, rc, id)
	if err != nil {
		return wait("IdentityConflict", err.Error()), nil
	}
	now := metav1.Now()
	st.IdentityRestoredTime = &now
	st.Phase = forgejov1.RestoredFromRestoringData
	st.Message = fmt.Sprintf("Restored the identity of %s/%s (Secrets %v) from backup %s; the database and the data volume are restored next",
		m.Namespace, m.Forgejo, restored, m.Backup)
	rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "RestoringData", st.Message)
	rc.Event(corev1.EventTypeNormal, "IdentityRestored", "RestoreFrom", "%s", st.Message)
	return forgejo.StepResult{}, nil
}

// restoreIdentitySecrets creates the instance's identity Secrets from id. A Secret that exists
// already is kept when it holds the backup's values, and is a conflict otherwise: the operator
// never overwrites a Secret. Secrets the instance takes from elsewhere (admin.existingSecret,
// hostKeys.existingSecret) are not created. It returns the names of the Secrets in use.
func restoreIdentitySecrets(ctx context.Context, rc *forgejo.ReconcileContext, id *Identity) ([]string, error) {
	f := rc.Instance
	targets := []struct {
		role, name string
		skip       bool
	}{
		{IdentityRoleSecrets, forgejo.SecretsName(f), false},
		{IdentityRoleAdmin, forgejo.AdminSecretName(f), f.Spec.Admin.ExistingSecret != ""},
		{IdentityRoleSSHHostKeys, forgejo.SSHHostKeysSecretName(f), !forgejo.SSHHostKeysManaged(f) ||
			forgejo.SSHHostKeysSecret(f) != forgejo.SSHHostKeysSecretName(f)},
	}
	var names []string
	for _, t := range targets {
		src := id.Secret(t.role)
		if t.skip || src == nil {
			continue
		}
		existing := &corev1.Secret{}
		err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: t.name}, existing)
		switch {
		case err == nil:
			if !containsData(existing.Data, src.Data) {
				return nil, fmt.Errorf("secret %s exists with other values than the backup's %s; delete it to restore the backup's, "+
					"or remove spec.restoreFrom", t.name, src.Name)
			}
		case apierrors.IsNotFound(err):
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: t.name, Namespace: f.Namespace,
					Labels: forgejo.Labels(f, forgejo.ComponentServer), Annotations: src.Annotations},
				Type: src.Type,
				Data: src.Data,
			}
			if sec.Type == "" {
				sec.Type = corev1.SecretTypeOpaque
			}
			// Identity Secrets follow retainPolicy, as when the secrets step creates them.
			if rc.OwnerRefsEnabled() {
				if err := controllerutil.SetControllerReference(f, sec, rc.Scheme); err != nil {
					return nil, err
				}
			}
			if err := rc.Client.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("creating Secret %s: %w", t.name, err)
			}
		default:
			return nil, fmt.Errorf("reading Secret %s: %w", t.name, err)
		}
		names = append(names, t.name)
	}
	return names, nil
}

// containsData reports whether have holds every key of want with the same value.
func containsData(have, want map[string][]byte) bool {
	for k, v := range want {
		if !bytes.Equal(have[k], v) {
			return false
		}
	}
	return true
}

// ReconcileBootstrap creates the ForgejoRestore of spec.restoreFrom once the identity is restored
// and the database is ready, unless the database or the data volume may already hold data, and
// follows it into status.restoredFrom and RestoredFromBackup. Once the restore has restored
// everything, it records that on "<name>-secrets" before Forgejo starts.
func ReconcileBootstrap(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	st := f.Status.RestoredFrom
	if f.Spec.RestoreFrom == nil || st == nil || st.IdentityRestoredTime == nil || st.Phase == forgejov1.RestoredFromCompleted {
		return forgejo.StepResult{}, nil
	}
	name := BootstrapRestoreName(f)
	st.RestoreName = name
	rs := &forgejov1.ForgejoRestore{}
	err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, rs)
	if apierrors.IsNotFound(err) {
		if !rc.DatabaseReady {
			return forgejo.StepResult{RequeueAfter: 10 * time.Second}, nil
		}
		marker, err := restoredMarker(ctx, rc)
		if err != nil {
			return forgejo.StepResult{}, err
		}
		if marker == restoreFromLocation(f) {
			// The restore reached ScalingUp and was deleted before its completion was recorded:
			// Forgejo has run on the restored data since.
			completeBootstrap(rc, fmt.Sprintf("Restored from %s (backup %s); ForgejoRestore %s is gone", st.Location, st.BackupName, name))
			return forgejo.StepResult{}, nil
		}
		why, err := targetInUse(ctx, rc, marker)
		if err != nil {
			return forgejo.StepResult{}, err
		}
		if why != "" {
			msg := fmt.Sprintf("spec.restoreFrom was not restored: %s. Remove spec.restoreFrom to start the instance on its existing "+
				"data, or set the annotation %s=true on the Forgejo to replace it with the backup", why, forgejo.AnnotationRestoreFromOverwrite)
			if st.Phase != forgejov1.RestoredFromFailed || st.Message != msg {
				rc.Event(corev1.EventTypeWarning, "TargetNotEmpty", "RestoreFrom", "%s", msg)
			}
			st.Phase, st.Message = forgejov1.RestoredFromFailed, msg
			rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "TargetNotEmpty", msg)
			return forgejo.StepResult{RequeueAfter: bootstrapRetry}, nil
		}
		return forgejo.StepResult{}, createBootstrapRestore(ctx, rc, name)
	}
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if !metav1.IsControlledBy(rs, f) {
		msg := fmt.Sprintf("ForgejoRestore %s exists and does not belong to this instance; delete it so that spec.restoreFrom can be restored", name)
		st.Phase, st.Message = forgejov1.RestoredFromFailed, msg
		rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "RestoreConflict", msg)
		return forgejo.StepResult{RequeueAfter: bootstrapRetry}, nil
	}
	switch rs.Status.Phase {
	case forgejov1.RestorePhaseScalingUp, forgejov1.RestorePhaseCompleted:
		// Recorded before Forgejo starts (bootstrapHold holds it until then).
		if err := markRestored(ctx, rc); err != nil {
			return forgejo.StepResult{}, err
		}
		completeBootstrap(rc, fmt.Sprintf("Restored from %s (backup %s); ForgejoRestore %s is %s: %s",
			st.Location, st.BackupName, name, rs.Status.Phase, rs.Status.Message))
	case forgejov1.RestorePhaseFailed:
		msg := fmt.Sprintf("ForgejoRestore %s failed: %s. Delete it to try again, or remove spec.restoreFrom to start Forgejo without the backup",
			name, rs.Status.Message)
		if st.Phase != forgejov1.RestoredFromFailed {
			rc.Event(corev1.EventTypeWarning, "RestoreFromFailed", "RestoreFrom", "%s", msg)
		}
		st.Phase, st.Message = forgejov1.RestoredFromFailed, msg
		rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "RestoreFailed", msg)
	default:
		st.Phase = forgejov1.RestoredFromRestoringData
		st.Message = fmt.Sprintf("ForgejoRestore %s is %s: %s", name, phaseOr(rs.Status.Phase), rs.Status.Message)
		rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "RestoringData", st.Message)
	}
	return forgejo.StepResult{}, nil
}

func phaseOr(p forgejov1.RestorePhase) string {
	if p == "" {
		return string(forgejov1.RestorePhasePending)
	}
	return string(p)
}

func createBootstrapRestore(ctx context.Context, rc *forgejo.ReconcileContext, name string) error {
	f := rc.Instance
	src := f.Spec.RestoreFrom.Remote.DeepCopy()
	// The key is pinned, so that the restore reads the backup with the key the identity was
	// read with, whatever spec.backup.remote says later.
	key := SourceKeyRef(f, src)
	src.KeySecret = &key
	rs := &forgejov1.ForgejoRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace,
			Labels: map[string]string{forgejo.LabelForgejo: f.Name}},
		Spec: forgejov1.ForgejoRestoreSpec{ForgejoRef: forgejov1.ForgejoRef{Name: f.Name}, Remote: src},
	}
	if err := controllerutil.SetControllerReference(f, rs, rc.Scheme); err != nil {
		return err
	}
	if err := rc.Client.Create(ctx, rs); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ForgejoRestore %s: %w", name, err)
	}
	st := f.Status.RestoredFrom
	st.Phase = forgejov1.RestoredFromRestoringData
	st.Message = fmt.Sprintf("ForgejoRestore %s restores the database and the data volume from %s", name, st.Location)
	rc.SetCondition(forgejov1.ConditionRestoredFromBackup, metav1.ConditionFalse, "RestoringData", st.Message)
	rc.Event(corev1.EventTypeNormal, "RestoreFromStarted", "RestoreFrom", "%s", st.Message)
	return nil
}
