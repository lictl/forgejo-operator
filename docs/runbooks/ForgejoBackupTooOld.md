# ForgejoBackupTooOld

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `time() - max(forgejo_operator_backup_last_success_timestamp_seconds{namespace="<ns>", name="<name>"}) > RPO` |
| For | 5m |
| Rendered | with `spec.backup.enabled` |

## Meaning

The newest successful backup of the instance (of any reason) is older than its **recovery point
objective (RPO)**: two intervals of `spec.backup.schedule` (the longest gap between two runs:
48h for the default `0 3 * * *`, 144h for weekdays only, 2h for hourly). One missed or failed
run is tolerated; this alert means the second one did not produce a backup either.

## Impact

If the instance's data is lost now, a restore loses more than the agreed RPO of changes
(repositories, issues, users). Nothing is down; this is a data-safety alert.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.backup}{"\n"}'     # lastSuccessfulBackupTime, nextScheduleTime
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")]}{"\n"}'
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N --sort-by=.metadata.creationTimestamp | tail
kubectl -n $NS get fj $N -o jsonpath='{.spec.paused} {.spec.backup.schedule}{"\n"}'
```

```promql
forgejo_slo:backup_recovery_point_age_seconds{namespace="<ns>", name="<name>"}
forgejo_slo:backup_recovery_point_objective_seconds{namespace="<ns>", name="<name>"}
```

`BackupHealthy`: `BackupFailed` (the latest scheduled backup failed), `BackupStale` (two runs
passed without success), `ReconcileBlocked` (the backup-schedule step does not run, the message
names why), `InvalidSchedule`.

## Likely causes

- Scheduled backups fail: see [ForgejoBackupFailed](ForgejoBackupFailed.md).
- No backups are created: the instance is paused (`spec.paused` stops scheduling), an upgrade
  or restore defers them for long, the operator was down, or a reconcile step blocks
  (`BackupHealthy=Unknown/ReconcileBlocked`).
- A backup has been `Running` for hours (slow storage, huge volume) and later ticks wait for it.

## Mitigation

1. Fix the failing backups or what blocks scheduling (unpause, finish or fail the stuck
   restore/upgrade).
2. Take a manual backup right away (see [ForgejoBackupFailed](ForgejoBackupFailed.md#mitigation));
   the alert resolves once it completes.
3. If the schedule is intentionally sparse, the RPO follows it; there is no separate threshold.

## Escalation

Escalate to the service owner when no backup can be taken within another schedule interval,
and to the operator's maintainers when backups are not scheduled although `BackupHealthy` and
the conditions show no reason.
