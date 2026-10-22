/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Defaults shared by the API and the controllers.
const (
	// DefaultImage is the current Forgejo LTS release (see docs/versions.md). Changing it
	// only affects new instances: existing ones keep the image stored in their spec.
	DefaultImage          = "codeberg.org/forgejo/forgejo:15"
	DefaultPostgresImage  = "ghcr.io/cloudnative-pg/postgresql:17"
	DefaultValkeyImage    = "docker.io/valkey/valkey:8-alpine"
	DefaultAdminUsername  = "forgejo_admin"
	DefaultAdminEmail     = "forgejo_admin@forgejo.local"
	DefaultBackupSchedule = "0 3 * * *"
)

// Condition types reported on Forgejo.status.conditions.
const (
	// ConditionReady is True when Forgejo is serving traffic with the desired spec.
	ConditionReady = "Ready"
	// ConditionDatabaseReady is True when the PostgreSQL backend is reachable/healthy.
	ConditionDatabaseReady = "DatabaseReady"
	// ConditionProgressing is True while a rollout, upgrade or restore is in flight.
	ConditionProgressing = "Progressing"
	// ConditionDegraded is True when something is wrong but the operator keeps trying.
	ConditionDegraded = "Degraded"
	// ConditionUpgrading is True while a version upgrade is being orchestrated.
	ConditionUpgrading = "Upgrading"
	// ConditionBackupHealthy reflects the state of scheduled backups.
	ConditionBackupHealthy = "BackupHealthy"
	// ConditionStorageHealthy reflects PVC capacity/usage.
	ConditionStorageHealthy = "StorageHealthy"
	// ConditionHealthy reflects Forgejo's own /api/healthz checks.
	ConditionHealthy = "Healthy"
	// ConditionVersionSupported is True when the running Forgejo major is within the
	// operator's support policy (the current LTS and the latest release). The reason is
	// Supported, Deprecated, EndOfLife, Future or Unknown; see docs/versions.md.
	ConditionVersionSupported = "VersionSupported"
	// ConditionRoutesReady is True when every Gateway API route of spec.gateway is accepted
	// by each of its parent Gateways and its backend references resolve. Only reported
	// while spec.gateway is set; see docs/networking.md.
	ConditionRoutesReady = "RoutesReady"
	// ConditionTLSReady is True when the cert-manager Certificate of spec.tls.certManager is
	// ready (and the ReferenceGrant of spec.tls.gatewayNamespaces exists). Only reported
	// while spec.tls is set; see docs/networking.md.
	ConditionTLSReady = "TLSReady"
)

// ForgejoPhase is a coarse, human-readable summary of the instance state.
// +kubebuilder:validation:Enum=Pending;Provisioning;Running;Upgrading;Restoring;Degraded;Failed;Paused
type ForgejoPhase string

const (
	PhasePending      ForgejoPhase = "Pending"
	PhaseProvisioning ForgejoPhase = "Provisioning"
	PhaseRunning      ForgejoPhase = "Running"
	PhaseUpgrading    ForgejoPhase = "Upgrading"
	PhaseRestoring    ForgejoPhase = "Restoring"
	PhaseDegraded     ForgejoPhase = "Degraded"
	PhaseFailed       ForgejoPhase = "Failed"
	PhasePaused       ForgejoPhase = "Paused"
)

// ForgejoSpec defines the desired state of a Forgejo instance.
type ForgejoSpec struct {
	// image is the Forgejo container image. The official root image
	// (codeberg.org/forgejo/forgejo:<version>) and the "-rootless" variant are supported.
	// Changing it triggers an orchestrated upgrade (backup, migrate, verify, auto-rollback).
	// The default is the current LTS release when the object is created.
	// +kubebuilder:default="codeberg.org/forgejo/forgejo:15"
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	ImagePullPolicy corev1.PullPolicy `json:"imagePullPolicy,omitempty"`

	// +optional
	ImagePullSecrets []corev1.LocalObjectReference `json:"imagePullSecrets,omitempty"`

	// replicas is the desired number of Forgejo pods. Values greater than 1 require
	// highAvailability.enabled. Ignored while horizontal autoscaling is enabled.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=0
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// paused stops the operator from changing any child resource. Status is still reported.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// server configures the public identity and network exposure of Forgejo.
	// +kubebuilder:default={}
	// +optional
	Server ServerSpec `json:"server,omitempty"`

	// ingress exposes the HTTP service through an Ingress.
	// +optional
	Ingress *IngressSpec `json:"ingress,omitempty"`

	// gateway exposes Forgejo through Gateway API routes attached to Gateways that already
	// exist: HTTPRoute "<name>" for HTTP and, optionally, TCPRoute "<name>-ssh" for SSH. The
	// operator never creates a Gateway. Requires the Gateway API CRDs; see docs/networking.md.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// tls says that Forgejo is served over HTTPS (ROOT_URL becomes https) and can request the
	// certificate from cert-manager. See docs/networking.md.
	// +optional
	TLS *TLSSpec `json:"tls,omitempty"`

	// database selects the PostgreSQL backend. Defaults to an operator-managed CloudNativePG cluster.
	// +kubebuilder:default={}
	// +optional
	Database DatabaseSpec `json:"database,omitempty"`

	// storage configures the data volume (repositories, LFS, attachments, avatars).
	// +kubebuilder:default={}
	// +optional
	Storage StorageSpec `json:"storage,omitempty"`

	// admin configures the bootstrap administrator account.
	// +kubebuilder:default={}
	// +optional
	Admin AdminSpec `json:"admin,omitempty"`

	// config holds raw app.ini overrides as section -> key -> value. Use the section name
	// "DEFAULT" for root-level keys (e.g. APP_NAME). Operator-managed keys (database, secrets,
	// paths) win over overrides only where noted in the documentation.
	// +optional
	Config map[string]map[string]string `json:"config,omitempty"`

	// configFrom injects app.ini values read from Secrets (mailer passwords, OAuth secrets, ...).
	// +optional
	ConfigFrom []ConfigValueFromSecret `json:"configFrom,omitempty"`

	// resources for the Forgejo container. When vertical autoscaling is enabled these are the
	// starting point and the operator may raise them within autoscaling.vertical bounds.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// env adds extra environment variables to the Forgejo container.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// +optional
	PodTemplate PodTemplateSpec `json:"podTemplate,omitempty"`

	// backup configures scheduled backups (PostgreSQL dump + data archive).
	// +kubebuilder:default={}
	// +optional
	Backup BackupSpec `json:"backup,omitempty"`

	// upgrade configures how image changes are rolled out.
	// +kubebuilder:default={}
	// +optional
	Upgrade UpgradeSpec `json:"upgrade,omitempty"`

	// monitoring configures metrics, ServiceMonitor, PrometheusRule and dashboards.
	// +kubebuilder:default={}
	// +optional
	Monitoring MonitoringSpec `json:"monitoring,omitempty"`

	// highAvailability runs Forgejo with shared cache/session/queue in Valkey so that more
	// than one replica can serve traffic.
	// +kubebuilder:default={}
	// +optional
	HighAvailability HighAvailabilitySpec `json:"highAvailability,omitempty"`

	// autoscaling configures horizontal (HPA) and vertical (operator-driven) scaling.
	// +kubebuilder:default={}
	// +optional
	Autoscaling AutoscalingSpec `json:"autoscaling,omitempty"`

	// autoPilot configures self-healing, auto-tuning and storage auto-expansion.
	// +kubebuilder:default={}
	// +optional
	AutoPilot AutoPilotSpec `json:"autoPilot,omitempty"`
}

// ServerSpec configures Forgejo's public identity and Services.
type ServerSpec struct {
	// appName is the site title (APP_NAME).
	// +kubebuilder:default="Forgejo"
	// +optional
	AppName string `json:"appName,omitempty"`

	// domain is the public host name (DOMAIN / SSH_DOMAIN). Defaults to ingress.host, the
	// first gateway.hostnames entry that is not a wildcard, or the in-cluster HTTP service DNS
	// name.
	// +optional
	Domain string `json:"domain,omitempty"`

	// rootURL overrides ROOT_URL. Defaults to http(s)://<host><path>/ from the Ingress or the
	// HTTPRoute, else http(s)://<domain>/; https when the Ingress has a TLS Secret or spec.tls
	// is set.
	// +optional
	RootURL string `json:"rootURL,omitempty"`

	// trustedProxies are the networks (CIDRs, or "*" for every peer) whose X-Forwarded-For and
	// X-Real-IP headers Forgejo trusts to tell the client's address ([security]
	// REVERSE_PROXY_TRUSTED_PROXIES). Defaults to the loopback and private ranges
	// (127.0.0.0/8, ::1/128, 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 100.64.0.0/10,
	// fc00::/7), where a cluster's ingress controllers and gateways run; to loopback only
	// (127.0.0.0/8, ::1/128) when http.type is LoadBalancer or NodePort, whose clients
	// kube-proxy shows with a node's private address. Narrow it to the proxies' pod network
	// when other workloads in those ranges could reach Forgejo directly.
	// +kubebuilder:validation:MaxItems=64
	// +kubebuilder:validation:items:MaxLength=64
	// +kubebuilder:validation:items:Pattern=`^(\*|[0-9A-Fa-f.:]+/[0-9]{1,3})$`
	// +listType=atomic
	// +optional
	TrustedProxies []string `json:"trustedProxies,omitempty"`

	// disableRegistration turns off self sign-up.
	// +optional
	DisableRegistration bool `json:"disableRegistration,omitempty"`

	// requireSignInView forces login to browse anything.
	// +optional
	RequireSignInView bool `json:"requireSignInView,omitempty"`

	// http configures the HTTP Service.
	// +kubebuilder:default={}
	// +optional
	HTTP ServiceSpec `json:"http,omitempty"`

	// ssh configures Git-over-SSH.
	// +kubebuilder:default={}
	// +optional
	SSH SSHSpec `json:"ssh,omitempty"`
}

// ServiceSpec configures a Kubernetes Service.
type ServiceSpec struct {
	// +kubebuilder:default=ClusterIP
	// +kubebuilder:validation:Enum=ClusterIP;NodePort;LoadBalancer
	// +optional
	Type corev1.ServiceType `json:"type,omitempty"`

	// port is the Service port.
	// +optional
	Port int32 `json:"port,omitempty"`

	// nodePort, only for NodePort/LoadBalancer.
	// +optional
	NodePort int32 `json:"nodePort,omitempty"`

	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// +optional
	LoadBalancerClass *string `json:"loadBalancerClass,omitempty"`
}

// SSHSpec configures Git-over-SSH.
type SSHSpec struct {
	// enabled exposes SSH. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// externalPort is the port shown in clone URLs (SSH_PORT). Defaults to the service port.
	// With spec.gateway.ssh, set it to the port of the Gateway's TCP listener.
	// +optional
	ExternalPort int32 `json:"externalPort,omitempty"`

	// service configures the SSH Service. Port defaults to 22.
	// +kubebuilder:default={}
	// +optional
	Service ServiceSpec `json:"service,omitempty"`

	// hostKeys configures the SSH server's host keys. By default the operator generates them
	// once into the Secret "<name>-ssh-host-keys" and never rotates them.
	// +optional
	HostKeys *SSHHostKeysSpec `json:"hostKeys,omitempty"`
}

// SSHHostKeysSpec configures the SSH host keys.
type SSHHostKeysSpec struct {
	// existingSecret names a Secret with your own host keys, in OpenSSH or PEM format, under
	// one or more of the keys "ssh_host_ed25519_key", "ssh_host_ecdsa_key" and
	// "ssh_host_rsa_key" (the files "ssh-keygen -A" writes). The operator neither generates
	// nor changes keys then.
	// +optional
	ExistingSecret string `json:"existingSecret,omitempty"`
}

// IngressSpec exposes Forgejo over HTTP(S).
type IngressSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// className selects the IngressClass. When unset, the class the cluster's default
	// IngressClass admission assigned at creation is kept.
	// +optional
	ClassName *string `json:"className,omitempty"`

	// host is the public host name.
	// +optional
	Host string `json:"host,omitempty"`

	// +kubebuilder:default="/"
	// +optional
	Path string `json:"path,omitempty"`

	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// tlsSecretName enables TLS using this Secret. Defaults to "<name>-tls", the Secret of
	// the certificate, when spec.tls.certManager is set.
	// +optional
	TLSSecretName string `json:"tlsSecretName,omitempty"`
}

// GatewaySpec attaches Forgejo to existing Gateways through Gateway API routes.
type GatewaySpec struct {
	// parentRefs are the Gateways, or their listeners (sectionName), that HTTPRoute "<name>"
	// attaches to. Their listeners must allow routes from the instance's namespace. A
	// reference without sectionName or port attaches to every listener that matches the host
	// names, plain HTTP ones included: with spec.tls, name the HTTPS listeners.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	ParentRefs []GatewayParentReference `json:"parentRefs"`

	// hostnames the HTTPRoute matches. Defaults to the public host names that are DNS names:
	// the hosts of ROOT_URL and DOMAIN (spec.server.domain, ingress.host or the host of
	// spec.server.rootURL). Without any, no HTTPRoute is created (RoutesReady=False,
	// NoHostnames): a route without host names would take every host name of its listeners.
	// Unless spec.server.domain or an Ingress host is set, the first one that is not a
	// wildcard is the public host name (DOMAIN, ROOT_URL).
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^(\*\.)?[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +listType=atomic
	// +optional
	Hostnames []string `json:"hostnames,omitempty"`

	// path is the path prefix Forgejo is served under, "/" by default. Another prefix becomes
	// part of ROOT_URL, and the HTTPRoute removes it before requests reach Forgejo (URLRewrite,
	// Gateway API extended support).
	// +kubebuilder:default="/"
	// +kubebuilder:validation:MaxLength=256
	// +kubebuilder:validation:Pattern=`^/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*$`
	// +optional
	Path string `json:"path,omitempty"`

	// requestTimeout is the HTTPRoute's rules[].timeouts.request, a Gateway API duration such
	// as "0s" (no timeout), "30m" or "1h". Defaults to "0s": Gateways otherwise end requests
	// after their own default (15s with Envoy Gateway), which cuts long clones, pushes and
	// downloads. An empty string leaves timeouts out of the route, for implementations that
	// do not support them.
	// +kubebuilder:default="0s"
	// +kubebuilder:validation:Pattern=`^(([0-9]{1,5}(h|m|s|ms)){1,4})?$`
	// +optional
	RequestTimeout *string `json:"requestTimeout,omitempty"`

	// removeRequestHeaders are request headers the HTTPRoute removes before requests reach
	// Forgejo (a RequestHeaderModifier filter), besides X-Real-IP, which it always removes:
	// Forgejo prefers it to X-Forwarded-For, and Gateways such as Envoy Gateway pass a
	// client's own X-Real-IP through, which would let any client choose the address Forgejo
	// logs and rate-limits.
	// +kubebuilder:validation:MaxItems=15
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=256
	// +kubebuilder:validation:items:Pattern=`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`
	// +listType=atomic
	// +optional
	RemoveRequestHeaders []string `json:"removeRequestHeaders,omitempty"`

	// annotations are added to the routes.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// ssh adds TCPRoute "<name>-ssh" for Git over SSH through a TCP listener of a Gateway.
	// Without it, SSH is served by Service "<name>-ssh" only (spec.server.ssh.service). Set
	// spec.server.ssh.externalPort to the listener's port, which clone URLs show.
	// +optional
	SSH *GatewaySSHSpec `json:"ssh,omitempty"`
}

// GatewaySSHSpec routes Git over SSH through a TCP listener of a Gateway.
type GatewaySSHSpec struct {
	// parentRefs are the Gateways, usually one TCP listener of each (sectionName), that
	// TCPRoute "<name>-ssh" attaches to.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=32
	// +listType=atomic
	ParentRefs []GatewayParentReference `json:"parentRefs"`
}

// GatewayParentReference identifies a Gateway, or some of its listeners, that a route
// attaches to (a Gateway API ParentReference).
type GatewayParentReference struct {
	// group of the parent resource. Defaults to gateway.networking.k8s.io.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	Group string `json:"group,omitempty"`

	// kind of the parent resource. Defaults to Gateway.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-zA-Z]([-a-zA-Z0-9]*[a-zA-Z0-9])?$`
	// +optional
	Kind string `json:"kind,omitempty"`

	// namespace of the parent. Defaults to the instance's namespace.
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// name of the parent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// sectionName selects one listener of the Gateway by its name.
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	// +optional
	SectionName string `json:"sectionName,omitempty"`

	// port selects the Gateway's listeners on this port (Gateway API extended support).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int32 `json:"port,omitempty"`
}

// TLSSpec says that Forgejo is served over HTTPS.
type TLSSpec struct {
	// certManager requests the certificate from cert-manager: the operator creates Certificate
	// "<name>-tls", which cert-manager keeps in Secret "<name>-tls". An Ingress uses that
	// Secret by default; a Gateway's HTTPS listener references it in its certificateRefs.
	// Without certManager, the Ingress's tlsSecretName or the Gateway's own certificate
	// serves TLS. Requires cert-manager.
	// +optional
	CertManager *CertManagerTLSSpec `json:"certManager,omitempty"`

	// gatewayNamespaces are the namespaces of Gateways whose HTTPS listeners reference Secret
	// "<name>-tls". The operator creates ReferenceGrant "<name>-tls" in the instance's
	// namespace, which lets Gateways in these namespaces read that Secret and nothing else.
	// A Gateway in the instance's namespace needs none. Requires the Gateway API CRDs.
	// +kubebuilder:validation:MaxItems=16
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=63
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +listType=atomic
	// +optional
	GatewayNamespaces []string `json:"gatewayNamespaces,omitempty"`
}

// CertManagerTLSSpec requests a certificate from cert-manager.
type CertManagerTLSSpec struct {
	// issuerRef is the Issuer (in the instance's namespace) or ClusterIssuer that signs the
	// certificate.
	IssuerRef CertManagerIssuerRef `json:"issuerRef"`

	// dnsNames of the certificate. Defaults to the public host names that are DNS names: the
	// hosts of ROOT_URL and DOMAIN, ingress.host and the HTTPRoute's host names.
	// +kubebuilder:validation:MaxItems=32
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +listType=atomic
	// +optional
	DNSNames []string `json:"dnsNames,omitempty"`
}

// CertManagerIssuerRef references a cert-manager issuer.
type CertManagerIssuerRef struct {
	// name of the issuer.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// kind of the issuer: Issuer (the default) or ClusterIssuer, or the kind of an external
	// issuer.
	// +kubebuilder:default=Issuer
	// +kubebuilder:validation:MaxLength=63
	// +optional
	Kind string `json:"kind,omitempty"`

	// group of the issuer. Defaults to cert-manager.io; set it for external issuers.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	Group string `json:"group,omitempty"`
}

// DatabaseType selects the PostgreSQL provider.
// +kubebuilder:validation:Enum=CNPG;External
type DatabaseType string

const (
	DatabaseTypeCNPG     DatabaseType = "CNPG"
	DatabaseTypeExternal DatabaseType = "External"
)

// DatabaseSpec selects the PostgreSQL backend.
type DatabaseSpec struct {
	// +kubebuilder:default=CNPG
	// +optional
	Type DatabaseType `json:"type,omitempty"`

	// cnpg configures the operator-managed CloudNativePG cluster.
	// +kubebuilder:default={}
	// +optional
	CNPG CNPGSpec `json:"cnpg,omitempty"`

	// external points at an existing PostgreSQL server.
	// +optional
	External *ExternalDatabaseSpec `json:"external,omitempty"`
}

// CNPGSpec configures the CloudNativePG Cluster created for Forgejo.
type CNPGSpec struct {
	// instances is the number of PostgreSQL instances (1 primary + N-1 replicas).
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// imageName is the PostgreSQL operand image. Also used for backup/restore jobs.
	// +kubebuilder:default="ghcr.io/cloudnative-pg/postgresql:17"
	// +optional
	ImageName string `json:"imageName,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Storage VolumeSpec `json:"storage,omitempty"`

	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// parameters are extra postgresql.conf parameters.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// enablePodMonitor asks CNPG to create a PodMonitor for PostgreSQL metrics.
	// +optional
	EnablePodMonitor bool `json:"enablePodMonitor,omitempty"`
}

// ExternalDatabaseSpec points at an existing PostgreSQL server.
type ExternalDatabaseSpec struct {
	// host of the PostgreSQL server. Short names are resolved from the instance's namespace,
	// like they are inside its pods.
	// +kubebuilder:validation:MinLength=1
	Host string `json:"host"`

	// +kubebuilder:default=5432
	// +optional
	Port int32 `json:"port,omitempty"`

	// +kubebuilder:default=forgejo
	// +optional
	Database string `json:"database,omitempty"`

	// +kubebuilder:default=disable
	// +kubebuilder:validation:Enum=disable;require;verify-ca;verify-full
	// +optional
	SSLMode string `json:"sslMode,omitempty"`

	// credentialsSecret holds the username and password.
	CredentialsSecret SecretCredentialsRef `json:"credentialsSecret"`
}

// SecretCredentialsRef references username/password keys in a Secret.
type SecretCredentialsRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:default=username
	// +optional
	UsernameKey string `json:"usernameKey,omitempty"`

	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// VolumeSpec describes a PVC.
type VolumeSpec struct {
	// +kubebuilder:default="5Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// RetainPolicy decides what happens to data when the Forgejo resource is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type RetainPolicy string

const (
	RetainPolicyRetain RetainPolicy = "Retain"
	RetainPolicyDelete RetainPolicy = "Delete"
)

// StorageSpec configures the Forgejo data volume.
type StorageSpec struct {
	// +kubebuilder:default="10Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// accessModes for the data PVC. ReadWriteMany is recommended for multi-node HA.
	// +optional
	AccessModes []corev1.PersistentVolumeAccessMode `json:"accessModes,omitempty"`

	// existingClaim uses a pre-provisioned PVC instead of creating one.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`

	// retainPolicy controls whether the data PVC, backup PVC and CNPG cluster are deleted
	// together with the Forgejo resource.
	// +kubebuilder:default=Retain
	// +optional
	RetainPolicy RetainPolicy `json:"retainPolicy,omitempty"`
}

// AdminSpec configures the bootstrap administrator.
type AdminSpec struct {
	// +kubebuilder:default="forgejo_admin"
	// +optional
	Username string `json:"username,omitempty"`

	// +kubebuilder:default="forgejo_admin@forgejo.local"
	// +optional
	Email string `json:"email,omitempty"`

	// existingSecret holds the admin password under passwordKey. When empty the operator
	// generates "<name>-admin" with keys "username" and "password".
	// +optional
	ExistingSecret string `json:"existingSecret,omitempty"`

	// +kubebuilder:default=password
	// +optional
	PasswordKey string `json:"passwordKey,omitempty"`
}

// ConfigValueFromSecret sets one app.ini key from a Secret.
type ConfigValueFromSecret struct {
	// +kubebuilder:validation:MinLength=1
	Section string `json:"section"`
	// +kubebuilder:validation:MinLength=1
	Key string `json:"key"`

	SecretKeyRef corev1.SecretKeySelector `json:"secretKeyRef"`
}

// PodTemplateSpec carries scheduling knobs for Forgejo pods.
type PodTemplateSpec struct {
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`
	// +optional
	PriorityClassName string `json:"priorityClassName,omitempty"`
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// BackupSpec configures scheduled backups for a Forgejo instance.
type BackupSpec struct {
	// enabled turns on scheduled backups.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// schedule in cron format.
	// +kubebuilder:default="0 3 * * *"
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// retention is the number of successful scheduled backups to keep.
	// +kubebuilder:default=7
	// +kubebuilder:validation:Minimum=1
	// +optional
	Retention int32 `json:"retention,omitempty"`

	// includeData adds the data volume (repositories, LFS, attachments) to backups. Defaults to true.
	// +optional
	IncludeData *bool `json:"includeData,omitempty"`

	// storage configures the backup PVC ("<name>-backups"), used by every backup kind
	// (scheduled, manual, pre-upgrade).
	// +kubebuilder:default={}
	// +optional
	Storage BackupStorageSpec `json:"storage,omitempty"`
}

// BackupStorageSpec configures where backups are written.
type BackupStorageSpec struct {
	// +kubebuilder:default="20Gi"
	// +optional
	Size resource.Quantity `json:"size,omitempty"`

	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`

	// existingClaim writes backups to a pre-provisioned PVC.
	// +optional
	ExistingClaim string `json:"existingClaim,omitempty"`
}

// UpgradeSpec configures image change orchestration.
type UpgradeSpec struct {
	// backupBeforeUpgrade takes a ForgejoBackup before the new image is rolled out. Defaults to true.
	// +optional
	BackupBeforeUpgrade *bool `json:"backupBeforeUpgrade,omitempty"`

	// autoRollback restores the pre-upgrade backup and reverts the image when the new version
	// does not become healthy within timeout. Defaults to true.
	// +optional
	AutoRollback *bool `json:"autoRollback,omitempty"`

	// timeout for the new version to become Ready.
	// +kubebuilder:default="15m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive Go duration such as 30s, 5m or 1h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// allowDowngrade permits moving to an older version. Forgejo does not support database
	// downgrades, so this is only safe together with a restore.
	// +optional
	AllowDowngrade bool `json:"allowDowngrade,omitempty"`
}

// MonitoringSpec configures observability integration.
type MonitoringSpec struct {
	// enabled exposes Forgejo's /metrics endpoint protected by a generated bearer token.
	// Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// serviceMonitor creates a prometheus-operator ServiceMonitor. Defaults to true when the
	// CRD is installed.
	// +optional
	ServiceMonitor *PrometheusResourceSpec `json:"serviceMonitor,omitempty"`

	// prometheusRule creates alerting rules. Defaults to true when the CRD is installed.
	// +optional
	PrometheusRule *PrometheusResourceSpec `json:"prometheusRule,omitempty"`

	// grafanaDashboard creates a ConfigMap with a Grafana dashboard (sidecar-discoverable).
	// +optional
	GrafanaDashboard *PrometheusResourceSpec `json:"grafanaDashboard,omitempty"`

	// statsInterval is how often the operator samples Forgejo statistics into status.stats.
	// +kubebuilder:default="5m"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive Go duration such as 30s, 5m or 1h"
	// +optional
	StatsInterval *metav1.Duration `json:"statsInterval,omitempty"`
}

// PrometheusResourceSpec toggles a monitoring resource and its labels.
type PrometheusResourceSpec struct {
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// interval, for ServiceMonitor only, in Prometheus duration format (e.g. 60s, 1m30s).
	// +kubebuilder:validation:Pattern=`^(0|(([0-9]+)y)?(([0-9]+)w)?(([0-9]+)d)?(([0-9]+)h)?(([0-9]+)m)?(([0-9]+)s)?(([0-9]+)ms)?)$`
	// +optional
	Interval string `json:"interval,omitempty"`
}

// HighAvailabilitySpec configures multi-replica operation.
type HighAvailabilitySpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default={}
	// +optional
	Valkey ValkeySpec `json:"valkey,omitempty"`
}

// ValkeySpec configures the Valkey (Redis-compatible) instance used for cache, sessions and queues.
type ValkeySpec struct {
	// +kubebuilder:default="docker.io/valkey/valkey:8-alpine"
	// +optional
	Image string `json:"image,omitempty"`

	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`
}

// AutoscalingSpec configures scaling.
type AutoscalingSpec struct {
	// +optional
	Horizontal *HorizontalAutoscalingSpec `json:"horizontal,omitempty"`

	// +optional
	Vertical *VerticalAutoscalingSpec `json:"vertical,omitempty"`
}

// HorizontalAutoscalingSpec creates a HorizontalPodAutoscaler. Requires highAvailability.enabled.
type HorizontalAutoscalingSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// +kubebuilder:default=2
	// +kubebuilder:validation:Minimum=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// +kubebuilder:default=75
	// +optional
	TargetCPUUtilizationPercentage int32 `json:"targetCPUUtilizationPercentage,omitempty"`

	// +optional
	TargetMemoryUtilizationPercentage *int32 `json:"targetMemoryUtilizationPercentage,omitempty"`
}

// VerticalAutoscalingSpec lets the operator right-size the Forgejo container from observed
// usage (metrics.k8s.io) and OOM kills.
type VerticalAutoscalingSpec struct {
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// minAllowed resources the operator will not go below.
	// +optional
	MinAllowed corev1.ResourceList `json:"minAllowed,omitempty"`

	// maxAllowed resources the operator will not exceed.
	// +optional
	MaxAllowed corev1.ResourceList `json:"maxAllowed,omitempty"`

	// targetUtilizationPercentage of requests the operator aims for.
	// +kubebuilder:default=70
	// +optional
	TargetUtilizationPercentage int32 `json:"targetUtilizationPercentage,omitempty"`
}

// AutoPilotSpec configures autonomous operations.
type AutoPilotSpec struct {
	// +kubebuilder:default={}
	// +optional
	SelfHealing SelfHealingSpec `json:"selfHealing,omitempty"`

	// +kubebuilder:default={}
	// +optional
	AutoTuning AutoTuningSpec `json:"autoTuning,omitempty"`

	// +kubebuilder:default={}
	// +optional
	StorageAutoExpand StorageAutoExpandSpec `json:"storageAutoExpand,omitempty"`
}

// SelfHealingSpec configures abnormality detection and remediation.
type SelfHealingSpec struct {
	// enabled defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// healthCheckInterval between /api/healthz probes performed by the operator.
	// +kubebuilder:default="30s"
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=32
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|μs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="must be a positive Go duration such as 30s, 5m or 1h"
	// +optional
	HealthCheckInterval *metav1.Duration `json:"healthCheckInterval,omitempty"`

	// failureThreshold consecutive failed health checks before remediation.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// configRollback reverts to the last known good app.ini when a configuration change makes
	// Forgejo crash-loop. Defaults to true.
	// +optional
	ConfigRollback *bool `json:"configRollback,omitempty"`
}

// AutoTuningSpec configures automatic Forgejo tuning from the pod's resources.
type AutoTuningSpec struct {
	// enabled derives GOMEMLIMIT, DB connection pool sizes and queue workers from resources.
	// Values in spec.config always win. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`
}

// StorageAutoExpandSpec grows PVCs before they fill up.
type StorageAutoExpandSpec struct {
	// enabled defaults to true. Requires a StorageClass with allowVolumeExpansion.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// thresholdPercent of used capacity that triggers an expansion.
	// +kubebuilder:default=80
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=99
	// +optional
	ThresholdPercent int32 `json:"thresholdPercent,omitempty"`

	// incrementPercent of current size added on each expansion.
	// +kubebuilder:default=50
	// +kubebuilder:validation:Minimum=10
	// +optional
	IncrementPercent int32 `json:"incrementPercent,omitempty"`

	// maxSize caps automatic growth.
	// +kubebuilder:default="200Gi"
	// +optional
	MaxSize resource.Quantity `json:"maxSize,omitempty"`
}

// ForgejoStatus defines the observed state of Forgejo.
type ForgejoStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// +optional
	Phase ForgejoPhase `json:"phase,omitempty"`

	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// image currently rolled out and verified healthy.
	// +optional
	Image string `json:"image,omitempty"`

	// version reported by the running Forgejo (/api/v1/version).
	// +optional
	Version string `json:"version,omitempty"`

	// url is the external ROOT_URL.
	// +optional
	URL string `json:"url,omitempty"`

	// internalURL is the in-cluster HTTP endpoint.
	// +optional
	InternalURL string `json:"internalURL,omitempty"`

	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// selector for the scale subresource.
	// +optional
	Selector string `json:"selector,omitempty"`

	// adminSecretName holds the bootstrap admin credentials.
	// +optional
	AdminSecretName string `json:"adminSecretName,omitempty"`

	// configHash of the app.ini currently deployed.
	// +optional
	ConfigHash string `json:"configHash,omitempty"`

	// +optional
	Database *DatabaseStatus `json:"database,omitempty"`

	// +optional
	Upgrade *UpgradeStatus `json:"upgrade,omitempty"`

	// +optional
	Backup *BackupStatusSummary `json:"backup,omitempty"`

	// +optional
	Health *HealthStatus `json:"health,omitempty"`

	// +optional
	Stats *ForgejoStats `json:"stats,omitempty"`

	// +optional
	Storage *StorageStatus `json:"storage,omitempty"`

	// +optional
	AutoPilot *AutoPilotStatus `json:"autoPilot,omitempty"`

	// ssh reports the SSH server's host keys.
	// +optional
	SSH *SSHStatus `json:"ssh,omitempty"`
}

// SSHHostKeysSource tells where an instance's SSH host keys came from.
// +kubebuilder:validation:Enum=Generated;ExistingSecret
type SSHHostKeysSource string

const (
	// SSHHostKeysGenerated: the operator generated the keys.
	SSHHostKeysGenerated SSHHostKeysSource = "Generated"
	// SSHHostKeysExistingSecret: spec.server.ssh.hostKeys.existingSecret, or a
	// "<name>-ssh-host-keys" Secret the user created.
	SSHHostKeysExistingSecret SSHHostKeysSource = "ExistingSecret"
)

// SSHStatus reports the host keys of Forgejo's SSH server.
type SSHStatus struct {
	// hostKeysSecretName is the Secret that holds the host keys.
	// +optional
	HostKeysSecretName string `json:"hostKeysSecretName,omitempty"`
	// hostKeysSource tells where the keys came from.
	// +optional
	HostKeysSource SSHHostKeysSource `json:"hostKeysSource,omitempty"`
	// hostKeys are the public host keys and their SHA256 fingerprints, to publish to users
	// (e.g. in known_hosts).
	// +optional
	HostKeys []SSHHostKey `json:"hostKeys,omitempty"`
	// message explains a problem with the keys.
	// +optional
	Message string `json:"message,omitempty"`
}

// SSHHostKey is one public host key.
type SSHHostKey struct {
	// type is the key algorithm, e.g. ssh-ed25519.
	Type string `json:"type"`
	// fingerprint is the SHA256 fingerprint as ssh-keygen -l prints it ("SHA256:...").
	Fingerprint string `json:"fingerprint"`
	// publicKey in authorized_keys format ("<type> <base64>").
	PublicKey string `json:"publicKey"`
}

// DatabaseStatus reports the PostgreSQL backend.
type DatabaseStatus struct {
	// +optional
	Type DatabaseType `json:"type,omitempty"`
	// +optional
	ClusterName string `json:"clusterName,omitempty"`
	// +optional
	Host string `json:"host,omitempty"`
	// +optional
	SecretName string `json:"secretName,omitempty"`
	// +optional
	Instances int32 `json:"instances,omitempty"`
	// +optional
	ReadyInstances int32 `json:"readyInstances,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
}

// UpgradePhase tracks an orchestrated upgrade.
// +kubebuilder:validation:Enum=Pending;BackingUp;RollingOut;Verifying;Succeeded;Failed;RollingBack;RolledBack;Cancelled
type UpgradePhase string

const (
	UpgradePhasePending     UpgradePhase = "Pending"
	UpgradePhaseBackingUp   UpgradePhase = "BackingUp"
	UpgradePhaseRollingOut  UpgradePhase = "RollingOut"
	UpgradePhaseVerifying   UpgradePhase = "Verifying"
	UpgradePhaseSucceeded   UpgradePhase = "Succeeded"
	UpgradePhaseFailed      UpgradePhase = "Failed"
	UpgradePhaseRollingBack UpgradePhase = "RollingBack"
	UpgradePhaseRolledBack  UpgradePhase = "RolledBack"
	// UpgradePhaseCancelled: spec.image changed again before the new image was deployed.
	UpgradePhaseCancelled UpgradePhase = "Cancelled"
)

// UpgradeStatus reports the latest upgrade.
type UpgradeStatus struct {
	// +optional
	Phase UpgradePhase `json:"phase,omitempty"`
	// +optional
	FromImage string `json:"fromImage,omitempty"`
	// +optional
	ToImage string `json:"toImage,omitempty"`
	// +optional
	BackupName string `json:"backupName,omitempty"`
	// +optional
	RestoreName string `json:"restoreName,omitempty"`
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// rolloutStartTime is when the new image was first deployed; spec.upgrade.timeout counts
	// from it. A restore that pauses the upgrade moves it to the end of that pause.
	// +optional
	RolloutStartTime *metav1.Time `json:"rolloutStartTime,omitempty"`
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
	// failedImage is not retried until spec.image changes to something else.
	// +optional
	FailedImage string `json:"failedImage,omitempty"`
	// failureReason explains why the new version was considered failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`
	// retry marks the rollout of an image that a failed upgrade left deployed. Its server pods
	// created before startTime belong to the failed attempt.
	// +optional
	Retry bool `json:"retry,omitempty"`
	// rollbackRestoreFailed is set when the rollback's ForgejoRestore failed: the database may
	// still be in the new version's state, so Forgejo is kept stopped until a backup is restored.
	// +optional
	RollbackRestoreFailed bool `json:"rollbackRestoreFailed,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// BackupStatusSummary summarizes scheduled backups.
type BackupStatusSummary struct {
	// +optional
	LastBackupName string `json:"lastBackupName,omitempty"`
	// +optional
	LastSuccessfulBackupName string `json:"lastSuccessfulBackupName,omitempty"`
	// +optional
	LastSuccessfulBackupTime *metav1.Time `json:"lastSuccessfulBackupTime,omitempty"`
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`
	// +optional
	ActiveRestore string `json:"activeRestore,omitempty"`
}

// HealthStatus reports operator-observed application health.
type HealthStatus struct {
	// healthy is always reported, false included.
	// +optional
	Healthy bool `json:"healthy"`
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`
	// +optional
	LastHealthyTime *metav1.Time `json:"lastHealthyTime,omitempty"`
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
	// checks reported by /api/healthz, name -> status.
	// +optional
	Checks map[string]string `json:"checks,omitempty"`
}

// ForgejoStats are sampled from Forgejo's metrics endpoint.
type ForgejoStats struct {
	// +optional
	Users int64 `json:"users,omitempty"`
	// +optional
	Organizations int64 `json:"organizations,omitempty"`
	// +optional
	Repositories int64 `json:"repositories,omitempty"`
	// +optional
	Issues int64 `json:"issues,omitempty"`
	// +optional
	PullRequests int64 `json:"pullRequests,omitempty"`
	// +optional
	LastUpdated *metav1.Time `json:"lastUpdated,omitempty"`
}

// VolumeUsage reports usage of one PVC.
type VolumeUsage struct {
	ClaimName string `json:"claimName"`
	// +optional
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// +optional
	UsedBytes int64 `json:"usedBytes,omitempty"`
	// +optional
	UsedPercent int32 `json:"usedPercent,omitempty"`
	// +optional
	RequestedSize string `json:"requestedSize,omitempty"`
	// +optional
	ExpansionSupported bool `json:"expansionSupported,omitempty"`
	// +optional
	LastExpansionTime *metav1.Time `json:"lastExpansionTime,omitempty"`
}

// StorageStatus reports PVC usage.
type StorageStatus struct {
	// +optional
	Volumes []VolumeUsage `json:"volumes,omitempty"`
	// +optional
	LastCheckTime *metav1.Time `json:"lastCheckTime,omitempty"`
}

// AutoPilotAction records one autonomous action.
type AutoPilotAction struct {
	Time metav1.Time `json:"time"`
	// type, e.g. StorageExpanded, ConfigRolledBack, PodRestarted, ResourcesScaled, UpgradeRolledBack.
	Type string `json:"type"`
	// +optional
	Message string `json:"message,omitempty"`
}

// AutoPilotStatus reports the autonomous controller state.
type AutoPilotStatus struct {
	// lastKnownGoodConfigHash is the most recent app.ini hash that ran healthy.
	// +optional
	LastKnownGoodConfigHash string `json:"lastKnownGoodConfigHash,omitempty"`
	// rejectedConfigHash is a spec-derived app.ini hash that crash-looped and was rolled back.
	// +optional
	RejectedConfigHash string `json:"rejectedConfigHash,omitempty"`
	// recommendedResources from vertical autoscaling.
	// +optional
	RecommendedResources *corev1.ResourceRequirements `json:"recommendedResources,omitempty"`
	// appliedResources currently set on the Forgejo container by the operator.
	// +optional
	AppliedResources *corev1.ResourceRequirements `json:"appliedResources,omitempty"`
	// tuning lists the auto-tuned app.ini/env values.
	// +optional
	Tuning map[string]string `json:"tuning,omitempty"`
	// recentActions, newest first, capped at 10.
	// +optional
	RecentActions []AutoPilotAction `json:"recentActions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas,selectorpath=.status.selector
// +kubebuilder:resource:path=forgejos,shortName=fj,categories=forgejo
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.status.version`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Support",type=string,JSONPath=`.status.conditions[?(@.type=="VersionSupported")].reason`,priority=1
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Forgejo is a managed Forgejo software forge instance.
type Forgejo struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Forgejo
	// +required
	Spec ForgejoSpec `json:"spec"`

	// status defines the observed state of Forgejo
	// +optional
	Status ForgejoStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ForgejoList contains a list of Forgejo
type ForgejoList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Forgejo `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Forgejo{}, &ForgejoList{})
		return nil
	})
}
