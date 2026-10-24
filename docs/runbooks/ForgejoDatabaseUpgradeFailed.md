# ForgejoDatabaseUpgradeFailed

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` (database type CNPG) |
| Expression | `forgejo_operator_database_upgrades_total{namespace="<ns>", name="<name>", result!="succeeded"}` increased in the last hour (or its series appeared) |
| For | – (fires for an hour after each failure) |

## Meaning

A PostgreSQL major upgrade of the instance's CNPG cluster (a newer major in
`spec.database.cnpg.imageName`) failed (`result="failed"`) or was rolled back automatically
(`result="rolledback"`) within the last hour. The image is recorded in
`status.database.upgrade.failedImage` and is not tried again until `spec.database.cnpg.imageName`
changes or the retry annotation is set (after a failed rollback restore: only once a restore has
succeeded). See [upgrades.md](../upgrades.md#postgresql-major-upgrades).

## Impact

- **Reverted** (`RolledBack`, the message says "nothing had to be restored"): pg_upgrade failed
  or could not start; CloudNativePG started the untouched old data again (same system ID). No
  data was lost; Forgejo was down for the attempt.
- **Rolled back from the backup** (`RolledBack`, the message names a `ForgejoRestore`): the
  cluster was recreated on the old image and the cold pre-upgrade backup restored into it.
  Forgejo was stopped from the backup on, so nothing written by users is lost.
  `Degraded=True/PostgresUpgradeRolledBack`.
- **Aborted before the new image was written** (`Failed`): the old major keeps running; nothing
  changed.
- **Rollback failed** (`Failed`, `rollbackRestoreFailed: true`): the recreated database may be
  empty or half-restored, so **Forgejo stays scaled to zero** until a `ForgejoRestore` restores
  a backup. No upgrade starts meanwhile: another image and the retry annotation wait
  (`DatabaseUpgrading=False/WaitingForRestore`). `ForgejoDown`, `ForgejoNotReady` and the
  burn-rate alerts keep firing.
- **Not rolled back, cluster kept** (`Failed`, the message says "could not be rolled back" or
  "pg_upgrade never started"): the pre-upgrade backup could not restore the database (missing,
  being deleted, without a dump of the old major, or its volume gone), or a revert whose
  `pg_upgrade` never started did not bring the old data back in time. Nothing was deleted: the
  Cluster keeps its data (on the new major after a failed verification, otherwise the untouched
  old data), and Forgejo runs on it once it serves.
- **Failed without rollback** (`spec.upgrade.autoRollback: false`): the cluster stays on the new
  major (or on a revert that did not come up); Forgejo may not serve.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.database}{"\n"}'          # image, majorVersion, systemID, upgrade
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="DatabaseUpgrading")]}{"\n"}'
kubectl -n $NS events --for fj/$N | grep -E 'DatabaseUpgrade|Postgres'
kubectl -n $NS get cluster.postgresql.cnpg.io $N-db -o jsonpath='{.status.phase}{" "}{.status.pgDataImageInfo}{"\n"}'
kubectl -n $NS get jobs,pods -l cnpg.io/cluster=$N-db
kubectl -n $NS logs job/<primary>-major-upgrade --all-containers   # pg_upgrade output, while the Job exists
kubectl -n $NS get fjb,fjr -l forgejo.lucas.icu/instance=$N
```

```promql
increase(forgejo_operator_database_upgrades_total{namespace="<ns>", name="<name>"}[1d])
```

## Likely causes

- The new image cannot be pulled (a typo in the tag, registry access).
- The new image is built on another Debian release than the old one, under a tag that does not
  say so: pg_upgrade then fails at `pg_controldata` (the operator refuses tags that name another
  release, `PostgresUpgradePathUnsupported`).
- `pg_upgrade` refuses the old cluster: failed-upgrade directories left from an earlier attempt
  (`*.failed_*` next to `pgdata`), extensions or settings the new major does not accept.
- Forgejo does not become available on the new major within `spec.upgrade.timeout` (slow start,
  a crash loop): the cluster is rolled back from the backup.
- The restore of the rollback failed: see the `ForgejoRestore` named in `status.database.upgrade.restoreName`.
- The pre-upgrade backup was deleted, or its volume lost, before the rollback needed it: the
  Cluster was kept.

## Mitigation

1. Read `status.database.upgrade.failureReason` and the upgrade Job's log; fix the cause.
2. Except when `rollbackRestoreFailed` is true (step 3 first): retry the same image with
   `kubectl -n $NS annotate fj $N forgejo.lucas.icu/retry-database-upgrade=true`, or set
   `spec.database.cnpg.imageName` to another image of the new major.
3. Rollback restore failed: once its cause is fixed, restore the pre-upgrade backup
   (`status.database.upgrade.backupName`) with a database-only `ForgejoRestore`
   (`restoreData: false`), or another backup of the old major if that one is gone; Forgejo starts
   when it reaches `ScalingUp`. A retry or another image asked for meanwhile starts once that
   restore has completed.
4. Failed without rollback, or not rolled back with the cluster kept, and you want the old major
   back: follow
   [upgrades.md, Rolling back a PostgreSQL upgrade](../upgrades.md#rolling-back-a-postgresql-upgrade).

## Escalation

Escalate immediately (page) when `rollbackRestoreFailed` is true: Forgejo is down until a backup
is restored. Escalate to the operator's maintainers with `status.database`, the events and the
upgrade Job's log when an upgrade between verified majors (17 and 18) fails without a reason in
the image or the cluster's settings.
