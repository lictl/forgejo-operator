# ForgejoUpgradeFailed

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `forgejo_operator_upgrades_total{namespace="<ns>", name="<name>", result!="succeeded"}` increased in the last hour (or its series appeared) |
| For | – (fires for an hour after each failure) |

## Meaning

An image upgrade (a change of `spec.image`) failed (`result="failed"`) or was rolled back
automatically (`result="rolledback"`) within the last hour. The failed image is recorded in
`status.upgrade.failedImage` and is not retried until `spec.image` changes or the retry
annotation is set.

## Impact

- **Rolled back**: the old image runs again on the restored pre-upgrade backup; changes made
  between the backup and the rollback are lost. `Degraded=True/UpgradeRolledBack`.
- **Failed before anything changed** (the pre-upgrade backup failed): the old image runs.
- **Failed without rollback** (`autoRollback: false` or no backup): the new image stays
  deployed with one replica, possibly not serving, and the workload is held (no HPA, no
  self-healing restarts).
- **Rollback failed** (`rollbackRestoreFailed: true`): Forgejo stays scaled to zero until a
  later restore has restored the database, and the data volume when its backup has one. Forgejo is down: the phase is `Degraded`, and `ForgejoDown`,
  `ForgejoNotReady` and the burn-rate alerts keep firing after this alert has ended.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.upgrade}{"\n"}'     # phase, failedImage, failureReason, rollbackRestoreFailed
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="Upgrading")]}{"\n"}'
kubectl -n $NS events --for fj/$N | grep -E 'Upgrade|Rollback|PreUpgrade'
kubectl -n $NS get fjb,fjr -l forgejo.lucas.icu/instance=$N
kubectl -n $NS get pods -l app.kubernetes.io/instance=$N
kubectl -n $NS logs <failed pod> -c bootstrap --previous        # migration errors
```

```promql
increase(forgejo_operator_upgrades_total{namespace="<ns>", name="<name>"}[1d])
```

## Likely causes

- The new image cannot be pulled (typo in the tag, registry credentials).
- `forgejo migrate` fails on the new major (incompatible `spec.config`, see `ConfigIncompatible`
  events and [versions.md](../versions.md)).
- The new version crashes at start, or does not become available within `spec.upgrade.timeout`.
- The pre-upgrade backup failed ([ForgejoBackupFailed](ForgejoBackupFailed.md)).

## Mitigation

1. Read `status.upgrade.failureReason` and the failed pod's logs; fix the cause (image, config).
2. Retry the same image: `kubectl -n $NS annotate fj $N forgejo.lucas.icu/retry-upgrade=true`,
   or set `spec.image` to a fixed patch release.
3. New image left deployed and you want the old one back: follow
   [upgrades.md, Rolling back manually](../upgrades.md#rolling-back-manually); do not just set
   `spec.image` back unless the new version cannot have migrated anything.
4. Failed rollback restore: run a `ForgejoRestore` of the pre-upgrade backup once its cause is
   fixed ([backup-restore.md](../backup-restore.md#restoring)).

## Escalation

Escalate immediately (page) when the rollback restore failed and Forgejo stays at zero
replicas. Escalate to the operator's maintainers with `status.upgrade`, the events and the
failed pod's `bootstrap` log when a supported upgrade path (see [versions.md](../versions.md))
fails without a configuration reason.
