package core

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const testInternalToken = "the-internal-token"

// testDB is the database app.ini points at.
func testDB() *forgejo.DBConnection {
	return &forgejo.DBConnection{Host: "db", Port: 5432, Name: testForgejoName, User: testForgejoName, Password: "pw", SSLMode: "disable"}
}

// renderConfig runs the config step for f, deploying status.image or else spec.image, and
// returns app.ini, Secret "<name>-config" and the events. With highAvailability, Valkey is at
// testValkeyURL, as the HA step of the autopilot sets it.
func renderConfig(t *testing.T, f *forgejov1.Forgejo) (string, *corev1.Secret, []string) {
	t.Helper()
	ctx := context.Background()
	secrets := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: forgejo.SecretsName(f), Namespace: f.Namespace},
		Data: map[string][]byte{forgejo.SecretKeyInternalToken: []byte(testInternalToken)}}
	rc := testContext(t, f, secrets)
	rec := events.NewFakeRecorder(10)
	rc.Recorder = rec
	rc.TargetImage = forgejo.Image(f)
	if f.Status.Image != "" {
		rc.TargetImage = f.Status.Image
	}
	rc.Layout = forgejo.LayoutForImage(rc.TargetImage)
	rc.DB = testDB()
	if f.Spec.HighAvailability.Enabled {
		rc.ValkeyURL = testValkeyURL
	}
	if _, err := ReconcileConfig(ctx, rc); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.ConfigSecretName(f)}, sec); err != nil {
		t.Fatal(err)
	}
	return string(sec.Data[forgejo.ConfigSecretKey]), sec, drainEvents(rec)
}

// section returns the lines of one app.ini section.
func section(ini, name string) string {
	_, rest, _ := strings.Cut(ini, "\n["+name+"]\n")
	body, _, _ := strings.Cut(rest, "\n[")
	return body
}

func TestConfigFollowsForgejoMajor(t *testing.T) {
	const (
		socket = "INTERNAL_LISTENER_PATH = /tmp/forgejo-run/forgejo.sock"
		digest = "codeberg.org/forgejo/forgejo@sha256:0123"
		both   = "internal-token+internal-socket"
	)
	for _, tc := range []struct {
		name          string
		image         string
		ha            bool // highAvailability with three replicas
		verified18    bool // Forgejo 18 is one of the verified releases
		token, socket bool
		profile       string
	}{
		{name: "15", image: img15, token: true, profile: forgejo.LegacyConfigProfile},
		{name: "16", image: img16, token: true, profile: forgejo.LegacyConfigProfile},
		{name: "17-rootless", image: img17 + "-rootless", token: true, profile: forgejo.LegacyConfigProfile},
		// Majors newer than the verified releases get the token of 17 and the per-pod socket of
		// the prepared 18 row. An HA instance moved to 18 before the operator verified it needs
		// that socket: the default path is on the shared data volume, where the pods would
		// replace each other's socket.
		{name: "18 before it is verified", image: "codeberg.org/forgejo/forgejo:18", ha: true, token: true, socket: true, profile: both},
		// An image of unknown version gets both: 17 and older ignore the socket, 18 only logs
		// the token.
		{name: "digest", image: digest, token: true, socket: true, profile: both},
		// Verifying Forgejo 18 activates its prepared traits: no INTERNAL_TOKEN, a per-pod socket.
		{name: "18 verified", image: "codeberg.org/forgejo/forgejo:18-rootless", verified18: true, socket: true, profile: "internal-socket"},
		{name: "17 with 18 verified", image: img17, verified18: true, token: true, profile: forgejo.LegacyConfigProfile},
		{name: "digest with 18 verified", image: digest, verified18: true, token: true, socket: true, profile: both},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.verified18 {
				old := forgejo.Versions
				forgejo.Versions = old.WithVerified(18)
				t.Cleanup(func() { forgejo.Versions = old })
			}
			f := testInstance()
			f.Spec.Image = tc.image
			if tc.ha {
				f = withHA(f)
				f.Spec.Replicas = ptr.To[int32](3)
			}
			ini, sec, _ := renderConfig(t, f)
			if strings.Contains(ini, "INTERNAL_TOKEN") != tc.token || strings.Contains(ini, "INTERNAL_LISTENER_PATH") != tc.socket ||
				tc.token && !strings.Contains(section(ini, "security"), "INTERNAL_TOKEN = "+testInternalToken) ||
				tc.socket && !strings.Contains(section(ini, "server"), socket) {
				t.Errorf("want INTERNAL_TOKEN %t and the socket %t:\n%s", tc.token, tc.socket, ini)
			}
			if got := sec.Annotations[forgejo.AnnotationConfigProfile]; got != tc.profile {
				t.Errorf("profile %q, want %q", got, tc.profile)
			}
		})
	}
}

// Forgejo 17 refuses durations without a unit, 16 parses them leniently. A digest-pinned
// instance is judged by the version Forgejo reports once it runs that image; before a version
// is known the warning is conditional.
func TestConfigWarnsAboutDurationsForgejo17Refuses(t *testing.T) {
	const (
		digest  = "codeberg.org/forgejo/forgejo@sha256:0123"
		refuses = " refuses to start with [server] PER_WRITE_TIMEOUT"
	)
	for _, tc := range []struct {
		name, image string
		version     string // status.version
		want        string // the event's prefix, "" for no event
	}{
		{"16", img16, "", ""},
		{"17", img17, "", "Warning ConfigIncompatible " + img17 + refuses},
		{"digest reported as 16", digest, "16.0.6+gitea-1.22.0", ""},
		{"digest reported as 17", digest, "17.0.1+gitea-1.22.0", "Warning ConfigIncompatible " + digest + refuses},
		{"digest of unknown version", digest, "", "Warning ConfigIncompatible If " + digest + " is Forgejo 17 or newer, it" + refuses},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testInstance()
			f.UID = types.UID(t.Name()) // the warning is rate-limited per instance
			f.Spec.Image, f.Status.Image, f.Status.Version = tc.image, tc.image, tc.version
			// CONN_MAX_IDLETIME has a unit.
			f.Spec.Config = map[string]map[string]string{testSectionServer: {"PER_WRITE_TIMEOUT": "-1"}, "database": {"CONN_MAX_IDLETIME": "5m"}}
			_, _, evs := renderConfig(t, f)
			switch {
			case tc.want == "" && len(evs) != 0:
				t.Errorf("want no event, got %q", evs)
			case tc.want != "" && (len(evs) != 1 || !strings.HasPrefix(evs[0], tc.want) || strings.Contains(evs[0], "CONN_MAX_IDLETIME")):
				t.Errorf("want %q, got %q", tc.want, evs)
			}
		})
	}
}

// Rendered names follow the detected cluster domain.
func TestClusterDomainNames(t *testing.T) {
	old := forgejo.ClusterDomain
	defer func() { forgejo.ClusterDomain = old }()
	forgejo.ClusterDomain = "k8s.example"
	f := testInstance()
	if got := RootURL(f); got != "http://fj-http.ns.svc.k8s.example:3000/" {
		t.Fatalf("ROOT_URL = %s", got)
	}
	if got := sshDomain(f); got != "fj-ssh.ns.svc.k8s.example" {
		t.Fatalf("SSH_DOMAIN = %s", got)
	}
	if got := probeHosts("pg", "ns"); strings.Join(got, ",") != "pg.ns.svc,pg.ns.svc.k8s.example,pg" {
		t.Fatalf("probeHosts = %v", got)
	}
}
