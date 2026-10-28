package forgejo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

const (
	// AnnotationIdentity on the data volume and the CNPG cluster is the SecretKeyFingerprint of
	// the instance they were created for (or adopted by). A volume or cluster of another identity
	// is never used (condition Broken); one without the annotation is adopted.
	AnnotationIdentity = "forgejo.lucas.icu/identity"
	// AnnotationRecreateDatabase=true on the Forgejo lets the operator create the CNPG cluster of
	// an instance that has run again, empty, to restore a backup into it (BrokenDatabaseMissing
	// otherwise). Forgejo stays stopped meanwhile (BrokenDatabaseRestorePending) until a
	// ForgejoRestore of the database created after that cluster has restored a backup; then the
	// operator removes the annotation.
	AnnotationRecreateDatabase = "forgejo.lucas.icu/recreate-database"
	// AnnotationRestorePending=true on a CNPG cluster marks one the operator created again,
	// empty, for AnnotationRecreateDatabase, until a restore of the database into it. The
	// annotation on the Forgejo has no effect on a cluster without it.
	AnnotationRestorePending = "forgejo.lucas.icu/restore-pending"
	// AnnotationRunningImage on the data volume and the CNPG cluster is the last image verified
	// running on them (status.image). It shows that the instance has run, also once status is
	// gone, and status.image is recovered from it.
	AnnotationRunningImage = "forgejo.lucas.icu/running-image"
)

// Reasons of condition Broken.
const (
	BrokenSecretsMissing    = "SecretsMissing"
	BrokenDataVolumeMissing = "DataVolumeMissing"
	BrokenDatabaseMissing   = "DatabaseMissing"
	BrokenIdentityMismatch  = "IdentityMismatch"
	// BrokenDatabaseUpgradeFailed: a PostgreSQL major upgrade failed after pg_upgrade may have
	// changed the data, or Forgejo did not start on the new major; Forgejo stays stopped.
	BrokenDatabaseUpgradeFailed = "DatabaseUpgradeFailed"
	// BrokenDatabaseRestorePending: AnnotationRecreateDatabase created the CNPG cluster again,
	// empty; Forgejo stays stopped until a restore of the database into it.
	BrokenDatabaseRestorePending = "DatabaseRestorePending"
)

// IdentitySecretKeys are the keys of "<name>-secrets" that Forgejo encrypts and signs with.
// They are generated for a new instance only: new values would leave two-factor secrets,
// Actions secrets and mirror credentials undecryptable and invalidate every token. The other
// keys (the metrics token, the salts) may be generated when missing.
var IdentitySecretKeys = []string{SecretKeySecretKey, SecretKeyInternalToken, SecretKeyJWTSecret, SecretKeyLFSJWTSecret}

// SecretKeyFingerprint identifies the SECRET_KEY of "<name>-secrets" (its data) without
// revealing it: a restore tells by it whether a backup was taken by an instance with the same
// identity, and the data volume and the CNPG cluster carry it (AnnotationIdentity).
// 16 hex digits.
func SecretKeyFingerprint(data map[string][]byte) string {
	key := data[SecretKeySecretKey]
	if len(key) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte("forgejo-operator secret-key fingerprint v1\x00"))
	h.Write(key)
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Bootstrapping reports whether f is still being created from spec.restoreFrom: the bootstrap
// restores the identity, the database and the data volume from the backup.
func Bootstrapping(f *forgejov1.Forgejo) bool {
	return f.Spec.RestoreFrom != nil && (f.Status.RestoredFrom == nil || f.Status.RestoredFrom.Phase != forgejov1.RestoredFromCompleted)
}

// DataObjects are the objects that hold an instance's data: the data volume and, for database
// type CNPG, the CNPG cluster. A field is nil when its object does not exist.
type DataObjects struct {
	VolumeName string
	Volume     *corev1.PersistentVolumeClaim
	// ClusterChecked is false when the instance has no CNPG cluster (database type External)
	// or the CNPG API is not served; Cluster is nil then.
	ClusterChecked bool
	ClusterName    string
	Cluster        *unstructured.Unstructured
}

// Present returns the objects that exist, as client objects with a kind for messages.
func (d *DataObjects) Present() []IdentityObject {
	var out []IdentityObject
	if d.Volume != nil {
		out = append(out, IdentityObject{Kind: "PVC", Object: d.Volume})
	}
	if d.Cluster != nil {
		out = append(out, IdentityObject{Kind: "CNPG cluster", Object: d.Cluster})
	}
	return out
}

// IdentityObject is one of DataObjects.
type IdentityObject struct {
	Kind   string
	Object client.Object
}

// ReadDataObjects reads the data volume (spec.storage.existingClaim, or "<name>-data") and the
// CNPG cluster of rc.Instance. Absence is confirmed with a live read, since the decisions taken
// on it (condition Broken) must not follow a lagging cache.
func ReadDataObjects(ctx context.Context, rc *ReconcileContext) (*DataObjects, error) {
	f := rc.Instance
	d := &DataObjects{VolumeName: DataPVCName(f), ClusterName: DatabaseClusterName(f)}
	if f.Spec.Storage.ExistingClaim != "" {
		d.VolumeName = f.Spec.Storage.ExistingClaim
	}
	pvc := &corev1.PersistentVolumeClaim{}
	found, err := GetConfirmed(ctx, rc, types.NamespacedName{Namespace: f.Namespace, Name: d.VolumeName}, pvc)
	if err != nil {
		return nil, fmt.Errorf("reading PVC %s: %w", d.VolumeName, err)
	}
	if found {
		d.Volume = pvc
	}
	if DatabaseType(f) == forgejov1.DatabaseTypeExternal || !rc.HasAPI(CNPGClusterGVK) {
		return d, nil
	}
	d.ClusterChecked = true
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(CNPGClusterGVK)
	found, err = GetConfirmed(ctx, rc, types.NamespacedName{Namespace: f.Namespace, Name: d.ClusterName}, cluster)
	if err != nil {
		return nil, fmt.Errorf("reading CNPG cluster %s: %w", d.ClusterName, err)
	}
	if found {
		d.Cluster = cluster
	}
	return d, nil
}

// GetConfirmed reads obj, confirming a NotFound with rc.APIReader when there is one.
func GetConfirmed(ctx context.Context, rc *ReconcileContext, key types.NamespacedName, obj client.Object) (bool, error) {
	err := rc.Client.Get(ctx, key, obj)
	if apierrors.IsNotFound(err) && rc.APIReader != nil {
		err = rc.APIReader.Get(ctx, key, obj)
	}
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// IsBroken reports whether condition Broken is True.
func IsBroken(f *forgejov1.Forgejo) bool {
	return meta.IsStatusConditionTrue(f.Status.Conditions, forgejov1.ConditionBroken)
}

// SetBroken sets condition Broken with reason and msg, and emits a Warning event when the
// message is new.
func (rc *ReconcileContext) SetBroken(reason, msg string) {
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionBroken); c == nil || c.Message != msg {
		rc.Event("Warning", reason, "Broken", "%s", msg)
	}
	rc.SetCondition(forgejov1.ConditionBroken, metav1.ConditionTrue, reason, msg)
}

// ClearBroken removes condition Broken if one of reasons set it: each step clears only what it
// reports, so that one check passing does not hide another one's finding.
func (rc *ReconcileContext) ClearBroken(reasons ...string) {
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionBroken); c != nil && slices.Contains(reasons, c.Reason) {
		meta.RemoveStatusCondition(&rc.Instance.Status.Conditions, forgejov1.ConditionBroken)
	}
}

// RecreateDatabasePending reports whether the annotation AnnotationRecreateDatabase asks for the
// CNPG cluster c (nil when it does not exist) to be created again, or keeps one it created
// waiting for its restore.
func RecreateDatabasePending(f *forgejov1.Forgejo, c *CNPGCluster) bool {
	return f.Annotations[AnnotationRecreateDatabase] == "true" && DatabaseType(f) == forgejov1.DatabaseTypeCNPG &&
		(c == nil || c.Deleting || c.RestorePending)
}

// ResetIdentity stamps obj with fingerprint and removes AnnotationRunningImage: its contents
// were replaced by a restore of that identity, and what ran on it before is unknown.
func ResetIdentity(ctx context.Context, c client.Client, obj client.Object, fingerprint string) error {
	ann := obj.GetAnnotations()
	if _, ran := ann[AnnotationRunningImage]; !ran && ann[AnnotationIdentity] == fingerprint {
		return nil
	}
	orig, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	merged := maps.Clone(ann)
	if merged == nil {
		merged = map[string]string{}
	}
	merged[AnnotationIdentity] = fingerprint
	delete(merged, AnnotationRunningImage)
	obj.SetAnnotations(merged)
	return c.Patch(ctx, obj, client.MergeFrom(orig))
}

// StampIdentity sets the identity annotations on obj (AnnotationIdentity to fingerprint and,
// unless image is empty, AnnotationRunningImage to image) with a merge patch, if they differ.
func StampIdentity(ctx context.Context, c client.Client, obj client.Object, fingerprint, image string) error {
	want := map[string]string{AnnotationIdentity: fingerprint}
	if image != "" {
		want[AnnotationRunningImage] = image
	}
	ann := obj.GetAnnotations()
	changed := false
	for k, v := range want {
		if ann[k] != v {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	orig, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return nil
	}
	merged := maps.Clone(ann)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, want)
	obj.SetAnnotations(merged)
	return c.Patch(ctx, obj, client.MergeFrom(orig))
}
