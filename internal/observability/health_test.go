package observability

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// useFastClient shortens the probe timeouts for the duration of a test.
func useFastClient(t *testing.T) {
	t.Helper()
	orig := httpClient
	httpClient = newProbeClient(300*time.Millisecond, 300*time.Millisecond)
	t.Cleanup(func() { httpClient = orig })
}

// rawServer accepts connections, reads the request headers and hands the connection to
// handle. It returns the base URL.
func rawServer(t *testing.T, handle func(net.Conn)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { close(done); _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				handle(c)
				<-done
				_ = c.Close()
			}()
		}
	}()
	return "http://" + ln.Addr().String()
}

func httpServer(t *testing.T, code int, contentType, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

const (
	passBody = `{"status":"pass","checks":{"cache:ping":[{"status":"pass"}],"database:ping":[{"status":"pass"}]}}`
	failBody = `{"status":"fail","checks":{"cache:ping":[{"status":"pass"}],"database:ping":[{"status":"fail","output":"dial tcp: connection refused"}]}}`
)

func TestProbeHealthzClassification(t *testing.T) {
	useFastClient(t)

	closed := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		return "http://" + addr
	}

	tests := []struct {
		name        string
		base        func() string
		healthy     bool
		unreachable bool
	}{
		{name: "pass", base: func() string { return httpServer(t, 200, "application/health+json", passBody) }, healthy: true},
		{name: "fail 424 counts", base: func() string { return httpServer(t, 424, "application/health+json", failBody) }},
		{name: "500 page counts", base: func() string { return httpServer(t, 500, "text/html", "<html>Internal Server Error</html>") }},
		{name: "unparsable 200 counts", base: func() string { return httpServer(t, 200, "text/plain", "ok") }},
		{name: "accepted but no response counts", base: func() string {
			return rawServer(t, func(net.Conn) {}) // never writes
		}},
		{name: "connection refused", base: closed, unreachable: true},
		{name: "dns failure", base: func() string { return "http://forgejo-http.does-not-exist.invalid:3000" }, unreachable: true},
		{name: "blackholed", base: func() string { return "http://192.0.2.1:3000" }, unreachable: true},
		{name: "reset after accept", base: func() string {
			return rawServer(t, func(c net.Conn) {
				_ = c.(*net.TCPConn).SetLinger(0)
				_ = c.Close()
			})
		}, unreachable: true},
		{name: "closed after accept", base: func() string {
			return rawServer(t, func(c net.Conn) { _ = c.Close() })
		}, unreachable: true},
		{name: "mesh 403", base: func() string { return httpServer(t, 403, "text/plain", "RBAC: access denied") }, unreachable: true},
		{name: "mesh 503", base: func() string {
			return httpServer(t, 503, "text/plain", "upstream connect error or disconnect/reset before headers. reset reason: connection termination")
		}, unreachable: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := time.Now()
			res := probeHealthz(context.Background(), tt.base())
			if res.healthy != tt.healthy || res.unreachable != tt.unreachable {
				t.Fatalf("healthy=%v unreachable=%v, want healthy=%v unreachable=%v (message %q)",
					res.healthy, res.unreachable, tt.healthy, tt.unreachable, res.message)
			}
			t.Log(res.message)
			if res.message == "" {
				t.Fatal("empty message")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("probe took %s", d)
			}
		})
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := c.Write(m); err != nil {
		t.Fatal(err)
	}
	return m.GetCounter().GetValue()
}

func newHealthRC(name string) (*forgejo.ReconcileContext, *events.FakeRecorder) {
	rec := events.NewFakeRecorder(20)
	f := &forgejov1.Forgejo{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name}}
	f.Status.Health = &forgejov1.HealthStatus{}
	return &forgejo.ReconcileContext{Instance: f, Recorder: rec}, rec
}

func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestUnreachableDoesNotFeedSelfHealing(t *testing.T) {
	rc, rec := newHealthRC("unreachable")
	h := rc.Instance.Status.Health
	counter := forgejo.HealthCheckFailures.WithLabelValues("ns", "unreachable")
	now := metav1.Now()

	unreachable := healthResult{unreachable: true, message: "GET /api/healthz: dial tcp: i/o timeout"}
	failed := healthResult{message: "status fail: database:ping=fail", checks: map[string]string{"database:ping": "fail"}}

	// Real failures build a streak.
	for i := 1; i <= 3; i++ {
		recordHealth(rc, failed, now)
		if h.ConsecutiveFailures != int32(i) {
			t.Fatalf("after %d failures: consecutiveFailures=%d", i, h.ConsecutiveFailures)
		}
	}
	if got := counterValue(t, counter); got != 3 {
		t.Fatalf("failure counter %v, want 3", got)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "HealthCheckFailed") {
		t.Fatalf("events %v, want one HealthCheckFailed", ev)
	}

	// Unreachable probes reset the streak, are not counted and warn once.
	for range 10 {
		recordUnreachable(rc, unreachable, now)
	}
	if h.ConsecutiveFailures != 0 || h.Healthy || h.LastCheckTime == nil {
		t.Fatalf("unexpected health after unreachable probes: %+v", h)
	}
	if got := counterValue(t, counter); got != 3 {
		t.Fatalf("failure counter %v after unreachable probes, want 3", got)
	}
	c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionHealthy)
	if c == nil || c.Status != metav1.ConditionUnknown || c.Reason != ReasonUnreachable ||
		!strings.Contains(c.Message, "NetworkPolicies") {
		t.Fatalf("Healthy condition %+v", c)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "HealthCheckUnreachable") {
		t.Fatalf("events %v, want one HealthCheckUnreachable", ev)
	}

	// A real failure after being unreachable starts a new streak and warns again.
	recordHealth(rc, failed, now)
	if h.ConsecutiveFailures != 1 {
		t.Fatalf("consecutiveFailures=%d, want 1", h.ConsecutiveFailures)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "HealthCheckFailed") {
		t.Fatalf("events %v, want one HealthCheckFailed", ev)
	}

	// Recovery from unreachable is reported.
	recordUnreachable(rc, unreachable, now)
	recordHealth(rc, healthResult{healthy: true, message: "all checks pass"}, now)
	if ev := drain(rec); len(ev) != 2 || !strings.Contains(ev[1], "HealthCheckRecovered") {
		t.Fatalf("events %v, want HealthCheckUnreachable then HealthCheckRecovered", ev)
	}
	c = meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionHealthy)
	if c.Status != metav1.ConditionTrue || h.ConsecutiveFailures != 0 || !h.Healthy {
		t.Fatalf("after recovery: condition %+v health %+v", c, h)
	}
}
