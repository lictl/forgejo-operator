# ForgejoBackupUploadFailed

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `forgejo_operator_backup_uploads_total{namespace="<ns>", name="<name>", result="failed"}` increased in the last hour (or its series appeared) |
| For | – (fires for an hour after each failure) |
| Rendered | with `spec.backup.remote` |

## Meaning

An upload of a completed backup to `spec.backup.remote` (S3, encrypted with the instance's
backup key) failed in the last hour. The backup itself is fine and stays on the `<name>-backups`
PVC; only the off-cluster copy is missing.

## Impact

The off-cluster copy, which a disaster recovery (losing the namespace or the cluster) depends
on, is older than it should be. Repeated failures lead to
[ForgejoBackupUploadTooOld](ForgejoBackupUploadTooOld.md).

## Diagnosis

```sh
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N -o wide     # REMOTE column
B=<backup>
kubectl -n $NS get fjb $B -o jsonpath='{.status.remote.phase} {.status.remote.message}{"\n"}'
kubectl -n $NS events --for fj/$N | grep -E 'Upload|BackupKey'
kubectl -n $NS get fj $N -o jsonpath='{.status.backup.remote}{"\n"}'
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")]}{"\n"}'
```

## Likely causes

- S3 credentials (`spec.backup.remote.s3.credentialsSecret`) wrong, expired or without write
  permission on the bucket/prefix.
- The bucket endpoint is unreachable from the instance's namespace (egress NetworkPolicy, proxy,
  DNS, a private CA not configured).
- The backup key Secret is missing or unusable (`BackupKeyMissing`, `BackupKeyUnusable`).
- Quota or object-lock rules on the bucket.

## Mitigation

1. Fix credentials, network or key as the message says.
2. Uploads are not retried for an old backup; the next scheduled backup is uploaded. To get an
   off-cluster copy now, take a manual backup (see
   [ForgejoBackupFailed](ForgejoBackupFailed.md#mitigation)); it is uploaded when it completes.

## Escalation

Escalate to the storage/S3 owners for bucket permissions and reachability. Escalate to the
operator's maintainers when the upload Job's error comes from the operator (verification of the
uploaded files fails although the bucket works), with the backup's `status.remote`.
