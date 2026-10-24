# ForgejoBackupUploadMissing

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `absent(forgejo_operator_backup_last_upload_success_timestamp_seconds{namespace="<ns>", name="<name>"}) and on() count(forgejo_operator_instance_ready{...}) > 0` |
| For | the RPO (two schedule intervals) |
| Rendered | with `spec.backup.remote` and `spec.backup.enabled` |

## Meaning

`spec.backup.remote` is set, scheduled backups are on, the instance's operator metrics are
scraped, and for a whole RPO no backup has ever been uploaded (`status.backup.remote.lastUploadTime`
is empty).

## Impact

There is no off-cluster recovery point: losing the cluster or the namespace loses the instance.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.backup.remote}{"\n"}'
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N -o wide
kubectl -n $NS events --for fj/$N | grep -E 'Upload|BackupKey'
```

## Likely causes

- Every upload failed since `spec.backup.remote` was added ([ForgejoBackupUploadFailed](ForgejoBackupUploadFailed.md)).
- No local backup has completed since then ([ForgejoBackupMissing](ForgejoBackupMissing.md)).
- The backup key Secret is missing or unusable.

## Mitigation

Fix the uploads, then take a manual backup and confirm its `REMOTE` column turns `Uploaded`.

## Escalation

As for [ForgejoBackupUploadFailed](ForgejoBackupUploadFailed.md#escalation).
