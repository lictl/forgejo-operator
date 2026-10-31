# Observability

This page covers the operator's Level IV ("deep insights") features:

- Prometheus metrics from Forgejo and from the operator.
- One ServiceMonitor, one PrometheusRule and an optional Grafana dashboard for each `Forgejo`.
- Service level objectives: an availability SLI that does not hide downtime, burn-rate alerts
  and a recovery point objective derived from the backup schedule.
- A runbook for every alert ([runbooks/](runbooks/README.md)).
- Health, version and statistics in `status`.
- Conditions and events.

Code: `internal/observability` (steps `monitoring` and `health`; `slo.go` and
`availability.go` for the SLOs), `internal/forgejo/metrics.go` (operator metrics) and
`config/prometheus` (the operator's own ServiceMonitor and alerts). Rule tests:
`test/prometheus` (`make test-rules`).

## Prerequisites

Everything below works with [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack).
Each integration is optional:

- **No prometheus-operator CRDs:** the operator skips the ServiceMonitor and PrometheusRule and
  still serves its own metrics.
- **No Grafana sidecar:** the dashboard ConfigMap is harmless.

A test setup like the one used for development:

```yaml
# helm upgrade --install kps prometheus-community/kube-prometheus-stack -n monitoring --create-namespace -f values.yaml
prometheus:
  prometheusSpec:
    # Select ServiceMonitors/PrometheusRules from every namespace, without a release label.
    serviceMonitorSelectorNilUsesHelmValues: false
    ruleSelectorNilUsesHelmValues: false
    podMonitorSelectorNilUsesHelmValues: false
grafana:
  sidecar:
    dashboards:
      enabled: true
      label: grafana_dashboard
      labelValue: "1"
      searchNamespace: ALL
      folderAnnotation: grafana_folder   # dashboards land in a "Forgejo" folder
      provider:
        foldersFromFilesStructure: true
```

If your Prometheus selects objects by label (for example `release: kps`), add that label to
each object with `spec.monitoring.serviceMonitor.labels` and `spec.monitoring.prometheusRule.labels`.

### Network access from the operator

The `health` step probes Forgejo from the operator pod through
`http://<name>-http.<namespace>.svc.cluster.local:<port>` (port 3000 by default). The operator
must be allowed to open that connection:

- **NetworkPolicy:** if the Forgejo namespace denies ingress by default, allow ingress to the
  Forgejo pods on the HTTP port from the operator's namespace (`forgejo-operator-system` with the
  default kustomization).
- **Service mesh:** with Istio `STRICT` mTLS (or an authorization policy that only admits the
  ingress gateway), either use `PERMISSIVE` mode for that port, give the operator a sidecar, or
  allow its identity.

If the operator cannot reach Forgejo, the Healthy condition is `Unknown` with reason
`Unreachable`, and a `HealthCheckUnreachable` event names the error. Unreachable probes never
count as health failures, so they never trigger self-healing restarts. They are counted in
`forgejo_operator_health_check_unreachable_total`, and the `ForgejoHealthProbeUnreachable` alert
fires when they go on for 15 minutes. Health, version and statistics are not updated until the
operator can reach Forgejo again.

### Scraping the operator

**The per-instance alerts need this.** Every alert in the generated PrometheusRule except
`ForgejoDown` is computed from the operator's `forgejo_operator_*` metrics, and the default
install (`make deploy`, `make build-installer`) does not scrape them. Until Prometheus scrapes the
operator, those alerts can never fire. Each instance's `ForgejoOperatorMetricsMissing` alert
reports this after 15 minutes (see [Alerts](#alerts)).

Enable `../prometheus` in `config/default/kustomization.yaml` (the `[PROMETHEUS]` sections).
This deploys two objects:

- `config/prometheus/monitor.yaml`, a ServiceMonitor for the operator's HTTPS metrics endpoint.
- `config/prometheus/rules.yaml`, the operator-level alerts.

The metrics endpoint authenticates and authorizes every scrape (`TokenReview` and
`SubjectAccessReview`). Prometheus's ServiceAccount needs `get` on the non-resource URL
`/metrics`:

- kube-prometheus-stack already grants it in its Prometheus ClusterRole.
- For other installs, uncomment `metrics_reader_binding.yaml` in
  `config/prometheus/kustomization.yaml` and set the ServiceAccount there. It binds the operator's
  `metrics-reader` ClusterRole.

If the scrape is denied, the target shows `server returned HTTP status 403 Forbidden`.

The ServiceMonitor sets **`honorLabels: true`**, and this is required. Operator metrics
identify an instance by their own `namespace` and `name` labels. Without `honorLabels`,
Prometheus renames `namespace` to `exported_namespace`, and the generated alerts and the
dashboard no longer match. If you scrape the operator some other way (a custom scrape config, a
PodMonitor), keep `honor_labels: true`.

## Per-instance resources

The `monitoring` step creates these objects. Each gets the standard labels plus
`spec.monitoring.<resource>.labels`, and has the `Forgejo` as its controller owner.

| Object | Name | Created when |
|--------|------|--------------|
| ServiceMonitor | `<name>` | ServiceMonitor CRD present, `spec.monitoring.enabled` (default true) and `spec.monitoring.serviceMonitor.enabled` (default true) |
| PrometheusRule | `<name>` | PrometheusRule CRD present, `spec.monitoring.enabled` and `spec.monitoring.prometheusRule.enabled` (default true) |
| ConfigMap (Grafana dashboard) | `<name>-dashboard` | `spec.monitoring.enabled` and `spec.monitoring.grafanaDashboard.enabled` (default **false**) |

Turning a toggle off deletes the object again, but only if this instance controls it.

Monitoring is optional and never blocks the rest of the reconcile. The three objects are applied
independently, so an invalid ServiceMonitor does not keep the PrometheusRule (with its backup and
volume alerts) from being created. When an object cannot be applied, the operator does this:

- It sets `Degraded=True` with reason `MonitoringFailed`, unless another feature already reports
  `Degraded`. The message names the object and the error.
- It emits a `MonitoringFailed` Warning event, once for each distinct error and then at most once
  an hour.
- It retries every minute.

The steps after `monitoring` (scheduled backups, health checks and autopilot) keep running.
Examples of such errors are a scrape interval that is not a Prometheus duration (`1min` instead of
`1m`), a label key or value Kubernetes rejects, or an object of the same name that another owner
controls. The operator checks the interval and labels itself before sending anything, and leaves
an existing object unchanged when they are invalid. `ForgejoDown` is rendered only while the
ServiceMonitor exists.

```yaml
spec:
  monitoring:
    enabled: true            # /metrics in app.ini, protected by a generated bearer token
    statsInterval: 5m        # status.stats sampling period
    serviceMonitor:
      interval: 60s          # default; every scrape runs COUNT(*) queries in Forgejo's database
      labels: {release: kps}
    prometheusRule:
      labels: {release: kps}
    grafanaDashboard:
      enabled: true
      labels: {}
    slo:
      availabilityTarget: "99.5"   # default; a string, quote it
      window: 30d                  # default
    runbookBaseURL: https://wiki.example.com/forgejo/runbooks   # default: the operator's --runbook-base-url
```

### ServiceMonitor

The ServiceMonitor scrapes port `http` of Service `<name>-http` at path `/metrics`. It
authenticates with `authorization: {type: Bearer, credentials: <name>-secrets/metrics-token}`.

- **Labels:** prometheus-operator sets `job="<name>-http"`, `namespace`, `service`, `pod` and `instance`.
- **Token:** the core step generates it and renders it as `[metrics] TOKEN` in app.ini. Requests
  without it get `401`.
- **Exposure:** Forgejo serves `/metrics` on its public HTTP listener, so block `/metrics` at your
  ingress or proxy if the token alone is not enough for you.

### Grafana dashboard

The dashboard ConfigMap carries the label `grafana_dashboard: "1"` and the annotation
`grafana_folder: Forgejo`. Its data key is `forgejo-<namespace>-<name>.json`.

| | |
|---|---|
| Title | `Forgejo / <namespace>/<name>` |
| UID | `forgejo-<namespace>-<name>`, or a hash when that is longer than 40 characters |
| Hidden constants | `$namespace`, `$forgejo`, `$job` and `$slo_window`; edit these to reuse the JSON for another instance |
| Links | **Runbooks** (the [runbook index](runbooks/README.md) under the instance's runbook base); the SLO panels link the runbooks of their alerts |
| Template | `internal/observability/dashboard.json` |

| Row | Panels |
|-----|--------|
| Overview | Forgejo targets up, Ready, version, health check failures (1h), age of the last successful backup, data volume usage |
| Service level objectives | Availability over the SLO window (red below the objective), objective, error budget remaining, recovery point age; burn rate per alert window (thresholds 1, 3, 14.4), availability samples by result and planned, recovery point age against the RPO |
| Forgejo statistics | Users, repositories, organizations, issues, plus their trends (open/closed issues, comments, mirrors) |
| Operator | Health check failures, autopilot actions by type, volume usage by PVC, backups by reason/result, backup size and duration |
| Runtime | Forgejo RSS and heap, CPU, goroutines (per pod) |

The stats *Last successful backup* and *Recovery point age* turn orange at one and a half
intervals of `spec.backup.schedule` (a run was missed) and red above the
[RPO](#recovery-point-objective) (two intervals), where `ForgejoBackupTooOld` (or
`ForgejoBackupUploadTooOld`) fires: the operator renders the dashboard with the instance's RPO.

## Metrics

### Forgejo (`/metrics`, scraped through the ServiceMonitor)

Forgejo 16 keeps the `gitea_` prefix.

| Metric | Meaning |
|--------|---------|
| `gitea_build_info{version,goversion,...}` | Always 1; carries the running version |
| `gitea_users`, `gitea_organizations`, `gitea_repositories`, `gitea_teams` | Object counts |
| `gitea_issues`, `gitea_issues_open`, `gitea_issues_closed`, `gitea_comments`, `gitea_labels`, `gitea_milestones` | Issue counts |
| `gitea_mirrors`, `gitea_releases`, `gitea_stars`, `gitea_watches`, `gitea_follows`, `gitea_webhooks`, `gitea_hooktasks`, `gitea_publickeys`, `gitea_oauths`, `gitea_loginsources`, `gitea_projects`, `gitea_projects_boards`, `gitea_attachments`, `gitea_accesses`, `gitea_updatetasks` | Other counts |
| `go_*`, `process_*` | Go runtime and process metrics |

Forgejo (15 to 17) exports no pull-request count, so `status.stats.pullRequests` stays 0. The
high-cardinality `gitea_issues_by_label` and `gitea_issues_by_repository` are only exported when
you enable `[metrics] ENABLED_ISSUE_BY_LABEL` / `ENABLED_ISSUE_BY_REPOSITORY` through `spec.config`.

### Operator (manager `/metrics`)

Every series has the labels `namespace` and `name` (the `Forgejo` object). The operator removes
all of an instance's series when the instance is deleted, and a `ForgejoRunner`'s series when it
is deleted.

| Metric | Type | Extra labels | Meaning |
|--------|------|--------------|---------|
| `forgejo_operator_instance_ready` | gauge | | 1 when the Ready condition is True |
| `forgejo_operator_instance_info` | gauge | `image`, `version`, `phase` | Always 1 |
| `forgejo_operator_health_check_failures_total` | counter | | Failed `/api/healthz` probes made by the operator. Exported at 0 from the first reconcile |
| `forgejo_operator_health_check_unreachable_total` | counter | | Probes that never reached Forgejo (DNS, dial or connection errors, a proxy answering). Not health failures. Exported at 0 from the first reconcile |
| `forgejo_operator_availability_probes_total` | counter | `result` (up/down), `planned` (true/false) | The availability SLI: one sample per health-check interval of time, also while no pod is Ready (see [Service level objectives](#service-level-objectives)). All four series are exported at 0 from the first reconcile |
| `forgejo_operator_stats` | gauge | `kind` (users, organizations, repositories, issues) | Same values as `status.stats` |
| `forgejo_operator_backup_last_success_timestamp_seconds` | gauge | | Unix time of the last successful backup |
| `forgejo_operator_backup_retention_held` | gauge | | 1 while the size-drop guard holds retention (`BackupHealthy=False/BackupSizeDropped`), else 0 |
| `forgejo_operator_backups_total` | counter | `reason`, `result` | Finished backups |
| `forgejo_operator_backup_duration_seconds` | gauge | | Duration of the last finished backup |
| `forgejo_operator_backup_size_bytes` | gauge | `part` (database/data) | Size of the last successful backup |
| `forgejo_operator_backup_uploads_total` | counter | `reason`, `result` | Finished uploads of backups to `spec.backup.remote` ([backup-restore.md](backup-restore.md#remote-backups)) |
| `forgejo_operator_backup_last_upload_success_timestamp_seconds` | gauge | | Unix time of the last backup uploaded and verified; re-exported from `status.backup.remote.lastUploadTime` on every reconcile, never lowered |
| `forgejo_operator_restores_total` | counter | `result` | Finished restores |
| `forgejo_operator_upgrades_total` | counter | `result` (succeeded/failed/rolledback) | Finished upgrades |
| `forgejo_operator_database_upgrades_total` | counter | `result` (succeeded/failed/rolledback) | Finished PostgreSQL major upgrades of the CNPG cluster ([upgrades.md](upgrades.md#postgresql-major-upgrades)) |
| `forgejo_operator_autopilot_actions_total` | counter | `action` | Autonomous actions, by type |
| `forgejo_operator_volume_used_ratio` | gauge | `claim` | Used/capacity of each PVC |
| `forgejo_operator_version_supported` | gauge | `major`, `support` | 1 when the running Forgejo major is within the support policy, else 0; `support` is `Supported`, `Deprecated`, `EndOfLife`, `Future` or `Unknown` ([versions.md](versions.md)) |
| `forgejo_operator_version_end_of_life_timestamp_seconds` | gauge | `major` | Upstream end of life of the running major, when it is in the version table |
| `forgejo_operator_version_advisory` | gauge | `major` | 1 when the running patch release is older than the one an advisory of the version table recommends (critical security fixes, or a patch upstream advises against), else 0; absent while the version is unknown ([versions.md](versions.md#metrics-and-alert)) |
| `forgejo_operator_valkey_master_available` | gauge | | With HA: 1 while `ValkeyReady` is True (or Unknown with Valkey pods Ready), else 0 ([high-availability.md](high-availability.md#valkey-with-sentinel)) |
| `forgejo_operator_valkey_ready_members` | gauge | | With HA: Ready Valkey pods (3 when healthy) |
| `forgejo_operator_valkey_failovers_total` | counter | | Changes of the Valkey primary seen by the operator |
| `forgejo_operator_cron_runner_ready` | gauge | | With HA, while the cron runner should run: 1 when its pod is Ready ([high-availability.md](high-availability.md#the-cron-runner)) |
| `forgejo_operator_runners_desired` | gauge | `runner` | `spec.replicas` of a `ForgejoRunner` of the instance ([actions-runners.md](actions-runners.md#metrics-and-alert)) |
| `forgejo_operator_runners_registered` | gauge | `runner` | Its runners recorded as registered |
| `forgejo_operator_runners_online` | gauge | `runner` | Its runners Forgejo reports online; absent while the operator cannot read Forgejo's API |
| `forgejo_operator_runner_jobs_waiting` | gauge | `runner` | Waiting jobs in its scope that its labels could run; absent while the API cannot be read |

The endpoint also serves the standard controller-runtime metrics:

- `controller_runtime_reconcile_total`, `controller_runtime_reconcile_errors_total` and
  `controller_runtime_reconcile_time_seconds`, each with a `controller` label: `forgejo`,
  `forgejobackup`, `forgejorestore` or `forgejorunner`.
- `workqueue_*`
- `rest_client_*`
- Go runtime metrics.

## Alerts

### Per instance (`PrometheusRule <name>`)

All alerts carry the labels `severity`, `namespace` and `forgejo: <name>`, and the annotations
`summary`, `description` (with the `kubectl` commands to start investigating with) and
`runbook_url` (see [Runbooks](#runbooks)). The PrometheusRule has three groups:
`forgejo.<ns>.<name>` (the alerts below), `forgejo-slo.<ns>.<name>` (SLO recording rules and the
burn-rate alerts) and `forgejo-slo-window.<ns>.<name>` (recording rules over the whole SLO window,
evaluated every 3m).

Only `ForgejoDown` uses Forgejo's own metrics. All the other alerts read operator metrics, so
they work only when the operator is scraped with `honorLabels: true` (see
[Scraping the operator](#scraping-the-operator)). `ForgejoOperatorMetricsMissing` reports when
those metrics are missing.

| Alert | Severity | Expression (simplified) | For |
|-------|----------|-------------------------|-----|
| `ForgejoDown` | critical | `max(up{job="<name>-http"}) == 0` or the target is absent; suppressed while the phase is Restoring or Paused (a failed restore that holds the instance at zero is Degraded, so it alerts). Only rendered when the ServiceMonitor is enabled and replicas > 0 or HPA is enabled | 5m |
| `ForgejoOperatorMetricsMissing` | warning | `absent(forgejo_operator_instance_ready)` for the instance: the operator is not running, or not scraped (or scraped without `honorLabels`). All other alerts except `ForgejoDown` are inactive meanwhile | 15m |
| `ForgejoNotReady` | warning | `forgejo_operator_instance_ready == 0`; suppressed while Restoring or Paused (not while a failed restore holds the instance: phase Degraded). Only rendered when replicas > 0 or HPA is enabled, so an instance intentionally scaled to `spec.replicas: 0` does not alert | 10m |
| `ForgejoHealthCheckFailing` | warning | `increase(forgejo_operator_health_check_failures_total[10m]) > 3` | – |
| `ForgejoHealthProbeUnreachable` | warning | `increase(forgejo_operator_health_check_unreachable_total[W]) > 0`, where W is 5m or twice `healthCheckInterval`, whichever is longer: the operator's probes do not reach Forgejo. A NetworkPolicy or service mesh must let the operator connect to `<name>-http` on the HTTP port (3000 by default) | 15m |
| `ForgejoBackupFailed` | warning | A `result="failed"` backup in the last hour | – |
| `ForgejoBackupTooOld` | warning | RPO: `time() - backup_last_success_timestamp > RPO`. Only rendered when `spec.backup.enabled` | 5m |
| `ForgejoBackupMissing` | warning | No successful backup recorded at all, while the instance's operator metrics are present (`absent(last_success) and on() count(instance_ready) > 0`), so a scrape gap is not reported as a missing backup. Only rendered when `spec.backup.enabled` | RPO |
| `ForgejoBackupUploadFailed` | warning | A `result="failed"` upload to `spec.backup.remote` in the last hour. Only rendered with `spec.backup.remote` | – |
| `ForgejoBackupUploadTooOld` | warning | RPO of the off-cluster copy: `time() - backup_last_upload_success_timestamp > RPO`. Only rendered with `spec.backup.remote` and `spec.backup.enabled` | 5m |
| `ForgejoBackupUploadMissing` | warning | No upload recorded at all while the instance's operator metrics are present. Only rendered with `spec.backup.remote` and `spec.backup.enabled` | RPO |
| `ForgejoVolumeAlmostFull` | warning | `forgejo_operator_volume_used_ratio > 0.9` | 10m |
| `ForgejoUpgradeFailed` | warning | An upgrade with `result!="succeeded"` (failed or rolled back) in the last hour | – |
| `ForgejoDatabaseUpgradeFailed` | warning | A PostgreSQL major upgrade with `result!="succeeded"` (failed or rolled back) in the last hour. Only rendered with database type CNPG | – |
| `ForgejoVersionUnsupported` | warning | `forgejo_operator_version_supported == 0`: the running major is `Deprecated`, `EndOfLife`, `Future` or `Unknown`; the `VersionSupported` condition says what to upgrade to and by when | 1h |
| `ForgejoVersionAdvisory` | warning | `forgejo_operator_version_advisory == 1`: a supported major runs a patch release that an advisory recommends replacing (e.g. 15.0.7 without critical security fixes, or 17.0.0); the `VersionSupported` condition names the minimum patch | 1h |
| `ForgejoRunnersOffline` | warning | Per `ForgejoRunner` (label `runner`): `runner_jobs_waiting > 0 and runners_online == 0 and runners_desired > 0`: jobs wait and none of its runners is online ([actions-runners.md](actions-runners.md)) | 10m |
| `ForgejoValkeyNoMaster` | critical | With HA: `forgejo_operator_valkey_master_available == 0`, suppressed while Restoring or Paused: Valkey has no Ready primary (no Sentinel majority, or no replica to promote), so every server pod fails its health check | 2m |
| `ForgejoValkeyFailedOver` | warning | With HA: the Valkey primary moved to another pod in the last 15 minutes (`forgejo_operator_valkey_failovers_total` increased) | – |
| `ForgejoValkeyDegraded` | warning | With HA: fewer than 3 Valkey pods Ready, suppressed while Restoring or Paused | 15m |
| `ForgejoCronRunnerDown` | warning | With HA: `forgejo_operator_cron_runner_ready == 0`; the gauge exists only while the cron runner should run (not during upgrades or restores) | 15m |
| `ForgejoAutoPilotActive` | warning | At least 5 autopilot actions in the last hour | – |
| `ForgejoAvailabilityFastBurn` | critical | Availability SLO, page: burn rate above 14.4 over 1h and 5m, or above 6 over 6h and 30m (at a 30d window; labels `long`, `short`, `slo: availability`). See [Burn-rate alerts](#burn-rate-alerts) | – |
| `ForgejoAvailabilitySlowBurn` | warning | Availability SLO, ticket: burn rate above 3 over 1d and 2h, or above 1 over 3d and 6h (at a 30d window) | – |

`RPO` is the [recovery point objective](#recovery-point-objective): twice the longest gap between
two runs of `spec.backup.schedule`.

A counter series appears with value 1 on its first increment, and `increase()` cannot see that
first step. The backup and upgrade alerts therefore also fire when a failure series appeared
within the window. The two health-check counters do not need this, because they are exported at
0 from the first reconcile.

`forgejo_operator_backup_last_success_timestamp_seconds` and
`forgejo_operator_backup_last_upload_success_timestamp_seconds` are re-exported from
`status.backup` on every reconcile, so the RPO alerts keep working across operator restarts.

### Operator (`config/prometheus/rules.yaml`)

| Alert | Severity | Condition |
|-------|----------|-----------|
| `ForgejoOperatorDown` | critical | No `controller_runtime_reconcile_total{controller="forgejo"}` for 10m |
| `ForgejoOperatorReconcileErrors` | warning | More than 5 reconcile errors in 15m of the `forgejo`, `forgejobackup`, `forgejorestore` or `forgejorunner` controller, sustained for 15m |
| `ForgejoOperatorWorkQueueBacklog` | warning | Depth of one of these work queues above 20 for 15m |

Their `runbook_url` annotations link the default runbook base; patch them with kustomize if you
host the runbooks elsewhere.

### Runbooks

Every alert links `<base>/<AlertName>.md` in its `runbook_url` annotation. The runbooks are in
[docs/runbooks](runbooks/README.md): what the alert means, the impact, how to confirm it
(`kubectl` and PromQL), likely causes, remediation and when to escalate. The base is, in this
order, `spec.monitoring.runbookBaseURL`, the operator's `--runbook-base-url` (environment
variable `RUNBOOK_BASE_URL` in `config/manager/manager.yaml`), and
`https://github.com/lictl/forgejo-operator/blob/main/docs/runbooks`. The dashboard links
`<base>/README.md`.

A base is an `http(s)` URL with a host name or IPv4 address and an optional port, without user
information, query or fragment. Its path may hold only the characters RFC 3986 allows there;
percent-encode the others (`%20` for a space). Prometheus expands every annotation as a
template, so a `{{` in `runbook_url` would invalidate the whole PrometheusRule, with all the
instance's alerts, and a quote or backslash would break the dashboard's JSON. The API server
refuses any other `runbookBaseURL`, and the operator does not start with any other
`--runbook-base-url`.

### Rule tests

`make test-rules` renders the PrometheusRule of representative specs (default, daily backups
with a 99.9%/28d objective, hourly backups uploaded off-cluster with 99%/1d, HA) and the
operator's rules into `test/prometheus/rules/` (as deployed, checked with `promtool check rules`)
and `test/prometheus/unit-rules/` (without the long `description` annotations), then runs the
promtool unit tests `test/prometheus/*_test.yaml` with the official `prom/prometheus` image
(`PROMETHEUS_IMAGE`, through `CONTAINER_TOOL`, `docker` by default). The rendered files are
checked in; `go test ./internal/observability` fails when they are stale, when an alert lacks a
firing or a non-firing promtool case in a test file whose `rule_files` define it (promtool
passes a non-firing case of an alert it has not loaded), or when it has no runbook
(`TestEveryAlertHasARunbook`).

## Service level objectives

### Availability SLI

The health probe (see [`status.health`](#statushealth)) is suspended while no pod is Ready, so it
cannot measure outages by itself. The operator therefore takes an **availability sample** once
per `spec.autoPilot.selfHealing.healthCheckInterval` (30s by default) and counts it in
`forgejo_operator_availability_probes_total{namespace, name, result, planned}`:

| `result` | When |
|----------|------|
| `up` | The probe passed (`/api/healthz` 200 with `pass` or `warn`) |
| `down` | The probe failed, did not reach Forgejo (`Unreachable`), or no Forgejo pod is Ready while replicas are desired |

`planned="true"` marks samples taken while unavailability is expected; they count as available:

- a restore holds the instance in maintenance (`ScalingDown`, `Restoring`, `spec.restoreFrom`
  in progress) or brings it back up (`ScalingUp`);
- an orchestrated upgrade of Forgejo or PostgreSQL is in flight (`rc.Upgrading`: the cold
  pre-upgrade backup, the rollout or CloudNativePG's `pg_upgrade`, the verification, a rollback);
- `spec.paused` (no step runs; the sample is `up` while a pod is Ready, without a probe);
- zero desired replicas (`spec.replicas: 0` without an HPA);
- the instance has never been available (`status.health.lastHealthyTime` empty): the first
  installation.

Everything else is unplanned: a crash loop after a configuration change, a database outage,
a Recreate rollout of a single replica, a failed upgrade holding its image, pods scaled to zero
out of band, and a hold that only a person ends (`rc.MaintenanceHeld`, phase `Degraded`): a
failed restore that changed the instance (also the rollback of a failed upgrade) or a failed
`spec.restoreFrom` (`restoredFrom.phase: Failed`, e.g. `TargetNotEmpty`), even before the first
healthy probe. Pause and zero replicas stay planned during such a hold. Unreachable probes count as `down` because nothing shows that users can reach
Forgejo either; `ForgejoHealthProbeUnreachable` tells this case apart.

Samples are taken by the reconcile that probes (or would probe) Forgejo, and measure time, not
reconciles: such a reconcile counts one sample for every interval that has passed since the last
interval counted, all with the result it sees; normally that is one. A reconcile that comes late
counts the intervals it missed. While a step fails (`Degraded=ReconcileError`), the operator
backs the instance off for up to a minute, and probes that time out slow reconciles down, both
typically during an outage. An outage therefore weighs as much as the same time of normal
operation. One reconcile counts at most 3 minutes' worth of
samples (one sample when the interval is longer): the operator reconciles every instance at
least once a minute, so a longer gap means that it did not watch the instance, and the rest of
the gap is not counted. Restarting the operator resets the counters (`rate()` handles that);
while the operator does not run, no samples are taken (`ForgejoOperatorDown`).

### Availability objective

```yaml
spec:
  monitoring:
    slo:
      availabilityTarget: "99.5"   # percent of the window, < 100; the error budget is 100 - target
      window: 30d                  # hours, days or weeks: 12h, 7d, 28d, 4w
```

The SLI over a window `w` is the ratio of unplanned down samples to all samples, planned ones
included (they count as available):

```promql
forgejo_slo:availability_error_ratio{namespace="<ns>", name="<name>", window="<w>"}
  = sum(rate(forgejo_operator_availability_probes_total{..., planned="false", result="down"}[<w>]))
  / sum(rate(forgejo_operator_availability_probes_total{...}[<w>]))
```

Over the windows of the burn-rate alerts the denominator is at least one sample per
health-check interval, `clamp_min(..., 1 / <interval in seconds>)`: time in the window without
samples (before the instance existed, while the operator did not run) counts as available, so a
window that holds few samples is not dominated by one failed probe. The ratio over the SLO
window is not clamped: it covers the samples Prometheus holds.

Recording rules (labels `namespace`, `name`, and `window`):

| Rule | Meaning |
|------|---------|
| `forgejo_slo:availability_error_ratio` | The error ratio above, for every window of the burn-rate alerts (with the denominator clamped) and for the SLO window |
| `forgejo_slo:availability_burn_rate` | Error ratio / error budget, for every window of the burn-rate alerts. 1 spends the budget exactly over the SLO window |
| `forgejo_slo:availability_objective` | The target as a ratio (0.995), `window` = the SLO window |
| `forgejo_slo:availability_error_budget_remaining` | 1 - error ratio over the SLO window / budget: 1 = untouched, 0 = spent, negative = objective missed |
| `forgejo_slo:backup_recovery_point_age_seconds` | Age of the newest backup, `copy="local"`, and of the newest upload, `copy="remote"` (with `spec.backup.remote`). Only with `spec.backup.enabled` |
| `forgejo_slo:backup_recovery_point_objective_seconds` | The RPO. Only with `spec.backup.enabled` |

An SLO window longer than Prometheus's retention (kube-prometheus-stack keeps 10 days by
default) only covers the retained samples: availability and budget are then computed over the
retention.

The SLI is keyed by namespace and name, not by the object's UID: an instance deleted and created
again under the same name keeps the samples Prometheus still holds of its predecessor, so
recreating an instance does not reset its error budget.

### Burn-rate alerts

The multiwindow, multi-burn-rate alerts of the
[SRE workbook](https://sre.google/workbook/alerting-on-slos/). Each fires when the burn rate is
above its threshold over both a long window (the outage is significant) and a short one (it is
still going on, so the alert resolves soon after recovery):

| Alert | Severity | Long window | Short window | Burn rate | Budget spent when it fires |
|-------|----------|-------------|--------------|-----------|----------------------------|
| `ForgejoAvailabilityFastBurn` | critical (page) | 1h | 5m | 14.4 | 2% in 1h |
| `ForgejoAvailabilityFastBurn` | critical (page) | 6h | 30m | 6 | 5% in 6h |
| `ForgejoAvailabilitySlowBurn` | warning (ticket) | 1d | 2h | 3 | 10% in 1d |
| `ForgejoAvailabilitySlowBurn` | warning (ticket) | 3d | 6h | 1 | 10% in 3d |

The windows are those of a 30-day SLO window. For another window they scale by `window / 30d`,
rounded to minutes and never below **5m** (shorter windows hold too few probes and scrapes) nor
below **three health-check intervals**, rounded up to minutes (so that a window always holds
samples: with slow probes a shorter one would fall between two of them, and the alert, which has
no `for`, would resolve and fire again within one outage); the burn rates stay. The alert's labels
`long` and `short` name the effective pair that fired. Examples at the default interval (30s):

| `window` | Fast burn | Slow burn |
|----------|-----------|-----------|
| `30d` | 1h/5m, 6h/30m | 1d/2h, 3d/6h |
| `28d` | 56m/5m, 5h36m/28m | 22h24m/1h52m, 2d19h12m/5h36m |
| `7d` | 14m/5m, 1h24m/7m | 5h36m/28m, 16h48m/1h24m |
| `1d` | 5m/5m, 12m/5m | 48m/5m, 2h24m/12m |

With `healthCheckInterval: 10m` (shortest window 30m), `30d` gives 1h/30m, 6h/30m, 1d/2h, 3d/6h;
with `1h` (3h), 3h/3h, 6h/3h, 1d/3h, 3d/6h.

At the defaults (99.5%, probes every 30s), a full outage pages after about 5 minutes (4.3 min of
downtime in an hour is 2% of the 3.6h monthly budget, and the 5m window must hold one down
sample); a single failed probe never does, also not right after a restore or on a new instance,
and not when it counts the intervals of a late reconcile (at most 3 minutes, 5% of an hour).
Each sample stands for one interval of downtime, though: from an interval of about 4.5 minutes on
(at 99.5%), one failed probe is more than 7.2% of an hour and pages until the short window has
let go of it. A short window and a high target, for example
99.9% over `1d`, page within one or two minutes of an outage, which the end-to-end checks use.

### Recovery point objective

The RPO of an instance with scheduled backups is **two intervals of `spec.backup.schedule`**
(the longest gap between two runs): one missed or failed run is tolerated, the second one is
not.

| Schedule | RPO |
|----------|-----|
| `0 3 * * *` (the default) | 48h |
| `0 3 * * 1-5` (weekdays only) | 144h, because of the weekend gap |
| `0 * * * *` | 2h |
| `*/5 * * * *` | 10m |

`ForgejoBackupTooOld` fires when the last successful backup (of any reason, from
`forgejo_operator_backup_last_success_timestamp_seconds`) is older than the RPO for 5 minutes,
and `ForgejoBackupMissing` when there has been none for a whole RPO. With `spec.backup.remote`,
the off-cluster copy is the recovery point of a disaster recovery and has the same RPO:
`ForgejoBackupUploadTooOld` and `ForgejoBackupUploadMissing` watch
`forgejo_operator_backup_last_upload_success_timestamp_seconds`. Pausing an instance stops
scheduled backups but not these alerts: a paused instance can still change.

## Status

### `status.health`

The `health` step fills `status.health` by probing `GET /api/healthz` through
`http://<name>-http.<namespace>.svc.cluster.local:<port>`. The connection, including the DNS
lookup, must be made within 2s, and the response headers must arrive within 5s after that. See
[Network access from the operator](#network-access-from-the-operator).

| Field | Meaning |
|-------|---------|
| `healthy` | Last probe returned 200 with `status` pass or warn. False while probes are suspended or Forgejo is unreachable |
| `lastCheckTime` | Time of the last probe, unreachable ones included |
| `lastHealthyTime` | Time of the last successful probe |
| `consecutiveFailures` | Failed probes in a row against Ready pods. Reset by a success, an unreachable probe, or while probes are suspended |
| `message` | `all checks pass`, the failing checks (`status fail: database:ping=fail`), the connection error, or why probing is suspended |
| `checks` | Map from check name to status, e.g. `database:ping: pass`, `cache:ping: pass` |

Each probe has one of three outcomes:

| Outcome | When | Effect |
|---------|------|--------|
| Passed | HTTP 200 with `status` pass or warn | Healthy `True`, streak reset |
| Failed | Forgejo answered with anything else (424 `fail`, another status, a body that is not a health document), or accepted the connection and sent no response headers within 5s (a hung process) | Healthy `False` (`HealthCheckFailed`), `consecutiveFailures` and `forgejo_operator_health_check_failures_total` increase |
| Unreachable | No answer from Forgejo: DNS error, dial timeout, connection refused, or the connection reset or closed before a response (a NetworkPolicy or mesh mTLS shutting the operator out). An HTTP 401, 403, 502, 503 or 504 that is not a health document also counts here, because Forgejo always answers `/api/healthz` with one, so it came from a proxy in between. | Healthy `Unknown` (`Unreachable`), streak reset, not counted as a failure, `forgejo_operator_health_check_unreachable_total` increases, `HealthCheckUnreachable` event |

Only Failed probes feed self-healing. A hung Forgejo whose accept backlog is full may look like a
dial timeout and be reported as Unreachable. Its pod also fails its readiness probe, and
autopilot's pod-level hung-pod check covers it.

How probing behaves:

- **Rate:** at most once per `spec.autoPilot.selfHealing.healthCheckInterval` (default 30s). The
  operator requeues the instance accordingly.
- **Suspended:** while no pod of the instance is Ready and not terminating, or while a restore
  holds the instance in maintenance. The Healthy condition is then Unknown (`NoReadyReplicas` /
  `Maintenance`), the health counters do not move, and the message names the failing checks of
  the last probe. The availability SLI still takes its sample (down; planned during
  maintenance, unless a failed restore holds it), and the instance is requeued every interval
  meanwhile.
- **Why only Ready pods count:** `consecutiveFailures` feeds self-healing, which restarts pods
  that stay Ready but unhealthy, so a pod that is not serving traffic must not add to it.
- **Effect of the readiness probe:** the pod's readiness probe also uses `/api/healthz`. In an
  outage that affects every pod, such as the database being unreachable, the pods leave the
  Service after about 30s. The operator records the failures until then and then reports Unknown
  together with the last failing checks. The Ready condition, `ForgejoNotReady` and the kubelet's
  `Unhealthy` events cover the rest of the outage.

### `status.version`

The version comes from `GET /api/v1/version` after each successful probe, for example
`16.0.6+gitea-1.22.0`. It is also shown in the `VERSION` column of `kubectl get fj`.

### `status.stats`

`users`, `organizations`, `repositories`, `issues` and `lastUpdated` are sampled from
Forgejo's `/metrics` with the bearer token. The values are also exported as
`forgejo_operator_stats`.

When sampling happens:

- **Preconditions:** only after a successful probe, and only while monitoring is enabled.
- **Rate:** at most once per `spec.monitoring.statsInterval` (default 5m). The interval is counted
  from the later of `lastUpdated` and the last attempt, so failed attempts wait as well. Only the
  first failure in a row is retried sooner, after half the interval (at least 1m).
- **In the background:** Forgejo runs a `COUNT(*)` query per statistic before it answers, which
  can take seconds on a large instance. The operator fetches `/metrics` in the background, so a
  slow answer never holds up the reconciles of other instances. A later reconcile (within one
  `healthCheckInterval`) copies the result into `status.stats`.
- **Timeouts:** the connection must be made within 2s, the response headers must arrive within
  20s, and the whole request must finish within 27s.
- **Size:** the body is read as a stream and reading stops once all four statistics are found. If
  they are not found within the first 64 MiB, the attempt fails and no partial values are
  stored. The high-cardinality `ENABLED_ISSUE_BY_LABEL` and `ENABLED_ISSUE_BY_REPOSITORY`
  families come before `gitea_users` in the output.

A failed attempt is logged. If the retry fails too, a `StatsSamplingFailed` Warning event names
the error (for example `GET /metrics: HTTP 401`, or `timeout awaiting response headers`). The
event repeats at most once an hour while sampling keeps failing. `StatsSamplingRecovered` follows
once sampling works again. `status.stats` keeps the last good values in the meantime.

## Conditions

| Type | Set by | Meaning |
|------|--------|---------|
| `Ready` | core | Forgejo is serving with the desired spec |
| `Progressing` | core | A rollout, upgrade or restore is in flight |
| `Degraded` | core / orchestrator, feature packages | Something is wrong but the operator keeps trying (e.g. `ReconcileError` names the failing step, `MonitoringFailed` a monitoring object that could not be applied) |
| `DatabaseReady` | core | The PostgreSQL backend is reachable/healthy |
| `Upgrading` | upgrade | A version upgrade is being orchestrated |
| `DatabaseUpgrading` | upgrade | A PostgreSQL major upgrade of the CNPG cluster is being orchestrated (reason = phase); otherwise its outcome, a refusal or what it waits for ([upgrades.md](upgrades.md#postgresql-major-upgrades)). Only with database type CNPG |
| `BackupHealthy` | backup | State of scheduled backups (`BackupSizeDropped` while retention holds because the newest one is less than half the size of an earlier one, see [backup-restore.md](backup-restore.md#scheduled-backups-and-retention); `UploadFailed` when the latest one was not uploaded to `spec.backup.remote`; `BackupKeyNotExported`, `BackupKeyMissing`, `BackupKeyUnusable` while the backup key is not acknowledged as copied out of the cluster, missing or unusable, see [backup-restore.md](backup-restore.md#the-key)) |
| `RestoredFromBackup` | backup | Bootstrap of `spec.restoreFrom`: `False` while the identity, database and data volume are restored (`TargetNotEmpty` when the database or data volume predates the instance), `True`/`Restored` once they are restored and recorded on `<name>-secrets` ([backup-restore.md](backup-restore.md#disaster-recovery)) |
| `StorageHealthy` | autopilot | PVC capacity/usage |
| `ValkeyReady` | autopilot | With HA only: Valkey has a Ready primary its Sentinels agree on. `True/Available`, `True/Degraded` (fewer than two replicas in sync, three Sentinels or three Ready pods), `False/NoMaster`, `False/NotReady`, `False/NotProvisioned`, `Unknown/UnreachableFromOperator` ([high-availability.md](high-availability.md#valkey-with-sentinel)) |
| `Healthy` | observability | Result of the operator's `/api/healthz` probe. Reasons: `HealthCheckPassed`, `HealthCheckFailed` (message includes the streak), `Unreachable` (the operator cannot connect; status `Unknown`), `NoReadyReplicas`, `Maintenance` |

## Events

The observability steps emit these events on the `Forgejo` object:

| Reason | Type | When |
|--------|------|------|
| `HealthCheckFailed` | Warning | The probe outcome changes to failing (from passing, unreachable or never probed) |
| `HealthCheckUnreachable` | Warning | The probe outcome changes to unreachable. The message names the error and the network requirement |
| `HealthCheckRecovered` | Normal | The probe outcome changes from failing or unreachable to passing |
| `MonitoringFailed` | Warning | A ServiceMonitor, PrometheusRule or dashboard ConfigMap could not be applied (once per distinct error, then at most hourly) |
| `StatsSamplingFailed` | Warning | Sampling `status.stats` from `/metrics` failed twice or more in a row (then at most hourly) |
| `StatsSamplingRecovered` | Normal | Sampling works again after `StatsSamplingFailed` |
| `VersionDetected` | Normal | `/api/v1/version` reports a different version than `status.version` that it has not reported before for the current `status.image` (in this operator process), so pods on different patch releases of a floating tag do not repeat it on every probe |

Other packages emit their own events:

- Autopilot actions appear as events named after the action (`PodRestarted`,
  `ConfigRolledBack`, `ResourcesScaled`, `StorageExpanded`, ...) and are also listed in
  `status.autoPilot.recentActions`.
- `ReconcileError` (Warning) names a failing reconcile step.
- Backup, maintenance, HA and config events include `BackupScheduled`, `BackupPruned`,
  `BackupSizeDropped` (Warning: retention holds, see
  [backup-restore.md](backup-restore.md#scheduled-backups-and-retention)),
  `MaintenanceStarted`, `MaintenanceEnded`, `LastKnownGoodConfig`, `ConfigRetry`,
  `ValkeyFailedOver` (Warning), `WaitingForValkey`, `ValkeyReset`, `WaitingForCronRunner`,
  `CronRunnerStuck` (Warning) and `CronRunnerDeleted`.
- Remote backups and disaster recovery: `BackupKeyGenerated`, `BackupUploaded`,
  `BackupUploadFailed`, `RemoteBackupPruned`, `RemoteRetentionFailed`, `IdentityRestored`,
  `RestoreFromStarted`, `RestoredFromBackup`, `RestoreFromFailed` (see
  [backup-restore.md](backup-restore.md#remote-backups)).

## How to look at it

```sh
# Summary, conditions and health
kubectl get fj -A
kubectl describe fj <name> -n <ns>
kubectl get fj <name> -n <ns> -o jsonpath='{.status.health}{"\n"}{.status.stats}{"\n"}'
kubectl get fj <name> -n <ns> -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl events -n <ns> --for fj/<name>

# Generated monitoring objects
kubectl get servicemonitor,prometheusrule,configmap -n <ns> -l app.kubernetes.io/instance=<name>
```

`kubectl get forgejo-all -n <ns>` lists the instances, backups, restores and runners together
(the category of all four kinds).

Useful PromQL:

```promql
up{namespace="<ns>", job="<name>-http"}                                   # Forgejo targets
max(gitea_users{namespace="<ns>", job="<name>-http"})                    # users
forgejo_operator_instance_ready{namespace="<ns>", name="<name>"}
increase(forgejo_operator_health_check_failures_total{namespace="<ns>", name="<name>"}[1h])
time() - forgejo_operator_backup_last_success_timestamp_seconds{namespace="<ns>", name="<name>"}
ALERTS{forgejo="<name>"}
forgejo_slo:availability_burn_rate{namespace="<ns>", name="<name>"}
forgejo_slo:availability_error_budget_remaining{namespace="<ns>", name="<name>"}
sum by (result, planned) (increase(forgejo_operator_availability_probes_total{namespace="<ns>", name="<name>"}[1h]))
```

Grafana: open **Dashboards → Forgejo → Forgejo / &lt;ns&gt;/&lt;name&gt;**.
