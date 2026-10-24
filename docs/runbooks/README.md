# Runbooks

One runbook per alert the operator ships. Every alert links its runbook in the `runbook_url`
annotation, and the Grafana dashboard of each instance links this index.

- **Per-instance alerts** come from the PrometheusRule `<name>` that the operator generates for
  each `Forgejo` (`internal/observability`, see [observability.md](../observability.md#alerts)).
  They carry the labels `severity`, `namespace` and `forgejo: <name>`.
- **Operator alerts** come from `config/prometheus/rules.yaml` (deployed with `config/monitoring`
  or `config/prometheus`).

`severity: critical` pages (someone acts now), `severity: warning` is a ticket (fix it within
working hours). The two burn-rate alerts follow the same split.

## Index

| Alert | Severity | Source | Fires when |
|-------|----------|--------|------------|
| [ForgejoAvailabilityFastBurn](ForgejoAvailabilityFastBurn.md) | critical | instance (SLO) | The availability error budget burns 14.4x (1h/5m) or 6x (6h/30m) faster than sustainable |
| [ForgejoAvailabilitySlowBurn](ForgejoAvailabilitySlowBurn.md) | warning | instance (SLO) | The availability error budget burns 3x (1d/2h) or 1x (3d/6h) faster than sustainable |
| [ForgejoDown](ForgejoDown.md) | critical | instance | Prometheus scrapes no Forgejo pod for 5m |
| [ForgejoNotReady](ForgejoNotReady.md) | warning | instance | The instance has not been Ready for 10m |
| [ForgejoHealthCheckFailing](ForgejoHealthCheckFailing.md) | warning | instance | More than 3 operator probes of `/api/healthz` failed in 10m |
| [ForgejoHealthProbeUnreachable](ForgejoHealthProbeUnreachable.md) | warning | instance | The operator's probes have not reached Forgejo for 15m |
| [ForgejoOperatorMetricsMissing](ForgejoOperatorMetricsMissing.md) | warning | instance | The instance's operator metrics have been missing for 15m |
| [ForgejoBackupFailed](ForgejoBackupFailed.md) | warning | instance | A backup failed in the last hour |
| [ForgejoBackupTooOld](ForgejoBackupTooOld.md) | warning | instance (RPO) | The last successful backup is older than the RPO (two schedule intervals) |
| [ForgejoBackupMissing](ForgejoBackupMissing.md) | warning | instance (RPO) | No successful backup at all for a whole RPO |
| [ForgejoBackupUploadFailed](ForgejoBackupUploadFailed.md) | warning | instance | An upload to `spec.backup.remote` failed in the last hour |
| [ForgejoBackupUploadTooOld](ForgejoBackupUploadTooOld.md) | warning | instance (RPO) | The last off-cluster backup is older than the RPO |
| [ForgejoBackupUploadMissing](ForgejoBackupUploadMissing.md) | warning | instance (RPO) | No off-cluster backup at all for a whole RPO |
| [ForgejoVolumeAlmostFull](ForgejoVolumeAlmostFull.md) | warning | instance | A PVC of the instance is more than 90% full for 10m |
| [ForgejoUpgradeFailed](ForgejoUpgradeFailed.md) | warning | instance | An upgrade failed or was rolled back in the last hour |
| [ForgejoVersionUnsupported](ForgejoVersionUnsupported.md) | warning | instance | The running major is outside the support policy for 1h |
| [ForgejoVersionAdvisory](ForgejoVersionAdvisory.md) | warning | instance | The running patch release has an advisory for 1h |
| [ForgejoRunnersOffline](ForgejoRunnersOffline.md) | warning | instance | Jobs wait and none of a `ForgejoRunner`'s runners is online for 10m |
| [ForgejoValkeyNoMaster](ForgejoValkeyNoMaster.md) | critical | instance (HA) | Valkey has had no usable primary for 2m |
| [ForgejoValkeyFailedOver](ForgejoValkeyFailedOver.md) | warning | instance (HA) | The Valkey primary moved in the last 15m |
| [ForgejoValkeyDegraded](ForgejoValkeyDegraded.md) | warning | instance (HA) | Fewer than three Valkey pods Ready for 15m |
| [ForgejoCronRunnerDown](ForgejoCronRunnerDown.md) | warning | instance (HA) | The cron runner pod has not been Ready for 15m |
| [ForgejoAutoPilotActive](ForgejoAutoPilotActive.md) | warning | instance | At least 5 autopilot actions in the last hour |
| [ForgejoOperatorDown](ForgejoOperatorDown.md) | critical | operator | No metrics of the operator's controller for 10m |
| [ForgejoOperatorReconcileErrors](ForgejoOperatorReconcileErrors.md) | warning | operator | More than 5 reconcile errors in 15m, for 15m |
| [ForgejoOperatorWorkQueueBacklog](ForgejoOperatorWorkQueueBacklog.md) | warning | operator | A work queue holds more than 20 items for 15m |

`ForgejoDown`, `ForgejoNotReady` and the burn-rate alerts often fire together: the first two
say *that* Forgejo is down, the burn-rate alerts how much of the availability objective the
outage costs. Work from the runbook of the most specific alert (for example
`ForgejoValkeyNoMaster` or `ForgejoVolumeAlmostFull`) when one fires as well.

## Service level objectives

- **Availability.** `spec.monitoring.slo.availabilityTarget` (default `"99.5"`) percent of the
  operator's availability samples over `spec.monitoring.slo.window` (default `30d`) must be up.
  One sample per health-check interval: up when `/api/healthz` passes; down when it fails, does
  not reach Forgejo, or no pod is Ready while replicas are desired. Samples during planned
  downtime (restores, orchestrated upgrades, `spec.paused`, zero replicas, the first
  installation) count as available; a failed restore that holds the instance at zero does not.
  The burn-rate alerts are the multiwindow, multi-burn-rate alerts of the
  [SRE workbook](https://sre.google/workbook/alerting-on-slos/), with their windows scaled to the
  SLO window (never below 5m nor three health-check intervals).
- **Recovery point objective (RPO).** Two intervals of `spec.backup.schedule`: one missed or
  failed run is tolerated, the second is not. The local backups and, with `spec.backup.remote`,
  the off-cluster copies each have to meet it.

Details, metrics and recording rules: [observability.md](../observability.md#service-level-objectives).

## Conventions

The runbooks use these shell variables (take them from the alert's `namespace` and `forgejo`
labels) and run PromQL in the Prometheus UI (**Graph**) or with `curl`:

```sh
NS=<namespace>; N=<name>
PROM=http://<prometheus>:9090    # kube-prometheus-stack: kps-kube-prometheus-stack-prometheus.monitoring.svc:9090
q() { curl -s "$PROM/api/v1/query" --data-urlencode "query=$1" | jq -c '.data.result[] | [.metric, .value[1]]'; }
q "forgejo_operator_instance_ready{namespace=\"$NS\", name=\"$N\"}"
```

`kubectl get fj` (not `kubectl get forgejo`, which is ambiguous: `forgejo` is also the category of
all the operator's kinds). The operator runs as Deployment `forgejo-operator-controller-manager`
in namespace `forgejo-operator-system` with the default kustomization:

```sh
OPNS=forgejo-operator-system
kubectl -n $OPNS logs deploy/forgejo-operator-controller-manager --since=1h | grep "\"namespace\":\"$NS\""
```

## Where the runbooks live

The `runbook_url` of every alert is `<base>/<AlertName>.md`. The base is, in this order:

1. `spec.monitoring.runbookBaseURL` of the `Forgejo`,
2. the operator's `--runbook-base-url` flag, or its `RUNBOOK_BASE_URL` environment variable
   (`config/manager/manager.yaml`),
3. `https://github.com/lictl/forgejo-operator/blob/main/docs/runbooks` (this directory).

Copy this directory to your own wiki or repository to add site-specific steps (on-call
rotations, escalation contacts) and point the base there. The alerts in
`config/prometheus/rules.yaml` link the default base; patch them with kustomize if you move
the runbooks.

Every alert needs a runbook here: `TestEveryAlertHasARunbook` (`internal/observability`) fails
when an alert has no `runbook_url`, its file is missing, it is not in the index, or a runbook
has no Meaning, Impact, Diagnosis, Likely causes, Mitigation or Escalation section.
