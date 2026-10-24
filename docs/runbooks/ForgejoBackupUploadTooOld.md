# ForgejoBackupUploadTooOld

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `time() - max(forgejo_operator_backup_last_upload_success_timestamp_seconds{namespace="<ns>", name="<name>"}) > RPO` |
| For | 5m |
| Rendered | with `spec.backup.remote` and `spec.backup.enabled` |

## Meaning

The newest backup uploaded and verified off-cluster is older than the recovery point objective
(two intervals of `spec.backup.schedule`). The off-cluster copy is the recovery point of a
disaster recovery (`spec.restoreFrom` in a new namespace or cluster), so it has its own RPO
alert, next to the local [ForgejoBackupTooOld](ForgejoBackupTooOld.md).

## Impact

Losing the cluster (or the namespace with its PVCs) now loses more than the RPO of changes.
Local restores are not affected while the local backups are fresh.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.backup.remote}{"\n"}'    # lastUploadTime, message
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N -o wide
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")]}{"\n"}'
```

```promql
forgejo_slo:backup_recovery_point_age_seconds{namespace="<ns>", name="<name>", copy="remote"}
forgejo_slo:backup_recovery_point_objective_seconds{namespace="<ns>", name="<name>"}
```

## Likely causes

- Uploads fail ([ForgejoBackupUploadFailed](ForgejoBackupUploadFailed.md)).
- Local backups fail or are not taken ([ForgejoBackupTooOld](ForgejoBackupTooOld.md)): nothing
  to upload.
- The backup key is not acknowledged as exported (`BackupHealthy=False/BackupKeyNotExported`
  does not stop uploads, but `BackupKeyMissing`/`BackupKeyUnusable` do).

## Mitigation

1. Fix the uploads or the local backups.
2. Take a manual backup to create a fresh off-cluster copy once uploads work.

## Escalation

As for [ForgejoBackupUploadFailed](ForgejoBackupUploadFailed.md#escalation).
