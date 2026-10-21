# Auto Pilot (Capability Level V)

The `internal/autopilot` package makes the operator act on what it observes, not only on
what the spec says. Every autonomous action is:

- recorded in `status.autoPilot.recentActions` (newest first, at most 10),
- emitted as a Kubernetes event with the action type as the reason (`kubectl get events
  --field-selector involvedObject.name=<name>`),
- counted in `forgejo_operator_autopilot_actions_total{namespace,name,action}`.

Action types: `ConfigRolledBack`, `PodRestarted`, `ResourcesScaled`, `StorageExpanded`
(`UpgradeRolledBack` belongs to the upgrade feature).

Cooldowns are derived from `recentActions`, so they survive operator restarts. Sample
windows, volume measurements and event rate limits are kept in memory and simply start over
after a restart.

**Migration holds.** While a migration owns the workload, the autopilot leaves the pod
template and replica count alone: during an upgrade, while a failed upgrade's new image is
still deployed and unverified (its `bootstrap` init container may still be running
`forgejo migrate`, which takes no lock), and while a restored database waits for its first
pod to migrate it. Then there is no HPA or PDB, no config rollback or last-known-good
bookkeeping, no health-based restart and no vertical scaling. Restores (scaled to zero) are
treated the same way.

| Feature | Spec | Default |
|---|---|---|
| High availability (Valkey) | `spec.highAvailability.enabled` | off |
| Horizontal autoscaling (HPA) | `spec.autoscaling.horizontal` | off; needs HA |
| PodDisruptionBudget | automatic when more than one replica | — |
| Auto-tuning | `spec.autoPilot.autoTuning.enabled` | on |
| Config rollback | `spec.autoPilot.selfHealing.configRollback` | on |
| Health-based restarts | `spec.autoPilot.selfHealing.enabled`, `failureThreshold`, `healthCheckInterval` | on, 5, 30s |
| OOM and usage-based vertical scaling | `spec.autoscaling.vertical` | off |
| Storage monitoring and auto-expansion | `spec.autoPilot.storageAutoExpand` | on, 80%, +50%, max 200Gi |

## High availability: Valkey

With `highAvailability.enabled` the operator runs Valkey as Deployment and Service
`<name>-valkey` (image `spec.highAvailability.valkey.image`, default
`docker.io/valkey/valkey:8-alpine`). Forgejo then uses it for cache (db 0), sessions (db 1)
and queues (db 2). The issue indexer switches to `db` and the repository indexer is turned off,
because bleve indexes cannot be shared between pods.

- **Authentication.** A random password is added to `<name>-secrets` as `valkey-password`;
  the other keys in that Secret are left alone. Forgejo connects with
  `redis://:<password>@<name>-valkey.<ns>.svc.cluster.local:6379/<db>`.
- **Hardening.** Valkey runs as uid 999 with a read-only root filesystem and no
  capabilities. It has an exec `PING` readiness probe and a TCP liveness probe. Default
  resources are 50m CPU and 64Mi memory requested, with a 256Mi memory limit.
  `maxmemory` is 80% of the memory limit. The eviction policy is `volatile-lru`, so only
  keys with a TTL (cache, sessions) can be evicted; queue entries never are.
- **Persistence.** Data is an append-only file (`appendfsync everysec`) on an emptyDir.
  Queues hold pending work such as webhook deliveries, mirror syncs and PR checks. The AOF
  survives Valkey container restarts, including OOM kills and crashes. It does not survive
  the pod being rescheduled: cache and sessions are then rebuilt, and Forgejo's cron and
  retry logic re-creates most queued tasks. A PVC would tie Valkey to one node and add a
  storage dependency for data that is mostly transient, so emptyDir is the deliberate
  compromise.
- **Ordering.** Pods are only rolled onto the redis configuration once Valkey is ready.
  While Valkey is not ready, the `high-availability` step stops, so the steps that depend
  on it (app.ini, workload, HPA, post-rollout actions) wait, with a `WaitingForValkey`
  event and requeues every 5s. The redis URL itself (`rc.ValkeyURL`) is set as soon as the
  password is known, ready or not, so every step renders the same app.ini. During a restore
  the step does not stop: Forgejo is scaled to zero anyway, and scaling it down must not
  wait for Valkey. If the cache has not seen `<name>-secrets` yet, the password is read
  live.
- **Disabling HA.** Valkey is deleted only when nothing can bring a redis app.ini back:
  - the desired (non-redis) app.ini is deployed, not a rolled-back last known good one, and
    no rollback is pending;
  - the Deployment is fully rolled out and every Forgejo pod runs that app.ini;
  - with config rollback on, the last known good app.ini no longer uses Valkey. The LKG
    Secret is annotated `forgejo.lucas.icu/uses-valkey: "true"` when it was rendered
    for HA (older ones are recognised by their content). Valkey therefore stays until the
    non-HA app.ini has run healthy for a minute and replaced the LKG.

  Until then a `ValkeyRetained` event (at most hourly) says why. If Valkey is gone anyway
  (for example config rollback was off at the time), a rollback to an LKG that needs it is
  declined with `ConfigRollbackUnavailable`.
- **Shared storage warning.** Several replicas sharing a `ReadWriteOnce` data volume work on
  a single node, but not across nodes. When the replica count (or HPA maximum) is greater
  than 1, the data PVC is not `ReadWriteMany`, and the cluster has more than one schedulable
  node, the operator sets `Degraded=True` with reason `HARequiresSharedStorage` and emits a
  Warning event at most hourly.

Observe: `kubectl get deploy,svc <name>-valkey`, `Degraded` condition, events
`ValkeyPasswordGenerated`, `WaitingForValkey`, `ValkeyRetained`, `ValkeyDeleted`.

## Horizontal scaling and disruption budget

- **HPA.** When `autoscaling.horizontal.enabled` and `highAvailability.enabled` are both
  set, the operator creates HPA `<name>` (autoscaling/v2) targeting Deployment `<name>`.
  - It uses `minReplicas`/`maxReplicas` and a CPU utilization target, plus a memory target
    when `targetMemoryUtilizationPercentage` is set.
  - Scale-down is stabilized over 300s.
  - The workload step then keeps the replica count the HPA chose.
  - Requires metrics-server (`metrics.k8s.io`).
  - The HPA computes utilization against the Forgejo container's requests. Without a CPU
    request (or a memory request with a memory target) it cannot scale and only enforces
    min/max. The operator then sets `Degraded=True` with reason
    `HPAMissingResourceRequests` (unless `Degraded` is held by another reason) and emits an
    hourly Warning. A limit counts, since the API server copies it into the request, and so
    does a request a namespace LimitRange gives the pods.
- **First install.** The HPA is created only after the first rollout has completed
  (`status.image` is set). Until then the Deployment runs one pod, because the first pod's
  `bootstrap` init container migrates the empty database and must not run twice at once.
- **Horizontal scaling without HA.** The HPA is not created. `Degraded=True` with reason
  `HorizontalScalingRequiresHA` and an hourly Warning event explain why.
- **Upgrades and restores.** The HPA is removed during [migration holds](#auto-pilot-capability-level-v)
  and restores, because those run with one or zero replicas, and re-created afterwards.
- **PDB.** PDB `<name>` (`minAvailable: 1`, `unhealthyPodEvictionPolicy: AlwaysAllow`)
  exists while the minimum replica count is greater than 1. A PDB on a single replica would
  block node drains.

Observe: `kubectl get hpa,pdb <name>`, event `HPAConfigured`.

## Auto-tuning

Auto-tuning is derived from the effective container resources, which are
`status.autoPilot.appliedResources` when vertical scaling has set them, otherwise
`spec.resources`. Values in `spec.config` / `spec.configFrom` and `GOMEMLIMIT` in
`spec.env` always win. Every tuned value is listed in `status.autoPilot.tuning` as
`section.KEY` (or `env.NAME`).

| Value | Rule |
|---|---|
| `env.GOMEMLIMIT` | 85% of the memory limit (only with a limit). Go 1.25+ already derives GOMAXPROCS from the CPU limit, so that is not set. |
| `database.MAX_OPEN_CONNS` | `min(50, (max_connections − 10) / pods)`. `max_connections` comes from `spec.database.cnpg.parameters` or PostgreSQL's default of 100. `pods` is the HPA maximum or `spec.replicas`, plus one surge pod when HA rolling updates are used. Forgejo's own default is 30 per pod, regardless of replica count. |
| `database.MAX_IDLE_CONNS` | half of `MAX_OPEN_CONNS`, at least 2 |
| `database.CONN_MAX_LIFETIME` | `5m`, so pools move to the new primary soon after a CNPG switchover. Forgejo's default of 0 keeps connections forever. |
| `queue.MAX_WORKERS` | CPU limit (or request) ÷ 2, clamped to 1–10. Forgejo computes the same formula from the node's CPU count, not the container's. |

Tuning depends only on the spec and the applied resources. It never depends on transient
state such as upgrades, restores or HPA decisions, so it never rolls pods by itself.
Changing `spec.replicas` or the HPA maximum changes `MAX_OPEN_CONNS`, which causes one
rollout.

## Config rollback

The operator renders `app.ini` into `<name>-config`. Its hash is
`status.configHash`; the operator also tracks the hash the spec would produce.

1. **Last known good (LKG).** When the Deployment is fully rolled out with the desired
   config, every replica is listed and has been Ready for 1 minute, and
   `status.health.healthy` is true, the config is copied to `<name>-config-lkg`. Its hash is
   stored in `status.autoPilot.lastKnownGoodConfigHash` (event `LastKnownGoodConfig`).
   - Pods in phase `Failed` or `Succeeded` are ignored here and everywhere else in the
     autopilot. Such pods, for example ones evicted under node pressure or stopped by a
     graceful node shutdown, stay in the API until pod GC after their ReplicaSet has
     replaced them.
   - When the operator cannot reach Forgejo at all (`HealthProbeUnreachable`, see
     [Health-based restarts](#health-based-restarts)), pod readiness stands in for
     `status.health.healthy`. The kubelet's readiness probe calls the same `/api/healthz`.
2. **Detection.** A pod running a config that differs from the LKG counts as failing when
   it is not Ready and one of its containers, init containers included (the `bootstrap`
   init container runs `forgejo migrate` with the same app.ini), is failing now:
   - it has 3 or more restarts, or is in `CrashLoopBackOff` after at least 2, and its last
     exit was after the database last became ready (`DatabaseReady` turned True). Restart
     counts never reset, so a completed init container or a Ready container is never
     counted, and restarts during a database outage (for example CNPG restarting the
     primary after a `max_connections` change) are put down to the outage;
   - its `forgejo` container has been running for more than 5 minutes, counted from the
     later of its start and the database becoming ready, without becoming Ready.

   OOM kills are excluded; they are vertical scaling's job. When a failing pod is found,
   the operator sets `status.autoPilot.rejectedConfigHash` to the desired hash and records
   `ConfigRolledBack`.
3. **Rollback.** While the desired hash equals the rejected one, the pods mount
   `<name>-config-lkg` instead and `Degraded=True` has reason `ConfigRolledBack`. With
   `highAvailability` (rolling updates with maxUnavailable 0), the old pods keep serving
   throughout. A single replica (Recreate) is down from the bad rollout until the rollback
   pod is Ready, which was about a minute in testing.
4. **Recovery.** Any change to the rendered app.ini clears the rejection and deploys the new
   config (event `ConfigRetry`). Usually that is a spec fix, but operator-derived values
   such as tuning or a rotated DB password also count. `Degraded` returns to False.

Safeguards:
- Nothing happens without an LKG, for example on a first install that never became healthy.
- Nothing happens during upgrades, restores and other migration holds: image failures
  belong to the upgrade's auto-rollback. An active rollback stays in place through them.
- With `highAvailability` off, an LKG that uses Valkey is only rolled back to while Valkey
  still exists; otherwise the rejection is dropped (`ConfigRollbackUnavailable`).
- Nothing happens while the database is not ready.
- A config is rejected at most once per 5 minutes.
- Set `selfHealing.configRollback: false` (or `selfHealing.enabled: false`) to always deploy
  the spec as is.

Caveat: the LKG app.ini embeds the values of the time it was stored. If something it
references changed since, for example a rotated database password, the rolled-back config can
fail too. The fix is the same: change the spec.

## Health-based restarts

The liveness probe is a TCP check. A `/api/healthz` liveness probe would restart every pod
during a database failover or a Valkey blip. A TCP check, however, misses a hung process:
the kernel keeps accepting connections for a stopped process. The operator fills the gap in
two ways:

- **Pod level.** A server container that passed its startup probe and is still running, but
  whose pod has been NotReady for `failureThreshold × healthCheckInterval` (default 2m30s),
  is hung. Only pods running the LKG config qualify; otherwise the failure is config
  rollback's case. The pod is deleted with a 5s grace period, because a hung process never
  shuts down gracefully and keeps file locks (for example the LevelDB queue of a non-HA
  instance) that its replacement needs. Records `PodRestarted`.
- **Instance level.** `status.health.consecutiveFailures ≥ failureThreshold` while at least
  one pod is Ready and every server container is running and not crash-looping, and the
  latest probe was not `Unreachable`. Health is probed through the Service by the
  observability feature and only counts while a pod is Ready. It counts probes Forgejo
  answered with failing checks or an error page, and probes Forgejo accepted and then left
  hanging; probes that never reached Forgejo are not counted. The response is a rolling
  restart: annotation `forgejo.lucas.icu/restarted-at` on the pod template. Records
  `PodRestarted`.
  - In practice this covers a healthz that flaps: it fails at every operator probe but
    never three kubelet probes in a row. When Forgejo fails healthz steadily, the kubelet's
    readiness probe on the same endpoint marks the pods NotReady within about 30s. That
    resets the streak and leaves the hang to the pod-level check.
- **Unanswered probes never restart.** Some probes say nothing about Forgejo:
  - a timeout, a refused or reset connection, or a DNS failure;
  - a reply that is not Forgejo's healthz document, for example from a mesh sidecar.

  The health step reports these as `Healthy=Unknown` with reason `Unreachable` and does not
  count them as failures; that reason is the only signal the autopilot uses. When they
  continue for 1 minute while pods keep passing their readiness probe, the operator cannot
  reach Forgejo, but Forgejo is serving. Restarting would only cause outages, so the
  operator instead:
  - sets `Degraded=True` with reason `HealthProbeUnreachable`, unless `Degraded` is already
    held by another reason;
  - emits a `HealthProbeUnreachable` Warning event at most hourly;
  - records the last known good config from pod readiness.

  The condition clears once a probe gets through again.

Network requirements: the operator probes
`http://<name>-http.<namespace>.svc.cluster.local:<port>`, so it must run in the cluster (or
somewhere that resolves and routes `svc.cluster.local`). Under a default-deny ingress
NetworkPolicy, allow the operator's namespace to reach the Forgejo pods on the HTTP
container port (3000). With a service mesh in STRICT mTLS mode, either add the operator to
the mesh or exempt that port. Otherwise `status.health`, version and statistics stay empty
and instance-level restarts never happen. The pod-level check still works, because it
relies on the kubelet's readiness probe and an LKG recorded from pod readiness.

Safeguards:
- Only while the database is ready. With HA, Valkey must also be ready, otherwise the
  pipeline is paused.
- Never during upgrades, restores or other migration holds.
- At most one restart per 10 minutes.

## Vertical scaling

Enabled with `spec.autoscaling.vertical.enabled`. Resources the operator chooses are
stored in `status.autoPilot.appliedResources` and replace `spec.resources` on the Forgejo
containers (main and init) from the next reconcile, which rolls the pods. Disabling vertical
scaling clears them and returns to `spec.resources`.

- **Spec changes win.** The Deployment's annotation `forgejo.lucas.icu/vertical-base`
  records which `spec.resources` the applied resources were derived from. When
  `spec.resources` changes, the applied resources are dropped (event `VerticalRebased`):
  the new spec takes effect and later scaling starts from it.
- **Bounds are re-applied.** Applied resources are kept within `minAllowed`/`maxAllowed` on
  every reconcile, also after the bounds change: requests within both, limits at most
  `maxAllowed`, requests at most limits (event `VerticalClamped`).
- **OOM.** A container (init containers included) that ran with the current memory limit
  and has `OOMKilled` as its last termination reason triggers the following:
  - Memory limit and request are raised by 50%, rounded up to MiB. They are never lowered:
    a value already at or above its cap stays.
  - Caps: `maxAllowed.memory`, or without it max(4 × the spec's memory, 1Gi). Limits also
    stop at the largest schedulable node's allocatable memory and requests at 90% of it, so
    the pod still fits a node. A limit without a request counts as a request, since the API
    server copies it. Setting `maxAllowed` is recommended.
  - Without a limit, only the request is raised.
  - Records `ResourcesScaled`. Pods still running an older, smaller limit are ignored, and
    so is an OOM older than the last `ResourcesScaled` action.
  - At a cap, a Warning `OOMAtMaxAllowed` (spec bound or default ceiling) or
    `OOMAtNodeCapacity` is emitted at most hourly.
  - In testing, a 48Mi limit grew to 72Mi and then 108Mi within a minute, after which the
    pod ran healthy.
- **Unschedulable pods.** When a server pod requesting the current memory has been
  unschedulable with `Insufficient memory` for 2 minutes, the last change is reverted (to
  the previous applied resources, or `spec.resources` after an operator restart), recorded
  as `ResourcesScaled`, and a `ResourcesUnschedulable` Warning is emitted. Until the operator
  restarts, OOM growth no longer raises the memory request above the reverted value.
- **Usage-based right-sizing.** Needs metrics-server.
  - Every reconcile samples `metrics.k8s.io` PodMetrics of Ready server pods (the `forgejo`
    container), keeping up to 120 samples (about 2h) per instance.
  - From 10 samples on, the recommendation is: requests = p90 usage ÷
    `targetUtilizationPercentage`, clamped to `minAllowed`/`maxAllowed` and to at least
    10m CPU and 32Mi memory. It is published in `status.autoPilot.recommendedResources`.
  - It is applied only when CPU or memory requests differ by more than 20%, at most once
    per 30 minutes (any `ResourcesScaled` counts, including OOM bumps), and never in the
    reconcile that saw an OOM.
  - Limits are only ever raised, to fit a larger request. Lowering limits is left to the
    user, so right-sizing can never cause an OOM loop.
  - **With the HPA.** The HPA scales on CPU utilization of the requests (and memory with
    `targetMemoryUtilizationPercentage`). Right-sizing leaves those requests alone, since
    raising a request lowers utilization and makes the HPA scale in, and the other way round.
    The recommendation is still published, a Normal `VerticalLimitedByHPA` event says so at
    most hourly, and OOM-driven memory growth still applies.
- None of this runs during upgrades, restores or other migration holds; spec rebasing and
  clamping wait for them to end, because they roll the pods.

## Storage

Volumes are always measured and reported. `spec.autoPilot.storageAutoExpand.enabled`
(default on) only decides whether they are also grown: with it off, `status.storage`, the
`StorageHealthy` condition and the `forgejo_operator_volume_used_ratio` metric (and so the
`ForgejoVolumeAlmostFull` alert) stay.

Storage is its own pipeline step (`autopilot.ReconcileStorage`) with no prerequisites. A
full volume is often the reason an earlier step stops: PostgreSQL cannot start on a full
CNPG volume, so the database step waits, and with it the workload and post-rollout steps.
The storage step still runs then, without the database, before the Deployment exists and
during maintenance, and it is the only place the storage pass runs.

- **Volumes.** The data PVC (`<name>-data` or `storage.existingClaim`) and the CNPG PVCs
  (label `cnpg.io/cluster=<name>-db`, data and WAL).
- **Measurement.** Through `pods/exec` in a running container that mounts the claim (the
  `forgejo` container, or CNPG's `postgres`), at most once a minute per claim. Pods are
  found by label: the server pods (from the operator's cache) and, when there are CNPG
  volumes, the pods labelled `cnpg.io/cluster=<name>-db`.
  - `df -Pk <mountPath>` gives the file system's size and usage. When the size is at most
    125% of the claim's request, the file system is the volume itself and df is the
    measurement.
  - Otherwise the claim is a directory on a larger file system. `local-path` volumes, for
    example, live on the node's file system (verified on k3s and a local cluster: 1Gi PVCs on
    about 420GB), as do hostPath volumes and many NFS exports. The operator then runs
    `du -skx <mountPath>` at most every 5 minutes per claim, with `nice -n 19` and killed
    inside the container after 110s, and compares the result with the PVC's requested size.
  - Measurements run in the background, at most four at a time, so a slow volume never
    holds up reconciliation; a reconcile waits up to 3s for them, and slower results are
    reported by a later one (requeued after 30s). Failed attempts back off exponentially
    (df from 1 minute, du from 5 minutes, both up to 1 hour). Until a new measurement
    succeeds, the last one is reported.
  - Without `pods/exec` permission in the namespace, nothing can be measured:
    `StorageHealthy=Unknown`/`MeasurementUnavailable` says so and the operator logs it once.
- **Report.** `status.storage.volumes[]` (`usedBytes`, `capacityBytes`, `usedPercent`,
  `requestedSize`, `expansionSupported`, `lastExpansionTime`), the metric
  `forgejo_operator_volume_used_ratio{namespace,name,claim}`, and the `StorageHealthy`
  condition:

  | Status / reason | Meaning |
  |---|---|
  | True `WithinThreshold` | every volume is below `thresholdPercent` |
  | True `Expanding` | a volume above the threshold is being expanded |
  | False `ExpansionStalled` | a resize has been pending for 30 minutes; the message lists the PVC's resize conditions (`ControllerResizeError`, `NodeResizeError`, `FileSystemResizePending`, …) and `allocatedResourceStatuses` (e.g. `ControllerResizeInfeasible`). Warning event at most hourly per claim |
  | False `DatabaseClusterMissing` | a CNPG volume above the threshold whose Cluster `<name>-db` (or the CNPG API) does not exist, so it cannot be grown; Warning event at most hourly per claim |
  | False `ExpansionUnsupported` | above the threshold, StorageClass without `allowVolumeExpansion`; Warning event at most hourly per claim |
  | False `MaxSizeReached` | above the threshold and already at `maxSize` |
  | False `AutoExpandDisabled` | above the threshold with `storageAutoExpand.enabled: false`; Warning event at most hourly per claim |
  | Unknown `MeasurementUnavailable` | nothing could be measured yet |
- **Expansion.** At or above `thresholdPercent`, when auto-expansion is on and the
  StorageClass allows it:
  - The new size is the current request plus `incrementPercent`, rounded up to MiB and
    capped at `maxSize`.
  - The data PVC is patched directly. CNPG volumes are grown through the Cluster's
    `spec.storage.size` / `spec.walStorage.size`, so CNPG resizes every instance
    consistently.
  - The core reconciler only ever grows these sizes, so expansions are not undone.
  - Records `StorageExpanded`.
  - No new expansion of a claim while a resize is pending, within 5 minutes of the last
    one, or more than once per volume role per check.
  - No new expansion on a measurement taken before the volume last grew. When no pod can
    run to measure it (PostgreSQL on a full disk), the last measurement is still reported,
    but once the PVC's capacity has grown it no longer counts: `StorageHealthy` stays
    `Expanding` until a fresh measurement comes in. A measurement taken by the running
    operator is compared with the PVC capacity at the time; one carried over in
    `status.storage` after a restart counts as older when its size is more than 10% below
    the PVC's capacity.
- **Offline file system resize.** Some drivers grow the file system only while mounting
  the volume (`FileSystemResizePending`). When the data PVC has waited for that for 10
  minutes, the operator restarts Forgejo once per pending resize through the
  `restarted-at` annotation (not during migration holds, restores or upgrades, including
  those only recorded in `status.backup.activeRestore` and `status.upgrade`, not at zero
  replicas, and at most once per 10 minutes) and records `PodRestarted`. CNPG volumes are
  only reported.
- **Upgrades and restores.** Measurement and expansion continue during them, since growing
  a volume is safe online. While a restore has scaled Forgejo to zero, the data PVC keeps
  its last measurement.

## RBAC

On top of the core permissions, the autopilot needs:
- `nodes` get/list, to count schedulable nodes and find the largest allocatable memory.
- `pods/exec` create/get, to measure volumes with df and du. This is the only use; without
  it storage reporting degrades to `MeasurementUnavailable`. The operator does not use
  `nodes/proxy` (kubelet stats), which would open every node's kubelet API.
- `metrics.k8s.io` pods get/list.
- `storageclasses` get/list/watch.
- `persistentvolumeclaims` patch.
- CNPG `clusters` patch.
