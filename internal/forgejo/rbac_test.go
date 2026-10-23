package forgejo

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"
)

// The operator's permissions are split over several files (docs/permissions.md):
// config/rbac/role.yaml (generated from the +kubebuilder:rbac markers), the pods/exec role of
// config/rbac/volume_usage_role.yaml, and the cluster-scoped reads that the namespaced
// overlay grants on their own in config/namespaced/cluster_reader_role.yaml. These tests keep
// them consistent.

// clusterScopedResources are the cluster-scoped resources manager-role may name. A
// RoleBinding (config/namespaced) cannot grant them, so cluster_reader_role.yaml must.
var clusterScopedResources = map[string]bool{
	"/namespaces":                   true,
	"/nodes":                        true,
	"/persistentvolumes":            true,
	"storage.k8s.io/storageclasses": true,
}

// namespacedResources are the namespaced resources manager-role may name. A resource in
// neither list fails TestManagerRoleResourcesClassified: add it to the right one (and, if it
// is cluster-scoped, to config/namespaced/cluster_reader_role.yaml).
var namespacedResources = map[string]bool{
	"/configmaps":                               true,
	"/persistentvolumeclaims":                   true,
	"/pods":                                     true,
	"/secrets":                                  true,
	"/services":                                 true,
	"apps/deployments":                          true,
	"apps/statefulsets":                         true,
	"autoscaling/horizontalpodautoscalers":      true,
	"batch/jobs":                                true,
	"cert-manager.io/certificates":              true,
	"events.k8s.io/events":                      true,
	"forgejo.lucas.icu/forgejobackups":          true,
	"forgejo.lucas.icu/forgejorestores":         true,
	"forgejo.lucas.icu/forgejorunners":          true,
	"forgejo.lucas.icu/forgejos":                true,
	"gateway.networking.k8s.io/httproutes":      true,
	"gateway.networking.k8s.io/referencegrants": true,
	"gateway.networking.k8s.io/tcproutes":       true,
	"metrics.k8s.io/pods":                       true,
	"monitoring.coreos.com/prometheusrules":     true,
	"monitoring.coreos.com/servicemonitors":     true,
	"networking.k8s.io/ingresses":               true,
	"policy/poddisruptionbudgets":               true,
	"postgresql.cnpg.io/clusters":               true,
}

// grants flattens the rules of the first ClusterRole in file into "group/resource" -> verbs.
func grants(t *testing.T, file string) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", file))
	if err != nil {
		t.Fatal(err)
	}
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		var role rbacv1.ClusterRole
		if err := yaml.Unmarshal([]byte(doc), &role); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if role.Kind != "ClusterRole" {
			continue
		}
		out := map[string][]string{}
		for _, r := range role.Rules {
			if len(r.ResourceNames) > 0 || len(r.NonResourceURLs) > 0 {
				t.Fatalf("%s: unexpected resourceNames or nonResourceURLs in %+v", file, r)
			}
			for _, g := range r.APIGroups {
				for _, res := range r.Resources {
					key := g + "/" + res
					for _, v := range r.Verbs {
						if v == "*" || g == "*" || res == "*" {
							t.Fatalf("%s: wildcard in %+v", file, r)
						}
						if !slices.Contains(out[key], v) {
							out[key] = append(out[key], v)
						}
					}
					slices.Sort(out[key])
				}
			}
		}
		return out
	}
	t.Fatalf("%s: no ClusterRole", file)
	return nil
}

func TestManagerRoleResourcesClassified(t *testing.T) {
	for key := range grants(t, "config/rbac/role.yaml") {
		base := key
		for _, sub := range []string{"/status", "/finalizers"} {
			base = strings.TrimSuffix(base, sub)
		}
		if !clusterScopedResources[base] && !namespacedResources[base] {
			t.Errorf("manager-role grants %s, which is in neither clusterScopedResources nor namespacedResources", key)
		}
	}
}

func TestClusterReaderMatchesManagerRole(t *testing.T) {
	manager := grants(t, "config/rbac/role.yaml")
	reader := grants(t, "config/namespaced/cluster_reader_role.yaml")
	want := map[string][]string{}
	for key, verbs := range manager {
		if clusterScopedResources[key] {
			want[key] = verbs
		}
	}
	for key, verbs := range want {
		if !slices.Equal(reader[key], verbs) {
			t.Errorf("cluster_reader_role.yaml grants %s %v, manager-role %v", key, reader[key], verbs)
		}
	}
	for key, verbs := range reader {
		if _, ok := want[key]; !ok {
			t.Errorf("cluster_reader_role.yaml grants %s %v, which manager-role does not", key, verbs)
		}
		for _, v := range verbs {
			if v != "get" && v != "list" && v != "watch" {
				t.Errorf("cluster_reader_role.yaml grants %s %q; it must stay read-only", key, v)
			}
		}
	}
}

func TestPodsExecOnlyInVolumeUsageRole(t *testing.T) {
	if verbs, ok := grants(t, "config/rbac/role.yaml")["/pods/exec"]; ok {
		t.Errorf("manager-role grants pods/exec %v; it belongs in config/rbac/volume_usage_role.yaml only", verbs)
	}
	got := grants(t, "config/rbac/volume_usage_role.yaml")
	want := map[string][]string{"/pods/exec": {"create", "get"}}
	if len(got) != len(want) || !slices.Equal(got["/pods/exec"], want["/pods/exec"]) {
		t.Errorf("volume_usage_role.yaml grants %v, want %v", got, want)
	}
}
