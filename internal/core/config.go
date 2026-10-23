package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const settingRedis = "redis"

// rootSection is the name used in spec.config for app.ini keys outside any section.
const rootSection = "DEFAULT"

// defaultTrustedProxies are the private ranges a cluster's ingress controllers and gateways
// live in (spec.server.trustedProxies overrides them).
const defaultTrustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7"

// loopbackTrustedProxies are trusted by default when the HTTP Service is a LoadBalancer or a
// NodePort: with externalTrafficPolicy Cluster, kube-proxy replaces the address of a client
// on the internet with a node's, which is in the private ranges, so trusting those would let
// any client choose the address Forgejo logs.
const loopbackTrustedProxies = "127.0.0.0/8,::1/128"

// iniFile maps section -> key -> value; the root section is "".
type iniFile map[string]map[string]string

func (c iniFile) set(section, key, value string) {
	if section == rootSection {
		section = ""
	} else {
		section = strings.ToLower(section)
	}
	if c[section] == nil {
		c[section] = map[string]string{}
	}
	c[section][key] = value
}

func (c iniFile) merge(other map[string]map[string]string) {
	for section, kv := range other {
		for k, v := range kv {
			c.set(section, k, v)
		}
	}
}

// render writes sections and keys in sorted order so the output (and its hash) is stable.
func (c iniFile) render() string {
	var b strings.Builder
	writeKeys := func(kv map[string]string) {
		keys := make([]string, 0, len(kv))
		for k := range kv {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %s\n", k, quoteINI(kv[k]))
		}
	}
	writeKeys(c[""])
	sections := make([]string, 0, len(c))
	for s := range c {
		if s != "" {
			sections = append(sections, s)
		}
	}
	slices.Sort(sections)
	for _, s := range sections {
		fmt.Fprintf(&b, "\n[%s]\n", s)
		writeKeys(c[s])
	}
	return b.String()
}

// quoteINI protects values that go-ini (as configured by Forgejo, i.e. with inline
// comments enabled) would otherwise alter. go-ini strips the first and the last
// backtick of a single-line value, so backticks inside the value are fine.
func quoteINI(v string) string {
	if strings.Contains(v, "\n") {
		return `"""` + v + `"""`
	}
	if strings.ContainsAny(v, "#;`\"'\\") || v != strings.TrimSpace(v) {
		return "`" + v + "`"
	}
	return v
}

// ReconcileConfig renders app.ini into "<name>-config" and sets rc.ConfigSecretName,
// rc.ConfigHash and rc.DesiredConfigHash.
func ReconcileConfig(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if rc.DB == nil {
		return forgejo.StepResult{Stop: true, RequeueAfter: dbNotReadyRequeue}, nil
	}
	secrets := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, secrets); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("reading %s: %w", forgejo.SecretsName(f), err)
	}

	cfg := defaultConfig(rc, secrets.Data)
	if err := objectStorageConfig(cfg, f); err != nil {
		return forgejo.StepResult{}, fmt.Errorf("objectStorage: %w", err)
	}
	if rc.ValkeyURL != "" {
		cfg.merge(haConfig(rc.ValkeyURL))
	}
	cfg.merge(rc.Tuning)
	cfg.merge(f.Spec.Config)
	for _, from := range f.Spec.ConfigFrom {
		v, ok, err := secretValue(ctx, rc, from.SecretKeyRef)
		if err != nil {
			return forgejo.StepResult{}, fmt.Errorf("configFrom %s.%s: %w", from.Section, from.Key, err)
		}
		if ok {
			cfg.set(from.Section, from.Key, v)
		}
	}

	if strict, known := strictDurations(rc); strict {
		if bad := forgejo.InvalidDurations(cfg); len(bad) > 0 {
			what := rc.TargetImage
			if !known {
				what = fmt.Sprintf("If %s is Forgejo %d or newer, it", rc.TargetImage, forgejo.Versions.StrictDurationsSince())
			}
			warnEvery(rc, "invalid-durations/"+strings.Join(bad, ","), "ConfigIncompatible", "Config",
				"%s refuses to start with %s: these must be durations with a unit (e.g. 30s, -1s). Fix them "+
					"in spec.config or configFrom.", what, strings.Join(bad, ", "))
		}
	}

	if hk := rc.SSHHostKeys; hk != nil {
		cfg.set("server", "SSH_SERVER_HOST_KEYS", hostKeyPaths(rc.Layout, hk))
	}
	content := cfg.render()
	hash := configHash(content)

	// While the Deployment is held for missing SSH host keys (rc.SSHHostKeysUnavailable), the
	// pods keep the app.ini they run: it is mounted without subPath, so a change would reach
	// them at their next container restart, without the rollout that the config guard watches.
	// It is then rendered for the hashes only.
	if !rc.SSHHostKeysUnavailable {
		if err := writeConfig(ctx, rc, content, hash); err != nil {
			return forgejo.StepResult{}, err
		}
	}

	rc.ConfigHash = hash
	rc.DesiredConfigHash = hash
	if rc.ConfigSecretName == "" {
		rc.ConfigSecretName = forgejo.ConfigSecretName(f)
	}
	return forgejo.StepResult{}, nil
}

// strictDurations reports whether the deployed image refuses unparsable durations
// (forgejo.Traits.StrictDurations), and whether its version is known. An image whose tag
// carries no version (a digest, a custom tag) gets CompatTraits, which are strict so that a
// digest-pinned 17 is warned about; once it runs (status.image) and Forgejo has reported its
// version, that version decides, so that a digest-pinned 15 or 16 is not.
func strictDurations(rc *forgejo.ReconcileContext) (strict, known bool) {
	strict = rc.Layout.Traits.StrictDurations
	if _, ok := forgejo.ImageVersion(rc.TargetImage); ok {
		return strict, true
	}
	if st := rc.Instance.Status; rc.TargetImage == st.Image {
		if v, ok := forgejo.InstanceVersion(rc.TargetImage, st.Version); ok {
			return forgejo.Versions.Traits(v, true).StrictDurations, true
		}
	}
	return strict, false
}

// configHash identifies a rendered app.ini (the pod template's config-hash annotation).
func configHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

// writeConfig stores the rendered app.ini in "<name>-config".
func writeConfig(ctx context.Context, rc *forgejo.ReconcileContext, content, hash string) error {
	f := rc.Instance
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.ConfigSecretName(f), Namespace: f.Namespace}}
	if err := apply(ctx, rc, sec, true, func() error {
		sec.Labels = mergeMaps(sec.Labels, forgejo.Labels(f, forgejo.ComponentServer))
		sec.Annotations = mergeMaps(sec.Annotations, map[string]string{
			forgejo.AnnotationConfigHash:    hash,
			forgejo.AnnotationConfigProfile: rc.Layout.Traits.ConfigProfile(),
		})
		sec.Type = corev1.SecretTypeOpaque
		sec.Data = map[string][]byte{forgejo.ConfigSecretKey: []byte(content)}
		return nil
	}); err != nil {
		return fmt.Errorf("config secret: %w", err)
	}
	return nil
}

// secretValue reads one configFrom value; ok is false for a missing optional reference.
func secretValue(ctx context.Context, rc *forgejo.ReconcileContext, ref corev1.SecretKeySelector) (string, bool, error) {
	optional := forgejo.BoolOr(ref.Optional, false)
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: rc.Instance.Namespace, Name: ref.Name}, sec); err != nil {
		if optional && apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	v, found := sec.Data[ref.Key]
	if !found {
		if optional {
			return "", false, nil
		}
		return "", false, fmt.Errorf("secret %q has no key %q", ref.Name, ref.Key)
	}
	return string(v), true, nil
}

// defaultConfig is everything the operator pins: paths from the image layout, the
// network identity, the database and the generated secrets.
func defaultConfig(rc *forgejo.ReconcileContext, secrets map[string][]byte) iniFile {
	f, l, db := rc.Instance, rc.Layout, rc.DB
	sshEnabled := forgejo.SSHEnabled(f)
	cfg := iniFile{}
	set := cfg.set

	set(rootSection, "APP_NAME", appName(f))
	set(rootSection, "RUN_MODE", "prod")
	set(rootSection, "RUN_USER", l.RunUser)
	set(rootSection, "WORK_PATH", l.WorkPath)

	set("server", "APP_DATA_PATH", l.WorkPath)
	set("server", "PROTOCOL", "http")
	set("server", "HTTP_ADDR", "0.0.0.0")
	set("server", "HTTP_PORT", strconv.Itoa(int(l.HTTPContainerPort)))
	set("server", "DOMAIN", Domain(f))
	set("server", "ROOT_URL", RootURL(f))
	set("server", "LFS_START_SERVER", "true")
	set("server", "LFS_JWT_SECRET", string(secrets[forgejo.SecretKeyLFSJWTSecret]))
	set("server", "DISABLE_SSH", strconv.FormatBool(!sshEnabled))
	set("server", "START_SSH_SERVER", strconv.FormatBool(sshEnabled))
	if sshEnabled {
		set("server", "SSH_DOMAIN", sshDomain(f))
		set("server", "SSH_PORT", strconv.Itoa(int(sshExternalPort(f))))
		set("server", "SSH_LISTEN_PORT", strconv.Itoa(int(l.SSHContainerPort)))
	}

	set("repository", "ROOT", l.RepoRoot)
	if lfsOnVolume(f) {
		set("lfs", "PATH", l.LFSPath)
	}

	set("database", "DB_TYPE", "postgres")
	set("database", "HOST", hostPort(db.Host, db.Port))
	set("database", "NAME", db.Name)
	set("database", "USER", db.User)
	set("database", "PASSWD", db.Password)
	set("database", "SSL_MODE", db.SSLMode)

	set("security", "INSTALL_LOCK", "true")
	set("security", "SECRET_KEY", string(secrets[forgejo.SecretKeySecretKey]))
	// Per Forgejo major (forgejo.Traits). The token stays in "<name>-secrets" either way, so
	// that rolling back to an older major finds it.
	if l.Traits.InternalToken {
		set("security", "INTERNAL_TOKEN", string(secrets[forgejo.SecretKeyInternalToken]))
	}
	if l.Traits.InternalSocket {
		set("server", "INTERNAL_LISTENER_PATH", l.InternalSocketPath)
	}
	set("security", "REVERSE_PROXY_TRUSTED_PROXIES", trustedProxies(f))
	set("oauth2", "JWT_SECRET", string(secrets[forgejo.SecretKeyJWTSecret]))

	set("service", "DISABLE_REGISTRATION", strconv.FormatBool(f.Spec.Server.DisableRegistration))
	set("service", "REQUIRE_SIGNIN_VIEW", strconv.FormatBool(f.Spec.Server.RequireSignInView))
	// Sessions in the database survive restarts and work across replicas.
	set("session", "PROVIDER", "db")

	if forgejo.MonitoringEnabled(f) {
		set("metrics", "ENABLED", "true")
		set("metrics", "TOKEN", string(secrets[forgejo.SecretKeyMetricsToken]))
	} else {
		set("metrics", "ENABLED", "false")
	}

	set("log", "MODE", "console")
	set("log", "LEVEL", "Info")
	set("cron.update_checker", "ENABLED", "false")
	return cfg
}

// haConfig moves cache, sessions and queues to Valkey so that replicas share state, and
// keeps indexers out of the (per-pod) local disk.
func haConfig(valkeyURL string) map[string]map[string]string {
	db := func(n int) string { return strings.TrimRight(valkeyURL, "/") + "/" + strconv.Itoa(n) }
	return map[string]map[string]string{
		"cache":   {"ADAPTER": settingRedis, "HOST": db(0)},
		"session": {"PROVIDER": settingRedis, "PROVIDER_CONFIG": db(1)},
		"queue":   {"TYPE": settingRedis, "CONN_STR": db(2)},
		"indexer": {"ISSUE_INDEXER_TYPE": "db", "REPO_INDEXER_ENABLED": "false"},
	}
}

func hostPort(host string, port int32) string {
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

func appName(f *forgejov1.Forgejo) string {
	if f.Spec.Server.AppName != "" {
		return f.Spec.Server.AppName
	}
	return "Forgejo"
}

// sshExternalPort is SSH_PORT, the port of clone URLs: spec.server.ssh.externalPort, else
// the port that every spec.gateway.ssh parent reference names (the Gateway's TCP listener),
// else the SSH Service's port. The operator does not read Gateways, so parent references by
// sectionName only, or with different ports, need externalPort.
func sshExternalPort(f *forgejov1.Forgejo) int32 {
	if p := f.Spec.Server.SSH.ExternalPort; p != 0 {
		return p
	}
	if p := gatewaySSHPort(f); p != 0 {
		return p
	}
	return forgejo.SSHServicePort(f)
}

// gatewaySSHPort is the port all spec.gateway.ssh parent references name, or 0 when one of
// them names none or they name different ones.
func gatewaySSHPort(f *forgejov1.Forgejo) int32 {
	if !forgejo.GatewaySSHEnabled(f) {
		return 0
	}
	var port int32
	for _, r := range f.Spec.Gateway.SSH.ParentRefs {
		if r.Port == nil || *r.Port == 0 || (port != 0 && *r.Port != port) {
			return 0
		}
		port = *r.Port
	}
	return port
}

// trustedProxies is REVERSE_PROXY_TRUSTED_PROXIES: spec.server.trustedProxies, else only
// loopback when clients reach the HTTP Service directly (LoadBalancer, NodePort), else the
// private ranges.
func trustedProxies(f *forgejov1.Forgejo) string {
	if len(f.Spec.Server.TrustedProxies) > 0 {
		return strings.Join(f.Spec.Server.TrustedProxies, ",")
	}
	switch f.Spec.Server.HTTP.Type {
	case corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort:
		return loopbackTrustedProxies
	}
	return defaultTrustedProxies
}

func ingressEnabled(f *forgejov1.Forgejo) bool {
	return f.Spec.Ingress != nil && f.Spec.Ingress.Enabled
}

// ingressTLSSecret is the Secret the Ingress serves TLS with: ingress.tlsSecretName, else the
// certificate's Secret when spec.tls.certManager is set.
func ingressTLSSecret(f *forgejov1.Forgejo) string {
	if f.Spec.Ingress != nil && f.Spec.Ingress.TLSSecretName != "" {
		return f.Spec.Ingress.TLSSecretName
	}
	if forgejo.CertManagerEnabled(f) {
		return forgejo.TLSSecretName(f)
	}
	return ""
}

const (
	schemeHTTP  = "http"
	schemeHTTPS = "https"
)

// publicScheme is the scheme of ROOT_URL for a public host name: https when spec.tls says
// that Forgejo is served over HTTPS.
func publicScheme(f *forgejov1.Forgejo) string {
	if forgejo.TLSEnabled(f) {
		return schemeHTTPS
	}
	return schemeHTTP
}

func httpServiceFQDN(f *forgejov1.Forgejo) string {
	return forgejo.ServiceHost(forgejo.HTTPServiceName(f), f.Namespace)
}

// sshDomain is SSH_DOMAIN: the public domain when one is configured, else the SSH
// Service DNS name (the HTTP Service does not serve SSH).
func sshDomain(f *forgejov1.Forgejo) string {
	if d := Domain(f); d != httpServiceFQDN(f) {
		return d
	}
	return forgejo.ServiceHost(forgejo.SSHServiceName(f), f.Namespace)
}

// Domain is DOMAIN: spec.server.domain, else the ingress host, else the first HTTPRoute
// host name that is not a wildcard, else the host of spec.server.rootURL, else the HTTP
// Service DNS name.
func Domain(f *forgejov1.Forgejo) string {
	if f.Spec.Server.Domain != "" {
		return f.Spec.Server.Domain
	}
	if ingressEnabled(f) && f.Spec.Ingress.Host != "" {
		return f.Spec.Ingress.Host
	}
	if h := gatewayHost(f); h != "" {
		return h
	}
	if u, err := url.Parse(f.Spec.Server.RootURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return httpServiceFQDN(f)
}

// EffectiveRootURL is the ROOT_URL Forgejo runs with, including a spec.config override.
func EffectiveRootURL(f *forgejov1.Forgejo) string {
	for section, kv := range f.Spec.Config {
		if strings.EqualFold(section, "server") && kv["ROOT_URL"] != "" {
			return strings.TrimRight(kv["ROOT_URL"], "/") + "/"
		}
	}
	return RootURL(f)
}

// RootURL is the operator's ROOT_URL (always with a trailing slash, as Forgejo
// normalizes it).
func RootURL(f *forgejov1.Forgejo) string {
	u := func() string {
		if f.Spec.Server.RootURL != "" {
			return f.Spec.Server.RootURL
		}
		if ingressEnabled(f) && f.Spec.Ingress.Host != "" {
			scheme := publicScheme(f)
			if ingressTLSSecret(f) != "" {
				scheme = schemeHTTPS
			}
			return scheme + "://" + f.Spec.Ingress.Host + ingressPath(f)
		}
		if h := gatewayHost(f); h != "" {
			return publicScheme(f) + "://" + h + gatewayPath(f)
		}
		if f.Spec.Server.Domain != "" {
			return publicScheme(f) + "://" + f.Spec.Server.Domain + "/"
		}
		return "http://" + httpServiceFQDN(f) + ":" + strconv.Itoa(int(forgejo.HTTPServicePort(f))) + "/"
	}()
	return strings.TrimRight(u, "/") + "/"
}

func ingressPath(f *forgejov1.Forgejo) string {
	if f.Spec.Ingress == nil || f.Spec.Ingress.Path == "" {
		return "/"
	}
	return f.Spec.Ingress.Path
}
