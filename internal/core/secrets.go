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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// Keys of the generated admin Secret.
const (
	adminUsernameKey = "username"
	adminPasswordKey = "password"
)

// ReconcileSecrets ensures "<name>-secrets" (SECRET_KEY, INTERNAL_TOKEN, JWT secrets,
// metrics token) and the admin credentials secret exist. Never rotates existing values.
func ReconcileSecrets(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	generators := map[string]func() (string, error){
		forgejo.SecretKeySecretKey:     func() (string, error) { return randomBase64(32) },
		forgejo.SecretKeyInternalToken: newInternalToken,
		forgejo.SecretKeyJWTSecret:     func() (string, error) { return randomBase64(32) },
		forgejo.SecretKeyLFSJWTSecret:  func() (string, error) { return randomBase64(32) },
		forgejo.SecretKeyMetricsToken:  func() (string, error) { return randomString(40) },
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
			pw, err := randomString(24)
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

// newInternalToken mimics "forgejo generate secret INTERNAL_TOKEN": an HS256 JWT with
// an nbf claim, signed with a throwaway random key. Forgejo only compares it verbatim.
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
