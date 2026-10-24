package observability

import (
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/common/model"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
)

// DefaultRunbookBaseURL holds the runbooks of every alert the operator ships (docs/runbooks).
const DefaultRunbookBaseURL = "https://github.com/lictl/forgejo-operator/blob/main/docs/runbooks"

// RunbookBaseURL is the operator-wide base of the alerts' runbook_url annotations
// (--runbook-base-url, else RUNBOOK_BASE_URL, else DefaultRunbookBaseURL).
// spec.monitoring.runbookBaseURL overrides it per instance.
var RunbookBaseURL = envOr("RUNBOOK_BASE_URL", DefaultRunbookBaseURL)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ValidateRunbookBaseURL checks a runbook base: an absolute http(s) URL without query or
// fragment, since "/<AlertName>.md" is appended to it.
func ValidateRunbookBaseURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("runbook base URL %q: %w", s, err)
	}
	web := strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
	if !web || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(s, " \t\n") {
		return fmt.Errorf("runbook base URL %q must be an http(s) URL without query or fragment", s)
	}
	return nil
}

// runbookBase is the runbook base of the instance, without a trailing slash.
func runbookBase(f *forgejov1.Forgejo) string {
	base := RunbookBaseURL
	if b := f.Spec.Monitoring.RunbookBaseURL; b != "" {
		base = b
	}
	return strings.TrimRight(base, "/")
}

// runbookURL is the runbook_url annotation of an alert.
func runbookURL(f *forgejov1.Forgejo, alert string) string {
	return runbookBase(f) + "/" + alert + ".md"
}

const (
	defaultAvailabilityTarget = "99.5"
	defaultSLOWindow          = "30d"
	// referenceSLOWindow is the window the SRE workbook's burn-rate windows are given for.
	referenceSLOWindow = 30 * 24 * time.Hour
	// minBurnWindow is the shortest window a burn rate is computed over: shorter windows
	// hold too few probes and scrapes (probes every 30s, scrapes every 30-60s).
	minBurnWindow = 5 * time.Minute
	// burnWindowSamples is the fewest health-check intervals a burn-rate window spans, so
	// that it always holds samples: with slow probes, a window that can fall between two of
	// them would let an alert without `for` resolve and fire again within one outage.
	burnWindowSamples = 3
)

// burnAlert is one window pair of the SRE workbook's multiwindow, multi-burn-rate alerts
// (https://sre.google/workbook/alerting-on-slos/), for a 30-day window.
type burnAlert struct {
	alert       string
	severity    string
	long, short time.Duration
	// burn is the error rate, in multiples of the error budget, that the alert fires at:
	// 14.4 spends 2% of a 30-day budget in an hour.
	burn string
}

var burnAlerts = []burnAlert{
	{alert: alertFastBurn, severity: severityCritical, long: time.Hour, short: 5 * time.Minute, burn: "14.4"},
	{alert: alertFastBurn, severity: severityCritical, long: 6 * time.Hour, short: 30 * time.Minute, burn: "6"},
	{alert: alertSlowBurn, severity: severityWarning, long: 24 * time.Hour, short: 2 * time.Hour, burn: "3"},
	{alert: alertSlowBurn, severity: severityWarning, long: 3 * 24 * time.Hour, short: 6 * time.Hour, burn: "1"},
}

const (
	alertFastBurn    = "ForgejoAvailabilityFastBurn"
	alertSlowBurn    = "ForgejoAvailabilitySlowBurn"
	severityCritical = "critical"
	severityWarning  = "warning"
)

// Recording rules of the SLOs (docs/observability.md, "Service level objectives").
const (
	recErrorRatio      = "forgejo_slo:availability_error_ratio"
	recBurnRate        = "forgejo_slo:availability_burn_rate"
	recObjective       = "forgejo_slo:availability_objective"
	recBudgetRemaining = "forgejo_slo:availability_error_budget_remaining"
	recRecoveryAge     = "forgejo_slo:backup_recovery_point_age_seconds"
	recRPO             = "forgejo_slo:backup_recovery_point_objective_seconds"
)

// Label keys of the rendered rules.
const (
	labelNamespace = "namespace"
	labelName      = "name"
)

// slo is an instance's availability objective.
type slo struct {
	// target as written ("99.5"), objective = target/100 and budget = 1-objective, both
	// formatted for PromQL.
	target, objective, budget string
	window                    time.Duration
	// windowName is spec.monitoring.slo.window as written (a Prometheus duration such as
	// 28d): the window label of the rules over the whole window, and the dashboard's.
	windowName string
	// interval is spec.autoPilot.selfHealing.healthCheckInterval: one availability sample
	// per interval. minWindow, the shortest burn-rate window, spans burnWindowSamples of them
	// and at least minBurnWindow, in whole minutes.
	interval, minWindow time.Duration
}

// sloOf parses spec.monitoring.slo; the CRD has validated both fields already.
func sloOf(f *forgejov1.Forgejo) (slo, error) {
	s := slo{target: f.Spec.Monitoring.SLO.AvailabilityTarget}
	if s.target == "" {
		s.target = defaultAvailabilityTarget
	}
	t, ok := new(big.Rat).SetString(s.target)
	hundred := big.NewRat(100, 1)
	if !ok || t.Sign() <= 0 || t.Cmp(hundred) >= 0 || strings.ContainsAny(s.target, "/eE") {
		return slo{}, fmt.Errorf("spec.monitoring.slo.availabilityTarget %q is not a percentage between 0 and 100 such as 99.5", s.target)
	}
	obj := new(big.Rat).Quo(t, hundred)
	budget := new(big.Rat).Sub(big.NewRat(1, 1), obj)
	s.objective, s.budget = ratString(obj), ratString(budget)

	w := f.Spec.Monitoring.SLO.Window
	if w == "" {
		w = defaultSLOWindow
	}
	d, err := model.ParseDuration(w)
	if err != nil || d < model.Duration(time.Hour) {
		return slo{}, errors.Join(fmt.Errorf("spec.monitoring.slo.window %q is not a duration of at least 1h such as 30d", w), err)
	}
	s.window, s.windowName = time.Duration(d), w

	s.interval = healthInterval(f)
	// Bounded, so that the product cannot overflow; windows of days are long enough.
	span := burnWindowSamples * min(s.interval, 24*time.Hour)
	s.minWindow = max(minBurnWindow, span.Truncate(time.Minute))
	if s.minWindow < span {
		s.minWindow += time.Minute
	}
	return s, nil
}

// ratString formats r as the shortest decimal that parses back to the same float64.
func ratString(r *big.Rat) string {
	f, _ := r.Float64()
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// scaled is a burn-rate window of the 30-day reference scaled to the instance's window,
// in whole minutes and at least minWindow.
func (s slo) scaled(d time.Duration) time.Duration {
	return max(s.minWindow, time.Duration(float64(d)*float64(s.window)/float64(referenceSLOWindow)).Round(time.Minute))
}

// exhaustion is how long the whole budget lasts at burn times the sustainable rate.
func (s slo) exhaustion(burn string) model.Duration {
	b, _ := strconv.ParseFloat(burn, 64)
	return model.Duration((time.Duration(float64(s.window) / b)).Round(time.Minute))
}

// sloGroups renders the SLO recording rules and the burn-rate alerts (one group, evaluated
// in order, so the alerts see this evaluation's ratios), and the recording rules over the
// whole SLO window (a group of its own with a slower interval: a range of 30 days is
// expensive to evaluate every 30s). RPO rules are added when scheduled backups are on.
func sloGroups(f *forgejov1.Forgejo, s slo, op, ref, kc string) []ruleGroup {
	rateOf := func(sel, w string) string {
		return fmt.Sprintf(`sum by (namespace, name) (rate(forgejo_operator_availability_probes_total{%s%s}[%s]))`, op, sel, w)
	}
	// The error ratio is the share of unplanned down samples among all samples: planned
	// samples count as available, so a restore or a pause neither burns the budget nor
	// shrinks the denominator of the windows that follow it.
	errorRatio := func(w string) string {
		return rateOf(`, planned="false", result="down"`, w) + " / " + rateOf("", w)
	}
	// Over a burn-rate window the denominator is at least one sample per health-check
	// interval, so that a window holding fewer samples (a new instance, an operator that did
	// not run) is not dominated by a single failed probe.
	burnRatio := func(w string) string {
		return fmt.Sprintf("%s / clamp_min(%s, 1 / %s)", rateOf(`, planned="false", result="down"`, w), rateOf("", w),
			strconv.FormatFloat(s.interval.Seconds(), 'g', -1, 64))
	}
	sloLabels := func(w string) map[string]string {
		return map[string]string{labelNamespace: f.Namespace, labelName: f.Name, "window": w}
	}

	var windows []time.Duration
	for _, b := range burnAlerts {
		windows = append(windows, s.scaled(b.long), s.scaled(b.short))
	}
	slices.Sort(windows)
	windows = slices.Compact(windows)

	var rules []rule
	for _, w := range windows {
		mw := model.Duration(w).String()
		rules = append(rules, rule{Record: recErrorRatio, Expr: burnRatio(mw), Labels: sloLabels(mw)})
	}
	for _, w := range windows {
		mw := model.Duration(w).String()
		rules = append(rules, rule{
			Record: recBurnRate,
			Expr:   fmt.Sprintf(`%s{%s, window=%q} / %s`, recErrorRatio, op, mw, s.budget),
			Labels: sloLabels(mw),
		})
	}
	window := s.windowName
	for _, b := range burnAlerts {
		long, short := model.Duration(s.scaled(b.long)), model.Duration(s.scaled(b.short))
		burnOver := func(w model.Duration) string {
			return fmt.Sprintf(`max by (namespace, name) (%s{%s, window=%q}) > %s`, recBurnRate, op, w, b.burn)
		}
		kind, act := "fast", "Page: act now."
		if b.alert == alertSlowBurn {
			kind, act = "slow", "Ticket: fix it within working hours."
		}
		over := fmt.Sprintf("Over the last %s, and still over the last %s,", long, short)
		if long == short {
			over = fmt.Sprintf("Over the last %s,", long)
		}
		rules = append(rules, rule{
			Alert: b.alert,
			Expr:  burnOver(long) + " and on (namespace, name) " + burnOver(short),
			Labels: map[string]string{
				"severity": b.severity, labelNamespace: f.Namespace, "forgejo": f.Name,
				"slo": "availability", "long": long.String(), "short": short.String(),
			},
			Annotations: map[string]string{
				"summary": fmt.Sprintf("Forgejo %s burns its availability error budget at %sx over %s (%s burn)", ref, b.burn, long, kind),
				"description": fmt.Sprintf("%[1]s the operator's probes found Forgejo "+
					"unavailable {{ $value | printf \"%%.1f\" }} times as often as the objective allows (%[2]s%% over %[3]s, "+
					"error budget %[4]s). At %[5]sx the whole budget is gone within %[6]s. Planned samples (restores, "+
					"orchestrated upgrades, spec.paused, zero replicas, the first installation) count as available. %[7]s Start with `kubectl describe fj %[8]s %[9]s` "+
					"(Ready, Healthy, DatabaseReady), `kubectl get pods %[9]s -l app.kubernetes.io/instance=%[8]s` and the "+
					"ForgejoDown, ForgejoNotReady, ForgejoHealthCheckFailing and ForgejoHealthProbeUnreachable alerts.",
					over, s.target, window, s.budget, b.burn, s.exhaustion(b.burn), act, f.Name, kc),
				"runbook_url": runbookURL(f, b.alert),
			},
		})
	}

	if f.Spec.Backup.Enabled {
		rpo := backupRPO(f)
		copies := []struct{ copy, metric string }{{"local", "forgejo_operator_backup_last_success_timestamp_seconds"}}
		if f.Spec.Backup.Remote != nil {
			copies = append(copies, struct{ copy, metric string }{"remote", "forgejo_operator_backup_last_upload_success_timestamp_seconds"})
		}
		for _, c := range copies {
			rules = append(rules, rule{
				Record: recRecoveryAge,
				Expr:   fmt.Sprintf(`time() - max by (namespace, name) (%s{%s})`, c.metric, op),
				Labels: map[string]string{labelNamespace: f.Namespace, labelName: f.Name, "copy": c.copy},
			})
		}
		rules = append(rules, rule{
			Record: recRPO,
			Expr:   fmt.Sprintf("vector(%d)", int64(rpo.Seconds())),
			Labels: map[string]string{labelNamespace: f.Namespace, labelName: f.Name},
		})
	}

	windowRules := []rule{
		{Record: recErrorRatio, Expr: errorRatio(window), Labels: sloLabels(window)},
		{Record: recObjective, Expr: fmt.Sprintf("vector(%s)", s.objective), Labels: sloLabels(window)},
		{
			Record: recBudgetRemaining,
			Expr:   fmt.Sprintf(`1 - %s{%s, window=%q} / %s`, recErrorRatio, op, window, s.budget),
			Labels: sloLabels(window),
		},
	}
	return []ruleGroup{
		{Name: "forgejo-slo." + f.Namespace + "." + f.Name, Rules: rules},
		{Name: "forgejo-slo-window." + f.Namespace + "." + f.Name, Interval: sloWindowInterval, Rules: windowRules},
	}
}

// sloWindowInterval is the evaluation interval of the rules over the whole SLO window.
const sloWindowInterval = "3m"
