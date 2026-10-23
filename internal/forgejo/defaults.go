package forgejo

import (
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// The API server applies most defaults through CRD markers. These helpers resolve the
// remaining optional pointers and guard against objects created before a default existed.

func BoolOr(b *bool, def bool) bool {
	if b == nil {
		return def
	}
	return *b
}

func DurationOr(d *metav1.Duration, def time.Duration) time.Duration {
	if d == nil || d.Duration <= 0 {
		return def
	}
	return d.Duration
}

// Image is spec.image. The API server always stores one (the CRD defaults it and refuses an
// empty string, so an instance never follows DefaultImage of a later operator release); the
// fallback only serves objects built without API-server defaulting, such as in unit tests.
func Image(f *forgejov1.Forgejo) string {
	if f.Spec.Image != "" {
		return f.Spec.Image
	}
	return forgejov1.DefaultImage
}

func PostgresImage(f *forgejov1.Forgejo) string {
	if f.Spec.Database.CNPG.ImageName != "" {
		return f.Spec.Database.CNPG.ImageName
	}
	return forgejov1.DefaultPostgresImage
}

func DesiredReplicas(f *forgejov1.Forgejo) int32 {
	if f.Spec.Replicas == nil {
		return 1
	}
	return *f.Spec.Replicas
}

func DatabaseType(f *forgejov1.Forgejo) forgejov1.DatabaseType {
	if f.Spec.Database.Type == "" {
		return forgejov1.DatabaseTypeCNPG
	}
	return f.Spec.Database.Type
}

func SSHEnabled(f *forgejov1.Forgejo) bool { return BoolOr(f.Spec.Server.SSH.Enabled, true) }

// sshHostKeysKey is the app.ini key ([server]) that lists the SSH host keys.
const sshHostKeysKey = "SSH_SERVER_HOST_KEYS"

// SSHHostKeysOverridden reports whether spec.config or an entry of spec.configFrom sets
// [server] SSH_SERVER_HOST_KEYS (the section matches case-insensitively, the key exactly). The
// user then manages the host keys, and the operator leaves them alone: it neither generates,
// mounts, publishes nor backs them up, and app.ini keeps the user's value. A configFrom entry
// counts even while its (optional) Secret is missing; Forgejo then uses its own default key.
func SSHHostKeysOverridden(f *forgejov1.Forgejo) bool {
	for section, kv := range f.Spec.Config {
		if !strings.EqualFold(section, "server") {
			continue
		}
		if _, ok := kv[sshHostKeysKey]; ok {
			return true
		}
	}
	for _, from := range f.Spec.ConfigFrom {
		if strings.EqualFold(from.Section, "server") && from.Key == sshHostKeysKey {
			return true
		}
	}
	return false
}

// SSHHostKeysManaged reports whether the operator manages the SSH host keys: SSH is enabled
// and neither spec.config nor spec.configFrom sets SSH_SERVER_HOST_KEYS.
func SSHHostKeysManaged(f *forgejov1.Forgejo) bool {
	return SSHEnabled(f) && !SSHHostKeysOverridden(f)
}

// SSHHostKeysSecret is the Secret the SSH host keys are served from: the user's
// spec.server.ssh.hostKeys.existingSecret, else the operator's "<name>-ssh-host-keys".
func SSHHostKeysSecret(f *forgejov1.Forgejo) string {
	if hk := f.Spec.Server.SSH.HostKeys; hk != nil && hk.ExistingSecret != "" {
		return hk.ExistingSecret
	}
	return SSHHostKeysSecretName(f)
}

// GatewayEnabled reports whether spec.gateway asks for an HTTPRoute.
func GatewayEnabled(f *forgejov1.Forgejo) bool { return f.Spec.Gateway != nil }

// GatewaySSHEnabled reports whether spec.gateway.ssh asks for a TCPRoute and SSH is enabled.
func GatewaySSHEnabled(f *forgejov1.Forgejo) bool {
	return f.Spec.Gateway != nil && f.Spec.Gateway.SSH != nil && SSHEnabled(f)
}

// TLSEnabled reports whether spec.tls says that Forgejo is served over HTTPS.
func TLSEnabled(f *forgejov1.Forgejo) bool { return f.Spec.TLS != nil }

// CertManagerEnabled reports whether spec.tls.certManager asks for a Certificate.
func CertManagerEnabled(f *forgejov1.Forgejo) bool {
	return f.Spec.TLS != nil && f.Spec.TLS.CertManager != nil
}

func MonitoringEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.Monitoring.Enabled, true)
}

func SelfHealingEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.SelfHealing.Enabled, true)
}

func AutoTuningEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.AutoTuning.Enabled, true)
}

func StorageAutoExpandEnabled(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.AutoPilot.StorageAutoExpand.Enabled, true)
}

func HorizontalAutoscalingEnabled(f *forgejov1.Forgejo) bool {
	h := f.Spec.Autoscaling.Horizontal
	return h != nil && h.Enabled && f.Spec.HighAvailability.Enabled
}

// HPAMinReplicas is the HPA's replica floor: minReplicas clamped to [1, maxReplicas].
func HPAMinReplicas(f *forgejov1.Forgejo) int32 {
	h := f.Spec.Autoscaling.Horizontal
	if h == nil {
		return 1
	}
	return min(max(h.MinReplicas, 1), max(h.MaxReplicas, 1))
}

func VerticalAutoscalingEnabled(f *forgejov1.Forgejo) bool {
	v := f.Spec.Autoscaling.Vertical
	return v != nil && v.Enabled
}

func BackupIncludeData(f *forgejov1.Forgejo) bool {
	return BoolOr(f.Spec.Backup.IncludeData, true)
}

func RetainData(f *forgejov1.Forgejo) bool {
	return f.Spec.Storage.RetainPolicy != forgejov1.RetainPolicyDelete
}

func QuantityOr(q resource.Quantity, def string) resource.Quantity {
	if q.IsZero() {
		return resource.MustParse(def)
	}
	return q
}
