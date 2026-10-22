package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// rootSection is the name used in spec.config for app.ini keys outside any section.
const rootSection = "DEFAULT"

// trustedProxies are the private ranges a cluster's ingress controllers live in.
const trustedProxies = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7"

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
		sort.Strings(keys)
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
	sort.Strings(sections)
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

	if rc.Layout.Traits.StrictDurations {
		if bad := forgejo.InvalidDurations(cfg); len(bad) > 0 {
			warnEvery(rc, "invalid-durations/"+strings.Join(bad, ","), warnInterval, "ConfigIncompatible", "Config",
				"%s refuses to start with %s: these must be durations with a unit (e.g. 30s, -1s). Fix them "+
					"in spec.config or configFrom.", rc.TargetImage, strings.Join(bad, ", "))
		}
	}

	content := cfg.render()
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])[:16]

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
		return forgejo.StepResult{}, fmt.Errorf("config secret: %w", err)
	}

	rc.ConfigHash = hash
	rc.DesiredConfigHash = hash
	if rc.ConfigSecretName == "" {
		rc.ConfigSecretName = forgejo.ConfigSecretName(f)
	}
	return forgejo.StepResult{}, nil
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
	set("lfs", "PATH", l.LFSPath)

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
	set("security", "REVERSE_PROXY_TRUSTED_PROXIES", trustedProxies)
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
		"cache":   {"ADAPTER": "redis", "HOST": db(0)},
		"session": {"PROVIDER": "redis", "PROVIDER_CONFIG": db(1)},
		"queue":   {"TYPE": "redis", "CONN_STR": db(2)},
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

func sshExternalPort(f *forgejov1.Forgejo) int32 {
	if p := f.Spec.Server.SSH.ExternalPort; p != 0 {
		return p
	}
	return forgejo.SSHServicePort(f)
}

func ingressEnabled(f *forgejov1.Forgejo) bool {
	return f.Spec.Ingress != nil && f.Spec.Ingress.Enabled
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

// Domain is DOMAIN: spec.server.domain, else the ingress host, else the
// host of spec.server.rootURL, else the HTTP Service DNS name.
func Domain(f *forgejov1.Forgejo) string {
	if f.Spec.Server.Domain != "" {
		return f.Spec.Server.Domain
	}
	if ingressEnabled(f) && f.Spec.Ingress.Host != "" {
		return f.Spec.Ingress.Host
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
			scheme := "http"
			if f.Spec.Ingress.TLSSecretName != "" {
				scheme = "https"
			}
			return scheme + "://" + f.Spec.Ingress.Host + ingressPath(f)
		}
		if f.Spec.Server.Domain != "" {
			return "http://" + f.Spec.Server.Domain + "/"
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
