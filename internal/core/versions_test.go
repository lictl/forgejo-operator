package core

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"

	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// renderConfig runs the config step for image and returns app.ini, the Secret's profile
// annotation and the events.
func renderConfig(t *testing.T, image string, config map[string]map[string]string) (string, string, []string) {
	t.Helper()
	ctx := context.Background()
	f := testInstance()
	f.Spec.Image = image
	f.Spec.Config = config
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data: map[string][]byte{forgejo.SecretKeyInternalToken: []byte("the-internal-token")}}
	rc := testContext(t, f, secrets)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	rc.TargetImage = image
	rc.Layout = forgejo.LayoutForImage(image)
	rc.DB = &forgejo.DBConnection{Host: "db", Port: 5432, Name: "forgejo", User: "forgejo", Password: "pw", SSLMode: "disable"}
	if _, err := ReconcileConfig(ctx, rc); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, sec); err != nil {
		t.Fatal(err)
	}
	return string(sec.Data[forgejo.ConfigSecretKey]), sec.Annotations[forgejo.AnnotationConfigProfile], drainEvents(rec)
}

// section returns the lines of one app.ini section.
func section(ini, name string) string {
	_, rest, _ := strings.Cut(ini, "\n["+name+"]\n")
	body, _, _ := strings.Cut(rest, "\n[")
	return body
}

func TestConfigFollowsForgejoMajor(t *testing.T) {
	const socket = "INTERNAL_LISTENER_PATH = /tmp/forgejo-run/forgejo.sock"
	for _, image := range []string{img15, img16, "codeberg.org/forgejo/forgejo:17-rootless", "codeberg.org/forgejo/forgejo:18"} {
		ini, profile, _ := renderConfig(t, image, nil)
		if !strings.Contains(section(ini, "security"), "INTERNAL_TOKEN = the-internal-token") || strings.Contains(ini, "INTERNAL_LISTENER_PATH") {
			t.Errorf("%s: want INTERNAL_TOKEN and no socket:\n%s", image, ini)
		}
		if profile != forgejo.LegacyConfigProfile {
			t.Errorf("%s: profile %q", image, profile)
		}
	}

	// Verifying Forgejo 18 activates its prepared traits: no INTERNAL_TOKEN, a per-pod socket.
	old := forgejo.Versions
	forgejo.Versions = old.WithVerified(18)
	t.Cleanup(func() { forgejo.Versions = old })
	ini, profile, _ := renderConfig(t, "codeberg.org/forgejo/forgejo:18-rootless", nil)
	if strings.Contains(ini, "INTERNAL_TOKEN") || !strings.Contains(section(ini, "server"), socket) || profile != "internal-socket" {
		t.Errorf("18: want the socket and no INTERNAL_TOKEN (profile %q):\n%s", profile, ini)
	}
	// 17 is unchanged by it.
	if ini, _, _ := renderConfig(t, "codeberg.org/forgejo/forgejo:17", nil); !strings.Contains(ini, "INTERNAL_TOKEN") || strings.Contains(ini, "INTERNAL_LISTENER_PATH") {
		t.Errorf("17 with 18 verified:\n%s", ini)
	}
	// An image of unknown version gets both: 17 and older ignore the socket, 18 only logs the token.
	ini, profile, _ = renderConfig(t, "codeberg.org/forgejo/forgejo@sha256:0123", nil)
	if !strings.Contains(ini, "INTERNAL_TOKEN") || !strings.Contains(section(ini, "server"), socket) || profile != "internal-token+internal-socket" {
		t.Errorf("digest: profile %q:\n%s", profile, ini)
	}
}

func TestConfigWarnsAboutDurationsForgejo17Refuses(t *testing.T) {
	bad := map[string]map[string]string{"server": {"PER_WRITE_TIMEOUT": "-1"}, "database": {"CONN_MAX_IDLETIME": "5m"}}
	if _, _, evs := renderConfig(t, img16, bad); len(evs) != 0 {
		t.Fatalf("16 parses durations leniently, got %q", evs)
	}
	_, _, evs := renderConfig(t, "codeberg.org/forgejo/forgejo:17", bad)
	if len(evs) != 1 || !strings.HasPrefix(evs[0], "Warning ConfigIncompatible") || !strings.Contains(evs[0], "[server] PER_WRITE_TIMEOUT") ||
		strings.Contains(evs[0], "CONN_MAX_IDLETIME") {
		t.Fatalf("17: %q", evs)
	}
}

// On Forgejo 18 the admin password reaches change-password through stdin, not argv.
func TestBootstrapScriptPasswordFromStdin(t *testing.T) {
	for _, tool := range []string{"sh", "awk", "cut"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	cli := filepath.Join(dir, "forgejo")
	fakeCLI := `#!/bin/sh
case "$1 $2 $3" in
  "migrate --config "*) ;;
  "admin user list") echo "ID Username Email IsActive IsAdmin 2FA"; echo "1 admin a@b true true false" ;;
  "admin user change-password") echo "$*" > "` + dir + `/args"; read -r p1; read -r p2; printf '%s|%s' "$p1" "$p2" > "` + dir + `/stdin" ;;
esac
`
	if err := os.WriteFile(cli, []byte(fakeCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	l := forgejo.ImageLayout{CLI: cli, ConfigPath: filepath.Join(dir, "app.ini"), WorkPath: filepath.Join(data, "gitea"),
		CustomPath: filepath.Join(data, "gitea", "custom"), HomePath: filepath.Join(data, "git"),
		RepoRoot: filepath.Join(data, "git", "repositories"), LFSPath: filepath.Join(data, "git", "lfs"),
		Traits: forgejo.Traits{PasswordFromStdin: true}}
	const password = `p a%s'"$x\`
	cmd := exec.Command("sh", "-c", bootstrapScript(l))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ADMIN_USERNAME=admin", "ADMIN_EMAIL=a@b", "ADMIN_PASSWORD=" + password}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if strings.Contains(string(args), "--password") || !strings.Contains(string(args), "--username admin") {
		t.Fatalf("args %q", args)
	}
	if string(stdin) != password+"|"+password {
		t.Fatalf("stdin %q", stdin)
	}
	if strings.Contains(bootstrapScript(forgejo.LayoutForImage(img16)), "printf '%s\\n%s\\n' \"$ADMIN_PASSWORD\"") {
		t.Fatal("16 must pass the password with --password")
	}
}
