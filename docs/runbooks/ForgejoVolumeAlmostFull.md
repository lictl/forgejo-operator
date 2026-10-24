# ForgejoVolumeAlmostFull

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Labels | `claim` (the PVC) |
| Expression | `max by (namespace, name, claim) (forgejo_operator_volume_used_ratio{namespace="<ns>", name="<name>"}) > 0.9` |
| For | 10m |

## Meaning

A PVC of the instance, the data volume `<name>-data` or a volume of CNPG cluster `<name>-db`
(data or WAL), has been more than 90% full for 10 minutes. With
`spec.autoPilot.storageAutoExpand` (on by default, threshold 80%) the operator should already
have grown it, so this alert usually means that expansion does not work or has reached
`maxSize`.

## Impact

When the data volume fills up, pushes, uploads, attachments and repository operations fail.
When a CNPG volume fills up, PostgreSQL stops accepting writes or does not start at all, and
Forgejo goes down. Backups of a full data volume still work; the backup PVC has its own sizing.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.storage}{"\n"}'
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="StorageHealthy")]}{"\n"}'
kubectl -n $NS get pvc
kubectl -n $NS describe pvc <claim>                 # resize conditions, events
kubectl get storageclass $(kubectl -n $NS get pvc <claim> -o jsonpath='{.spec.storageClassName}') -o jsonpath='{.allowVolumeExpansion}{"\n"}'
kubectl -n $NS events --for fj/$N | grep -E 'StorageExpanded|Expansion|MaxSize'
```

```promql
forgejo_operator_volume_used_ratio{namespace="<ns>", name="<name>"}
```

`StorageHealthy` reasons: `ExpansionUnsupported` (StorageClass without `allowVolumeExpansion`),
`MaxSizeReached`, `ExpansionStalled` (a resize pending for 30m), `AutoExpandDisabled`,
`DatabaseClusterMissing`.

## Likely causes

- The StorageClass does not allow expansion, or the CSI driver fails to resize.
- `spec.autoPilot.storageAutoExpand.maxSize` (default 200Gi) is reached.
- Auto-expansion is off.
- Fast growth (a large push, LFS uploads, WAL piling up because CNPG cannot archive or a
  replica lags).

## Mitigation

1. Grow the volume: raise `spec.storage.size` (data) or `spec.database.cnpg.storage.size` /
   `walStorage.size` (CNPG), or raise `storageAutoExpand.maxSize`.
2. StorageClass without expansion: move to one that allows it (new PVC plus restore from a
   backup), or free space.
3. Free space in Forgejo: delete unused repositories, run the admin cron tasks (garbage
   collection of repositories, cleanup of archives and packages) from **Site administration →
   Maintenance**, or move LFS and attachments to S3 (`spec.objectStorage`).
4. WAL growth: check CNPG's `kubectl -n $NS get cluster.postgresql.cnpg.io $N-db -o yaml`
   status for archiving and replication errors.

## Escalation

Escalate to the storage owners for CSI resize failures or a StorageClass without expansion.
Escalate to the operator's maintainers when `StorageHealthy` says `Expanding` for long without
the PVC's request changing.
