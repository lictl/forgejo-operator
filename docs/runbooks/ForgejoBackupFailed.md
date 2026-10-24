# ForgejoBackupFailed

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `forgejo_operator_backups_total{namespace="<ns>", name="<name>", result="failed"}` increased in the last hour (or its series appeared) |
| For | – (fires for an hour after each failure) |

## Meaning

At least one `ForgejoBackup` of the instance (scheduled, manual or pre-upgrade) failed in the
last hour. The files of a failed backup are removed right away; the object stays.

## Impact

No new recovery point from this run. The previous backups are still there. A failed
pre-upgrade backup also fails the upgrade before anything changed (the old image keeps
running). Repeated failures lead to [ForgejoBackupTooOld](ForgejoBackupTooOld.md) (RPO missed).

## Diagnosis

```sh
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N --sort-by=.metadata.creationTimestamp
B=<failed backup>
kubectl -n $NS get fjb $B -o jsonpath='{.status.phase} {.status.message}{"\n"}'
kubectl -n $NS events --for fjb/$B
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")]}{"\n"}'
kubectl -n $NS get pvc $N-backups
```

The backup Job is deleted once its result is recorded; its message in `status.message` keeps
the error (pg_dump output, `No space left on device`, timeouts).

```promql
sum by (reason, result) (increase(forgejo_operator_backups_total{namespace="<ns>", name="<name>"}[1d]))
```

## Likely causes

- The backup PVC `<name>-backups` is full (retention too high, data grew).
- `pg_dump` cannot connect: database down, credentials rotated, connection limit.
- The data volume cannot be mounted by the backup Job (ReadWriteOnce on another node).
- The Job was evicted or timed out (slow storage, large data volume).

## Mitigation

1. Full backup volume: lower `spec.backup.retention`, delete old manual backups
   (`kubectl -n $NS delete fjb <old>`), or grow `spec.backup.storage.size` (the operator grows
   the PVC when the StorageClass allows it).
2. Database errors: fix the database first ([ForgejoHealthCheckFailing](ForgejoHealthCheckFailing.md)).
3. Take a manual backup once the cause is fixed, and confirm it completes:
   ```sh
   printf 'apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoBackup\nmetadata: {name: %s-manual-%s}\nspec: {forgejoRef: {name: %s}}\n' \
     $N $(date +%Y%m%d%H%M) $N | kubectl -n $NS apply -f -
   kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N -w
   ```

## Escalation

Escalate when backups keep failing for longer than one schedule interval (the RPO is two):
storage owners for capacity, database owners for `pg_dump` errors, the operator's maintainers
for Job errors that come from the operator (wrong image, missing mounts).
