package core

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Keys of the generated admin Secret.
const (
	adminUsernameKey = "username"
	adminPasswordKey = "password"
	// adminPasswordLength of generated admin passwords; well above Forgejo's default
	// MIN_PASSWORD_LENGTH so that common hardening settings accept it.
	adminPasswordLength = 32
)

// secretKeyAdminSyncSalt in "<name>-secrets" salts the digest of the admin credentials the
// bootstrap init container keeps on the data volume (see bootstrapScript). It lives outside
// the volume, so data backups do not carry a digest that could be brute-forced on its own.
const secretKeyAdminSyncSalt = "admin-sync-salt"

// secretKeyPodDigestSalt in "<name>-secrets" keys the digests of secret values on the pod
// template (podDigest), such as the object storage credentials' annotation, which anyone who
// can read Deployments or Pods sees.
const secretKeyPodDigestSalt = "pod-digest-salt"

// ReconcileSecrets ensures "<name>-secrets" (SECRET_KEY, INTERNAL_TOKEN, JWT secrets,
// metrics token) and the admin credentials secret exist. Never rotates existing values.
func ReconcileSecrets(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	generators := map[string]func() (string, error){
		forgejo.SecretKeySecretKey:     func() (string, error) { return randomBase64() },
		forgejo.SecretKeyInternalToken: newInternalToken,
		forgejo.SecretKeyJWTSecret:     func() (string, error) { return randomBase64() },
		forgejo.SecretKeyLFSJWTSecret:  func() (string, error) { return randomBase64() },
		forgejo.SecretKeyMetricsToken:  func() (string, error) { return randomString(40) },
		secretKeyAdminSyncSalt:         func() (string, error) { return randomBase64() },
		secretKeyPodDigestSalt:         func() (string, error) { return randomBase64() },
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace}}
	if err := apply(ctx, rc, sec, rc.OwnerRefsEnabled(), func() error {
		sec.Labels = mergeMaps(sec.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		for key, gen := range generators {
			if len(sec.Data[key]) > 0 {
				continue
			}
			v, err := gen()
			if err != nil {
				return err
			}
			sec.Data[key] = []byte(v)
		}
		return nil
	}); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("secrets: %w", err)
	}
	// From the Secret as applied, so that a lagging cache cannot hand out another value.
	rc.PodDigestSalt = string(sec.Data[secretKeyPodDigestSalt])

	if err := ensureBackupKey(ctx, rc); err != nil {
		return forgejo.StepResult{}, err
	}

	if f.Spec.Admin.ExistingSecret != "" {
		existing := &corev1.Secret{}
		if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: f.Spec.Admin.ExistingSecret}, existing); err != nil {
			return forgejo.StepResult{}, fmt.Errorf("admin.existingSecret %q: %w", f.Spec.Admin.ExistingSecret, err)
		}
		if len(existing.Data[AdminPasswordKey(f)]) == 0 {
			return forgejo.StepResult{}, fmt.Errorf("admin.existingSecret %q has no key %q", f.Spec.Admin.ExistingSecret, AdminPasswordKey(f))
		}
		f.Status.AdminSecretName = f.Spec.Admin.ExistingSecret
		return forgejo.StepResult{}, nil
	}

	admin := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.AdminSecretName(f), Namespace: f.Namespace}}
	if err := apply(ctx, rc, admin, rc.OwnerRefsEnabled(), func() error {
		admin.Labels = mergeMaps(admin.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		if admin.Data == nil {
			admin.Data = map[string][]byte{}
		}
		admin.Data[adminUsernameKey] = []byte(AdminUsername(f))
		if len(admin.Data[adminPasswordKey]) == 0 {
			pw, err := randomPassword(adminPasswordLength)
			if err != nil {
				return err
			}
			admin.Data[adminPasswordKey] = []byte(pw)
		}
		return nil
	}); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("admin secret: %w", err)
	}
	f.Status.AdminSecretName = admin.Name
	return forgejo.StepResult{}, nil
}

// AdminUsername is the bootstrap administrator's login.
func AdminUsername(f *forgejov1.Forgejo) string {
	if f.Spec.Admin.Username != "" {
		return f.Spec.Admin.Username
	}
	return forgejov1.DefaultAdminUsername
}

// AdminCredentialsSecret is the Secret holding the admin password: spec.admin.existingSecret,
// or the generated "<name>-admin".
func AdminCredentialsSecret(f *forgejov1.Forgejo) string { return adminSecretName(f) }

// AdminPasswordKey is the key holding the admin password in status.adminSecretName.
func AdminPasswordKey(f *forgejov1.Forgejo) string {
	if f.Spec.Admin.ExistingSecret != "" && f.Spec.Admin.PasswordKey != "" {
		return f.Spec.Admin.PasswordKey
	}
	return adminPasswordKey
}

func adminEmail(f *forgejov1.Forgejo) string {
	if f.Spec.Admin.Email != "" {
		return f.Spec.Admin.Email
	}
	return forgejov1.DefaultAdminEmail
}

// newInternalToken mimics "forgejo generate secret INTERNAL_TOKEN" (Forgejo 17 and older;
// 18 removed the token and the command): an HS256 JWT with an nbf claim, signed with a
// throwaway random key. Forgejo only compares it verbatim. It is generated for every
// instance, so that an instance can always run (or roll back to) a major that needs it.
func newInternalToken() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." +
		enc.EncodeToString(fmt.Appendf(nil, `{"nbf":%d}`, time.Now().Unix()))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(unsigned))
	return unsigned + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// AnnotationBackupKey on "<name>-backup-key" records that the operator generated the key.
const AnnotationBackupKey = forgejo.AnnotationBackupKey

// ensureBackupKey generates the encryption key of remote backups into "<name>-backup-key"
// when spec.backup.remote is set without its own keySecret. The Secret is created once and
// then left alone: it has no owner reference (deleting the instance must not take the key of
// its off-site backups with it), and an existing one, e.g. the key restored from an offline
// copy before a disaster recovery, is used as it is. A key that encrypted backups (its
// fingerprint is in status.backup.remote or in a ForgejoBackup) is never replaced silently
// when its Secret goes missing: only the annotation forgejo.lucas.icu/backup-key-lost
// with that fingerprint lets a new one be generated. Until then BackupHealthy reports
// BackupKeyMissing and uploads fail.
func ensureBackupKey(ctx context.Context, rc *forgejo.ReconcileContext) error {
	f := rc.Instance
	if f.Spec.Backup.Remote == nil || f.Spec.Backup.Remote.Encryption.KeySecret != nil {
		return nil
	}
	name := forgejo.BackupKeySecretName(f)
	sec := &corev1.Secret{}
	err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: name}, sec)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	lost, err := lostBackupKey(ctx, rc, name)
	if err != nil {
		return err
	}
	if lost != "" && f.Annotations[forgejo.AnnotationBackupKeyLost] != lost {
		warnEvery(rc, "backup-key-missing", "BackupKeyMissing", "Backup",
			"Secret %s with the key of the remote backups (fingerprint %s) is missing, and no new key is generated: restore it "+
				"from your copy, or set the annotation %s=%s on the Forgejo to start over with a new key (the backups "+
				"encrypted with the lost one can then not be restored)", name, lost, forgejo.AnnotationBackupKeyLost, lost)
		return nil
	}
	key, err := randomBase64()
	if err != nil {
		return err
	}
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.Namespace,
			Labels:      forgejo.Labels(f, forgejo.ComponentBackup),
			Annotations: map[string]string{AnnotationBackupKey: "Generated"}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{forgejo.BackupKeySecretKey: []byte(key)},
	}
	if err := rc.Client.Create(ctx, sec); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating %s: %w", name, err)
	}
	rc.Event(corev1.EventTypeWarning, "BackupKeyGenerated", "Backup",
		"Generated the encryption key of remote backups in Secret %s. Copy it to a safe place outside the cluster now "+
			"(kubectl get secret %s -n %s -o jsonpath='{.data.key}' | base64 -d): remote backups cannot be restored without it. "+
			"Then annotate the Secret with %s=true; until then status.backup.remote.message says so and BackupHealthy (with scheduled "+
			"backups) is False", name, name, f.Namespace, forgejo.AnnotationBackupKeyExported)
	return nil
}

// lostBackupKey returns the fingerprint of the key that Secret name held when it encrypted
// the instance's backups, or "" when nothing records one: status.backup.remote, else the
// newest ForgejoBackup of the instance uploaded with that Secret.
func lostBackupKey(ctx context.Context, rc *forgejo.ReconcileContext, name string) (string, error) {
	f := rc.Instance
	if st := f.Status.Backup; st != nil && st.Remote != nil && st.Remote.KeySecretName == name && st.Remote.KeyFingerprint != "" {
		return st.Remote.KeyFingerprint, nil
	}
	list := &forgejov1.ForgejoBackupList{}
	if err := rc.Client.List(ctx, list, client.InNamespace(f.Namespace), client.MatchingLabels{forgejo.LabelForgejo: f.Name}); err != nil {
		return "", fmt.Errorf("listing backups: %w", err)
	}
	fp, at := "", time.Time{}
	for i := range list.Items {
		r := list.Items[i].Status.Remote
		if r == nil || r.KeySecret == nil || r.KeySecret.Name != name || r.KeyFingerprint == "" || r.StartTime == nil {
			continue
		}
		if fp == "" || r.StartTime.After(at) {
			fp, at = r.KeyFingerprint, r.StartTime.Time
		}
	}
	return fp, nil
}
