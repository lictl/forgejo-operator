# ForgejoBroken

| | |
|---|---|
| Severity | critical |
| Source | PrometheusRule `<name>`, group `forgejo.<ns>.<name>` |
| Expression | `max(forgejo_operator_instance_info{namespace="<ns>", name="<name>", phase="Broken"}) > 0` |
| For | 2m |

## Meaning

The operator found the instance in a state it does not repair, set condition `Broken`, and stopped
changing the instance. It never generates a new `SECRET_KEY`, creates an empty data volume or an
empty database, or rolls a failed PostgreSQL upgrade back by itself for an instance that has run:
each of these would leave you with an instance that looks healthy but has lost data or can no
longer decrypt it. The condition's reason says what is wrong:

| Reason | What happened |
|--------|---------------|
| `SecretsMissing` | Secret `<name>-secrets`, or one of its keys `secret-key`, `internal-token`, `oauth2-jwt-secret`, `lfs-jwt-secret`, is gone. |
| `DataVolumeMissing` | The data PVC (`<name>-data`, or `spec.storage.existingClaim`) is gone. |
| `DatabaseMissing` | The CNPG cluster `<name>-db` is gone, or `spec.database.type` was switched to `CNPG` for an instance that ran on another database. |
| `IdentityMismatch` | The data PVC or the CNPG cluster was created for another `SECRET_KEY` (annotation `forgejo.lucas.icu/identity`): it, or `<name>-secrets`, is not this instance's. |
| `DatabaseUpgradeFailed` | A PostgreSQL major upgrade failed after `pg_upgrade` may have changed the data, Forgejo did not start on the new major, or the cluster disappeared during it. |
| `DatabaseRestorePending` | The annotation `forgejo.lucas.icu/recreate-database=true` had the CNPG cluster created again, empty; Forgejo stays stopped until a restore of the database into it. |

"Has run" means that `status.image` is set, or that the data PVC or the CNPG cluster carries the
annotation `forgejo.lucas.icu/running-image`. The operator writes both annotations, so they survive
a lost status and a `Forgejo` object that was deleted and created again.

## Impact

Nothing about the instance changes: no rollout, no config change, no upgrade, no scheduled backup,
no retention, no volume growth and no autopilot action. Pods that still run keep serving with what
they loaded; a pod that restarts may not start again (its Secret or database is missing). Routes,
health checks and metrics keep working, so other alerts (`ForgejoDown`, `ForgejoNotReady`) tell
whether users are affected. Retention is suspended on purpose: the backups from before the
incident are what you restore from.

## Diagnosis

```sh
kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="Broken")]}{"\n"}'
kubectl -n $NS get events --field-selector involvedObject.name=$N --sort-by=.lastTimestamp | tail
kubectl -n $NS get secret $N-secrets
kubectl -n $NS get pvc -o custom-columns=NAME:.metadata.name,IDENTITY:.metadata.annotations.forgejo\\.lucas\\.icu/identity,IMAGE:.metadata.annotations.forgejo\\.lucas\\.icu/running-image
kubectl -n $NS get clusters.postgresql.cnpg.io $N-db -o jsonpath='{.metadata.annotations}{"\n"}'
```

## Likely causes

- A GitOps tool pruned `<name>-secrets` or the PVC because it is not in the repository.
- Someone deleted the namespace's Secrets, PVCs or CNPG clusters while cleaning up, or CloudNativePG
  was reinstalled together with its clusters.
- The `Forgejo` object was created again (from Git, or restored by Velero without status) next to
  leftovers of another instance with the same name.
- `<name>-secrets` was recreated by hand or by a secret manager with new values.
- A PostgreSQL major upgrade whose `pg_upgrade` failed after it started, or on whose result Forgejo
  did not start.

## Mitigation

Restore what is missing; the operator clears `Broken` on its next pass and carries on.

- **`SecretsMissing`**: recreate Secret `<name>-secrets` with its original values. Look in your
  own copies first (a secret manager, a GitOps repository, a Velero backup). A remote backup keeps
  them in `identity.json` (see
  [Reading a remote backup by hand](../backup-restore.md#reading-a-remote-backup-by-hand)). Do
  not create a Secret with new values: the operator would reject it as `IdentityMismatch`, and
  Forgejo could no longer decrypt two-factor secrets, Actions secrets and mirror credentials.
- **`DataVolumeMissing`**: restore the PVC from your storage's snapshots or backups; a PVC you put
  in place without the annotation `forgejo.lucas.icu/identity` is adopted. To restore a backup
  instead, set `spec.replicas: 0`, create an empty PVC of that name, and once `Broken` is gone apply
  a `ForgejoRestore` of the data (and the database, if it is not current); then set `spec.replicas`
  back. Or create the instance again from a remote backup with `spec.restoreFrom`
  ([Disaster recovery](../backup-restore.md#disaster-recovery)), which restores identity,
  database and data together.
- **`DatabaseMissing`**: restore the cluster from CloudNativePG's own backups if you keep any.
  Otherwise annotate the instance with `forgejo.lucas.icu/recreate-database=true`: the operator
  creates the cluster again, empty, and keeps Forgejo stopped (`DatabaseRestorePending`) until you
  restore a backup of the database into it with a `ForgejoRestore` (`restoreData: false`); see
  [Rolling back a PostgreSQL upgrade](../upgrades.md#rolling-back-a-postgresql-upgrade) for the
  steps. `spec.restoreFrom` works as for a lost volume.
- **`DatabaseRestorePending`**: create the `ForgejoRestore` of the database; Forgejo starts and the
  annotation is removed when it reaches `ScalingUp`.
- **`IdentityMismatch`**: find out which half belongs to the instance. If the PVC or the cluster is
  a leftover of another instance with the same name, delete it, or move the instance to another
  name. If `<name>-secrets` was recreated with new values, restore the original Secret. If the PVC
  or the cluster is the right one after all (you restored its contents yourself), remove its
  annotation `forgejo.lucas.icu/identity`: it is adopted on the next pass.
- **`DatabaseUpgradeFailed`**: Forgejo stays stopped. `status.database.upgrade` names the
  pre-upgrade backup and the failure; [ForgejoDatabaseUpgradeFailed](ForgejoDatabaseUpgradeFailed.md)
  has the steps for retrying on the upgraded cluster or restoring the backup.

To give up on the old data and start over, delete the `Forgejo` object and its leftovers
(`<name>-secrets`, the data PVC, the CNPG cluster), then create the instance again.

## Escalation

Escalate to the cluster's owners when objects disappear without anyone deleting them (GitOps
pruning, a namespace cleanup job). Escalate to the operator's maintainers when `Broken` stays after
the objects are back with their original contents.
