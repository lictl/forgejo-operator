package forgejo

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"time"
)

// defaultClusterDomain is the DNS domain of almost every cluster (kubelet --cluster-domain).
const defaultClusterDomain = "cluster.local"

// ClusterDomain is the cluster's DNS domain, used for the in-cluster Service names the
// operator renders (app.ini DOMAIN/SSH_DOMAIN/ROOT_URL defaults, the CNPG host) and probes.
// It comes from the CLUSTER_DOMAIN environment variable, else from the "svc.<domain>" search
// entry of the operator pod's /etc/resolv.conf, else "cluster.local" (e.g. an operator
// running outside the cluster). cmd/main.go overrides it with --cluster-domain when set.
var ClusterDomain = detectClusterDomain(os.Getenv("CLUSTER_DOMAIN"), "/etc/resolv.conf")

func detectClusterDomain(env, resolvConf string) string {
	if d := strings.Trim(strings.TrimSpace(env), "."); d != "" {
		return d
	}
	if b, err := os.ReadFile(resolvConf); err == nil {
		if d := clusterDomainFromResolvConf(string(b)); d != "" {
			return d
		}
	}
	return defaultClusterDomain
}

// clusterDomainFromResolvConf returns <domain> of the "svc.<domain>" search entry that the
// kubelet writes into every ClusterFirst pod, or "".
func clusterDomainFromResolvConf(content string) string {
	for line := range strings.SplitSeq(content, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "search" {
			continue
		}
		for _, s := range fields[1:] {
			if d, ok := strings.CutPrefix(strings.TrimSuffix(s, "."), "svc."); ok && d != "" {
				return d
			}
		}
	}
	return ""
}

// ServiceHost is the fully qualified in-cluster DNS name of a Service.
func ServiceHost(name, namespace string) string {
	return name + "." + namespace + ".svc." + ClusterDomain
}

// ProbeHosts lists the names to try for host, in the order a pod in namespace resolves them:
// a single-label name is a Service in that namespace first ("<host>.<ns>.svc" resolves through
// the pod's search path whatever the cluster domain; the fully qualified form serves an
// operator running outside the cluster), then the name as given (node or corporate search
// domains).
func ProbeHosts(host, namespace string) []string {
	if net.ParseIP(host) != nil || strings.Contains(host, ".") || strings.EqualFold(host, "localhost") {
		return []string{host}
	}
	return []string{host + "." + namespace + ".svc", ServiceHost(host, namespace), host}
}

// NamespaceDialer dials host names as a pod of namespace resolves them (ProbeHosts): a short
// name is a Service of that namespace. The operator usually runs in another namespace.
func NamespaceDialer(namespace string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		var firstErr error
		for _, h := range ProbeHosts(host, namespace) {
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(h, port))
			if err == nil {
				return conn, nil
			}
			if firstErr == nil {
				firstErr = err
			}
			if dnsErr := (*net.DNSError)(nil); !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
				return nil, err
			}
		}
		return nil, firstErr
	}
}
