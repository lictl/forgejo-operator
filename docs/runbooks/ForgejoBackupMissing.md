# ForgejoBackupMissing

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `absent(forgejo_operator_backup_last_success_timestamp_seconds{namespace="<ns>", name="<name>"}) and on() count(forgejo_operator_instance_ready{...}) > 0` |
| For | the RPO (two schedule intervals) |
| Rendered | with `spec.backup.enabled` |

## Meaning

Scheduled backups are enabled, the operator's metrics for the instance are scraped, and for a
whole RPO no successful backup has been recorded at all (`status.backup.lastSuccessfulBackupTime`
is empty). Typical for a new instance whose first backups fail, or after every successful backup
was deleted.

## Impact

There is no recovery point. Losing the data volume or the database now loses everything since
the instance was created.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.backup}{"\n"}'
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")]}{"\n"}'
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N
```

```promql
forgejo_operator_backups_total{namespace="<ns>", name="<name>"}
```

## Likely causes

- Every backup so far failed ([ForgejoBackupFailed](ForgejoBackupFailed.md)).
- No backup was ever created: the first tick is the next schedule time after enabling, the
  instance was paused, or the backup-schedule step is blocked (`ReconcileBlocked`).
- All successful backups were deleted by hand.

## Mitigation

1. Take a manual backup now (see [ForgejoBackupFailed](ForgejoBackupFailed.md#mitigation)) and
   make sure it completes.
2. Fix the cause of failing or missing scheduled backups.

## Escalation

As for [ForgejoBackupTooOld](ForgejoBackupTooOld.md#escalation); treat an instance without any
backup holding important data as urgent.
