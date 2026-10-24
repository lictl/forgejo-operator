# High availability

`spec.highAvailability.enabled: true` lets an instance run several Forgejo pods behind its
Services, scale them by hand or with an HPA, and roll them without downtime. Forgejo itself
was written for one process; this page says what the operator adds so that several processes
are safe, what HA covers and what it does not. How it is built is in [design.md](design.md);
scaling, the HPA and the PDB in [autopilot.md](autopilot.md#horizontal-scaling-and-disruption-budget).

```yaml
spec:
  replicas: 3                 # or autoscaling.horizontal
  highAvailability:
    enabled: true
  storage:
    accessModes: [ReadWriteMany]
  database:
    cnpg: {instances: 3}
  objectStorage: {...}        # recommended, see docs/object-storage.md
```

## What HA means here

| Covered | How |
|---------|-----|
| A server pod fails, is evicted or deleted | The other pods keep serving; rolling updates replace pods one at a time with a surge pod (`maxUnavailable: 0`); PDB `<name>` keeps one pod through drains. A terminating pod keeps serving for 10 s (preStop) while its endpoint is removed, then lets requests and git operations finish for up to 50 s (`GRACEFUL_HAMMER_TIME`). |
| Several pods and Forgejo's cron tasks | At most one pod, the **cron runner** (StatefulSet `<name>-cron`), runs them; see [below](#the-cron-runner). |
| Shared cache and queues | Valkey with Sentinel (StatefulSet `<name>-valkey`, three pods); the primary fails over to a replica in seconds; see [below](#valkey-with-sentinel). |
| Sessions and logins | Kept in PostgreSQL (`[session] PROVIDER=db`): a Valkey failover logs nobody out, and turning HA on or off keeps every session. |
| Spreading | Server and Valkey pods (and the cron runner, on a `ReadWriteMany` volume) prefer different nodes, and different zones where the nodes carry `topology.kubernetes.io/zone` (`ScheduleAnyway`, so one node still works); see [Spreading](#spreading). |

What it does **not** cover:

- **The data volume.** Git repositories live on one volume that every pod mounts, even at one
  replica: the cron runner and, during rolling updates, a surge pod mount it too. On a cluster
  with more than one node it must be `ReadWriteMany` (`spec.storage.accessModes`); the operator
  reports `Degraded=HARequiresSharedStorage` otherwise. A `ReadWriteOnce` volume works only on a
  single node (all pods share that node's mount); on it the cron runner is placed next to a
  server pod ([Spreading](#spreading)). The volume, and the storage behind it, stays a
  single point of failure unless the storage itself is replicated.
- **The database.** PostgreSQL is CloudNativePG's job: use `spec.database.cnpg.instances` of 2
  or more for failover. `CONN_MAX_LIFETIME` (auto-tuning) moves the pools to a new primary.
- **Upgrades and restores.** `forgejo migrate` takes no lock, so an upgrade still stops every
  pod for the cold pre-upgrade backup and migrates in a single pod, and a restore scales the
  instance to zero ([upgrades.md](upgrades.md), [backup-restore.md](backup-restore.md)).
- **Valkey durability.** Replication is asynchronous: a failover can lose the last second of
  writes to the cache and the queues (for example a webhook delivery queued at that moment).
  Valkey keeps its data on an emptyDir: a pod that is recreated resynchronises from the
  primary, but if every Valkey pod is recreated at once (all on one node that reboots), queued
  work is lost and the cache starts empty.
- **Several clusters or regions.** One instance lives in one namespace of one cluster; see
  [disaster recovery](backup-restore.md#disaster-recovery) for another cluster.

### Requirements

- `metadata.name` of at most 45 characters (the pods of StatefulSet `<name>-valkey` carry the
  label `controller-revision-hash=<name>-valkey-<hash>`); the API server refuses longer ones. It
  also refuses, HA or not, Forgejo names ending in `-valkey` (PDB `<name>` would be the Valkey PDB
  of instance `<name without -valkey>`) and `ForgejoRunner` names ending in `-valkey` or `-cron`
  (their StatefulSet would be an instance's Valkey or cron runner).
- A data volume every pod can mount (above), and preferably `spec.objectStorage`, so that LFS
  objects, attachments, packages and Actions artifacts are not on the shared volume.
- Three nodes for the Valkey pods to survive the loss of a node (each pod holds one of the three
  Sentinels; two must agree).
- The operator must reach the Valkey pods on port 26379 to report roles and failovers; without
  that, `ValkeyReady` is `Unknown/UnreachableFromOperator` and nothing else changes (see
  [design.md](design.md#network-requirements)).

## Objects

| Object | Content |
|--------|---------|
| Deployment `<name>` | The server pods (`app.kubernetes.io/component: server`), behind Services `<name>-http` and `<name>-ssh`. They mount `app-web.ini` of `<name>-config` as `/etc/forgejo/app.ini`: every cron task disabled. RollingUpdate (surge 1, unavailable 0) except during migrations |
| StatefulSet `<name>-cron` | The cron runner: one pod, `<name>-cron-0` (`component: cron`), with the same template (image, volumes, resources, config hash, restarts) and the full `app.ini`; `Parallel` pod management (a template change replaces a pod that is not Ready too), not behind any Service, no PDB |
| Secret `<name>-config` | `app.ini` (cron tasks as configured) and `app-web.ini` (the same without cron tasks); one hash covers both, so a change rolls both Deployments, and the last known good copy holds both |
| StatefulSet `<name>-valkey` | Three pods, each with a `valkey` and a `sentinel` container and the init container `config` (official image `spec.highAvailability.valkey.image`) |
| Service `<name>-valkey` | Headless, publishes not-ready pods: the pods' DNS names `<name>-valkey-<i>.<name>-valkey.<ns>.svc` |
| PDB `<name>-valkey` | `maxUnavailable: 1` |
| PDB `<name>`, HPA `<name>` | As without the cron runner ([autopilot.md](autopilot.md#horizontal-scaling-and-disruption-budget)) |

## The cron runner

Forgejo schedules its cron tasks in every process, without leader election or a lock (Forgejo
15 and 17; Gitea's `globallock` was never ported). Several of them break when two pods run them
at once: `start_schedule_tasks` creates every scheduled Actions workflow run twice,
`update_mirrors` fetches the same mirror twice (ref lock failures), `sync_external_users` races
on creating users, and garbage collection and health checks double the I/O.

So, whenever HA is enabled (also at one replica, since `kubectl scale` or the HPA can add pods
at any time without a new app.ini):

- **Server pods run no cron tasks.** `app-web.ini` sets `[cron] ENABLED=false` and
  `RUN_AT_START=false`, which Forgejo's go-ini parent-section fallback applies to every task, and
  forces `ENABLED=false` and `RUN_AT_START=false` into every `[cron.<task>]` section that sets
  them itself (`update_checker`, sections of `spec.config`). The tasks stay registered: the admin
  panel lists them and **Run** still runs one, in the pod that served the request.
- **The cron runner runs them**, with `spec.config` as given. It is the one pod of StatefulSet
  `<name>-cron`, and a StatefulSet creates a pod only once the previous one is gone from the API:
  after its kubelet confirmed that the containers stopped, or after a force delete. (A
  Deployment's ReplicaSet replaces a pod as soon as its deletion starts, so a pod on an
  unreachable node, still running and connected to the database, and its replacement would both
  run the tasks.) A run missed while it is down catches up: schedules and mirrors use "next run ≤
  now". Only a StatefulSet `<name>-cron` that the instance controls is its cron runner.
- **When its node is lost**, the pod is deleted after the node's taint tolerations (300 s by
  default) but stays `Terminating` until the node answers again. Meanwhile no cron task runs,
  `cronRunnerReady` is false (`ForgejoCronRunnerDown` after 15 min), and a Warning
  `CronRunnerStuck`, two minutes after the pod's deletion deadline and then hourly, names the pod
  and its node. Once the node is confirmed down, or its containers stopped, let the replacement
  start: `kubectl -n <ns> delete pod <name>-cron-0 --force --grace-period=0` (deleting the Node
  object, or the `node.kubernetes.io/out-of-service` taint of a non-graceful node shutdown, does
  the same). Pausing is the safe failure: a runner that is only cut off from the control plane
  may still reach PostgreSQL. The same applies to `kubectl delete pod` and the autopilot's
  deletion of a hung runner: the replacement starts once the old pod is gone.
- **It stops with the server pods.** `forgejo migrate` must run in one pod alone, so the runner is
  at zero replicas whenever the server pods do not run freely: maintenance (restores), an upgrade
  (`rc.Upgrading`, including the cold pre-upgrade backup and the rollback), a failed upgrade's held
  image, a restored database waiting for its first pod (migration hold), the first install until
  the first rollout is verified, and `spec.replicas: 0`. Before a new image is rolled out, the
  workload step scales the runner to zero and waits until its pod has stopped (event
  `WaitingForCronRunner`; a pod stuck on a lost node keeps it waiting, see above). Restores and
  cold backups count its pod (and a runner StatefulSet that still wants one) among the pods that
  must have stopped.
- **Statistics.** `GET /api/v1/admin/cron` reports run counts and last runs per process, from
  whichever pod the Service picks; the cron runner's are the real ones (ask its pod IP, as the
  [checklist](../test/e2e/checklists/high-availability.md) does).

`status.highAvailability.cronRunner` and `cronRunnerReady` name the runner pod;
`forgejo_operator_cron_runner_ready` is exported while it should run.

Without HA nothing changes: the single pod runs the cron tasks with `app.ini`, and there is no
`<name>-cron`. Turning HA off deletes the cron runner (event `CronRunnerDeleted`).

## Valkey with Sentinel

Forgejo keeps its cache (database 0) and its queues (database 2) in Valkey; every server pod
consumes every queue (`[queue] TYPE=redis`), so no queue needs a single consumer. A single Valkey
would be a single point of failure for the whole instance: `/api/healthz` pings the cache, so
about 30 s of Valkey outage turns every server pod NotReady.

**Topology.** Three pods, each a Valkey server and a Sentinel (official image; `valkey-sentinel`
is part of it). One server is the primary, two replicate from it (`replica-announce-ip` is the
pod's DNS name). The Sentinels monitor the group `forgejo` with quorum 2,
`down-after-milliseconds 5000` and `failover-timeout 60000`, resolve and announce host names, and
require the password. Forgejo connects through them:

```
redis+sentinel://:<password>@<name>-valkey-0.<name>-valkey.<ns>.svc.cluster.local:26379,...-1...,...-2.../<db>?mastername=forgejo&sentinelpassword=<password>
```

and follows a new primary as soon as the Sentinels announce it (`+switch-master`).

**Start.** The init container `config` asks the other pods' Sentinels for the primary and starts
the pod as its replica. When no Sentinel names one (a new group, or every Sentinel down), pod 0
starts as the primary and the others as its replicas. A pod that the Sentinels still name as the
primary, but that starts empty (it was recreated before they noticed), first has them promote a
replica (`SENTINEL FAILOVER`), so that the replicas never resynchronise from an empty primary;
only when no replica can be promoted (within 60 s) does it start as an empty primary. A Sentinel
also corrects a pod that starts with the wrong role.

**Failover.**

| Event | What happens | Measured on one node |
|-------|--------------|----------------------|
| The primary's pod is deleted or evicted (drain, rolling update) | Its preStop hook asks the other Sentinels for a failover and waits (up to 20 s) until they name another primary | about 2 s |
| The primary crashes or its node is lost | The Sentinels see it down after 5 s and elect a new primary; if its pod is recreated first, the init container forces the failover | 2-10 s |
| A replica or Sentinel fails | Nothing for Forgejo; the pod rejoins as a replica | – |
| Two pods fail | No Sentinel majority: no failover. A surviving primary keeps serving; otherwise `ValkeyReady=False/NoMaster` and Forgejo's health check fails until a majority is back | – |

Writes acknowledged by the old primary in its last second may be lost (asynchronous
replication; `min-replicas-to-write 0` favours availability). Forgejo's operations fail for the
few seconds of a failover (its queue retries most of them); its readiness probes tolerate 30 s.

**Data.** An append-only file (`appendfsync everysec`) on an emptyDir: a restarted container
keeps its data, a recreated pod resynchronises from the primary. `maxmemory` is 80% of the
`valkey` container's memory limit with `volatile-lru`, so only keys with a TTL (cache) are
evicted, never queued work.

**Password.** Generated once into `<name>-secrets` (`valkey-password`). The pods read it from
`secretKeyRef` environment variables and the init container writes it into the configuration
files on a memory-backed emptyDir; probes and hooks authenticate through `REDISCLI_AUTH`. It
never appears in arguments, logs, events or status.

**Readiness.** A `valkey` container is Ready when it is the primary or a replica whose link to
the primary is up, a `sentinel` container when it answers. The StatefulSet rolls one pod at a
time (highest ordinal first) and waits for each to be Ready, so a replica has resynchronised
before the next pod goes; the primary hands over in its preStop hook. The `high-availability`
step rolls Forgejo onto Valkey only while at least one Valkey pod is Ready.

**Status.** The `valkey-status` step asks each pod's Sentinel (port 26379, from the operator) for
the primary and its replicas and reports:

- `status.highAvailability.valkey`: `master`, `members` (each pod's role `master`, `replica` or
  `unknown`, and readiness), `sentinels` (how many answered), `lastFailoverTime`, `message`;
- condition `ValkeyReady`: `True/Available` (a Ready primary, two replicas in sync, three
  Sentinels), `True/Degraded` (usable, but one more failure would leave no replica or no
  majority), `False/NoMaster`, `False/NotReady`, `False/NotProvisioned`,
  `Unknown/UnreachableFromOperator` (pods Ready, Sentinels not reachable from the operator);
- event `ValkeyFailedOver` (Warning) when the primary moved to another pod;
- metrics `forgejo_operator_valkey_master_available`, `forgejo_operator_valkey_ready_members` and
  `forgejo_operator_valkey_failovers_total`, and alerts `ForgejoValkeyNoMaster` (critical, 2m),
  `ForgejoValkeyFailedOver`, `ForgejoValkeyDegraded` (15m) and `ForgejoCronRunnerDown` (15m)
  ([observability.md](observability.md#alerts)).

Rejected alternatives: a single Valkey with fast restarts (still a single point of failure on
node loss), Valkey cluster mode (six nodes, and no separate databases), Valkey operators (an
extra dependency; some use Bitnami images), and promotion by the operator (it would need
`pods/exec` and has no quorum).

## Spreading

Without `spec.podTemplate.topologySpreadConstraints`, HA pods get:

| Pods | Constraint (each `maxSkew: 1`, `whenUnsatisfiable: ScheduleAnyway`) |
|------|------------------------------------------------------------|
| Server | `kubernetes.io/hostname` over the server pods, plus `topology.kubernetes.io/zone` when a node carries that label |
| Cron runner | On a `ReadWriteMany` data volume: the same keys over the server and cron runner pods, so the runner prefers a node with fewer Forgejo pods. On any other volume: none; instead a required `podAffinity` to the server pods on `kubernetes.io/hostname`, because only pods on the node the volume is attached to can mount it (a pod elsewhere would wait on `Multi-Attach`, or take the attachment from the server pods after a restart) |
| Valkey | The same keys over the Valkey pods; replaced by `spec.highAvailability.valkey.topologySpreadConstraints` |

`spec.podTemplate.topologySpreadConstraints`, when set, replace the defaults of the server pods
and the cron runner (select them with `app.kubernetes.io/instance=<name>` and
`app.kubernetes.io/component=server` or `cron`); the cron runner's `podAffinity` on a volume that
is not `ReadWriteMany` is added to `spec.podTemplate.affinity` in any case.

The zone constraint depends on every node's labels, Ready or not, and is part of every pod
template, so a change rolls every HA pod (and fails Valkey over). The operator therefore keeps
its last answer when the nodes cannot be read; before the first successful read after it
started, the `high-availability` step fails and retries rather than guess. Without permission to
list nodes, pods are spread across nodes only. `ScheduleAnyway` is a preference: on a single
node, or with fewer nodes than pods, everything is still scheduled. Use `DoNotSchedule` in your
own constraints to make spreading a requirement.

## Turning HA on and off

- **On:** Valkey is created first; the server pods roll onto `app-web.ini` once a Valkey pod is
  Ready, and the cron runner starts. During the rolling update the old pod (with its cron tasks
  and LevelDB queues) and the cron runner run side by side for a moment. Sessions stay valid.
- **Off:** `spec.replicas` must be 1 (and the HPA off) first, as the API server requires. The cron
  runner is deleted, the single pod runs the full `app.ini` again (LevelDB queues), and Valkey is
  deleted once nothing can bring the HA config back ([autopilot.md](autopilot.md#high-availability-valkey)).
  Work still queued in Valkey at that moment is not carried over.

## Failure modes

| Failure | Effect |
|---------|--------|
| A server pod is deleted | No failed requests: its endpoint is removed during the 10 s preStop delay, then it finishes what it serves (268 clones in a loop while a pod was deleted: none failed) |
| A server pod crashes or hangs | Its connections fail; readiness removes it within 30 s; the autopilot restarts a hung pod ([autopilot.md](autopilot.md#health-based-restarts)) |
| The cron runner fails | Cron tasks pause until its replacement is Ready; missed runs catch up; `ForgejoCronRunnerDown` after 15 min |
| The cron runner's node is lost | Cron tasks pause until its pod is gone, never two runners; `CronRunnerStuck` names the pod; force-delete it once the node is confirmed down ([above](#the-cron-runner)) |
| The Valkey primary fails | Failover in seconds, see above |
| Two Valkey pods fail | No failover; Forgejo serves while the primary lives, otherwise every server pod turns NotReady |
| Every Valkey pod is recreated | Cache and queued work are lost; Forgejo recreates most tasks (cron, retries) |
| A node is lost | Its pods are rescheduled after the node's taint tolerations (300 s by default); other replicas serve meanwhile, if the data volume can be attached elsewhere (RWX) |
| PostgreSQL primary fails | CloudNativePG fails over; pods reconnect (`CONN_MAX_LIFETIME`) |
| The data volume fails | Forgejo is down; restore from a backup |

The end-to-end checks are in
[test/e2e/checklists/high-availability.md](../test/e2e/checklists/high-availability.md).
