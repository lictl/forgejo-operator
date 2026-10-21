// Package forgejo holds helpers shared by every feature package of the operator:
// resource naming, labels, image layout, defaults, the per-reconcile context and metrics.
package forgejo

import (
	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// Label keys and values used on every child resource.
const (
	LabelName      = "app.kubernetes.io/name"
	LabelInstance  = "app.kubernetes.io/instance"
	LabelComponent = "app.kubernetes.io/component"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPartOf    = "app.kubernetes.io/part-of"
	LabelVersion   = "app.kubernetes.io/version"

	// LabelForgejo marks ForgejoBackup/ForgejoRestore objects with their instance.
	LabelForgejo = "forgejo.lucas.icu/instance"
	// LabelBackupReason marks ForgejoBackup objects with their reason (for retention).
	LabelBackupReason = "forgejo.lucas.icu/backup-reason"

	// AnnotationConfigHash on the pod template triggers a rollout when app.ini changes.
	AnnotationConfigHash = "forgejo.lucas.icu/config-hash"
	// AnnotationRestartedAt on the pod template forces a restart (self-healing).
	AnnotationRestartedAt = "forgejo.lucas.icu/restarted-at"

	ManagedBy = "forgejo-operator"
	AppName   = "forgejo"

	ComponentServer   = "server"
	ComponentDatabase = "database"
	ComponentValkey   = "valkey"
	ComponentBackup   = "backup"
	ComponentRestore  = "restore"

	// FinalizerName is used by controllers that must clean up external state.
	FinalizerName = "forgejo.lucas.icu/finalizer"
)

// Child resource names. Every name is derived from the Forgejo object's name.

func DeploymentName(f *forgejov1.Forgejo) string      { return f.Name }
func HTTPServiceName(f *forgejov1.Forgejo) string     { return f.Name + "-http" }
func SSHServiceName(f *forgejov1.Forgejo) string      { return f.Name + "-ssh" }
func IngressName(f *forgejov1.Forgejo) string         { return f.Name }
func DataPVCName(f *forgejov1.Forgejo) string         { return f.Name + "-data" }
func BackupPVCName(f *forgejov1.Forgejo) string       { return f.Name + "-backups" }
func SecretsName(f *forgejov1.Forgejo) string         { return f.Name + "-secrets" }
func AdminSecretName(f *forgejov1.Forgejo) string     { return f.Name + "-admin" }
func ConfigSecretName(f *forgejov1.Forgejo) string    { return f.Name + "-config" }
func LKGConfigSecretName(f *forgejov1.Forgejo) string { return f.Name + "-config-lkg" }
func DatabaseClusterName(f *forgejov1.Forgejo) string { return f.Name + "-db" }
func ValkeyName(f *forgejov1.Forgejo) string          { return f.Name + "-valkey" }
func HPAName(f *forgejov1.Forgejo) string             { return f.Name }
func PDBName(f *forgejov1.Forgejo) string             { return f.Name }
func ServiceMonitorName(f *forgejov1.Forgejo) string  { return f.Name }
func PrometheusRuleName(f *forgejov1.Forgejo) string  { return f.Name }
func DashboardName(f *forgejov1.Forgejo) string       { return f.Name + "-dashboard" }

// Keys inside the generated "<name>-secrets" Secret.
const (
	SecretKeySecretKey     = "secret-key"
	SecretKeyInternalToken = "internal-token"
	SecretKeyJWTSecret     = "oauth2-jwt-secret"
	SecretKeyLFSJWTSecret  = "lfs-jwt-secret"
	SecretKeyMetricsToken  = "metrics-token"
	ConfigSecretKey        = "app.ini"
)

// SelectorLabels are the immutable labels used by the Deployment selector and Services.
func SelectorLabels(f *forgejov1.Forgejo) map[string]string {
	return map[string]string{
		LabelName:      AppName,
		LabelInstance:  f.Name,
		LabelComponent: ComponentServer,
	}
}

// Labels returns the standard labels for a child resource of the given component.
func Labels(f *forgejov1.Forgejo, component string) map[string]string {
	return map[string]string{
		LabelName:      AppName,
		LabelInstance:  f.Name,
		LabelComponent: component,
		LabelManagedBy: ManagedBy,
		LabelPartOf:    AppName,
	}
}

// InternalHTTPURL is the in-cluster base URL of the HTTP service (no trailing slash).
func InternalHTTPURL(f *forgejov1.Forgejo) string {
	return "http://" + HTTPServiceName(f) + "." + f.Namespace + ".svc.cluster.local:" + itoa(HTTPServicePort(f))
}

// HTTPServicePort returns the HTTP Service port (defaults to 3000).
func HTTPServicePort(f *forgejov1.Forgejo) int32 {
	if f.Spec.Server.HTTP.Port != 0 {
		return f.Spec.Server.HTTP.Port
	}
	return 3000
}

// SSHServicePort returns the SSH Service port (defaults to 22).
func SSHServicePort(f *forgejov1.Forgejo) int32 {
	if f.Spec.Server.SSH.Service.Port != 0 {
		return f.Spec.Server.SSH.Service.Port
	}
	return 22
}

func itoa(i int32) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b [12]byte
	n := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		n--
		b[n] = digits[i%10]
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
