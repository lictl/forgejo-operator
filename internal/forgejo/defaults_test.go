package forgejo

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

func TestSSHHostKeysOverridden(t *testing.T) {
	from := func(section, key string) []forgejov1.ConfigValueFromSecret {
		return []forgejov1.ConfigValueFromSecret{{Section: section, Key: key,
			SecretKeyRef: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "s"}, Key: "k"}}}
	}
	for _, tc := range []struct {
		name       string
		spec       forgejov1.ForgejoSpec
		overridden bool
	}{
		{"defaults", forgejov1.ForgejoSpec{}, false},
		{"spec.config", forgejov1.ForgejoSpec{Config: map[string]map[string]string{"Server": {"SSH_SERVER_HOST_KEYS": "/x"}}}, true},
		{"configFrom", forgejov1.ForgejoSpec{ConfigFrom: from("server", "SSH_SERVER_HOST_KEYS")}, true},
		{"configFrom, section case", forgejov1.ForgejoSpec{ConfigFrom: from("SERVER", "SSH_SERVER_HOST_KEYS")}, true},
		// Keys match exactly, as in spec.config.
		{"configFrom, key case", forgejov1.ForgejoSpec{ConfigFrom: from("server", "ssh_server_host_keys")}, false},
		{"configFrom, other section", forgejov1.ForgejoSpec{ConfigFrom: from("ssh", "SSH_SERVER_HOST_KEYS")}, false},
		{"configFrom, other key", forgejov1.ForgejoSpec{ConfigFrom: from("server", "SSH_DOMAIN")}, false},
	} {
		f := &forgejov1.Forgejo{Spec: tc.spec}
		if got := SSHHostKeysOverridden(f); got != tc.overridden {
			t.Errorf("%s: overridden %v, want %v", tc.name, got, tc.overridden)
		}
		if got := SSHHostKeysManaged(f); got != !tc.overridden {
			t.Errorf("%s: managed %v", tc.name, got)
		}
		f.Spec.Server.SSH.Enabled = ptr.To(false)
		if SSHHostKeysManaged(f) {
			t.Errorf("%s: managed with SSH disabled", tc.name)
		}
	}
}
