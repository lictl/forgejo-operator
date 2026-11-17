# Changelog

Operator versions follow semantic versions; the Forgejo server version table
is maintained separately in `docs/versions.md`.

## Unreleased

## v0.1.0-rc.2

Second release candidate. It changes the `Forgejo` API and the Helm chart in
incompatible ways: read "Upgrading from v0.1.0-rc.1" before updating an existing
installation. This is a prerelease; validate the installation and operational
requirements before production use.

### Upgrading from v0.1.0-rc.1

1. Check the `Forgejo` objects first:
   - `spec.ingress.enabled: true` needs an explicit `spec.ingress.host` now, also
     when `server.domain`, `server.rootURL` or Gateway host names are set. Find the
     objects that lack it with
     `kubectl get forgejos.forgejo.lucas.icu -A -o json | jq -r '.items[] | select(.spec.ingress.enabled == true and ((.spec.ingress.host // "") == "")) | "\(.metadata.namespace)/\(.metadata.name)"'`
     and set the host before you apply the new CRDs. Left as they are, the API
     server refuses every edit of the spec (tested on Kubernetes 1.36 and 1.37), and
     the operator reports `Degraded` because its network step fails and stops
     reconciling the Ingress. Older Kubernetes versions were not tested; they may
     refuse the operator's status writes as well.
   - `spec.database.cnpg.instances` is removed, also the `instances: 1` of the rc.1
     sample: delete it from your manifests and GitOps repositories, or `kubectl apply`
     refuses them (strict field validation). Once the new CRDs are applied, a bundled
     database with more than one instance is scaled down to one. For a replicated
     PostgreSQL run your own; moving an existing instance to `spec.database.type:
     External` is described under
     ["Moving to another database or volume"](https://github.com/lictl/forgejo-operator/blob/v0.1.0-rc.2/docs/backup-restore.md#moving-to-another-database-or-volume).
   - Let running backups, restores and Forgejo or PostgreSQL upgrades finish, and
     do not start one meanwhile. Remote uploads of a backup that the old operator
     started, or that had not been uploaded yet, are refused (`IdentityUnavailable`):
     take a fresh backup afterwards. Completed local copies stay restorable.
2. Apply the CRDs of this release before upgrading the chart; Helm never upgrades
   them: `helm show crds oci://ghcr.io/lictl/charts/forgejo-operator --version 0.1.0-rc.2 | kubectl apply -f -`.
3. Upgrade the chart. The `certManager` value is rejected now: remove it from your
   values. Use `--reset-then-reuse-values` (Helm 3.14 or newer), not `--reuse-values`:
   that would carry the defaults of the old chart over, among them
   `certManager.enabled: false` and the controller image tag `v0.1.0-rc.1`. A
   `manager.image.tag` you set yourself overrides the new default (the chart's
   own version) as well: remove or update it.
4. Afterwards: every instance reports `RetainPolicyApplied=True` (with the default
   `retainPolicy: Retain`) and none is `Broken`; if one is, the condition's message and
   the [troubleshooting guide](https://github.com/lictl/forgejo-operator/blob/v0.1.0-rc.2/docs/troubleshooting.md#broken-instances) say what is missing. The chart's
   ServiceMonitor keeps the `namespace` and `name` labels of the operator metrics now
   (`honorLabels: true`, which the per-instance alerts need): queries on
   `exported_namespace` stop matching, and alerts that never matched start to.

### Breaking changes

- The operator reports condition `Broken` and changes nothing, instead of generating
  a new `SECRET_KEY` or creating an empty data volume or database for an instance
  that has run. `spec.database.cnpg.instances` is removed and the bundled cluster is
  scaled to one instance. The PostgreSQL upgrade phase `RollingBack` and the fields
  `restoreName`, `storageSize` and `rollbackRestoreFailed` of `status.database.upgrade` are
  gone (`holdsForgejo` replaces the last; those of `status.upgrade`, for Forgejo upgrades,
  stay), and `spec.upgrade.autoRollback` no longer applies to PostgreSQL upgrades. A CNPG cluster of an instance that has run is created
  again only with the annotation `forgejo.lucas.icu/recreate-database=true`.
- `spec.database.type` and `spec.storage.existingClaim` cannot change on an existing
  `Forgejo`. To move an instance, delete it with `retainPolicy: Retain` and create it
  again (restoring the database from a backup, or with the data copied to the new
  claim), or create a new instance from a remote backup with `spec.restoreFrom`.
- `spec.highAvailability.enabled` cannot change after creation (omitted means false).
  Create another instance for the other mode.
- `ingress.enabled` requires `spec.ingress.host`.
- The `certManager` Helm value and the cert-manager patches for the operator's
  metrics are removed. `Forgejo.spec.tls.certManager` is unchanged.
- Restores that used to run with a `NewerBackup` or `IdentityMismatch` warning event
  now fail before anything changes: set `spec.image` to the backup's version, or create
  a new `ForgejoRestore` with `spec.allowIdentityMismatch: true`. `spec.restoreFrom` of a
  backup without a data archive needs the annotation
  `forgejo.lucas.icu/restore-from-database-only=true`. An in-place restore of an
  instance that has run takes a cold pre-restore backup first, which needs room on
  the backup volume and keeps Forgejo down longer; `spec.preRestoreBackup: false`
  restores without it.
- Remote uploads need the source Forgejo UID and `SECRET_KEY` fingerprint that are
  recorded before the backup Job starts. A backup without them is not uploaded.

### Data safety

- `retainPolicy: Retain` is applied on every pass by its own step, also while the
  instance is `Broken` or paused, and reported as condition `RetainPolicyApplied`.
  Before, switching from `Delete` to `Retain` and deleting the `Forgejo` of an
  instance that could not run its other steps still let garbage collection delete
  its data volume, database and identity Secrets. Delete a `Forgejo` after such a
  switch only once the condition is `True`.
- An object that has the name of one of the instance's children, that nothing
  controls and that lacks the instance's labels (a Secret of yours called
  `<name>-config`) is no longer overwritten and later deleted as the operator's own:
  the step fails and names the object. Identity Secrets, the data volume and the
  CNPG cluster that a person put in place to recover an instance are still adopted.
- Queued backups obey the same holds as scheduled ones (`Broken`, Forgejo and
  PostgreSQL upgrades, restores), dump and upload Jobs are journaled in status before
  they are created so that an interrupted start can be tracked or cleaned up, and a
  Job create that the API server refuses is retried with a growing pause.
- Retention counts and prunes only backups of the instance's own identity and holds
  all completed scheduled backups while the newest is less than half the size of an
  earlier one (`BackupSizeDropped`, metric `forgejo_operator_backup_retention_held`);
  annotate the `Forgejo` with `forgejo.lucas.icu/backup-size-accepted=<backup>` when the
  drop is expected. Pre-restore backups are never pruned.
- Deletes of objects that were read from the cache name the UID that was read, and a
  truncated S3 listing is an error instead of a partial list that remote retention
  could take for an incomplete backup.

### Upgrades and restores

- A Forgejo upgrade is judged on the live Deployment first: every replica available on
  the new image is a success whatever the timeout or the restart counts say. Before, the
  first pass after the operator had been away past `spec.upgrade.timeout` could restore
  the pre-upgrade backup over a working upgrade and lose every write since.
- The operator never deletes or recreates the CNPG cluster of a failed PostgreSQL
  upgrade; the instance stays `Broken` until you act.

### Forgejo versions

- Forgejo 15.0.11, 16.0.7 and 17.0.1 (2026-10-26) are security releases. The version
  table recommends 15.0.11 for 15 and 17.0.1 for 17, so instances on older patch
  releases get the `VersionAdvisory` event and the `ForgejoVersionAdvisory` alert.
  Upgrading from 15.0.10 to 17.0.1 through the operator was verified on a real instance,
  including pushes after the refs were packed, which 17.0.0 fails
  ([forgejo#14866](https://codeberg.org/forgejo/forgejo/issues/14866)).
- Forgejo 16 reached its end of life on 2026-10-29: upgrade to 17.
- `doctor check --all` cannot run on an instance of the operator; the docs give the
  command that runs the other checks.

### Chart, RBAC and operations

- Alerts `ForgejoBroken` and `ForgejoBackupSizeDropped`, each with a runbook, and the
  `Forgejo` phase `Broken`.
- The manager role gains `delete` on PersistentVolumeClaims (the
  `OwnerReferencesPermissionEnforcement` admission plugin needs it to change owner
  references, no code deletes a claim) and `patch` on StatefulSets.
- StatefulSets are no longer updated on every pass under Kubernetes 1.37, where the API
  server defaults `maxUnavailable`.
- The controller image is built with Go 1.26.10; Go modules and Kubernetes libraries
  (`k8s.io/api` 0.37) are updated.

### Distribution

- Publishing a release is gated on successful CI of its exact commit on GitHub, for
  both registries: all five workflows, including the new end-to-end suite on Kubernetes
  1.30 and 1.37 that runs a Forgejo lifecycle (writes, backup and restore, a 15 to 17
  upgrade, rollback, retention, recovery from a retained archive) in an isolated Kind
  cluster. Alert rules are tested with promtool in CI, and Go modules are scanned for
  reachable vulnerabilities.
- Third-party license notices are no longer kept in the repository or shipped in the controller
  image; the project's own license still is.

## v0.1.0-rc.1

First public release candidate of forgejo-operator. This is a prerelease;
validate the installation and operational requirements before production use.

### Operator capabilities

- Manage a Forgejo installation with CloudNativePG databases, Valkey Sentinel,
  Gateway API routes, TLS, storage, backups, restores, and version-gated upgrades.
- Provide availability and recovery-point metrics, alert rules, dashboards,
  and runbooks, plus a declarative Forgejo Actions runner resource.

### Distribution

- Publish a multi-architecture controller image for linux/amd64 and linux/arm64
  and an OCI Helm Chart from independently built GitHub and Forgejo workflows.
- Include public installation, configuration, operations, troubleshooting,
  development, and contribution documentation. The operator's own code is MIT
  licensed, with required third-party notices retained.
