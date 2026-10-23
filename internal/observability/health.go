package observability

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

const (
	statUsers         = "users"
	statOrganizations = "organizations"
	statRepositories  = "repositories"
	statIssues        = "issues"
	statusPass        = "pass"
)

const (
	defaultHealthInterval = 30 * time.Second
	defaultStatsInterval  = 5 * time.Minute
	// dialTimeout bounds DNS resolution plus the TCP connect. A blackholed Service
	// (NetworkPolicy, CNI fault) must not hold the reconcile worker for long.
	dialTimeout = 2 * time.Second
	// probeTimeout is how long Forgejo may take to send response headers once the
	// connection is established.
	probeTimeout = 5 * time.Second
	maxBody      = 8 << 20
	// endpointSettle is how long a pod must have been Ready before it is probed through
	// the Service: the EndpointSlice and kube-proxy rules follow the Ready condition with
	// a delay.
	endpointSettle = 5 * time.Second

	// Forgejo's /metrics runs a COUNT(*) query per statistic before it sends the
	// headers, which takes seconds on a large instance. Sampling runs in the background
	// (see statsSampler), so it may wait much longer than a health probe.
	statsHeaderTimeout = 20 * time.Second
	// maxStatsBody bounds how much of /metrics is read. The body is scanned as a stream
	// and not kept, so this only guards against a runaway response; the high-cardinality
	// gitea_issues_by_* families sort before gitea_organizations, gitea_repositories and
	// gitea_users.
	maxStatsBody = 64 << 20
	// minStatsRetry is the earliest retry after a first failed sampling attempt; later
	// failures wait a full statsInterval.
	minStatsRetry = time.Minute
	// statsWarnEvery rate-limits StatsSamplingFailed events while sampling keeps failing.
	statsWarnEvery = time.Hour
)

// Healthy condition reasons.
const (
	ReasonHealthy         = "HealthCheckPassed"
	ReasonUnhealthy       = "HealthCheckFailed"
	ReasonUnreachable     = "Unreachable"
	ReasonNoReadyReplicas = "NoReadyReplicas"
	ReasonMaintenance     = "Maintenance"
)

// httpClient talks to Forgejo through its HTTP Service. It uses its own transport so a
// failed probe can be classified (see probeError): dial and header timeouts fire before
// the overall client timeout and keep their own errors, no proxy is consulted for
// cluster-internal URLs, and every probe opens a fresh connection.
var httpClient = newProbeClient(dialTimeout, probeTimeout)

// statsClient fetches /metrics for status.stats, with a longer header timeout.
var statsClient = newProbeClient(dialTimeout, statsHeaderTimeout)

func newProbeClient(dial, header time.Duration) *http.Client {
	return &http.Client{
		// Covers dial + headers + reading the body; the specific timeouts fire first.
		Timeout: dial + header + 5*time.Second,
		Transport: &http.Transport{
			Proxy:                 nil,
			DialContext:           (&net.Dialer{Timeout: dial}).DialContext,
			ResponseHeaderTimeout: header,
			DisableKeepAlives:     true,
		},
	}
}

// statsMetrics maps Forgejo metric names to the "kind" label of forgejo_operator_stats.
var statsMetrics = map[string]string{
	"gitea_users":         statUsers,
	"gitea_organizations": statOrganizations,
	"gitea_repositories":  statRepositories,
	"gitea_issues":        statIssues,
}

// ReconcileHealth probes /api/healthz and /api/v1/version and samples statistics into
// status.health, status.version, status.stats and the Healthy condition. Statistics are
// sampled in the background (see statsSampler) and copied into status by a later reconcile.
//
// Probes go through the HTTP Service and only count while at least one pod is Ready and
// no restore is running: status.health.consecutiveFailures drives self-healing restarts,
// so it must only reflect real failures of pods that are serving traffic. For the same
// reason a probe that never got an answer from Forgejo (DNS, dial or connection-reset
// errors: a NetworkPolicy or service-mesh mTLS shutting the operator out) is reported as
// Unreachable and resets the streak instead of extending it.
func ReconcileHealth(ctx context.Context, rc *forgejo.ReconcileContext) (forgejo.StepResult, error) {
	f := rc.Instance
	if f.Status.Health == nil {
		f.Status.Health = &forgejov1.HealthStatus{}
	}
	h := f.Status.Health
	// Export the counters from the start so increase() also sees the first increment.
	forgejo.HealthCheckFailures.WithLabelValues(f.Namespace, f.Name).Add(0)
	forgejo.HealthCheckUnreachable.WithLabelValues(f.Namespace, f.Name).Add(0)
	if forgejo.MonitoringEnabled(f) {
		collectStats(rc)
	}
	publishStats(f)

	if rc.Maintenance {
		skipHealth(rc, ReasonMaintenance, "Health checks are suspended during maintenance: "+rc.MaintenanceReason)
		return forgejo.StepResult{}, nil
	}
	now := time.Now()
	ready, settling, err := servingPods(ctx, rc, now)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if ready == 0 && settling > 0 {
		// A pod just became Ready, but the Service may not route to it yet (kube-proxy
		// rejects connections to a Service without endpoints): probing now would report a
		// spurious Unreachable. Keep the previous result until the pod has settled.
		rc.Requeue(settling)
		return forgejo.StepResult{}, nil
	}
	if ready == 0 {
		skipHealth(rc, ReasonNoReadyReplicas, "No Forgejo pod is Ready; health checks resume once one is")
		return forgejo.StepResult{}, nil
	}

	interval := forgejo.DurationOr(f.Spec.AutoPilot.SelfHealing.HealthCheckInterval, defaultHealthInterval)
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	var last time.Time
	if h.LastCheckTime != nil {
		last = h.LastCheckTime.Time
	}
	if t, ok := lastProbe.Load(key); ok && t.(time.Time).After(last) {
		last = t.(time.Time)
	}
	// One second of tolerance for the second-precision lastCheckTime and timer jitter.
	if wait := interval - now.Sub(last) - time.Second; wait > 0 {
		rc.Requeue(wait)
		return forgejo.StepResult{}, nil
	}
	lastProbe.Store(key, now)
	rc.Requeue(interval)

	base := forgejo.InternalHTTPURL(f)
	res := probeHealthz(ctx, base)
	if res.unreachable {
		recordUnreachable(rc, res, metav1.NewTime(now))
		return forgejo.StepResult{}, nil
	}
	recordHealth(rc, res, metav1.NewTime(now))
	if !res.healthy {
		return forgejo.StepResult{}, nil
	}

	if v, err := fetchVersion(ctx, base); err != nil {
		logf.FromContext(ctx).Info("reading Forgejo version failed", "error", err.Error())
	} else if v != f.Status.Version {
		if newVersionFor(f, v) {
			rc.Event(corev1.EventTypeNormal, "VersionDetected", "HealthCheck", "Forgejo reports version %s", v)
		}
		f.Status.Version = v
	}

	if forgejo.MonitoringEnabled(f) {
		startStatsSample(ctx, rc, base, now)
	}
	return forgejo.StepResult{}, nil
}

// detectedVersions remembers, per instance, the versions Forgejo reported for its current
// status.image in this process.
var detectedVersions sync.Map // types.NamespacedName -> detected

type detected struct {
	uid, image string
	versions   []string
}

// newVersionFor reports whether version has not been reported before for the instance's
// status.image in this process, and records it. status.version follows every probe, but the
// probe goes through the Service: with a floating tag, nodes that pulled different patch
// releases make it alternate, and VersionDetected is not repeated for every switch.
func newVersionFor(f *forgejov1.Forgejo, version string) bool {
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	d := detected{uid: string(f.UID), image: f.Status.Image}
	if v, ok := detectedVersions.Load(key); ok {
		if prev := v.(detected); prev.uid == d.uid && prev.image == d.image {
			if slices.Contains(prev.versions, version) {
				return false
			}
			d.versions = slices.Clone(prev.versions)
		}
	}
	d.versions = append(d.versions, version)
	detectedVersions.Store(key, d)
	return true
}

// lastProbe remembers when each instance was last probed by this process. The informer
// cache may hand the next reconcile a copy of the instance from before the latest status
// patch, so status.health.lastCheckTime alone would let back-to-back reconciles probe twice.
var lastProbe sync.Map // types.NamespacedName -> time.Time

// ForgetInstance drops the in-memory bookkeeping of a deleted instance (probe and
// sampling times, rate-limited warnings). The controller calls it next to
// forgejo.DeleteInstanceMetrics; state is also keyed by UID where a stale entry would
// matter, so a re-created instance of the same name starts afresh either way.
func ForgetInstance(namespace, name string) {
	key := types.NamespacedName{Namespace: namespace, Name: name}
	lastProbe.Delete(key)
	statsSamplers.Delete(key)
	monitoringWarned.Delete(key)
	detectedVersions.Delete(key)
}

// servingPods counts the instance's Ready pods that are not terminating, i.e. the pods
// the HTTP Service routes to. Deployment status lags behind during rollouts. A pod counts
// once it has been Ready for endpointSettle; settling is how long until the first pod
// that is Ready but not yet counted will be.
func servingPods(ctx context.Context, rc *forgejo.ReconcileContext, now time.Time) (
	ready int, settling time.Duration, err error) {
	pods := &corev1.PodList{}
	if err := rc.Client.List(ctx, pods, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.SelectorLabels(rc.Instance))); err != nil {
		return 0, 0, err
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type != corev1.PodReady || c.Status != corev1.ConditionTrue {
				continue
			}
			// lastTransitionTime has second precision: allow one second more.
			if wait := c.LastTransitionTime.Add(endpointSettle + time.Second).Sub(now); wait > 0 {
				if settling == 0 || wait < settling {
					settling = wait
				}
				continue
			}
			ready++
		}
	}
	return ready, settling, nil
}

// skipHealth reports that no probe could be made. The failure streak is reset so that
// self-healing counts afresh once pods are Ready again (e.g. after it restarted them);
// the last observed checks are kept for diagnosis.
func skipHealth(rc *forgejo.ReconcileContext, reason, msg string) {
	h := rc.Instance.Status.Health
	if lastProbeFailed(h) {
		var bad []string
		for _, name := range slices.Sorted(maps.Keys(h.Checks)) {
			if st := h.Checks[name]; st != statusPass {
				bad = append(bad, name+"="+st)
			}
		}
		if len(bad) == 0 {
			bad = []string{"unreachable"}
		}
		msg += fmt.Sprintf(" (last probe at %s: %s)", h.LastCheckTime.UTC().Format(time.RFC3339), strings.Join(bad, ", "))
	}
	h.Healthy = false
	h.ConsecutiveFailures = 0
	h.Message = msg
	rc.SetCondition(forgejov1.ConditionHealthy, metav1.ConditionUnknown, reason, msg)
}

func lastProbeFailed(h *forgejov1.HealthStatus) bool {
	return h.LastCheckTime != nil && (h.LastHealthyTime == nil || h.LastHealthyTime.Before(h.LastCheckTime))
}

// Outcomes of the last probe, as far as status still tells them apart.
const (
	outcomeNone        = ""
	outcomePassed      = "passed"
	outcomeFailed      = "failed"
	outcomeUnreachable = "unreachable"
)

// lastOutcome is the outcome of the previous probe. The Healthy condition names it until
// a suspended period overwrites the reason; after that only "passed" or "failed" can be
// told apart from the timestamps.
func lastOutcome(rc *forgejo.ReconcileContext) string {
	h := rc.Instance.Status.Health
	if h.LastCheckTime == nil {
		return outcomeNone
	}
	if c := meta.FindStatusCondition(rc.Instance.Status.Conditions, forgejov1.ConditionHealthy); c != nil {
		switch c.Reason {
		case ReasonHealthy:
			return outcomePassed
		case ReasonUnhealthy:
			return outcomeFailed
		case ReasonUnreachable:
			return outcomeUnreachable
		}
	}
	if lastProbeFailed(h) {
		return outcomeFailed
	}
	return outcomePassed
}

// recordUnreachable stores a probe that got no answer from Forgejo. It is not a health
// failure: the streak is reset like for a suspended probe, so self-healing never restarts
// pods the operator merely cannot talk to.
func recordUnreachable(rc *forgejo.ReconcileContext, res healthResult, now metav1.Time) {
	f := rc.Instance
	h := f.Status.Health
	prev := lastOutcome(rc)

	h.LastCheckTime = &now
	h.Healthy = false
	h.ConsecutiveFailures = 0
	h.Message = res.message
	h.Checks = nil
	forgejo.HealthCheckUnreachable.WithLabelValues(f.Namespace, f.Name).Inc()
	msg := fmt.Sprintf("The operator cannot reach Forgejo (%s). It must be allowed to connect to "+
		"Service %s.%s port %d: check NetworkPolicies and service-mesh mTLS (PERMISSIVE, or a sidecar for "+
		"the operator). Unreachable probes do not count towards self-healing.",
		res.message, forgejo.HTTPServiceName(f), f.Namespace, forgejo.HTTPServicePort(f))
	rc.SetCondition(forgejov1.ConditionHealthy, metav1.ConditionUnknown, ReasonUnreachable, msg)
	if prev != outcomeUnreachable {
		rc.Event(corev1.EventTypeWarning, "HealthCheckUnreachable", "HealthCheck", "%s", msg)
	}
}

// recordHealth stores the result of a probe Forgejo answered (or accepted and then hung
// on). Events are emitted when the probe outcome flips, independent of skipped periods in
// between.
func recordHealth(rc *forgejo.ReconcileContext, res healthResult, now metav1.Time) {
	f := rc.Instance
	h := f.Status.Health
	prev := lastOutcome(rc)
	wasFailing := prev == outcomeFailed || prev == outcomeUnreachable

	h.LastCheckTime = &now
	h.Healthy = res.healthy
	h.Message = res.message
	h.Checks = res.checks
	if res.healthy {
		h.ConsecutiveFailures = 0
		h.LastHealthyTime = &now
		rc.SetCondition(forgejov1.ConditionHealthy, metav1.ConditionTrue, ReasonHealthy, res.message)
		if wasFailing {
			rc.Event(corev1.EventTypeNormal, "HealthCheckRecovered", "HealthCheck", "Forgejo is healthy again: %s", res.message)
		}
		return
	}
	h.ConsecutiveFailures++
	forgejo.HealthCheckFailures.WithLabelValues(f.Namespace, f.Name).Inc()
	rc.SetCondition(forgejov1.ConditionHealthy, metav1.ConditionFalse, ReasonUnhealthy,
		fmt.Sprintf("%s (%d consecutive failures)", res.message, h.ConsecutiveFailures))
	if prev != outcomeFailed {
		rc.Event(corev1.EventTypeWarning, "HealthCheckFailed", "HealthCheck", "Forgejo health check failed: %s", res.message)
	}
}

type healthResult struct {
	healthy bool
	// unreachable: Forgejo never answered (see probeError); not a health failure.
	unreachable bool
	message     string
	checks      map[string]string
}

type healthzResponse struct {
	Status string `json:"status"`
	Checks map[string][]struct {
		Status string `json:"status"`
		Output string `json:"output"`
	} `json:"checks"`
}

var statusRank = map[string]int{statusPass: 0, "warn": 1, "fail": 2}

// intermediaryStatus are codes that a proxy (service-mesh sidecar, authorization policy)
// returns when it does not let a request through to Forgejo. Combined with a body that is
// not a health document they mean Forgejo was never asked.
var intermediaryStatus = map[int]bool{
	http.StatusUnauthorized:       true,
	http.StatusForbidden:          true,
	http.StatusBadGateway:         true,
	http.StatusServiceUnavailable: true,
	http.StatusGatewayTimeout:     true,
}

// snippet returns the start of a response body for messages.
func snippet(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 80 {
		s = s[:80] + "..."
	}
	return s
}

// probeHealthz calls Forgejo's /api/healthz (200 for pass/warn, 424 for fail). Any HTTP
// answer other than 200 pass/warn, and a connection Forgejo accepted but never answered
// (a hung process), count as failures; everything else is unreachable.
func probeHealthz(ctx context.Context, base string) healthResult {
	body, code, err := get(ctx, base+"/api/healthz")
	if err != nil {
		var pe *probeError
		if errors.As(err, &pe) && pe.hung {
			return healthResult{message: "GET /api/healthz: " + err.Error()}
		}
		if code == 0 {
			return healthResult{unreachable: true, message: "GET /api/healthz: " + err.Error()}
		}
		// The headers arrived but reading the body failed.
		return healthResult{message: fmt.Sprintf("GET /api/healthz: HTTP %d, reading body: %v", code, err)}
	}
	var hz healthzResponse
	if err := json.Unmarshal(body, &hz); err != nil || hz.Status == "" {
		if intermediaryStatus[code] {
			// Forgejo always answers /api/healthz with a health document; this answer came
			// from a proxy in between, e.g. a service mesh denying the operator.
			return healthResult{unreachable: true, message: fmt.Sprintf(
				"GET /api/healthz: HTTP %d without a health document (%q), answered by a proxy rather than Forgejo",
				code, snippet(body))}
		}
		if err == nil {
			err = errors.New(`no "status"`)
		}
		return healthResult{message: fmt.Sprintf("GET /api/healthz: HTTP %d, unparsable body: %v", code, err)}
	}

	res := healthResult{checks: map[string]string{}}
	var problems []string
	for _, name := range slices.Sorted(maps.Keys(hz.Checks)) {
		worst := statusPass
		var outputs []string
		for _, c := range hz.Checks[name] {
			if statusRank[c.Status] > statusRank[worst] {
				worst = c.Status
			}
			if c.Output != "" {
				outputs = append(outputs, c.Output)
			}
		}
		res.checks[name] = worst
		if worst != statusPass {
			p := name + "=" + worst
			if len(outputs) > 0 {
				p += " (" + strings.Join(outputs, "; ") + ")"
			}
			problems = append(problems, p)
		}
	}

	res.healthy = code == http.StatusOK && (hz.Status == statusPass || hz.Status == "warn")
	switch {
	case len(problems) > 0:
		res.message = fmt.Sprintf("status %s: %s", hz.Status, strings.Join(problems, ", "))
	case res.healthy:
		res.message = "all checks pass"
	default:
		res.message = fmt.Sprintf("HTTP %d, status %q", code, hz.Status)
	}
	return res
}

func fetchVersion(ctx context.Context, base string) (string, error) {
	body, code, err := get(ctx, base+"/api/v1/version")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("GET /api/v1/version: HTTP %d", code)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("GET /api/v1/version: %w", err)
	}
	if v.Version == "" {
		return "", errors.New("GET /api/v1/version: empty version")
	}
	return v.Version, nil
}

// statsSampler is the background sampling state of one instance. Sampling runs in a
// goroutine, so a slow /metrics never holds the reconcile worker, which the forgejo
// controller shares between all instances. The next reconcile copies a finished sample
// into status.stats.
type statsSampler struct {
	uid types.UID

	mu      sync.Mutex
	running bool
	// lastAttempt is when the last attempt started, successful or not.
	lastAttempt time.Time
	// failures counts consecutive failed attempts; lastErr is the error of the last one.
	failures int
	lastErr  string
	// result is the last successful sample.
	result *forgejov1.ForgejoStats
	// warnedAt is when StatsSamplingFailed was last emitted; zero when not warned.
	warnedAt time.Time
}

var statsSamplers sync.Map // types.NamespacedName -> *statsSampler

// samplerFor returns the sampler of the instance, a fresh one for a re-created instance.
// Reconciles of one instance never run concurrently, so Load and Store do not race.
func samplerFor(f *forgejov1.Forgejo) *statsSampler {
	key := types.NamespacedName{Namespace: f.Namespace, Name: f.Name}
	if v, ok := statsSamplers.Load(key); ok && v.(*statsSampler).uid == f.UID {
		return v.(*statsSampler)
	}
	s := &statsSampler{uid: f.UID}
	statsSamplers.Store(key, s)
	return s
}

func (s *statsSampler) failed(log logr.Logger, err error) {
	s.failures++
	s.lastErr = err.Error()
	log.Info("Sampling Forgejo statistics failed", "error", s.lastErr, "consecutiveFailures", s.failures)
}

// collectStats copies the last background sample into status.stats (again, if the status
// patch that carried it was lost) and reports sampling that keeps failing: a Warning event
// once the retry after a first failure fails too, repeated at most hourly, and a Normal
// event when it works again.
func collectStats(rc *forgejo.ReconcileContext) {
	f := rc.Instance
	s := samplerFor(f)
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.result; r != nil {
		if cur := f.Status.Stats; cur == nil || cur.LastUpdated == nil || cur.LastUpdated.Before(r.LastUpdated) {
			f.Status.Stats = r.DeepCopy()
		}
	}
	now := time.Now()
	switch {
	case s.lastErr != "" && s.failures >= 2 && (s.warnedAt.IsZero() || now.Sub(s.warnedAt) >= statsWarnEvery):
		s.warnedAt = now
		interval := forgejo.DurationOr(f.Spec.Monitoring.StatsInterval, defaultStatsInterval)
		rc.Event(corev1.EventTypeWarning, "StatsSamplingFailed", "Stats",
			"Sampling status.stats from Forgejo's /metrics failed %d times in a row (retried every %s): %s",
			s.failures, interval, s.lastErr)
	case s.lastErr == "" && !s.warnedAt.IsZero():
		s.warnedAt = time.Time{}
		rc.Event(corev1.EventTypeNormal, "StatsSamplingRecovered", "Stats",
			"Sampling status.stats from Forgejo's /metrics works again")
	}
}

// startStatsSample starts a background sample of Forgejo's /metrics when one is due:
// statsInterval after the later of status.stats.lastUpdated and this process's last
// attempt, so failed attempts back off as well. Only the first failure in a row is retried
// sooner, after half the interval (at least a minute).
func startStatsSample(ctx context.Context, rc *forgejo.ReconcileContext, base string, now time.Time) {
	f := rc.Instance
	interval := forgejo.DurationOr(f.Spec.Monitoring.StatsInterval, defaultStatsInterval)
	s := samplerFor(f)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return
	}
	last := s.lastAttempt
	if st := f.Status.Stats; st != nil && st.LastUpdated != nil && st.LastUpdated.After(last) {
		last = st.LastUpdated.Time
	}
	wait := interval
	if s.failures == 1 {
		wait = min(interval, max(interval/2, minStatsRetry))
	}
	// One second of tolerance for the second-precision lastUpdated and timer jitter.
	if !last.IsZero() && now.Sub(last) < wait-time.Second {
		return
	}
	s.lastAttempt = now
	log := logf.FromContext(ctx)
	token, err := metricsToken(ctx, rc)
	if err != nil {
		s.failed(log, err)
		return
	}
	s.running = true
	// Detached from the reconcile, which may end (and cancel its context) first.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), statsClient.Timeout)
	go func() {
		defer cancel()
		values, err := fetchStats(sctx, base, token)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.running = false
		if err != nil {
			s.failed(log, err)
			return
		}
		s.failures, s.lastErr = 0, ""
		done := metav1.NewTime(time.Now().Truncate(time.Second))
		s.result = &forgejov1.ForgejoStats{
			Users:         values["users"],
			Organizations: values["organizations"],
			Repositories:  values["repositories"],
			Issues:        values["issues"],
			LastUpdated:   &done,
		}
	}()
}

// metricsToken reads the generated bearer token that protects Forgejo's /metrics.
func metricsToken(ctx context.Context, rc *forgejo.ReconcileContext) (string, error) {
	f := rc.Instance
	sec := &corev1.Secret{}
	key := types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}
	if err := rc.Client.Get(ctx, key, sec); err != nil {
		return "", fmt.Errorf("reading the metrics token: %w", err)
	}
	token := string(sec.Data[forgejo.SecretKeyMetricsToken])
	if token == "" {
		return "", fmt.Errorf("secret %s has no %q", sec.Name, forgejo.SecretKeyMetricsToken)
	}
	return token, nil
}

// fetchStats reads the gitea_* statistics from Forgejo's /metrics.
func fetchStats(ctx context.Context, base, token string) (map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/metrics", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := statsClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /metrics: HTTP %d", resp.StatusCode)
	}
	return scanStats(resp.Body, maxStatsBody)
}

// scanStats reads the gitea_* statistics from a Prometheus text exposition. It reads the
// body as a stream, stops once every statistic is found and fails instead of returning
// partial values when the body is longer than limit bytes.
func scanStats(r io.Reader, limit int64) (map[string]int64, error) {
	lr := &io.LimitedReader{R: r, N: limit + 1}
	br := bufio.NewReaderSize(lr, 64<<10)
	values := map[string]int64{}
	for len(values) < len(statsMetrics) {
		line, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			// An overlong line (long label values) is not one of the plain gitea_* gauges.
			for errors.Is(err, bufio.ErrBufferFull) {
				_, err = br.ReadSlice('\n')
			}
			line = nil
		}
		if kind, v, ok := parseStatsLine(line); ok {
			if _, seen := values[kind]; !seen {
				values[kind] = v
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("reading /metrics: %w", err)
		}
	}
	if len(values) < len(statsMetrics) && lr.N <= 0 {
		return nil, fmt.Errorf("/metrics is larger than %d MiB and ends before all of %s were found "+
			"(high-cardinality [metrics] ENABLED_ISSUE_BY_* families?)", limit>>20,
			strings.Join(slices.Sorted(maps.Keys(statsMetrics)), ", "))
	}
	if len(values) == 0 {
		return nil, errors.New("/metrics has none of the gitea_* statistics")
	}
	return values, nil
}

var giteaPrefix = []byte("gitea_")

// parseStatsLine parses one sample line ("name value", "name{labels} value", optionally
// followed by a timestamp) of a statistic in statsMetrics.
func parseStatsLine(line []byte) (kind string, value int64, ok bool) {
	if !bytes.HasPrefix(line, giteaPrefix) {
		return "", 0, false
	}
	end := bytes.IndexAny(line, "{ \t")
	if end < 0 {
		return "", 0, false
	}
	kind, ok = statsMetrics[string(line[:end])]
	if !ok {
		return "", 0, false
	}
	rest := line[end:]
	if rest[0] == '{' {
		i := bytes.LastIndexByte(rest, '}')
		if i < 0 {
			return "", 0, false
		}
		rest = rest[i+1:]
	}
	fields := bytes.Fields(rest)
	if len(fields) == 0 {
		return "", 0, false
	}
	v, err := strconv.ParseFloat(string(fields[0]), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return "", 0, false
	}
	return kind, int64(v), true
}

// publishStats exports status.stats as forgejo_operator_stats (also after an operator
// restart, before the next sample is due).
func publishStats(f *forgejov1.Forgejo) {
	s := f.Status.Stats
	if s == nil || s.LastUpdated == nil {
		return
	}
	for kind, v := range map[string]int64{
		statUsers: s.Users, statOrganizations: s.Organizations, statRepositories: s.Repositories,
		statIssues: s.Issues,
	} {
		forgejo.Stats.WithLabelValues(f.Namespace, f.Name, kind).Set(float64(v))
	}
}

// probeError is a request that got no HTTP response. hung is set when a connection was
// established and the request sent, but no response headers arrived in time: Forgejo
// (or at least its kernel) accepted the connection and then did not answer. DNS and dial
// errors, and connections reset or closed before any response (e.g. a service mesh
// rejecting the operator's plaintext request), leave it unset.
type probeError struct {
	err  error
	hung bool
}

func (e *probeError) Error() string {
	if e.hung {
		return e.err.Error() + " (connection accepted, no response)"
	}
	return e.err.Error()
}

func (e *probeError) Unwrap() error { return e.err }

// get performs a GET. A non-zero status code means Forgejo answered, even if reading the
// body then failed; transport failures are returned as *probeError.
func get(ctx context.Context, url string) ([]byte, int, error) {
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		var ne net.Error
		hung := connected.Load() && ctx.Err() == nil && errors.As(err, &ne) && ne.Timeout()
		return nil, 0, &probeError{err: err, hung: hung}
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	return body, resp.StatusCode, err
}
