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
windows, du caches and event rate limits are kept in memory and simply start over after a
restart.

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
- **Ordering.** Forgejo is only given the redis configuration once Valkey is ready.
  Reconciliation pauses, with a `WaitingForValkey` event and requeues every 5s, while
  Valkey is not ready.
- **Disabling HA.** Valkey is deleted only after every Forgejo pod runs the non-redis
  configuration.
- **Shared storage warning.** Several replicas sharing a `ReadWriteOnce` data volume work on
  a single node, but not across nodes. When the replica count (or HPA maximum) is greater
  than 1, the data PVC is not `ReadWriteMany`, and the cluster has more than one schedulable
  node, the operator sets `Degraded=True` with reason `HARequiresSharedStorage` and emits a
  Warning event at most hourly.

Observe: `kubectl get deploy,svc <name>-valkey`, `Degraded` condition, events
`ValkeyPasswordGenerated`, `WaitingForValkey`, `ValkeyDeleted`.

## Horizontal scaling and disruption budget

- **HPA.** When `autoscaling.horizontal.enabled` and `highAvailability.enabled` are both
  set, the operator creates HPA `<name>` (autoscaling/v2) targeting Deployment `<name>`.
  - It uses `minReplicas`/`maxReplicas` and a CPU utilization target, plus a memory target
    when `targetMemoryUtilizationPercentage` is set.
  - Scale-down is stabilized over 300s.
  - The workload step then keeps the replica count the HPA chose.
  - Requires metrics-server (`metrics.k8s.io`).
- **Horizontal scaling without HA.** The HPA is not created. `Degraded=True` with reason
  `HorizontalScalingRequiresHA` and an hourly Warning event explain why.
- **Upgrades and restores.** The HPA is removed while an upgrade or restore is running,
  because those run with one or zero replicas, and re-created afterwards.
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
   config, every pod has been Ready for 1 minute, and `status.health.healthy` is true, the
   config is copied to `<name>-config-lkg`. Its hash is stored in
   `status.autoPilot.lastKnownGoodConfigHash` (event `LastKnownGoodConfig`).
2. **Detection.** A pod running a config that differs from the LKG counts as failing when
   any of its containers, init containers included (the `bootstrap` init container runs
   `forgejo migrate` with the same app.ini), meets one of these:
   - has 3 or more restarts,
   - is in `CrashLoopBackOff` after at least 2 restarts,
   - has its `forgejo` container running for more than 5 minutes without becoming Ready.

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
- Nothing happens while an upgrade or restore runs: image failures belong to the upgrade's
  auto-rollback.
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
  one pod is Ready and every server container is running and not crash-looping. Health is
  probed through the Service by the observability feature and only counts while a pod is
  Ready. The response is a rolling restart: annotation
  `forgejo.lucas.icu/restarted-at` on the pod template. Records `PodRestarted`.

Safeguards:
- Only while the database is ready. With HA, Valkey must also be ready, otherwise the
  pipeline is paused.
- Never during upgrades or restores.
- At most one restart per 10 minutes.

## Vertical scaling

Enabled with `spec.autoscaling.vertical.enabled`. Resources the operator chooses are
stored in `status.autoPilot.appliedResources` and replace `spec.resources` on the Forgejo
containers (main and init) from the next reconcile, which rolls the pods. Disabling vertical
scaling clears them and returns to `spec.resources`. To reset to the spec, toggle it off
and on.

- **OOM.** A container (init containers included) that ran with the current memory limit
  and has `OOMKilled` as its last termination reason triggers the following:
  - Memory limit and request are raised by 50%, rounded up to MiB and capped at
    `maxAllowed.memory`.
  - Without a limit, only the request is raised.
  - Records `ResourcesScaled`. Pods still running an older, smaller limit are ignored, and
    so is an OOM older than the last `ResourcesScaled` action.
  - At the cap, a Warning `OOMAtMaxAllowed` is emitted at most hourly.
  - In testing, a 48Mi limit grew to 72Mi and then 108Mi within a minute, after which the
    pod ran healthy.
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
- Neither runs during upgrades or restores.

## Storage

Enabled by default (`spec.autoPilot.storageAutoExpand`). Turning it off removes
`status.storage` and the `StorageHealthy` condition.

- **Volumes.** The data PVC (`<name>-data` or `storage.existingClaim`) and the CNPG PVCs
  (label `cnpg.io/cluster=<name>-db`, data and WAL).
- **Measurement.** At most once a minute.
  - Kubelet volume stats (`/api/v1/nodes/<node>/proxy/stats/summary`) are used when they
    describe a dedicated volume, meaning the reported capacity is at most 125% of the
    request.
  - Some provisioners report something else. `local-path` reports the whole node
    filesystem (verified on k3s and a local cluster: 1Gi PVCs reported as about 420GB), and hostPath
    volumes report nothing. In those cases the operator runs `du -skx <mountPath>` through
    `pods/exec` in a running container that mounts the claim (the `forgejo` container, or
    CNPG's `postgres`), at most every 5 minutes per claim, and compares the result with the
    PVC's requested size.
- **Report.** `status.storage.volumes[]` (`usedBytes`, `capacityBytes`, `usedPercent`,
  `requestedSize`, `expansionSupported`, `lastExpansionTime`), the metric
  `forgejo_operator_volume_used_ratio{namespace,name,claim}`, and the `StorageHealthy`
  condition:

  | Status / reason | Meaning |
  |---|---|
  | True `WithinThreshold` | every volume is below `thresholdPercent` |
  | True `Expanding` | a volume above the threshold is being expanded |
  | False `ExpansionUnsupported` | above the threshold, StorageClass without `allowVolumeExpansion`; Warning event at most hourly per claim |
  | False `MaxSizeReached` | above the threshold and already at `maxSize` |
  | Unknown `MeasurementUnavailable` | nothing could be measured yet |
- **Expansion.** At or above `thresholdPercent`, when the StorageClass allows it:
  - The new size is the current request plus `incrementPercent`, rounded up to MiB and
    capped at `maxSize`.
  - The data PVC is patched directly. CNPG volumes are grown through the Cluster's
    `spec.storage.size` / `spec.walStorage.size`, so CNPG resizes every instance
    consistently.
  - The core reconciler only ever grows these sizes, so expansions are not undone.
  - Records `StorageExpanded`.
  - No new expansion of a claim while a resize is pending, within 5 minutes of the last
    one, or more than once per volume role per check.
- **Upgrades and restores.** Measurement and expansion continue during them, since growing
  a volume is safe online. While a restore has scaled Forgejo to zero, the data PVC keeps
  its last measurement.

## RBAC

On top of the core permissions, the autopilot needs:
- `nodes` get/list, to count schedulable nodes.
- `nodes/proxy` get, for kubelet stats.
- `pods/exec` create/get, for the du fallback.
- `metrics.k8s.io` pods get/list.
- `storageclasses` get/list/watch.
- `persistentvolumeclaims` patch.
- CNPG `clusters` patch.
