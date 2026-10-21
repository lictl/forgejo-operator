package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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
	defaultHealthInterval = 30 * time.Second
	defaultStatsInterval  = 5 * time.Minute
	// dialTimeout bounds DNS resolution plus the TCP connect. A blackholed Service
	// (NetworkPolicy, CNI fault) must not hold the reconcile worker for long.
	dialTimeout = 2 * time.Second
	// probeTimeout is how long Forgejo may take to send response headers once the
	// connection is established.
	probeTimeout = 5 * time.Second
	maxBody      = 8 << 20
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

func newProbeClient(dial, header time.Duration) *http.Client {
	return &http.Client{
		// Covers dial + headers + reading a /metrics body; the specific timeouts fire first.
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
	"gitea_users":         "users",
	"gitea_organizations": "organizations",
	"gitea_repositories":  "repositories",
	"gitea_issues":        "issues",
}

// ReconcileHealth probes /api/healthz and /api/v1/version and samples statistics into
// status.health, status.version, status.stats and the Healthy condition.
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
	// Export the failure counter from the start so increase() also sees the first failure.
	forgejo.HealthCheckFailures.WithLabelValues(f.Namespace, f.Name).Add(0)
	publishStats(f)

	if rc.Maintenance {
		skipHealth(rc, ReasonMaintenance, "Health checks are suspended during maintenance: "+rc.MaintenanceReason)
		return forgejo.StepResult{}, nil
	}
	ready, err := servingPods(ctx, rc)
	if err != nil {
		return forgejo.StepResult{}, err
	}
	if ready == 0 {
		skipHealth(rc, ReasonNoReadyReplicas, "No Forgejo pod is Ready; health checks resume once one is")
		return forgejo.StepResult{}, nil
	}

	now := time.Now()
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
		rc.Event(corev1.EventTypeNormal, "VersionDetected", "HealthCheck", "Forgejo reports version %s", v)
		f.Status.Version = v
	}

	if forgejo.MonitoringEnabled(f) {
		statsInterval := forgejo.DurationOr(f.Spec.Monitoring.StatsInterval, defaultStatsInterval)
		if s := f.Status.Stats; s == nil || s.LastUpdated == nil || now.Sub(s.LastUpdated.Time)+time.Second >= statsInterval {
			if err := sampleStats(ctx, rc, base); err != nil {
				logf.FromContext(ctx).Info("sampling Forgejo statistics failed", "error", err.Error())
			}
		}
	}
	return forgejo.StepResult{}, nil
}

// lastProbe remembers when each instance was last probed by this process. The informer
// cache may hand the next reconcile a copy of the instance from before the latest status
// patch, so status.health.lastCheckTime alone would let back-to-back reconciles probe twice.
var lastProbe sync.Map // types.NamespacedName -> time.Time

// servingPods counts the instance's Ready pods that are not terminating, i.e. the pods
// the HTTP Service routes to. Deployment status lags behind during rollouts.
func servingPods(ctx context.Context, rc *forgejo.ReconcileContext) (int, error) {
	pods := &corev1.PodList{}
	if err := rc.Client.List(ctx, pods, client.InNamespace(rc.Instance.Namespace),
		client.MatchingLabels(forgejo.SelectorLabels(rc.Instance))); err != nil {
		return 0, err
	}
	n := 0
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				n++
			}
		}
	}
	return n, nil
}

// skipHealth reports that no probe could be made. The failure streak is reset so that
// self-healing counts afresh once pods are Ready again (e.g. after it restarted them);
// the last observed checks are kept for diagnosis.
func skipHealth(rc *forgejo.ReconcileContext, reason, msg string) {
	h := rc.Instance.Status.Health
	if lastProbeFailed(h) {
		var bad []string
		for _, name := range slices.Sorted(maps.Keys(h.Checks)) {
			if st := h.Checks[name]; st != "pass" {
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

var statusRank = map[string]int{"pass": 0, "warn": 1, "fail": 2}

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
	body, code, err := get(ctx, base+"/api/healthz", "")
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
		worst := "pass"
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
		if worst != "pass" {
			p := name + "=" + worst
			if len(outputs) > 0 {
				p += " (" + strings.Join(outputs, "; ") + ")"
			}
			problems = append(problems, p)
		}
	}

	res.healthy = code == http.StatusOK && (hz.Status == "pass" || hz.Status == "warn")
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
	body, code, err := get(ctx, base+"/api/v1/version", "")
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

// sampleStats scrapes Forgejo's /metrics with the generated bearer token.
func sampleStats(ctx context.Context, rc *forgejo.ReconcileContext, base string) error {
	f := rc.Instance
	sec := &corev1.Secret{}
	if err := rc.Client.Get(ctx, types.NamespacedName{Namespace: f.Namespace, Name: forgejo.SecretsName(f)}, sec); err != nil {
		return err
	}
	token := string(sec.Data[forgejo.SecretKeyMetricsToken])
	if token == "" {
		return fmt.Errorf("secret %s has no %q", sec.Name, forgejo.SecretKeyMetricsToken)
	}
	body, code, err := get(ctx, base+"/metrics", token)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("GET /metrics: HTTP %d", code)
	}
	values, err := parseStats(body)
	if err != nil {
		return err
	}
	now := metav1.Now()
	f.Status.Stats = &forgejov1.ForgejoStats{
		Users:         values["users"],
		Organizations: values["organizations"],
		Repositories:  values["repositories"],
		Issues:        values["issues"],
		LastUpdated:   &now,
	}
	publishStats(f)
	return nil
}

func parseStats(body []byte) (map[string]int64, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parsing /metrics: %w", err)
	}
	values := map[string]int64{}
	for name, kind := range statsMetrics {
		mf := families[name]
		if mf == nil || len(mf.GetMetric()) == 0 {
			continue
		}
		m := mf.GetMetric()[0]
		switch {
		case m.GetGauge() != nil:
			values[kind] = int64(m.GetGauge().GetValue())
		case m.GetCounter() != nil:
			values[kind] = int64(m.GetCounter().GetValue())
		case m.GetUntyped() != nil:
			values[kind] = int64(m.GetUntyped().GetValue())
		}
	}
	if len(values) == 0 {
		return nil, errors.New("/metrics has none of the gitea_* statistics")
	}
	return values, nil
}

// publishStats exports status.stats as forgejo_operator_stats (also after an operator
// restart, before the next sample is due).
func publishStats(f *forgejov1.Forgejo) {
	s := f.Status.Stats
	if s == nil || s.LastUpdated == nil {
		return
	}
	for kind, v := range map[string]int64{
		"users": s.Users, "organizations": s.Organizations, "repositories": s.Repositories,
		"issues": s.Issues,
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
func get(ctx context.Context, url, bearer string) ([]byte, int, error) {
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
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
