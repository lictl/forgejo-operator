# forgejo-operator: agent guide

Rules that are specific to this repository. General Go, Kubebuilder and Kubernetes
practice is assumed. [docs/design.md](docs/design.md) is the architecture contract;
[CONTRIBUTING.md](CONTRIBUTING.md) lists what a pull request needs.

## Layout

| Path | Contents |
|------|----------|
| `api/v1alpha1` | `Forgejo`, `ForgejoBackup`, `ForgejoRestore`, `ForgejoRunner` (group `forgejo.lucas.icu`) |
| `internal/controller` | Controllers; `forgejo_controller.go` holds the reconcile step table |
| `internal/forgejo` | Shared helpers: names, labels, `ReconcileContext`, version table, cache scopes, metrics |
| `internal/core` | Identity, secrets, database, storage, `app.ini`, workload, Services, routes, TLS, status |
| `internal/backup` | Backups, restores, retention and maintenance mode |
| `internal/upgrade` | Forgejo and PostgreSQL major upgrades, Forgejo rollback, version support |
| `internal/autopilot` | Valkey HA, scaling, tuning, self-healing, volume growth |
| `internal/observability` | ServiceMonitor, alert rules, dashboard, health and statistics probes |
| `internal/runner` | Objects and Forgejo API calls of a `ForgejoRunner` |
| `internal/rclone`, `internal/s3` | rclone crypt file format; minimal S3 bucket check |
| `config/samples` | Example of each kind, validated by `TestSamplesAreValid` |
| `charts/forgejo-operator` | Helm chart, maintained by hand |
| `test/e2e/checklists` | End-to-end checklists that an agent runs against a local cluster |
| `test/prometheus` | Rendered alert rules and their promtool unit tests |
| `docs/runbooks` | One runbook per alert |

## Generated and copied files

- `config/crd/bases/*`, `config/rbac/role.yaml` and `**/zz_generated.*.go` come from
  `make manifests generate`. `test/prometheus/rules/*` and `test/prometheus/unit-rules/*` come
  from `make test-rules`, or without docker from
  `go test ./internal/observability -run TestPrometheusRuleFiles -args -update-rules`.
  `PROJECT` comes from the kubebuilder CLI. Do not edit any of them by hand.
- `make test` runs `make manifests generate` first, and CI fails when that leaves the tree
  dirty, so commit what it regenerates.
- `charts/forgejo-operator/crds/` is a copy of `config/crd/bases/` (CI diffs them). Copy the
  files again after an API change.
- `charts/forgejo-operator/templates/rbac/manager-role.yaml` and `volume-usage.yaml` carry the
  rules of `config/rbac/role.yaml` and `config/rbac/volume_usage_role.yaml`. Nothing checks
  them, so update them by hand. Do not regenerate the chart with the Kubebuilder Helm plugin.
- `make deploy` and `make deploy-local` rewrite the image in
  `config/manager/kustomization.yaml`; do not commit that change.

## Design rules

Details and reasons are in [docs/design.md](docs/design.md).

- A `Forgejo` is reconciled by the step table in `internal/controller/forgejo_controller.go`.
  A step belongs to one feature package, names the steps it needs and passes outputs through
  `ReconcileContext`. It changes the status only in memory; the pipeline patches it once at the
  end of the pass. Keep the step table in docs/design.md in sync.
- The operator never deletes data. Data and backup PVCs, the CNPG Cluster, scheduled and
  pre-upgrade backups and the identity Secrets get an owner reference only with
  `spec.storage.retainPolicy: Delete`; manual backups and `<name>-backup-key` never do.
- Report, don't repair: when a repair could lose data or identity (identity Secrets, data
  PVC, CNPG Cluster, a PostgreSQL upgrade that may have changed the data), set `Broken` with
  `rc.SetBroken` and let a person act. Never generate secrets or create an empty volume or
  database for an instance that has run (`rc.IdentityInitialized`). A step clears only its own
  reasons (`rc.ClearBroken`); a step that backs up, prunes or grows volumes sets
  `waitWhileBroken`.
- Admission checks are OpenAPI markers and CEL rules in `api/v1alpha1`; there are no webhooks.
  Each rule gets a case in `api/v1alpha1/validation_test.go`, and an API change updates
  `config/samples`.
- Names come from `internal/forgejo/names.go` (`internal/runner/names.go` for runners). Child
  objects carry `forgejo.Labels` (`runner.Labels` for runners): Secrets, ConfigMaps, Pods,
  Jobs, Deployments, StatefulSets and Services without them are invisible to the cache
  (`forgejo.CacheScopes`). Frequent Lists select on `app.kubernetes.io/name`, or they go to
  the API server. Read through `rc.APIReader` where a stale cache could make a step create or
  generate something.
- CNPG, prometheus-operator, Gateway API, cert-manager and metrics-server are optional. Check
  `rc.HasAPI` or `rc.FirstAPI` before touching their objects, use local mirror types or
  `unstructured.Unstructured`, and add no Go module dependency on them.
- `forgejo.Versions` (`internal/forgejo/versions.go`) is the one table of Forgejo majors.
  Rendering code reads `rc.Layout.Traits` and never compares major numbers. When
  `make forgejo-versions` reports a stale table, follow
  [docs/versions.md](docs/versions.md) and the workflow
  `.claude/workflows/forgejo-new-version.js`.
- The bundled CNPG database stays a single instance; replicated PostgreSQL is
  `spec.database.type: External`.
- Every alert needs a runbook in `docs/runbooks/` and a firing and a non-firing promtool case in
  `test/prometheus/`.
- An RBAC marker change also updates [docs/permissions.md](docs/permissions.md).
  `internal/forgejo/rbac_test.go` checks the namespaced overlay and keeps `pods/exec` out of
  the manager role.
- Feature packages test with `testing` and the fake client; Ginkgo and envtest are used only
  in `internal/controller`.
- New log messages follow the
  [Kubernetes message style](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines).
- A behavior change updates, in the same change, the docs that describe it: the step table in
  docs/design.md, the feature page, docs/permissions.md, runbooks.

## Checks

| Change | Run |
|--------|-----|
| Go code | `make lint test` |
| API or RBAC markers | `make manifests generate`, then refresh the chart copies above |
| Alert rules | `make test-rules` (needs docker; GitHub CI does not run promtool) |
| Chart | `helm lint --strict charts/forgejo-operator` and `helm template forgejo-operator charts/forgejo-operator --include-crds` |

Unit tests and envtest touch no cluster. `make test-e2e` needs Kind and docker; it creates
its own Kind cluster and deletes it afterwards. The checklists and every target that deploys,
installs or uninstalls (`make run`, `install`, `deploy`, `deploy-local`, `helm-deploy` and
their counterparts) use the current kubeconfig. Do not trust the current context: export
`KUBECONFIG` to a file made with
`kubectl config view --minify --flatten --context <local cluster>`, so it holds only a
dedicated local cluster, never a real or production one.

## Review every chunk

When you finish implementing a chunk of work (a fix, a reconcile step, a feature slice:
anything you would commit on its own), review it before you commit it or start the next one:

1. Run the checks above.
2. Spawn a fresh sub-agent to review the chunk: a new agent for each chunk, not a fork of your
   conversation. Give it the base to diff against (new files included), what the change is
   meant to do and the parts of docs/design.md it must respect, but not your own conclusions
   about it. Tell it to report findings and not to edit files.
3. Fix the issues it finds. When a finding is wrong, leave the code and say why in your final
   message.
4. Run the checks again.

If you cannot spawn agents yourself (you are a sub-agent), say in your final message that the
chunk is not reviewed yet, so that the agent that launched you has it reviewed.

## Commits

Follow [docs/commits.md](docs/commits.md) (Conventional Commits). Mark a breaking change with
`!` and a `BREAKING CHANGE:` footer; release notes are drafted from these messages.
