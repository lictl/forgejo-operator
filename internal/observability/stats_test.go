package observability

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const sampleMetrics = `# HELP gitea_issues Number of Issues
# TYPE gitea_issues gauge
gitea_issues 42
# HELP gitea_issues_by_repository Number of Issues
# TYPE gitea_issues_by_repository gauge
gitea_issues_by_repository{repository="org/a"} 40
gitea_issues_by_repository{repository="org/b"} 2
# HELP gitea_organizations Number of Organizations
# TYPE gitea_organizations gauge
gitea_organizations 3
gitea_repositories{} 1.2e+06 1700000000000
gitea_users 7
`

// failAfter returns data and then err, to prove that scanning stopped early.
type failAfter struct {
	r   io.Reader
	err error
}

func (f *failAfter) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if errors.Is(err, io.EOF) {
		return n, f.err
	}
	return n, err
}

func TestScanStats(t *testing.T) {
	want := map[string]int64{statIssues: 42, statOrganizations: 3, statRepositories: 1200000, statUsers: 7}
	check := func(t *testing.T, got map[string]int64, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}

	t.Run("text format", func(t *testing.T) {
		got, err := scanStats(strings.NewReader(sampleMetrics), maxStatsBody)
		check(t, got, err)
	})
	t.Run("stops once all are found", func(t *testing.T) {
		got, err := scanStats(&failAfter{r: strings.NewReader(sampleMetrics), err: errors.New("read past the statistics")}, maxStatsBody)
		check(t, got, err)
	})
	t.Run("overlong lines are skipped", func(t *testing.T) {
		long := `gitea_issues_by_label{label="` + strings.Repeat("x", 200<<10) + `"} 1` + "\n"
		got, err := scanStats(strings.NewReader(long+sampleMetrics), maxStatsBody)
		check(t, got, err)
	})
	t.Run("body over the limit is an error, not partial values", func(t *testing.T) {
		big := strings.Repeat(`gitea_issues_by_repository{repository="org/repo"} 1`+"\n", 1000)
		_, err := scanStats(strings.NewReader("gitea_issues 42\n"+big+sampleMetrics), 4<<10)
		if err == nil || !strings.Contains(err.Error(), "larger than") {
			t.Fatalf("got %v, want a size error", err)
		}
	})
	t.Run("partial but complete body", func(t *testing.T) {
		got, err := scanStats(strings.NewReader("gitea_users 5\n"), maxStatsBody)
		if err != nil || got["users"] != 5 || len(got) != 1 {
			t.Fatalf("got %v, %v", got, err)
		}
	})
	t.Run("no statistics", func(t *testing.T) {
		if _, err := scanStats(strings.NewReader("go_goroutines 12\n"), maxStatsBody); err == nil {
			t.Fatal("no error")
		}
	})
}

// statsRC returns a context for an instance whose metrics token Secret exists.
func statsRC(t *testing.T, name string) (*forgejo.ReconcileContext, *events.FakeRecorder, *statsSampler) {
	t.Helper()
	f := testInstance(name)
	rc, rec := monitoringRC(t, f, interceptor.Funcs{})
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: f.Namespace, Name: forgejo.SecretsName(f)},
		Data:       map[string][]byte{forgejo.SecretKeyMetricsToken: []byte("token")},
	}
	if err := rc.Client.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ForgetInstance(f.Namespace, f.Name) })
	return rc, rec, samplerFor(f)
}

func waitIdle(t *testing.T, s *statsSampler) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sampling did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStatsSamplingBacksOffOnFailure(t *testing.T) {
	var hits atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() || r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, sampleMetrics)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	rc, rec, s := statsRC(t, "backoff")
	t0 := time.Now()
	// Probes run every 30s; sampling is attempted after each of them while it is due.
	attempt := func(at time.Duration) {
		t.Helper()
		startStatsSample(ctx, rc, srv.URL, t0.Add(at))
		waitIdle(t, s)
		collectStats(rc)
	}
	for at := time.Duration(0); at <= 10*time.Minute; at += 30 * time.Second {
		attempt(at)
	}
	// Default interval 5m: attempts at 0, 2m30s (first retry after half the interval),
	// 7m30s; never on every probe.
	if n := hits.Load(); n != 3 {
		t.Fatalf("%d attempts in 10 minutes, want 3", n)
	}
	ev := drain(rec)
	if len(ev) != 1 || !strings.Contains(ev[0], "StatsSamplingFailed") || !strings.Contains(ev[0], "HTTP 401") {
		t.Fatalf("events %v, want one StatsSamplingFailed", ev)
	}
	if rc.Instance.Status.Stats != nil {
		t.Fatalf("stats %+v from failed samples", rc.Instance.Status.Stats)
	}

	// Recovery: the next due attempt succeeds and is copied into status.
	failing.Store(false)
	attempt(12*time.Minute + 30*time.Second)
	st := rc.Instance.Status.Stats
	if st == nil || st.Users != 7 || st.Repositories != 1200000 || st.LastUpdated == nil {
		t.Fatalf("stats %+v", st)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.Contains(ev[0], "StatsSamplingRecovered") {
		t.Fatalf("events %v, want StatsSamplingRecovered", ev)
	}

	// A lost status patch: the sample is copied again from memory.
	rc.Instance.Status.Stats = nil
	collectStats(rc)
	if rc.Instance.Status.Stats == nil || rc.Instance.Status.Stats.Users != 7 {
		t.Fatal("sample not restored into status")
	}
	// A newer sample in status (e.g. another operator replica) is kept.
	newer := metav1.NewTime(st.LastUpdated.Add(time.Hour))
	rc.Instance.Status.Stats = &forgejov1.ForgejoStats{Users: 9, LastUpdated: &newer}
	collectStats(rc)
	if rc.Instance.Status.Stats.Users != 9 {
		t.Fatal("newer status.stats overwritten")
	}
}

func TestStatsSamplingWaitsForSlowMetrics(t *testing.T) {
	orig := statsClient
	statsClient = newProbeClient(300*time.Millisecond, 3*time.Second)
	t.Cleanup(func() { statsClient = orig })
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // Forgejo counting rows before it sends the headers
		_, _ = io.WriteString(w, sampleMetrics)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})

	ctx := context.Background()
	rc, _, s := statsRC(t, "slow")
	start := time.Now()
	startStatsSample(ctx, rc, srv.URL, start)
	// The reconcile is not held while /metrics is slow.
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("startStatsSample blocked for %s", d)
	}
	// Still running: no second request is started.
	startStatsSample(ctx, rc, srv.URL, start.Add(time.Hour))
	time.Sleep(time.Second) // longer than a health probe may wait for headers
	close(release)
	waitIdle(t, s)
	collectStats(rc)
	if st := rc.Instance.Status.Stats; st == nil || st.Users != 7 {
		t.Fatalf("stats %+v", st)
	}
}

func TestServingPodsWaitsForEndpoints(t *testing.T) {
	ctx := context.Background()
	f := testInstance("settle")
	rc, _ := monitoringRC(t, f, interceptor.Funcs{})
	now := time.Now()
	pod := func(name string, readySince time.Time) {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: f.Namespace, Name: name, Labels: forgejo.SelectorLabels(f)}}
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(readySince),
		}}
		if err := rc.Client.Create(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	pod("fresh", now.Add(-2*time.Second))
	ready, settling, err := servingPods(ctx, rc, now)
	if err != nil || ready != 0 || settling <= 0 || settling > endpointSettle {
		t.Fatalf("ready=%d settling=%s err=%v, want 0 and a short wait", ready, settling, err)
	}
	// Probed once the pod has settled.
	if ready, settling, _ = servingPods(ctx, rc, now.Add(endpointSettle)); ready != 1 || settling != 0 {
		t.Fatalf("ready=%d settling=%s after settling", ready, settling)
	}
	// An established pod is probed right away, also while another one settles.
	pod("old", now.Add(-time.Hour))
	if ready, _, _ = servingPods(ctx, rc, now); ready != 1 {
		t.Fatalf("ready=%d, want 1", ready)
	}
}
