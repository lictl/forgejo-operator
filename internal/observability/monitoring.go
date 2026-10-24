package observability

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"time"

	"github.com/prometheus/common/model"
	"github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	forgejov1 "github.com/lictl/forgejo-operator/api/v1alpha1"
	"github.com/lictl/forgejo-operator/internal/forgejo"
)

// defaultScrapeInterval is deliberately slow: every Forgejo /metrics scrape runs a set of
// COUNT(*) queries against the database.
const defaultScrapeInterval = "60s"

// Minimal typed mirrors of the prometheus-operator API, so the operator has no Go
// dependency on prometheus-operator. Converted to unstructured before being applied.

type smSpec struct {
	Selector          metav1.LabelSelector `json:"selector"`
	NamespaceSelector smNamespaceSelector  `json:"namespaceSelector"`
	Endpoints         []smEndpoint         `json:"endpoints"`
}

type smNamespaceSelector struct {
	MatchNames []string `json:"matchNames"`
}

type smEndpoint struct {
	Port          string          `json:"port"`
	Path          string          `json:"path"`
	Scheme        string          `json:"scheme"`
	Interval      string          `json:"interval"`
	Authorization smAuthorization `json:"authorization"`
}

type smAuthorization struct {
	Type        string                   `json:"type"`
	Credentials corev1.SecretKeySelector `json:"credentials"`
}

type ruleSpec struct {
	Groups []ruleGroup `json:"groups"`
}

type ruleGroup struct {
	Name     string `json:"name"`
	Interval string `json:"interval,omitempty"`
	Rules    []rule `json:"rules"`
}

type rule struct {
	Record      string            `json:"record,omitempty"`
	Alert       string            `json:"alert,omitempty"`
	Expr        string            `json:"expr"`
	For         string            `json:"for,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// promDuration is prometheus-operator's Duration pattern (ServiceMonitor endpoint interval).
var promDuration = regexp.MustCompile(`^(0|(([0-9]+)y)?(([0-9]+)w)?(([0-9]+)d)?(([0-9]+)h)?(([0-9]+)m)?(([0-9]+)s)?(([0-9]+)ms)?)$`)

// serviceMonitorSpec scrapes the "http" port of "<name>-http" with the generated bearer token.
func serviceMonitorSpec(f *forgejov1.Forgejo) (*smSpec, error) {
	interval := defaultScrapeInterval
	if sm := f.Spec.Monitoring.ServiceMonitor; sm != nil && sm.Interval != "" {
		interval = sm.Interval
	}
	if !promDuration.MatchString(interval) {
		return nil, fmt.Errorf("spec.monitoring.serviceMonitor.interval %q is not a Prometheus duration (e.g. 30s, 1m, 1h30m)", interval)
	}
	return &smSpec{
		Selector:          metav1.LabelSelector{MatchLabels: forgejo.SelectorLabels(f)},
		NamespaceSelector: smNamespaceSelector{MatchNames: []string{f.Namespace}},
		Endpoints: []smEndpoint{{
			Port:     "http",
			Path:     "/metrics",
			Scheme:   "http",
			Interval: interval,
			Authorization: smAuthorization{
				Type: "Bearer",
				Credentials: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: forgejo.SecretsName(f)},
					Key:                  forgejo.SecretKeyMetricsToken,
				},
			},
		}},
	}, nil
}

// prometheusRuleSpec renders alerts scoped to one instance. Operator metrics carry the
// instance as {namespace, name} (the operator must be scraped with honorLabels: true);
// Forgejo's own metrics are selected by the scrape job, which prometheus-operator names
// after the HTTP Service.
//
// Every alert except ForgejoDown is computed from operator metrics, which the default
// install does not scrape. ForgejoOperatorMetricsMissing reports that, and absent()-based
// alerts only fire while the instance's operator metrics are present, so a scrape gap is
// not mistaken for a missing backup.
//
// Every alert links its runbook (runbook_url, docs/runbooks/<AlertName>.md). The SLO
// recording rules and burn-rate alerts are in groups of their own (see sloGroups).
func prometheusRuleSpec(f *forgejov1.Forgejo, scraped bool) (*ruleSpec, error) {
	s, err := sloOf(f)
	if err != nil {
		return nil, err
	}
	if b := f.Spec.Monitoring.RunbookBaseURL; b != "" {
		if err := ValidateRunbookBaseURL(b); err != nil {
			return nil, fmt.Errorf("spec.monitoring.runbookBaseURL: %w", err)
		}
	}
	op := fmt.Sprintf(`namespace=%q, name=%q`, f.Namespace, f.Name)
	job := fmt.Sprintf(`namespace=%q, job=%q`, f.Namespace, forgejo.HTTPServiceName(f))
	// Phases in which Forgejo is expected to be unavailable.
	expectedDown := fmt.Sprintf(`forgejo_operator_instance_info{%s, phase=~"%s|%s"}`,
		op, forgejov1.PhaseRestoring, forgejov1.PhasePaused)
	ref := f.Namespace + "/" + f.Name
	kc := fmt.Sprintf("-n %s", f.Namespace)

	var rules []rule
	add := func(alert, severity, forDur, expr, summary, description string) {
		rules = append(rules, rule{
			Alert: alert,
			Expr:  expr,
			For:   forDur,
			Labels: map[string]string{
				"severity":     severity,
				labelNamespace: f.Namespace,
				"forgejo":      f.Name,
			},
			Annotations: map[string]string{"summary": summary, "description": description, "runbook_url": runbookURL(f, alert)},
		})
	}

	if scraped && (forgejo.DesiredReplicas(f) > 0 || forgejo.HorizontalAutoscalingEnabled(f)) {
		add("ForgejoDown", "critical", "5m",
			fmt.Sprintf(`(max(up{%[1]s}) == 0 or absent(up{%[1]s})) unless on() %[2]s`, job, expectedDown),
			fmt.Sprintf("Forgejo %s is down", ref),
			fmt.Sprintf("Prometheus cannot scrape any Forgejo pod behind Service %s for 5 minutes. "+
				"Check `kubectl get pods %s -l app.kubernetes.io/instance=%s`, the pod logs and the "+
				"Ready/Healthy conditions of `kubectl describe fj %s %s`.",
				forgejo.HTTPServiceName(f), kc, f.Name, f.Name, kc))
	}

	// instance_ready is exported on every reconcile and, unlike instance_info, never
	// deleted and re-created, so its absence means the operator's metrics are not scraped.
	ready := fmt.Sprintf(`forgejo_operator_instance_ready{%s}`, op)
	add("ForgejoOperatorMetricsMissing", "warning", "15m",
		fmt.Sprintf(`absent(%s)`, ready),
		fmt.Sprintf("Operator metrics of Forgejo %s are missing", ref),
		fmt.Sprintf("Prometheus has no forgejo_operator_instance_ready series for this instance, so every other "+
			"alert of %s except ForgejoDown is inactive (backup, health, volume, upgrade and autopilot alerts). "+
			"Either the operator is not running, or it is not scraped: enable config/prometheus in "+
			"config/default/kustomization.yaml (its ServiceMonitor sets honorLabels: true, which these alerts "+
			"need) and allow Prometheus's ServiceAccount to GET the /metrics non-resource URL (the operator's "+
			"metrics-reader ClusterRole). See docs/observability.md, \"Scraping the operator\".", ref))

	// An instance intentionally scaled to zero is never Ready; with an HPA the floor is at
	// least one replica, so it must be Ready again.
	if forgejo.DesiredReplicas(f) > 0 || forgejo.HorizontalAutoscalingEnabled(f) {
		add("ForgejoNotReady", "warning", "10m",
			fmt.Sprintf(`max(%s) == 0 unless on() %s`, ready, expectedDown),
			fmt.Sprintf("Forgejo %s has not been Ready for 10 minutes", ref),
			fmt.Sprintf("The operator reports the instance as not Ready. Run `kubectl describe fj %s %s` and "+
				"look at status.conditions (DatabaseReady, Progressing, Degraded) and recent events.", f.Name, kc))
	}

	add("ForgejoHealthCheckFailing", "warning", "",
		fmt.Sprintf(`increase(forgejo_operator_health_check_failures_total{%s}[10m]) > 3`, op),
		fmt.Sprintf("Forgejo %s fails its health checks", ref),
		fmt.Sprintf("More than 3 operator probes of /api/healthz failed in the last 10 minutes "+
			"({{ $value | printf \"%%.0f\" }}). `kubectl get fj %s %s -o jsonpath='{.status.health}'` shows "+
			"the failing checks (database:ping, cache:ping). With self-healing enabled the operator restarts pods "+
			"after spec.autoPilot.selfHealing.failureThreshold consecutive failures.", f.Name, kc))

	probeWindow := model.Duration(max(5*time.Minute,
		2*forgejo.DurationOr(f.Spec.AutoPilot.SelfHealing.HealthCheckInterval, defaultHealthInterval)))
	add("ForgejoHealthProbeUnreachable", "warning", "15m",
		fmt.Sprintf(`increase(forgejo_operator_health_check_unreachable_total{%s}[%s]) > 0`, op, probeWindow),
		fmt.Sprintf("The operator cannot reach Forgejo %s", ref),
		fmt.Sprintf("For 15 minutes the operator's probes of /api/healthz have not reached Forgejo (DNS, dial or "+
			"connection errors, or a proxy answering instead). Health checks, self-healing, status.version and "+
			"status.stats are blind until it can: a NetworkPolicy or service mesh must allow the operator's pods "+
			"to connect to Service %s port %d in namespace %s (Istio STRICT mTLS: PERMISSIVE for that port, or "+
			"a sidecar for the operator). The Healthy condition of `kubectl describe fj %s %s` (reason "+
			"Unreachable) names the error.",
			forgejo.HTTPServiceName(f), forgejo.HTTPServicePort(f), f.Namespace, f.Name, kc))

	add("ForgejoBackupFailed", "warning", "",
		recentlyIncreased(fmt.Sprintf(`forgejo_operator_backups_total{%s, result="failed"}`, op), "1h"),
		fmt.Sprintf("A backup of Forgejo %s failed", ref),
		fmt.Sprintf("At least one ForgejoBackup failed in the last hour. Run `kubectl get forgejobackups %s "+
			"-l forgejo.lucas.icu/instance=%s` and inspect the failed backup's status and Job logs "+
			"(database reachability, space on the %s PVC).", kc, f.Name, forgejo.BackupPVCName(f)))

	// The recovery point objective (RPO) is two intervals of the backup schedule: a missed
	// run is tolerated, a second one is not.
	if f.Spec.Backup.Enabled {
		rpo := model.Duration(backupRPO(f))
		last := fmt.Sprintf(`forgejo_operator_backup_last_success_timestamp_seconds{%s}`, op)
		desc := fmt.Sprintf("Scheduled backups (%q) must keep the last successful backup younger than the recovery point "+
			"objective of %s (two schedule intervals). Check status.backup and the BackupHealthy condition of "+
			"`kubectl describe fj %s %s`, and the most recent ForgejoBackups (`kubectl get fjb %s -l "+
			"forgejo.lucas.icu/instance=%s`).", f.Spec.Backup.Schedule, rpo, f.Name, kc, kc, f.Name)
		add("ForgejoBackupTooOld", "warning", "5m",
			fmt.Sprintf(`time() - max(%s) > %d`, last, int64(time.Duration(rpo).Seconds())),
			fmt.Sprintf("The last successful backup of Forgejo %s is older than its RPO of %s", ref, rpo), desc)
		// No success recorded at all: only alert once a full RPO has passed without data,
		// and only while the instance's operator metrics are scraped at all (otherwise
		// ForgejoOperatorMetricsMissing fires).
		add("ForgejoBackupMissing", "warning", rpo.String(),
			fmt.Sprintf(`absent(%s) and on() count(%s) > 0`, last, ready),
			fmt.Sprintf("Forgejo %s has no successful backup", ref), desc)
	}

	if f.Spec.Backup.Remote != nil {
		add("ForgejoBackupUploadFailed", "warning", "",
			recentlyIncreased(fmt.Sprintf(`forgejo_operator_backup_uploads_total{%s, result="failed"}`, op), "1h"),
			fmt.Sprintf("A backup of Forgejo %s was not uploaded off-cluster", ref),
			fmt.Sprintf("At least one upload to spec.backup.remote failed in the last hour; the backup itself is kept on the "+
				"%s PVC. status.remote.message of the ForgejoBackup (`kubectl get forgejobackups %s -l "+
				"forgejo.lucas.icu/instance=%s -o wide`) and the BackupUploadFailed events name the cause (credentials, "+
				"key Secret, bucket reachability).", forgejo.BackupPVCName(f), kc, f.Name))
		if f.Spec.Backup.Enabled {
			rpo := model.Duration(backupRPO(f))
			last := fmt.Sprintf(`forgejo_operator_backup_last_upload_success_timestamp_seconds{%s}`, op)
			desc := fmt.Sprintf("The off-cluster copy is the recovery point of a disaster recovery: the last backup uploaded "+
				"to spec.backup.remote must be younger than the recovery point objective of %s (two intervals of %q). Check "+
				"status.backup.remote and the BackupHealthy condition of `kubectl describe fj %s %s`.", rpo,
				f.Spec.Backup.Schedule, f.Name, kc)
			add("ForgejoBackupUploadTooOld", "warning", "5m",
				fmt.Sprintf(`time() - max(%s) > %d`, last, int64(time.Duration(rpo).Seconds())),
				fmt.Sprintf("The last off-cluster backup of Forgejo %s is older than its RPO of %s", ref, rpo), desc)
			add("ForgejoBackupUploadMissing", "warning", rpo.String(),
				fmt.Sprintf(`absent(%s) and on() count(%s) > 0`, last, ready),
				fmt.Sprintf("Forgejo %s has no off-cluster backup", ref), desc)
		}
	}

	add("ForgejoVolumeAlmostFull", "warning", "10m",
		fmt.Sprintf(`max by (namespace, name, claim) (forgejo_operator_volume_used_ratio{%s}) > 0.9`, op),
		fmt.Sprintf("Volume {{ $labels.claim }} of Forgejo %s is almost full", ref),
		fmt.Sprintf("PVC {{ $labels.claim }} is {{ $value | humanizePercentage }} full. If "+
			"spec.autoPilot.storageAutoExpand is enabled, check status.storage and events for failed expansions "+
			"(the StorageClass needs allowVolumeExpansion, and maxSize may have been reached); otherwise "+
			"increase the PVC size: `kubectl get pvc %s`.", kc))

	add("ForgejoUpgradeFailed", "warning", "",
		recentlyIncreased(fmt.Sprintf(`forgejo_operator_upgrades_total{%s, result!="succeeded"}`, op), "1h"),
		fmt.Sprintf("An upgrade of Forgejo %s failed", ref),
		fmt.Sprintf("An image upgrade failed or was rolled back in the last hour. status.upgrade of "+
			"`kubectl get fj %s %s -o yaml` names the failed image; it is not retried until spec.image "+
			"changes. Check the logs of the failed pods for migration errors.", f.Name, kc))

	if forgejo.DatabaseType(f) == forgejov1.DatabaseTypeCNPG {
		add("ForgejoDatabaseUpgradeFailed", "warning", "",
			recentlyIncreased(fmt.Sprintf(`forgejo_operator_database_upgrades_total{%s, result!="succeeded"}`, op), "1h"),
			fmt.Sprintf("A PostgreSQL major upgrade of Forgejo %s failed", ref),
			fmt.Sprintf("A PostgreSQL major upgrade of the CNPG cluster failed or was rolled back in the last hour. "+
				"status.database.upgrade of `kubectl get fj %s %s -o yaml` names the failed image and the reason; it is not retried "+
				"until spec.database.cnpg.imageName changes. With rollbackRestoreFailed Forgejo stays stopped until a backup is "+
				"restored. See docs/upgrades.md, \"PostgreSQL major upgrades\".", f.Name, kc))
	}

	add("ForgejoVersionUnsupported", "warning", "1h",
		fmt.Sprintf(`max by (namespace, name, major, support) (forgejo_operator_version_supported{%s}) == 0`, op),
		fmt.Sprintf("Forgejo %s runs a version outside the operator's support policy ({{ $labels.support }})", ref),
		fmt.Sprintf("Forgejo major {{ $labels.major }} is {{ $labels.support }}: the operator supports the current "+
			"LTS release and the latest release (Deprecated: still maintained upstream but outside that policy; "+
			"EndOfLife: no more security fixes; Future: newer than the operator's version table; Unknown: the "+
			"version cannot be determined). The VersionSupported condition of `kubectl describe fj %s %s` says "+
			"what to upgrade to and by when; see docs/versions.md.", f.Name, kc))
	add("ForgejoVersionAdvisory", "warning", "1h",
		fmt.Sprintf(`max by (namespace, name, major) (forgejo_operator_version_advisory{%s}) == 1`, op),
		fmt.Sprintf("Forgejo %s runs a patch release that an advisory recommends replacing", ref),
		fmt.Sprintf("The running Forgejo {{ $labels.major }} patch release is older than the one the operator's version "+
			"table recommends (critical security fixes, or a release upstream advises against). The VersionSupported "+
			"condition of `kubectl describe fj %s %s` names the minimum patch release and why; pin spec.image to it "+
			"(N.0.P), or pull the floating tag again (imagePullPolicy: Always). See docs/versions.md.", f.Name, kc))

	// ForgejoRunners of the instance (docs/actions-runners.md): jobs wait while none of a
	// runner set's runners is online. Both series come from Forgejo's API, so they are absent
	// (and the alert inactive) while the operator cannot read it.
	add("ForgejoRunnersOffline", "warning", "10m",
		fmt.Sprintf(`max by (namespace, name, runner) (forgejo_operator_runner_jobs_waiting{%[1]s}) > 0 `+
			`and on (namespace, name, runner) max by (namespace, name, runner) (forgejo_operator_runners_online{%[1]s}) == 0 `+
			`and on (namespace, name, runner) max by (namespace, name, runner) (forgejo_operator_runners_desired{%[1]s}) > 0`, op),
		fmt.Sprintf("Jobs wait for ForgejoRunner {{ $labels.runner }} of Forgejo %s, but none of its runners is online", ref),
		fmt.Sprintf("{{ $value | printf \"%%.0f\" }} jobs have waited for at least 10 minutes for runners with the labels of "+
			"ForgejoRunner {{ $labels.runner }}, and Forgejo reports none of its runners online. `kubectl %s describe "+
			"forgejorunner {{ $labels.runner }}` shows why (Ready and Registered conditions); check the runner pods' logs "+
			"(`kubectl %s logs {{ $labels.runner }}-0 -c runner`) for connection or authentication errors.", kc, kc))

	if f.Spec.HighAvailability.Enabled {
		valkeyHelp := fmt.Sprintf("`kubectl get fj %s %s -o jsonpath='{.status.highAvailability.valkey}'` shows the primary and "+
			"each pod's role, the ValkeyReady condition of `kubectl describe fj %s %s` why; the Sentinels log every decision "+
			"(`kubectl %s logs %s-0 -c sentinel`). See docs/high-availability.md.", f.Name, kc, f.Name, kc, kc, forgejo.ValkeyName(f))
		add("ForgejoValkeyNoMaster", "critical", "2m",
			fmt.Sprintf(`max(forgejo_operator_valkey_master_available{%s}) == 0 unless on() %s`, op, expectedDown),
			fmt.Sprintf("Valkey of Forgejo %s has no usable primary", ref),
			"Forgejo keeps its cache and queues in Valkey: without a primary its health check fails and every server pod "+
				"turns NotReady. A failover normally takes about 10 seconds; for 2 minutes now the Sentinels have named no "+
				"Ready primary (no Sentinel majority, or no replica to promote). "+valkeyHelp)
		add("ForgejoValkeyFailedOver", "warning", "",
			recentlyIncreased(fmt.Sprintf(`forgejo_operator_valkey_failovers_total{%s}`, op), "15m"),
			fmt.Sprintf("The Valkey primary of Forgejo %s moved to another pod", ref),
			"The Sentinels promoted a replica in the last 15 minutes, after the primary's pod failed, was evicted or "+
				"was deleted (writes of its last second may be lost: replication is asynchronous). Check why the old "+
				"primary's pod went away. "+valkeyHelp)
		add("ForgejoValkeyDegraded", "warning", "15m",
			fmt.Sprintf(`max(forgejo_operator_valkey_ready_members{%s}) < %d unless on() %s`, op, 3, expectedDown),
			fmt.Sprintf("Valkey of Forgejo %s runs with fewer than three Ready pods", ref),
			"One more failure would leave Valkey without a replica to promote or without a Sentinel majority. "+valkeyHelp)
		add("ForgejoCronRunnerDown", "warning", "15m",
			fmt.Sprintf(`max(forgejo_operator_cron_runner_ready{%s}) == 0`, op),
			fmt.Sprintf("The cron runner of Forgejo %s is not Ready", ref),
			fmt.Sprintf("Pod %s-cron-* runs Forgejo's cron tasks (mirror updates, scheduled workflows, cleanups) for every "+
				"replica, and has not been Ready for 15 minutes. `kubectl %s get pods -l app.kubernetes.io/instance=%s,"+
				"app.kubernetes.io/component=cron` and its logs show why. See docs/high-availability.md.", f.Name, kc, f.Name))
	}

	add("ForgejoAutoPilotActive", "warning", "",
		fmt.Sprintf(`sum by (namespace, name) (increase(forgejo_operator_autopilot_actions_total{%s}[1h])) >= 5`, op),
		fmt.Sprintf("The operator keeps intervening on Forgejo %s", ref),
		fmt.Sprintf("About {{ $value | printf \"%%.0f\" }} autonomous actions (restarts, rollbacks, scaling, "+
			"expansion) were taken in the last hour, which usually hides a persistent problem. See "+
			"status.autoPilot.recentActions of `kubectl get fj %s %s -o yaml` and the events.", f.Name, kc))

	groups := append([]ruleGroup{{Name: "forgejo." + f.Namespace + "." + f.Name, Rules: rules}},
		sloGroups(f, s, op, ref, kc)...)
	return &ruleSpec{Groups: groups}, nil
}

// recentlyIncreased is true when the counter grew within window. A counter series is
// created on its first increment with value 1, which increase() cannot see, so new
// series count as an increase as well.
func recentlyIncreased(sel, window string) string {
	return fmt.Sprintf(`sum by (namespace, name) (increase(%[1]s[%[2]s]) > 0 or (%[1]s unless %[1]s offset %[2]s)) > 0`, sel, window)
}

// backupRPO is the recovery point objective of the instance: twice the longest gap
// between two scheduled runs, so one missed or failed run is tolerated. The gap is taken
// over a fixed reference window so the rendered rule is stable, and falls back to a day
// for unparsable schedules.
func backupRPO(f *forgejov1.Forgejo) time.Duration {
	period := 24 * time.Hour
	if sched, err := cron.ParseStandard(f.Spec.Backup.Schedule); err == nil {
		ref := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)
		var gap time.Duration
		prev := sched.Next(ref)
		for i := 0; i < 64 && !prev.IsZero() && prev.Before(ref.AddDate(1, 0, 0)); i++ {
			next := sched.Next(prev)
			if next.IsZero() {
				break
			}
			gap = max(gap, next.Sub(prev))
			prev = next
		}
		if gap > 0 {
			period = gap
		}
	}
	return (2 * period).Round(time.Minute)
}

// dashboardUID is a stable Grafana uid (max 40 chars) for the instance.
func dashboardUID(f *forgejov1.Forgejo) string {
	uid := "forgejo-" + f.Namespace + "-" + f.Name
	if len(uid) <= 40 {
		return uid
	}
	h := fnv.New64a()
	h.Write([]byte(f.Namespace + "/" + f.Name))
	return "forgejo-" + strconv.FormatUint(h.Sum64(), 36)
}
