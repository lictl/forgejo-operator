# ForgejoBackupSizeDropped

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` (only with `spec.backup.enabled`) |
| Expression | `max(forgejo_operator_backup_retention_held{namespace="<ns>", name="<name>"}) > 0` |
| For | – |

## Meaning

The newest completed scheduled backup's database dump or data archive is less than half the size
of another backup that retention keeps (locally, or in `spec.backup.remote`), and that larger
backup has at least 16 MiB. A sudden drop like this is what an instance that silently lost data
looks like: its next backups would push every good backup out of retention. So retention prunes
no completed backup while it lasts (`BackupHealthy=False/BackupSizeDropped`). See
[backup-restore.md](../backup-restore.md#scheduled-backups-and-retention).

## Impact

Nothing is lost by the hold itself: every backup is kept. The backup volume (and the bucket)
keeps growing until the hold ends, so backups eventually fail for lack of space
(`ForgejoBackupFailed`) if nobody acts.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="BackupHealthy")].message}{"\n"}'
kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=$N \
  -o custom-columns=NAME:.metadata.name,REASON:.spec.reason,PHASE:.status.phase,DB:.status.databaseSizeBytes,DATA:.status.dataSizeBytes,DONE:.status.completionTime
kubectl -n $NS get fj $N -o jsonpath='{.status.backup.remote.message}{"\n"}'   # the remote finding, with spec.backup.remote
```

Then look at the instance itself: does Forgejo still list its repositories, users and issues, and
does the data volume still hold the repositories?

## Likely causes

- The database or the data volume was replaced by an empty or older one (a restore of the wrong
  backup, a volume swapped by hand, a database pointed elsewhere).
- Many repositories, users or packages were deleted on purpose.
- Large objects moved to object storage (`spec.objectStorage`), so the data archive shrank.
- Repository maintenance (`git gc`, LFS pruning) shrank a large data volume.

## Mitigation

1. If data was lost, keep the hold and restore from one of the larger backups (see
   [backup-restore.md](../backup-restore.md#restoring)); the restore takes a pre-restore backup of
   the current state first.
2. If the drop is expected, accept it with the backup the condition names:
   `kubectl -n $NS annotate fj $N forgejo.lucas.icu/backup-size-accepted=<newest backup>`.
   Backups completed before it no longer count for the comparison, and retention resumes.

## Escalation

Escalate to the owners of the instance before accepting a drop you cannot explain. Escalate to
the operator's maintainers when the sizes in `status.backup` do not match what the backup Jobs
wrote.
