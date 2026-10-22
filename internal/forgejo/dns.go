package forgejo

import (
	"os"
	"strings"
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
