# ForgejoDatabaseUpgradeFailed

| | |
|---|---|
| Severity | warning |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` (database type CNPG) |
| Expression | `forgejo_operator_database_upgrades_total{namespace="<ns>", name="<name>", result!="succeeded"}` increased in the last hour (or its series appeared) |
| For | – (fires for an hour after each failure) |

## Meaning

A PostgreSQL major upgrade of the instance's CNPG cluster (a newer major in
`spec.database.cnpg.imageName`) failed (`result="failed"`) or was reverted to the old major
(`result="rolledback"`) within the last hour. The image is recorded in
`status.database.upgrade.failedImage` and is not tried again until `spec.database.cnpg.imageName`
changes or the retry annotation is set. The operator never rolls a PostgreSQL upgrade back from a
backup. See [upgrades.md](../upgrades.md#failures-of-a-postgresql-upgrade).

## Impact

- **Reverted** (`RolledBack`): `pg_upgrade` failed, could not start, or did not start within
  `spec.upgrade.timeout`; CloudNativePG started the untouched old data again (same system ID).
  No data was lost; Forgejo was down for the attempt. `Degraded=True/PostgresUpgradeRolledBack`.
- **Aborted before the new image was written** (`Failed`): the old major keeps running; nothing
  changed.
- **Revert did not come back although `pg_upgrade` never started** (`Failed`, "pg_upgrade never
  started"): nothing was changed; Forgejo runs once the cluster serves again.
- **Held for a person** (`Failed`, `holdsForgejo: true`, condition `Broken=True` with reason
  `DatabaseUpgradeFailed`, which also fires [ForgejoBroken](ForgejoBroken.md)): the old data did
  not come back after `pg_upgrade` had started, Forgejo did not start on the new major, or the
  cluster disappeared. **Forgejo stays scaled to zero**; nothing was rolled back or deleted.
  Scheduled backups, retention, other upgrades and volume growth wait. `ForgejoDown`,
  `ForgejoNotReady` and the burn-rate alerts keep firing.

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
- CloudNativePG's upgrade Job does not get to run (its pod stays `Pending`: node capacity, a
  quota, a local volume on an unavailable node), or CloudNativePG does not create it.
- `pg_upgrade` refuses the old cluster: failed-upgrade directories left from an earlier attempt
  (`*.failed_*` next to `pgdata`), extensions or settings the new major does not accept.
- Forgejo does not become available on the new major within `spec.upgrade.timeout` (slow start
  without planner statistics, a crash loop, Valkey or the bucket unavailable at that moment).

## Mitigation

1. Read `status.database.upgrade.failureReason`, the events and the upgrade Job's log; fix the
   cause.
2. **Held, the cluster serves the new major** (a failed verification): once Forgejo can start,
   `kubectl -n $NS annotate fj $N forgejo.lucas.icu/retry-database-upgrade=true` starts it on the
   new major again and clears the failure (`DatabaseUpgradeRetry` event).
3. **Held, the cluster does not serve** (the old data did not come back, or the cluster is gone):
   check the cluster's pods first. To give up on it, follow
   [upgrades.md, Rolling back a PostgreSQL upgrade](../upgrades.md#rolling-back-a-postgresql-upgrade):
   annotate `forgejo.lucas.icu/recreate-database=true`, delete the Cluster, and restore the
   pre-upgrade backup (`status.database.upgrade.backupName`) with a database-only
   `ForgejoRestore` (`restoreData: false`). Forgejo starts when the restore reaches `ScalingUp`.
   Only a restore of the database counts: one with `restoreDatabase: false` leaves Forgejo
   stopped.
4. **Reverted or aborted**: retry the same image with the annotation of step 2, or set
   `spec.database.cnpg.imageName` to another image of the new major.

## Escalation

Escalate immediately (page) when `holdsForgejo` is true: Forgejo is down until a person acts. Escalate to the operator's maintainers with `status.database`, the events and the
upgrade Job's log when an upgrade between verified majors (17 and 18) fails without a reason in
the image or the cluster's settings.
