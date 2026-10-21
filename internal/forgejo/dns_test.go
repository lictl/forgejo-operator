package forgejo

import (
	"os"
	"path/filepath"
	"testing"
)

// The cluster domain comes from the env, the pod's resolv.conf, or cluster.local.
func TestDetectClusterDomain(t *testing.T) {
	dir := t.TempDir()
	rcFile := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(rcFile, []byte("nameserver 10.43.0.10\nsearch ns.svc.k8s.example. svc.k8s.example k8s.example\noptions ndots:5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := detectClusterDomain("", rcFile); d != "k8s.example" {
		t.Fatalf("resolv.conf: got %q", d)
	}
	if d := detectClusterDomain("corp.internal.", rcFile); d != "corp.internal" {
		t.Fatalf("env: got %q", d)
	}
	if d := detectClusterDomain("", filepath.Join(dir, "missing")); d != defaultClusterDomain {
		t.Fatalf("fallback: got %q", d)
	}
	if d := clusterDomainFromResolvConf("search example.com\nnameserver 1.1.1.1\n"); d != "" {
		t.Fatalf("outside a cluster: got %q", d)
	}
	old := ClusterDomain
	defer func() { ClusterDomain = old }()
	ClusterDomain = "k8s.example"
	if got := ServiceHost("svc", "ns"); got != "svc.ns.svc.k8s.example" {
		t.Fatalf("ServiceHost = %s", got)
	}
}
