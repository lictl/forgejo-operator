# Forgejo Actions runners

A `ForgejoRunner` (short name `fjrn`) is a set of [Forgejo Runner](https://code.forgejo.org/forgejo/runner)
pods that run the Actions jobs of one `Forgejo` instance in the same namespace. The operator
registers every runner with the instance without exec into a pod and without exposing the
runners' secrets, starts the pods once their runners are registered, and unregisters runners
that are scaled away or deleted, so that Forgejo's list of runners holds no stale entries.

The code is in `internal/runner` (configuration, pods, registration Job, Forgejo API client,
metrics) and `internal/controller/forgejorunner_controller.go` (the reconcile loop).

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: ForgejoRunner
metadata:
  name: ci
spec:
  forgejoRef: {name: git}         # a Forgejo in this namespace
  executor: Docker                # required: Docker (privileged daemon) or Host
  replicas: 2                     # runners ci-0 and ci-1
  labels:
    - docker:docker://data.forgejo.org/oci/node:22-bookworm
  # scope: {organization: acme}   # default: every repository of the instance
  # docker: {mountSocket: true}   # steps may run docker build / docker run
```

```console
$ kubectl get fjrn
NAME   FORGEJO   EXECUTOR   DESIRED   READY   ONLINE   REGISTERED   STATUS          AGE
ci     git       Docker     2         2       2        True         RunnersOnline   1m
```

A workflow picks the runners by the name of one of their labels:

```yaml
# .forgejo/workflows/ci.yml
on: [push]
jobs:
  test:
    runs-on: docker
    steps:
      - uses: actions/checkout@v4
      - run: npm ci && npm test
```

## Spec

| Field | Default | Meaning |
|-------|---------|---------|
| `forgejoRef.name` | required, immutable | The `Forgejo` in the same namespace. |
| `scope` | the whole instance | `organization: <name>` (the runners take only jobs of that organization's (or user's) repositories) or `repository: <owner>/<name>`. Changing it registers the runners again for the new scope, without restarting them. |
| `labels` | `docker:docker://data.forgejo.org/oci/node:22-bookworm` (Docker), `host:host` (Host) | The labels the runners offer: `name:docker://image` for the Docker executor, `name:host` for the Host executor. A job's `runs-on` is matched against the names. |
| `replicas` | 1 | Number of runners (0–100). Also the scale subresource: `kubectl scale fjrn ci --replicas=3`, or an HPA/KEDA `ScaledObject` on the `ForgejoRunner`. |
| `capacity` | 1 | Jobs each runner runs at once (`runner.capacity`). With the Docker executor they share the runner's Docker daemon. |
| `executor` | required | `Docker` or `Host`, see [Executors](#executors). There is no default: the Docker executor runs a privileged pod, which is only created when `Docker` is asked for. |
| `image` | `data.forgejo.org/forgejo/runner:13.2.0` | Forgejo Runner. |
| `imagePullPolicy`, `imagePullSecrets` | | For all images of the pods. |
| `docker.image` | `docker.io/library/docker:29.9.2-dind` | The Docker daemon of the Docker executor. |
| `docker.mountSocket` | false | Mounts the daemon's socket into job containers at `/var/run/docker.sock` (`container.docker_host: automount`), so that steps can run `docker build` and `docker run`. |
| `docker.args` | | Extra `dockerd` arguments, e.g. `--mtu=1400`, `--registry-mirror=https://...`. |
| `docker.resources`, `docker.storageSizeLimit` | | Resources of the daemon container (job containers run inside it) and the size limit of its `emptyDir` (`/var/lib/docker`). |
| `host.image` | `data.forgejo.org/oci/node:22-bookworm` | The image the Host executor runs the runner and the job steps in. |
| `resources`, `env` | | Of the runner container. `env` is for the runner process (e.g. `HTTP_PROXY`); jobs get variables from `config.runner.envs`. |
| `cache.enabled`, `cache.sizeLimit` | true | The runner's built-in cache server for `actions/cache`, kept in an `emptyDir` of the pod. |
| `shutdownTimeout` | 10m | How long a stopping runner (scale-down, rollout, deletion) lets its running jobs finish before it cancels them. The pod's grace period is this plus 30 s. |
| `config` | | Merged into the runner's `config.yml` (keys of Forgejo Runner's `config.example.yaml`), e.g. `{runner: {timeout: 1h, envs: {...}}, container: {options: "--memory=2g"}}`. `server`, `runner.labels`, `runner.capacity` and `runner.shutdown_timeout` always come from the operator. |
| `podTemplate` | | Annotations, labels, node selector, tolerations, affinity, topology spread, priority class and service account of the runner pods. |

## Executors

| | Docker | Host |
|---|---|---|
| Where steps run | Containers of a Docker daemon in a sidecar of the runner pod, one container per job (from the label's image) | Processes in the runner's own container (`host.image`) |
| Labels | `name:docker://image` | `name:host` |
| Isolation | Jobs are separated from the runner and from each other by containers. With `mountSocket`, a job controls the privileged daemon, i.e. it is root on the node | None: a job can read the runner's secret and act as the runner |
| Pod Security | `privileged` (the daemon) | passes `restricted` |
| Use for | General CI, container builds | Trusted repositories in namespaces that only allow `restricted` pods |

### Docker

Each pod `<name>-<i>` has:

- **`docker`**, a native sidecar (an init container with `restartPolicy: Always`, so it starts
  before the runner and stops after it) running `dind dockerd` from `docker.image`, privileged. It
  listens only on the UNIX socket `/var/run/dind/docker.sock` in an `emptyDir` shared with the
  runner (owned by group 1000, the runner's group). There is no TCP listener, so nothing else on
  the pod network can reach the daemon. A startup probe (`docker info`) holds the runner back until
  the daemon answers. Images and containers are kept in an `emptyDir` (`/var/lib/docker`), so every
  pod starts with an empty image cache.
- **`runner`**: `forgejo-runner daemon` as uid 1000 with a read-only root file system, no
  capabilities and no privilege escalation, `DOCKER_HOST=unix:///var/run/dind/docker.sock`.

The namespace must admit privileged pods: `pod-security.kubernetes.io/enforce: privileged` (or no
label, where the cluster default allows it). When the namespace's `enforce` label is `baseline` or
`restricted`, `Ready` is `False` with reason `PodSecurityBlocked` and a Warning event says so; the
StatefulSet controller's `FailedCreate` events on `statefulset/<name>` give the details. A
cluster-wide default level (an `AdmissionConfiguration`) is not visible to the operator: then only
the StatefulSet's events tell.

`docker-in-docker rootless` (`docker:dind-rootless`) still needs `--privileged` and gains nothing
here. Clusters with [sysbox](https://github.com/nestybox/sysbox) can run the daemon unprivileged;
the operator does not set a `runtimeClassName` for it yet.

### Host

Each pod has an init container `install-runner` (from `image`) that copies the statically linked
`forgejo-runner` binary into an `emptyDir`, and the `runner` container, which runs it in
`host.image`. The stock runner image has no node, so JavaScript actions such as `actions/checkout`
need a toolchain image that has: the default `data.forgejo.org/oci/node:22-bookworm` has node, git,
bash and curl. Everything runs as uid 1000 (`runAsNonRoot`, no capabilities, no privilege
escalation, `RuntimeDefault` seccomp); the pod passes the `restricted` Pod Security Standard. The
root file system stays writable for the job steps; `$HOME` and the work directories are on
`emptyDir`s (`/data`, `/tmp`). The pod shares its process namespace, so the pause container reaps
the processes jobs leave behind.

Jobs run in the runner's container and can read every runner secret of the `ForgejoRunner`
(mounted for the runner) and use it to take further jobs of the same scope as the runner. Use the
Host executor only for repositories you trust, and narrow it with `scope`. Ephemeral runners (one
job per registration) would close this hole; they are not implemented yet (see
[Limitations](#limitations)).

## Registration

Nothing is ever executed in a pod, and no runner secret appears in pod arguments, environment
variables, logs, events, status or the runner configuration:

1. **Secrets.** Secret `<name>-tokens` holds one secret per runner (`token-<i>`, 40 hex
   characters from `crypto/rand`). It is generated once and kept as the runner's identity. Forgejo
   derives the runner's UUID from the first 16 characters, so the operator knows the UUID without
   asking.
2. **Registration Job.** For runners that are not registered yet (or registered for another
   scope), Job `<name>-register` runs `forgejo-cli actions register --secret-file ... --scope ...
   --name <name>-<i> --keep-labels` for each of them. It runs the instance's own running image
   (`status.image`), with its `app.ini` Secret (which has the database credentials) and only the
   secrets it registers, as uid 1000 with a read-only root file system and without the data
   volume: the command only writes to the database. It is idempotent: registering a runner again
   updates its name, scope and secret. It prints the UUIDs only; error output passes a filter that
   masks anything shaped like a secret (Forgejo prints the runner record, secret included, when an
   update fails). The Job waits until the instance is `Ready` and neither paused, upgrading nor
   being restored (`Registered=False/WaitingForForgejo`). When it succeeds, the operator records
   the runners in `status.runners` and only then deletes the Job (if the status cannot be written,
   e.g. because a scale came in at the same moment, the Job stays until the next attempt); a
   failed Job is kept for a minute (`Registered=False/RegistrationFailed`, with its error) and then
   retried.
3. **Pods.** StatefulSet `<name>` runs pods `<name>-0`, `<name>-1`, ... only for runners that are
   registered (a pod without a registration would only be refused by Forgejo). Pod `i` reads
   `config-<i>.yaml` from Secret `<name>-runner`, whose connection names the instance's in-cluster
   URL (`http://<forgejo>-http.<namespace>.svc.<domain>:3000/`), the runner's UUID and
   `token_url: file:/etc/forgejo-runner/tokens/token-<i>`: the secret stays in its own Secret. The
   runner declares its labels and version when it connects. A change of the shared configuration
   (labels, capacity, `config`, images) rolls the pods; adding or removing a runner does not
   touch the other pods (nothing per runner is on the pod template).

Forgejo Runner 13.2.0 is the default for both Forgejo 15 and 17: its `server.connections`
configuration needs runner 12.7 or newer, job summaries on Forgejo 17 need 13.2, and OIDC tokens
need Forgejo 15. Runner 13 needs Docker 25 or newer and dropped the `set-output`, `set-env` and
`add-path` workflow commands and the `GITEA_*` variables; if your workflows still use them, set
`image: data.forgejo.org/forgejo/runner:12.13.0` (the last 12.x release) until they are updated.

## Scaling and unregistering

- **Scale up.** The new runners are registered first, then their pods start.
- **Scale down.** The StatefulSet stops the highest pods. A runner that is running a job lets it
  finish within `shutdownTimeout`. Once the pod is gone, the operator deletes the runner in Forgejo
  (`DELETE /api/v1/admin/actions/runners/{id}`), drops it from `status.runners` and deletes its
  secret. A runner scaled back later gets a new secret, so a copied old secret stays useless.
- **Deletion.** A finalizer deletes the StatefulSet and a running registration Job, waits for their
  pods to stop (at most the grace period plus 2 minutes), then deletes every runner of the
  `ForgejoRunner` in Forgejo: the recorded ones (by their id) and any runner whose UUID one of its
  secrets gives (registered by a Job whose result was never recorded). If that still fails 5
  minutes after the first failed attempt, a Warning event `UnregisterFailed` names the runners and
  the `ForgejoRunner` goes; delete them in *Site administration > Actions > Runners*. When the
  instance itself is gone or being deleted, nothing is unregistered (its database goes with it).
- **Runners deleted in Forgejo.** A recorded runner that Forgejo no longer has (deleted in the
  web UI, by `[cron.cleanup_offline_runners]`, or a database restored from an earlier backup) is
  registered again with the same secret, so under the same UUID (`RunnerMissing` event,
  `status.runners[].missing: true` meanwhile), at most every 2 minutes per runner. The runner pods
  keep running: the pod reconnects once its runner is registered again.
- **Forgejo's list of runners** is ordered by the time Forgejo last heard from each runner, so
  runners move between its pages while they are read. The operator reads a list of more than one
  page twice (and again when its pages do not add up to the total), asks for a recorded runner the
  list lacks by its id before it takes it for gone, and deletes runners by their id.
- **A new instance under the same name.** Runners recorded for another instance UID are registered
  again with the new one.

Unregistering and the online state use Forgejo's API as the instance's admin: HTTP basic
authentication with `spec.admin.username` and the password of `<forgejo>-admin` (or
`spec.admin.existingSecret`), from the operator's pod to `<forgejo>-http`. This needs:

- the password in the Secret to be the admin's password (the bootstrap resets it from the Secret
  only when the Secret changes, so change it there, not in the web UI);
- no two-factor authentication or security key on that account;
- basic authentication not turned off (`[service] ENABLE_BASIC_AUTHENTICATION`).

Without it, `Degraded` is `True` with reason `ForgejoAPIUnavailable`: registration and the runners
work, but scaled-away runners stay registered (offline) and `Degraded=True/UnregisterFailed` lists
them until the API works again, and `onlineRunners` is unknown.

Several runner pods can share one registration, and Forgejo's own cleanup cron
(`[cron.cleanup_offline_runners]`, off by default) deletes every offline runner, including ones the
operator manages, which it then registers again. Neither is needed with a `ForgejoRunner`.

## The Forgejo side

- **Actions are on by default** on Forgejo 15 and 17 (`[actions] ENABLED = true`); the operator
  renders nothing for them. If `spec.config.actions.ENABLED` is `false`, the runners' `Ready` is
  `False` with reason `ActionsDisabled`.
- **Logs and artifacts** go to the bucket of `spec.objectStorage` (subsystems `actionsLogs`,
  `actionsArtifacts`, `actions_log/` and `actions_artifacts/` below the prefix), else to the data
  volume. Retention and compression are Forgejo settings (`[actions] LOG_RETENTION_DAYS` 365,
  `ARTIFACT_RETENTION_DAYS` 90, `LOG_COMPRESSION` zstd), set them with `spec.config.actions`. Forgejo
  17 refuses durations without a unit in `ZOMBIE_TASK_TIMEOUT`, `ENDLESS_TASK_TIMEOUT` and
  `ABANDONED_JOB_TIMEOUT` (see [versions.md](versions.md#16--17)).
- **Actions** such as `actions/checkout` are fetched from `[actions] DEFAULT_ACTIONS_URL`
  (`https://data.forgejo.org` by default): the runner pods need egress to it, or set it to `self`
  and mirror the actions into the instance for an air-gapped cluster.
- **Checkout** clones from the instance's `ROOT_URL` (`status.url`), from inside the job
  container. The default `ROOT_URL` is the in-cluster Service URL, which job containers resolve
  (the Docker daemon passes the pod's DNS configuration on). With a public `spec.server.domain`,
  the pods must be able to resolve and reach that name.
- **The ID token key** that signs OIDC tokens for jobs (Forgejo 15 and later) is kept on the data
  volume (`actions_id_token/`), so it is in backups like the other identity files.

## Status

```console
$ kubectl get fjrn ci -o jsonpath='{.status.runners}' | jq
[{"index": 0, "name": "ci-0", "uuid": "32383235-...", "scope": "", "id": 3, "status": "idle", "registeredTime": "..."}, ...]
```

| Field | Meaning |
|-------|---------|
| `replicas`, `readyReplicas`, `selector` | Of the StatefulSet (and for the scale subresource) |
| `onlineRunners` | Runners Forgejo reports `idle` or `active` (absent while unknown) |
| `runners[]` | Registered runners: index, name, UUID, scope, Forgejo's id and status (`offline`, `idle`, `active`); `missing: true` while Forgejo lost the runner and it is registered again |
| `forgejoUID` | The instance the runners are registered with |
| `image`, `configHash` | Of the runner pods |

| Condition | Reasons |
|-----------|---------|
| `Registered` | `Registered`; `Registering` (the Job runs), `WaitingForForgejo`, `RegistrationFailed`, `ForgejoNotFound` |
| `Ready` | `RunnersOnline`; `PodsReady` (pods ready, online state unknown), `ScaledToZero`; not ready: `PodsNotReady`, `RunnersOffline`, `PodSecurityBlocked`, `ActionsDisabled`, `InvalidConfig`, `ForgejoNotFound` or the reason of `Registered` |
| `Degraded` | `AsExpected`; `ForgejoAPIUnavailable`, `UnregisterFailed` |

Events on the `ForgejoRunner`: `Registered`, `Unregistered`, `RunnerMissing`, `RegistrationFailed`,
`UnregisterFailed`, `ForgejoAPIUnavailable`, `PodSecurityBlocked`.

### Metrics and alert

Labels: `namespace`, `name` (the `Forgejo`) and `runner` (the `ForgejoRunner`).

| Metric | Meaning |
|--------|---------|
| `forgejo_operator_runners_desired` | `spec.replicas` |
| `forgejo_operator_runners_registered` | Runners recorded as registered (without the missing ones) |
| `forgejo_operator_runners_online` | Runners Forgejo reports online; absent while its API cannot be read |
| `forgejo_operator_runner_jobs_waiting` | Waiting jobs in the runners' scope whose `runs-on` their labels satisfy; absent while the API cannot be read |

The instance's PrometheusRule has **`ForgejoRunnersOffline`** (warning, for 10m): jobs wait for a
`ForgejoRunner` with `replicas > 0`, and none of its runners is online.

## Security

- Runner secrets live only in `<name>-tokens` and the pods' file mounts. The registration Job
  mounts only the secrets it registers. Status holds UUIDs and Forgejo ids, which are not secret.
- The operator never execs into pods; it creates one short-lived Job per registration and talks
  to Forgejo's API as the instance admin.
- Runner and registration pods do not mount a service account token and have no service links
  in their environment.
- The Docker executor needs a privileged pod. Give jobs the daemon's socket (`mountSocket`) only for
  repositories you trust; scope runners that do (`scope.organization` or `scope.repository`).
- Runner pods connect to `<forgejo>-http` on the HTTP port; with NetworkPolicies, allow that, and
  allow the operator to reach the same Service (it already does for health checks).

## Troubleshooting

```console
$ kubectl describe fjrn ci                     # conditions and events
$ kubectl logs ci-0 -c runner                  # runner: connection, registration, jobs
$ kubectl logs ci-0 -c docker                  # the Docker daemon
$ kubectl logs job/ci-register                 # a registration in progress or failed
$ kubectl get events --field-selector involvedObject.name=ci   # StatefulSet FailedCreate (Pod Security)
```

| Symptom | Cause and fix |
|---------|---------------|
| `Ready=False/PodSecurityBlocked`, no pods | The namespace enforces `baseline` or `restricted`: label it `pod-security.kubernetes.io/enforce=privileged`, or use `executor: Host` |
| `Registered=False/WaitingForForgejo` | The instance is not Ready, upgrading, restoring or paused; registration starts when it is back |
| `Registered=False/RegistrationFailed` | The message has the command's error, e.g. `user does not exist` for a wrong `scope.organization` or `repository does not exist`; fix `scope`; a new attempt starts a minute later |
| Runner log `unauthenticated: unregistered runner` | The runner was deleted in Forgejo; the operator registers it again at its next check, within a minute or two (`RunnerMissing`) |
| `Ready=False/RunnersOffline` | Forgejo has not heard from the runners for a minute: see the runner log (instance unreachable, DNS, NetworkPolicy) |
| `Degraded=True/ForgejoAPIUnavailable` | The admin credentials do not work for the API (password changed in the UI, 2FA, basic auth off) or the operator cannot reach `<forgejo>-http` |
| Jobs stay `waiting` | No runner has a label named like `runs-on`, or the repository is outside the runners' `scope`; `forgejo_operator_runner_jobs_waiting` counts them |
| `actions/checkout` fails with `node: not found` | A Docker label image or `host.image` without node; use a node image |
| `actions/checkout` cannot connect | `ROOT_URL` is not reachable from the job container, or no egress to `DEFAULT_ACTIONS_URL` |
| `Cannot connect to the Docker daemon` in a step | `docker.mountSocket` is off (it is off by default) |
| Image pulls hit rate limits | Use the mirrors under `data.forgejo.org/oci/` in labels, or `docker.args: [--registry-mirror=...]` |
| Job containers stall on large transfers | The cluster network's MTU is below 1500: `docker.args: ["--mtu=1400"]` |

## Limitations

- **Ephemeral runners and queue-driven autoscaling** are not implemented. Forgejo 15 and 17 list
  waiting jobs with a handle (`GET /api/v1/admin/actions/runners/jobs`), and `forgejo-runner
  one-job --handle` runs exactly that job, so a later version can start one ephemeral pod per job.
  Meanwhile `replicas` can be driven by an HPA or KEDA through the scale subresource.
- **Revoking without the API.** When the API cannot be used, scaled-away and deleted runners stay
  registered (offline) and their secrets valid until they are deleted in Forgejo.
- **No Kubernetes job backend.** Forgejo Runner has no backend that starts a pod per job (the
  upstream proposal was closed in favour of plugins); LXC does not run on Kubernetes.
