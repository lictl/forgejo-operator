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
	// LabelRunner marks the objects of a ForgejoRunner with its name.
	LabelRunner = "forgejo.lucas.icu/runner"

	// AnnotationConfigHash on the pod template triggers a rollout when app.ini changes.
	AnnotationConfigHash = "forgejo.lucas.icu/config-hash"
	// AnnotationConfigProfile on an app.ini Secret names the Traits.ConfigProfile it was
	// rendered for; Secrets without it have LegacyConfigProfile.
	AnnotationConfigProfile = "forgejo.lucas.icu/config-profile"
	// AnnotationRestartedAt on the pod template forces a restart (self-healing).
	AnnotationRestartedAt = "forgejo.lucas.icu/restarted-at"
	// AnnotationSSHHostKeys on the pod template is a digest of the SSH host keys the pods
	// serve from their Secret, so that changed keys roll the pods.
	AnnotationSSHHostKeys = "forgejo.lucas.icu/ssh-host-keys"
	// AnnotationSSHHostKeysSource on "<name>-ssh-host-keys" records that the operator generated
	// its keys (Generated). A Secret without it was created by the user.
	AnnotationSSHHostKeysSource = "forgejo.lucas.icu/ssh-host-keys-source"
	// AnnotationObjectStorage on the pod template is a keyed digest (HMAC with the
	// pod-digest-salt of "<name>-secrets") of the object storage credentials and CA
	// certificate, so that changing them rolls the pods without exposing a hash of them.
	AnnotationObjectStorage = "forgejo.lucas.icu/object-storage"
	// AnnotationBackupKey on "<name>-backup-key" records that the operator generated the key
	// (Generated). A Secret without it was created by the user.
	AnnotationBackupKey = "forgejo.lucas.icu/backup-key"
	// AnnotationBackupKeyExported=true on a generated "<name>-backup-key" acknowledges that the
	// key was copied out of the cluster; until then BackupHealthy is False/BackupKeyNotExported.
	AnnotationBackupKeyExported = "forgejo.lucas.icu/backup-key-exported"
	// AnnotationBackupKeyLost on the Forgejo, set to the fingerprint of a generated backup key
	// whose Secret is gone, lets the operator generate a new key. Without it a missing key that
	// encrypted backups is reported (BackupKeyMissing), never silently replaced.
	AnnotationBackupKeyLost = "forgejo.lucas.icu/backup-key-lost"
	// AnnotationRestoredFrom on "<name>-secrets" records the location of the backup that
	// spec.restoreFrom restored, written before Forgejo starts on it. It outlives status (and,
	// under retainPolicy Retain, the Forgejo object), so the bootstrap never runs twice.
	AnnotationRestoredFrom = "forgejo.lucas.icu/restored-from"
	// AnnotationRestoreFromOverwrite=true on the Forgejo lets spec.restoreFrom restore over a
	// database or data volume that existed before the instance (RestoredFromBackup=
	// False/TargetNotEmpty otherwise).
	AnnotationRestoreFromOverwrite = "forgejo.lucas.icu/restore-from-overwrite"

	ManagedBy = "forgejo-operator"
	AppName   = "forgejo"

	ComponentServer   = "server"
	ComponentDatabase = "database"
	ComponentValkey   = "valkey"
	ComponentBackup   = "backup"
	ComponentRestore  = "restore"
	// ComponentRunner marks the pods of a ForgejoRunner, ComponentRunnerRegistration its
	// registration Job. The Forgejo controller ignores both.
	ComponentRunner             = "runner"
	ComponentRunnerRegistration = "runner-registration"

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
func HTTPRouteName(f *forgejov1.Forgejo) string       { return f.Name }
func SSHRouteName(f *forgejov1.Forgejo) string        { return f.Name + "-ssh" }

// TLSSecretName is the Secret that cert-manager keeps the certificate of spec.tls.certManager
// in ("<name>-tls"). The Certificate and the ReferenceGrant of spec.tls.gatewayNamespaces
// have the same name.
func TLSSecretName(f *forgejov1.Forgejo) string { return f.Name + "-tls" }

// SSHHostKeysSecretName is the operator's Secret of SSH host keys ("<name>-ssh-host-keys").
func SSHHostKeysSecretName(f *forgejov1.Forgejo) string { return f.Name + "-ssh-host-keys" }

// BackupKeySecretName is the Secret of the generated backup encryption key ("<name>-backup-key").
func BackupKeySecretName(f *forgejov1.Forgejo) string { return f.Name + "-backup-key" }

// BackupKeySecretKey is the key of the generated backup encryption key in its Secret.
const BackupKeySecretKey = "key"

// SecretKeyAdminSyncSalt in "<name>-secrets" salts the digest of the admin credentials that
// Forgejo's bootstrap init container keeps on the data volume.
const SecretKeyAdminSyncSalt = "admin-sync-salt"

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
	return "http://" + ServiceHost(HTTPServiceName(f), f.Namespace) + ":" + itoa(HTTPServicePort(f))
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
