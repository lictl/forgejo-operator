package core

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
	"github.com/lictl/forgejo-operator/internal/rclone"
)

// The backup key is generated once, without an owner reference even under retainPolicy
// Delete, and never replaced: an existing Secret (e.g. restored from an offline copy) is kept.
func TestBackupKeyIsGeneratedOnceAndNeverOwned(t *testing.T) {
	ctx := context.Background()
	f := testInstance()
	f.Spec.Storage.RetainPolicy = forgejov1.RetainPolicyDelete
	f.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{S3: forgejov1.S3Connection{Endpoint: "http://garage:3900", Bucket: "b"}}
	rc := testContext(t, f)
	if _, err := ReconcileSecrets(ctx, rc); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: f.Namespace, Name: "fj-backup-key"}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, key, sec); err != nil {
		t.Fatal(err)
	}
	value := string(sec.Data["key"])
	if err := rclone.ValidateKey(value); err != nil || len(sec.OwnerReferences) != 0 || sec.Annotations[AnnotationBackupKey] != "Generated" {
		t.Fatalf("key Secret %+v (%v)", sec.ObjectMeta, err)
	}
	// 32 random bytes as unpadded base64url (docs/backup-restore.md "The key").
	if raw, err := base64.RawURLEncoding.DecodeString(value); err != nil || len(value) != 43 || len(raw) != 32 {
		t.Errorf("the key is not 32 bytes of unpadded base64url: %d characters (%v)", len(value), err)
	}
	if _, err := ReconcileSecrets(ctx, rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Client.Get(ctx, key, sec)
	if string(sec.Data["key"]) != value {
		t.Error("the key changed")
	}

	// An own key Secret: nothing is generated.
	f2 := testInstance()
	f2.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{Encryption: forgejov1.BackupEncryptionSpec{KeySecret: &forgejov1.BackupKeySecretRef{Name: "mine"}}}
	rc2 := testContext(t, f2)
	if _, err := ReconcileSecrets(ctx, rc2); err != nil {
		t.Fatal(err)
	}
	if err := rc2.Client.Get(ctx, key, &corev1.Secret{}); err == nil {
		t.Error("a key was generated although spec.backup.remote.encryption.keySecret is set")
	}

	// A key Secret that exists before the instance is used as it is.
	f3 := testInstance()
	f3.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{}
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "fj-backup-key", Namespace: f3.Namespace}, Data: map[string][]byte{"key": []byte("the-old-key-from-the-safe")}}
	rc3 := testContext(t, f3, old)
	if _, err := ReconcileSecrets(ctx, rc3); err != nil {
		t.Fatal(err)
	}
	_ = rc3.Client.Get(ctx, key, sec)
	if string(sec.Data["key"]) != "the-old-key-from-the-safe" {
		t.Error("an existing key was replaced")
	}
}

const lostFingerprint = "0123456789abcdef"

// A generated key that encrypted backups is never replaced silently when its Secret goes
// missing: a new one is generated only after the annotation names the lost key's fingerprint.
func TestLostBackupKeyIsNotReplacedSilently(t *testing.T) {
	ctx := context.Background()
	var key types.NamespacedName
	for _, tc := range []struct {
		name     string
		status   bool // status.backup.remote records the fingerprint
		backup   bool // a ForgejoBackup records it
		lost     string
		generate bool
	}{
		{name: "fingerprint in status", status: true},
		{name: "fingerprint in a backup", backup: true},
		{name: "acknowledged as lost", status: true, lost: lostFingerprint, generate: true},
		{name: "another fingerprint acknowledged", status: true, lost: "ffffffffffffffff"},
		{name: "nothing recorded", generate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			key = types.NamespacedName{Namespace: f.Namespace, Name: forgejo.BackupKeySecretName(f)}
			f.Spec.Backup.Remote = &forgejov1.RemoteBackupSpec{S3: forgejov1.S3Connection{Endpoint: "http://garage:3900", Bucket: "b"}}
			if tc.status {
				f.Status.Backup = &forgejov1.BackupStatusSummary{Remote: &forgejov1.RemoteBackupSummary{
					KeySecretName: forgejo.BackupKeySecretName(f), KeyFingerprint: lostFingerprint}}
			}
			if tc.lost != "" {
				f.Annotations = map[string]string{forgejo.AnnotationBackupKeyLost: tc.lost}
			}
			var objs []client.Object
			if tc.backup {
				b := &forgejov1.ForgejoBackup{ObjectMeta: metav1.ObjectMeta{Name: "b1", Namespace: f.Namespace,
					Labels: map[string]string{forgejo.LabelForgejo: f.Name}}}
				b.Status.Remote = &forgejov1.RemoteBackupStatus{KeySecret: &forgejov1.BackupKeySecretRef{Name: forgejo.BackupKeySecretName(f), Key: forgejo.BackupKeySecretKey},
					KeyFingerprint: lostFingerprint, StartTime: &metav1.Time{Time: time.Now()}}
				objs = append(objs, b)
			}
			rc := testContext(t, f, objs...)
			if _, err := ReconcileSecrets(ctx, rc); err != nil {
				t.Fatal(err)
			}
			err := rc.Client.Get(ctx, key, &corev1.Secret{})
			if generated := err == nil; generated != tc.generate {
				t.Fatalf("generated = %t, want %t (%v)", generated, tc.generate, err)
			}
		})
	}
}
