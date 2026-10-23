<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklist: Forgejo Actions runners (namespace `e2e-runners`)

Shared facts:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- `docker` commands must reach the docker engine the local cluster runs on, so that pulled and built images are visible to the cluster; set `DOCKER_CONTEXT` if that is not your current docker context.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`) with the ForgejoRunner build already deployed. Do not modify or redeploy it. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep e2e-runners`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`) and `ForgejoRunner` (short `fjrn`). Behaviour is documented in `docs/actions-runners.md` (read it first).
- Objects of a ForgejoRunner `<r>`: StatefulSet `<r>` (pods `<r>-0`, `<r>-1`, ..., containers `docker` (native sidecar, Docker executor only) and `runner`), Secret `<r>-tokens` (keys `token-<i>`, the runner secrets), Secret `<r>-runner` (keys `config-<i>.yaml`), Job `<r>-register` (exists only while registering, or for 1 min after a failure). All carry the label `forgejo.lucas.icu/runner=<r>`. Runner `i` is named `<r>-<i>` in Forgejo.
- Instances: `f17` (`codeberg.org/forgejo/forgejo:17`, with object storage in the Garage lab) and `f15` (`codeberg.org/forgejo/forgejo:15`), both in `e2e-runners`. Instance `<N>` serves HTTP at `http://<N>-http.e2e-runners.svc.cluster.local:3000`; the Mac resolves these names directly.
- **ADMIN(N)** (admin credentials; passwords only contain URL-safe characters):
  ```sh
  NS=e2e-runners; N=<name>
  U=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.username}' | base64 -d)
  P=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.password}' | base64 -d)
  B=http://$N-http.$NS.svc.cluster.local:3000
  ```
- Images in the image store of the docker engine the local cluster runs on (no registry needed): `codeberg.org/forgejo/forgejo:15`, `codeberg.org/forgejo/forgejo:17`, `data.forgejo.org/forgejo/runner:13.2.0`, `docker.io/library/docker:29.9.2-dind`, `data.forgejo.org/oci/node:22-bookworm` (the Host executor's image). Check with `docker images`; pull a missing one with `docker pull <image>`. Job containers (`data.forgejo.org/oci/node:22-bookworm`, `docker.io/library/docker:29.9.2-cli`, `data.forgejo.org/oci/alpine:3.22`) and the actions (`https://data.forgejo.org/actions/checkout`) are pulled from the internet by each runner pod's own Docker daemon (or the runner) on first use: allow up to 3 min for the first job of a runner pod.
- Prometheus: `http://kps-kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090` (`/api/v1/query?query=...` with `--data-urlencode`, `/api/v1/rules`, `/api/v1/alerts`). Operator metrics are scraped every 30-60 s.
- Garage (S3) lab: namespace `s3-lab`, see `docs/object-storage.md` "Local lab". Listing a prefix from the Mac:
  ```sh
  KEY=$(kubectl -n s3-lab get secret s3-credentials -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
  SEC=$(kubectl -n s3-lab get secret s3-credentials -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)
  curl -s --aws-sigv4 aws:amz:garage:s3 --user "$KEY:$SEC" \
    "http://garage.s3-lab.svc.cluster.local:3900/forgejo?list-type=2&prefix=<prefix>" | grep -o '<Key>[^<]*</Key>'
  ```
- Repository root: the repository root (read only; do not edit files, do not run git commands that change state).
- Wait with bounded `until` loops (poll every 3-10 s, explicit timeout) or the Monitor tool. Never wait without a timeout. A fresh Forgejo install can take up to 8 min while others start.
- Work only in namespace `e2e-runners` (reading `s3-lab` Secrets and querying Garage and Prometheus is fine). Other namespaces belong to other work: do not touch them.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed). On failure also collect: `kubectl -n e2e-runners get fjrn <r> -o yaml` (status part), `kubectl -n e2e-runners describe fjrn <r> | tail -20`, `kubectl -n e2e-runners get pods,jobs,events --sort-by=.lastTimestamp | tail -30`, `kubectl -n e2e-runners logs <r>-0 -c runner --tail=50`, relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

Reusable procedures (referenced below; run ADMIN(N) first):
- **RUNNERS(N)**: Forgejo's own list of runners, including organization and repository runners (`visible=true` is required for those):
  ```sh
  curl -s -u "$U:$P" "$B/api/v1/admin/actions/runners?visible=true&limit=50" \
    | jq -r '.[] | "\(.id) \(.name) \(.status) owner=\(.owner_id) repo=\(.repo_id) labels=\(.labels|join(","))"'
  ```
  `status` is `idle` or `active` for an online runner, `offline` otherwise.
- **FJRN-READY(r, timeout)**: poll `kubectl -n e2e-runners get fjrn <r> -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}'` until `RunnersOnline`; then record `kubectl -n e2e-runners get fjrn <r>` (columns `DESIRED READY ONLINE REGISTERED STATUS`).
- **REPO(N, owner, repo)**: for `owner=$U`: `curl -s -o /dev/null -w '%{http_code}\n' -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/user/repos -d '{"name":"<repo>","auto_init":true}'`; for an organization: `... -X POST $B/api/v1/orgs/<owner>/repos -d '{"name":"<repo>","auto_init":true}'`. Expect `201`.
- **WORKFLOW(N, owner, repo, file)**: write the workflow YAML given in the check to a fresh temp file `<file>` under your scratch area (quoted heredoc, so nothing is expanded), then
  ```sh
  curl -s -o /dev/null -w '%{http_code}\n' -u "$U:$P" -H 'Content-Type: application/json' \
    -X POST "$B/api/v1/repos/<owner>/<repo>/contents/.forgejo/workflows/ci.yml" \
    -d "{\"message\":\"ci\",\"content\":\"$(base64 < <file> | tr -d '\n')\"}"
  ```
  Expect `201`. The commit triggers the workflow (`on: [push]`).
- **RUN-WAIT(N, owner, repo)**: poll `curl -s -u "$U:$P" "$B/api/v1/repos/<owner>/<repo>/actions/runs" | jq -c '.workflow_runs[0] | {index_in_repo, status}'` every 5 s until `status` is `success` or `failure` (timeout 6 min). Record the sequence of statuses and `index_in_repo` (call it `RUN`).
- **LOGS(N, owner, repo, RUN, job)**: the log of job number `<job>` (0 for the first job of the workflow, 1 for the second), through the web route, which accepts basic authentication on Forgejo 15 and 17: `curl -s -u "$U:$P" "$B/<owner>/<repo>/actions/runs/$RUN/jobs/<job>/attempt/1/logs"`. Lines start with a timestamp ending in `Z`.
- **CI workflow** (used by R4 and R12):
  ```yaml
  on: [push]
  jobs:
    checkout:
      runs-on: docker
      steps:
        - uses: actions/checkout@v4
        - run: cat README.md && node --version && echo checkout-ok
    dind:
      runs-on: docker
      container:
        image: docker.io/library/docker:29.9.2-cli
      steps:
        - run: |
            printf 'FROM data.forgejo.org/oci/alpine:3.22\nRUN echo built-in-dind > /m\nCMD cat /m\n' > Dockerfile
            docker build -t e2e-img .
            docker run --rm e2e-img
  ```
- **SECRET-SCAN(r)**: no runner secret of `<r>` appears anywhere but in Secret `<r>-tokens`:
  ```sh
  kubectl -n e2e-runners get secret <r>-tokens -o json | jq -r '.data[] | @base64d' > <tmp>/tokens
  { kubectl -n e2e-runners get pods,sts,jobs -l forgejo.lucas.icu/runner=<r> -o yaml
    kubectl -n e2e-runners get fjrn <r> -o yaml
    kubectl -n e2e-runners get secret <r>-runner -o json | jq -r '.data[] | @base64d'
    kubectl -n e2e-runners get events -o yaml
    for p in $(kubectl -n e2e-runners get pods -l forgejo.lucas.icu/runner=<r> -o name); do
      kubectl -n e2e-runners logs $p --all-containers; done
    kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=2h
  } > <tmp>/scan 2>&1
  while read -r t; do grep -c -- "$t" <tmp>/scan; grep -c -- "${t:16}" <tmp>/scan; done < <tmp>/tokens
  ```
  Every count must be `0` (`grep -c` exits 1 then; that is the expected outcome). `<tmp>/tokens` has one 40-hex line per runner.

---

## R0 — Prerequisites

1. **R0.1 images**: all five images listed in the shared facts are present (pull missing ones). Record `docker image inspect data.forgejo.org/forgejo/runner:13.2.0 --format '{{index .Config.Labels "org.opencontainers.image.version"}}'` → `13.2.0`.
2. **R0.2 CRD**: `kubectl get crd forgejorunners.forgejo.lucas.icu -o jsonpath='{.spec.names.shortNames} {.spec.names.categories} {.spec.versions[0].subresources.scale.specReplicasPath}'` → `["fjrn"] ["forgejo"] .spec.replicas`; `kubectl api-resources | grep forgejorunners` shows `fjrn`.
3. **R0.3 namespace**: `kubectl create namespace e2e-runners`; `kubectl label namespace e2e-runners pod-security.kubernetes.io/enforce=privileged` (the Docker executor runs a privileged daemon).
4. **R0.4 S3 credentials**: `kubectl -n s3-lab get secret s3-credentials -o json | jq '{apiVersion, kind, type, data, metadata: {name: .metadata.name}}' | kubectl -n e2e-runners apply -f -`. If `s3-lab` does not exist, run `hack/s3-lab/setup.sh` first (it only touches `s3-lab`).
5. **R0.5 instances**: apply
   ```yaml
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: Forgejo
   metadata: {name: f17, namespace: e2e-runners}
   spec:
     image: codeberg.org/forgejo/forgejo:17
     database: {cnpg: {instances: 1, storage: {size: 1Gi}}}
     storage: {size: 2Gi}
     objectStorage:
       s3:
         endpoint: http://garage.s3-lab.svc.cluster.local:3900
         bucket: forgejo
         region: garage
         pathStyle: true
         prefix: e2e-runners-f17
         credentialsSecret: {name: s3-credentials}
   ---
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: Forgejo
   metadata: {name: f15, namespace: e2e-runners}
   spec:
     image: codeberg.org/forgejo/forgejo:15
     database: {cnpg: {instances: 1, storage: {size: 1Gi}}}
     storage: {size: 2Gi}
   ```
   Within 8 min both have `status.phase=Running` and `Ready=True`; `status.version` of `f17` starts with `17.`, of `f15` with `15.`; `f17` has `ObjectStorageReady=True`.

## R1 — Validation and defaults

1. **R1.1 defaults**: a server dry-run (`kubectl apply --dry-run=server -o jsonpath='{.spec}' -f -`) of `{apiVersion: forgejo.lucas.icu/v1alpha1, kind: ForgejoRunner, metadata: {name: defaults, namespace: e2e-runners}, spec: {forgejoRef: {name: f17}}}` is refused with `spec.executor: Required value` (no default: the privileged Docker executor must be asked for). The same with `spec: {forgejoRef: {name: f17}, executor: Docker}` prints `"capacity":1`, `"executor":"Docker"`, `"image":"data.forgejo.org/forgejo/runner:13.2.0"`, `"replicas":1`.
2. **R1.2 label forms**: server dry-runs are refused with these messages: `executor: Host` with `labels: ["docker:docker://node:22"]` → `labels of the Host executor have the form name:host`; `executor: Docker` with `labels: ["docker"]` → `labels of the Docker executor have the form name:docker://image`.
3. **R1.3 scope**: `executor: Docker`, `scope: {organization: a, repository: a/b}` → refused with `set at most one of organization and repository`.
4. **R1.4 missing instance**: apply ForgejoRunner `ghost` with `forgejoRef: {name: nosuch}`, `executor: Docker`. Within 1 min `Ready` and `Registered` are `False` with reason `ForgejoNotFound`; no StatefulSet `ghost` exists. `kubectl -n e2e-runners delete fjrn ghost` completes within 30 s.

## R2 — Forgejo 17: Docker runners, replicas 2

Apply:
```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: ForgejoRunner
metadata: {name: d17, namespace: e2e-runners}
spec:
  forgejoRef: {name: f17}
  executor: Docker
  replicas: 2
  docker: {mountSocket: true}
```

1. **R2.1 registration first**: right after apply, poll every 1 s for 30 s: `kubectl -n e2e-runners get fjrn d17 -o jsonpath='{.status.conditions[?(@.type=="Registered")].reason}'` together with `kubectl -n e2e-runners get sts d17 -o jsonpath='{.spec.replicas}'`. At least one poll shows `Registering` (Job `d17-register` registers), and every poll that shows `Registering` shows replicas `0` (or no StatefulSet yet): pods start only once their runners are registered.
2. **R2.2 ready**: FJRN-READY(d17, 4 min): `DESIRED 2`, `READY 2`, `ONLINE 2`, `REGISTERED True`, `STATUS RunnersOnline`. `kubectl -n e2e-runners get pods -l forgejo.lucas.icu/runner=d17` shows `d17-0` and `d17-1`, both `2/2 Running`. Job `d17-register` no longer exists (`kubectl -n e2e-runners get job d17-register` → NotFound).
3. **R2.3 Forgejo's view**: ADMIN(f17); RUNNERS(f17) lists exactly two runners named `d17-0` and `d17-1`, each `idle`, `owner=0 repo=0`, `labels=docker`. Their ids equal `kubectl -n e2e-runners get fjrn d17 -o jsonpath='{range .status.runners[*]}{.name}={.id} {end}'`, and the `uuid`s from `.../admin/actions/runners?visible=true` (`jq -r '.[] | .uuid'`) equal `.status.runners[*].uuid`.
4. **R2.4 one registration**: `kubectl -n e2e-runners get events --field-selector involvedObject.name=d17,reason=Registered -o jsonpath='{range .items[*]}{.message}{"\n"}{end}'` → exactly one line, `Registered runners d17-0, d17-1 with Forgejo f17 for the whole instance`. `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep 'Created registration Job' | grep -c '"name": "d17"'` → `1`.
5. **R2.5 pod layout**: `kubectl -n e2e-runners get pod d17-0 -o json | jq '{init: [.spec.initContainers[] | {name, restartPolicy, privileged: .securityContext.privileged, command}], runner: (.spec.containers[] | {name, user: .securityContext.runAsUser, ro: .securityContext.readOnlyRootFilesystem, env: [.env[].name]})}'`: init container `docker` with `restartPolicy: Always`, `privileged: true` and a command containing `--host=unix:///var/run/dind/docker.sock` and no `tcp://`; container `runner` with user `1000`, `ro: true`, env exactly `POD_INDEX`, `HOME`, `TMPDIR`, `DOCKER_HOST`. `.spec.automountServiceAccountToken` is `false`.
6. **R2.6 configuration**: `kubectl -n e2e-runners get secret d17-runner -o jsonpath='{.data.config-1\.yaml}' | base64 -d` contains `token_url: file:/etc/forgejo-runner/tokens/token-1`, `uuid: <the uuid of d17-1>`, `url: http://f17-http.e2e-runners.svc.cluster.local:3000/`, `docker_host: automount`, and no inline secret: `... | grep -cE '^\s*token:'` → `0`.

## R3 — Forgejo 17: a workflow on the Docker runners

1. **R3.1 run**: ADMIN(f17); REPO(f17, $U, ci); WORKFLOW(f17, $U, ci, CI workflow); RUN-WAIT(f17, $U, ci) → `success` within 6 min.
2. **R3.2 checkout**: LOGS(f17, $U, ci, RUN, 0) contains `checkout-ok` and a node version line (`v22.`), and a line `Runner <uuid> ... received task` whose uuid is one of `.status.runners[*].uuid` of `d17`.
3. **R3.3 Docker in the job**: LOGS(f17, $U, ci, RUN, 1) | `grep -E 'Z built-in-dind$'` → at least one line (the output of `docker run`), and the log contains `naming to docker.io/library/e2e-img` (or `writing image`) from `docker build`.

## R4 — Forgejo 17: logs and artifacts in object storage (optional)

1. **R4.1 artifact**: REPO(f17, $U, art); WORKFLOW(f17, $U, art, file) with
   ```yaml
   on: [push]
   jobs:
     art:
       runs-on: docker
       steps:
         - run: mkdir out && echo artifact-content-e2e > out/result.txt
         - uses: actions/upload-artifact@v3
           with: {name: result, path: out/result.txt}
   ```
   RUN-WAIT → `success` (use `@v3`: `upload-artifact@v4` fails on Forgejo with `GHESNotSupportedError`). `curl -s -u "$U:$P" $B/api/v1/repos/$U/art/actions/artifacts | jq -c '.artifacts // . | map({name, size_in_bytes})'` lists `result` with a size > 0.
2. **R4.2 bucket**: listing prefix `e2e-runners-f17/actions_artifacts/` returns at least one key, and prefix `e2e-runners-f17/actions_log/` returns keys ending in `.log.zst` (the logs of R3 and R4.1).

## R5 — Forgejo 17: metrics and the offline alert

1. **R5.1 metrics**: within 3 min of R2.2, Prometheus returns for `{namespace="e2e-runners", runner="d17"}`: `forgejo_operator_runners_desired` = 2, `forgejo_operator_runners_registered` = 2, `forgejo_operator_runners_online` = 2, `forgejo_operator_runner_jobs_waiting` = 0, each with label `name="f17"`.
2. **R5.2 rule**: `kubectl -n e2e-runners get prometheusrule f17 -o yaml` contains an alert `ForgejoRunnersOffline` with `for: 10m`, `severity: warning` and an expr that starts with `max by (namespace, name, runner) (forgejo_operator_runner_jobs_waiting{namespace="e2e-runners", name="f17"}) > 0`; Prometheus `/api/v1/rules` lists it in group `forgejo.e2e-runners.f17` (allow 3 min).
3. **R5.3 offline with waiting jobs**: make the runner pods unschedulable and recreate them:
   `kubectl -n e2e-runners patch fjrn d17 --type merge -p '{"spec":{"podTemplate":{"nodeSelector":{"e2e-runners/unschedulable":"true"}}}}'` then `kubectl -n e2e-runners delete pods -l forgejo.lucas.icu/runner=d17 --wait=false`. REPO(f17, $U, waiting); WORKFLOW(f17, $U, waiting, file) with
   ```yaml
   on: [push]
   jobs:
     wait:
       runs-on: docker
       steps:
         - run: echo waited
   ```
   Within 3 min: both pods `Pending`; `Ready=False` with reason `PodsNotReady` (message `0 of 2 runner pods are ready`); RUNNERS(f17) shows `d17-0` and `d17-1` `offline`; Prometheus `forgejo_operator_runners_online{runner="d17"}` = 0 and `forgejo_operator_runner_jobs_waiting{runner="d17"}` = 1. Within 5 min `/api/v1/alerts` has `ForgejoRunnersOffline` with labels `forgejo="f17"`, `runner="d17"` and state `pending` (it fires after 10 min; do not wait for that).
4. **R5.4 recover**: `kubectl -n e2e-runners patch fjrn d17 --type json -p '[{"op":"remove","path":"/spec/podTemplate/nodeSelector"}]'`; `kubectl -n e2e-runners delete pods -l forgejo.lucas.icu/runner=d17 --wait=false`. FJRN-READY(d17, 4 min); RUN-WAIT(f17, $U, waiting) → `success`; within 3 min `forgejo_operator_runners_online{runner="d17"}` = 2 and the alert is gone from `/api/v1/alerts`. No new `Registered` event for `d17` (the runners kept their registrations).

## R6 — Forgejo 17: scale 2 → 1 leaves no ghost

1. **R6.1 scale down**: record the ids and uuids of `d17` (`.status.runners`), the uid of pod `d17-0` (`kubectl -n e2e-runners get pod d17-0 -o jsonpath='{.metadata.uid}'`) and the StatefulSet's `.status.updateRevision`. `kubectl -n e2e-runners scale fjrn d17 --replicas=1`. Within 2 min: only pod `d17-0` exists; RUNNERS(f17) lists exactly one runner whose name starts with `d17-`, namely `d17-0` (`idle`), and no runner with the old id of `d17-1`; `.status.runners` has one entry (`d17-0`); `kubectl -n e2e-runners get secret d17-tokens -o json | jq -c '.data | keys'` → `["token-0"]`; an event `Unregistered` for `d17` with message `Unregistered runners d17-1 (id <old id>) from Forgejo f17`; FJRN-READY shows `DESIRED 1 READY 1 ONLINE 1`.
2. **R6.2 scale up again**: `kubectl -n e2e-runners scale fjrn d17 --replicas=2`. FJRN-READY(d17, 4 min) with `ONLINE 2`. The new `d17-1` has a uuid and an id different from the ones recorded in R6.1 (a new secret); RUNNERS(f17) lists exactly `d17-0` and `d17-1`, both online. Pod `d17-0` still has the uid recorded in R6.1 and `restartCount` 0 in both containers, and the StatefulSet's `.status.updateRevision` is the one recorded in R6.1 (scaling down and up changed nothing on the pod template, so the running runner was not rolled).
3. **R6.3 a runner deleted in Forgejo**: record the uids of pods `d17-0` and `d17-1` and the id of `d17-0`. ADMIN(f17); `curl -s -o /dev/null -w '%{http_code}\n' -u "$U:$P" -X DELETE $B/api/v1/admin/actions/runners/<id of d17-0>` → `204`. Within 2 min: a Warning event `RunnerMissing` for `d17` names `d17-0`; `.status.runners[0]` shows (at some poll) `missing: true` or already a new id; then RUNNERS(f17) lists `d17-0` again with the same uuid as before and a new id, equal to `.status.runners[0].id`, and `.status.runners[0].missing` is absent. FJRN-READY(d17, 4 min) with `ONLINE 2`. Both pods keep the uids recorded at the start of this check (the operator did not stop the healthy `d17-1`, nor `d17-0`: its runner reconnects under the same uuid).

## R7 — Forgejo 17: an organization-scoped runner

1. **R7.1 organization**: ADMIN(f17); `curl -s -o /dev/null -w '%{http_code}\n' -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/orgs -d '{"username":"acme"}'` → `201`; REPO(f17, acme, app); REPO(f17, $U, notacme); record `ORG=$(curl -s -u "$U:$P" $B/api/v1/orgs/acme | jq .id)`.
2. **R7.2 runner**: apply ForgejoRunner `acme17` with `forgejoRef: {name: f17}`, `executor: Docker`, `replicas: 1`, `scope: {organization: acme}`, `labels: ["acme:docker://data.forgejo.org/oci/node:22-bookworm"]`. FJRN-READY(acme17, 4 min); the `Registered` message is `1 runner registered with Forgejo f17 for organization acme`; RUNNERS(f17) lists `acme17-0` with `owner=$ORG repo=0 labels=acme`; `.status.runners[0].scope` = `acme`.
3. **R7.3 own job**: WORKFLOW(f17, acme, app, file) with
   ```yaml
   on: [push]
   jobs:
     org:
       runs-on: acme
       steps:
         - uses: actions/checkout@v4
         - run: echo org-scope-ok
   ```
   RUN-WAIT(f17, acme, app) → `success`; LOGS(f17, acme, app, RUN, 0) contains `org-scope-ok` and `Runner <uuid of acme17-0>`.
4. **R7.4 other jobs untouched**: WORKFLOW(f17, $U, notacme, same file). After 2 min the run of `$U/notacme` is still `waiting` (`.../repos/$U/notacme/actions/runs` → `status` `waiting`), and `curl -s -u "$U:$P" "$B/api/v1/admin/actions/runners/jobs?labels=acme" | jq -c 'map({name, status})'` shows it `waiting`. Prometheus `forgejo_operator_runner_jobs_waiting{runner="acme17"}` = 0 (the waiting job is outside the runner's scope). Then delete the repository (`curl -s -o /dev/null -w '%{http_code}\n' -u "$U:$P" -X DELETE $B/api/v1/repos/$U/notacme` → `204`). Keep `acme17` for R15.

## R8 — Forgejo 17: secrets stay secret

1. **R8.1 running runners**: SECRET-SCAN(d17): all counts `0`. No `command`, `args` or `env` value of the runner pods has 40 hex characters: `kubectl -n e2e-runners get pods -l forgejo.lucas.icu/runner=d17 -o json | jq -r '.items[].spec | (.containers + (.initContainers // [])) | .[] | (.command // [] | join(" ")), (.args // [] | join(" ")), (.env // [] | map(.value // "") | join(" "))' | grep -cE '[0-9a-f]{40}'` → `0`.
2. **R8.2 a failing registration**: apply ForgejoRunner `badscope17` with `forgejoRef: {name: f17}`, `executor: Docker`, `scope: {organization: no-such-org}`. Within 2 min `Registered=False` with reason `RegistrationFailed` and a message containing `Could not register runner badscope17-0` and `user does not exist`; one Warning event `RegistrationFailed`; no pod `badscope17-0` exists (StatefulSet at 0 replicas). While Job `badscope17-register` still exists (it is kept 1 min after failing; if it is gone, wait for the next attempt within 2 min): `kubectl -n e2e-runners get job badscope17-register -o json | jq -c '.spec.template.spec.containers[0].env'` holds only `GITEA_*`, `HOME`, `USER`, `TMPDIR`, `RUNNERS` (`0::badscope17-0`, i.e. `<index>:<version>:<name>` with an empty version for a new runner) and `RUNNER_SCOPE` (`no-such-org`); its tokens volume has items `[token-0]`; the container runs as `1000` with `readOnlyRootFilesystem: true`, uses image `codeberg.org/forgejo/forgejo:17`, and mounts Secret `f17-config`. SECRET-SCAN(badscope17) (the scan includes `kubectl logs` of the Job's pods): all counts `0`.
3. **R8.3 cleanup**: `kubectl -n e2e-runners delete fjrn badscope17` completes within 1 min; no Job, Secret or StatefulSet with `forgejo.lucas.icu/runner=badscope17` remains.

## R9 — Forgejo 17: deleting a ForgejoRunner unregisters its runners

1. **R9.1 delete**: record RUNNERS(f17). `kubectl -n e2e-runners delete fjrn d17 --timeout=180s` returns within 3 min. Then: RUNNERS(f17) lists no runner whose name starts with `d17-` (only `acme17-0` remains); `kubectl -n e2e-runners get sts,secret,job,pod -l forgejo.lucas.icu/runner=d17` → `No resources found`; an event `Unregistered` for `d17` names `d17-0` and `d17-1` (`Unregistered runners d17-0 (id ...), d17-1 (id ...) from Forgejo f17`); Prometheus has no `forgejo_operator_runners_desired{runner="d17"}` series within 3 min.

## R10 — Forgejo 15: Docker runners, replicas 2

Apply ForgejoRunner `d15`: `forgejoRef: {name: f15}`, `executor: Docker`, `replicas: 2`, `docker: {mountSocket: true}`.

1. **R10.1 ready**: FJRN-READY(d15, 4 min): `DESIRED 2 READY 2 ONLINE 2 REGISTERED True RunnersOnline`; Job `d15-register` is gone; exactly one event `Registered` for `d15`: `Registered runners d15-0, d15-1 with Forgejo f15 for the whole instance`.
2. **R10.2 Forgejo's view**: ADMIN(f15); RUNNERS(f15) lists exactly `d15-0` and `d15-1`, `idle`, `labels=docker`, ids equal to `.status.runners[*].id`.

## R11 — Forgejo 15: a workflow on the Docker runners

1. **R11.1 run**: REPO(f15, $U, ci); WORKFLOW(f15, $U, ci, CI workflow); RUN-WAIT(f15, $U, ci) → `success` within 6 min.
2. **R11.2 logs**: LOGS(f15, $U, ci, RUN, 0) contains `checkout-ok`; LOGS(f15, $U, ci, RUN, 1) | `grep -E 'Z built-in-dind$'` → at least one line.

## R12 — Forgejo 15: scale 2 → 1 leaves no ghost

1. **R12.1**: as R6.1 for `d15` on `f15`: after `kubectl -n e2e-runners scale fjrn d15 --replicas=1`, within 2 min RUNNERS(f15) lists exactly one `d15-*` runner (`d15-0`), the `Unregistered` event names `d15-1`, `d15-tokens` has only `token-0`.

## R13 — Forgejo 15: an organization-scoped runner

1. **R13.1**: ADMIN(f15); create organization `acme`, repositories `acme/app` and `$U/notacme` (as R7.1); apply ForgejoRunner `acme15` (`forgejoRef: {name: f15}`, `executor: Docker`, `scope: {organization: acme}`, `labels: ["acme:docker://data.forgejo.org/oci/node:22-bookworm"]`); FJRN-READY(acme15, 4 min); RUNNERS(f15) shows `acme15-0` with `owner=<id of acme>`.
2. **R13.2**: the R7.3 workflow in `acme/app` → `success`; the same workflow in `$U/notacme` is still `waiting` after 2 min. Delete `$U/notacme` (204), then `kubectl -n e2e-runners delete fjrn acme15`; RUNNERS(f15) lists no `acme15-*` runner.

## R14 — Forgejo 15: deleting a ForgejoRunner unregisters its runners

1. **R14.1**: `kubectl -n e2e-runners delete fjrn d15 --timeout=180s` returns within 3 min; RUNNERS(f15) is empty (`[]` from the raw API); no object with `forgejo.lucas.icu/runner=d15` remains.

## R15 — Host executor in a namespace that enforces `restricted` (17 and 15)

1. **R15.1 restricted**: `kubectl label namespace e2e-runners pod-security.kubernetes.io/enforce=restricted --overwrite` (kubectl warns about the running privileged pod of `acme17`; existing pods keep running).
2. **R15.2 host runners**: apply
   ```yaml
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: ForgejoRunner
   metadata: {name: h17, namespace: e2e-runners}
   spec:
     forgejoRef: {name: f17}
     executor: Host
     labels: ["host17:host"]
   ---
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: ForgejoRunner
   metadata: {name: h15, namespace: e2e-runners}
   spec:
     forgejoRef: {name: f15}
     executor: Host
     labels: ["host15:host"]
   ```
   FJRN-READY(h17, 5 min) and FJRN-READY(h15, 5 min) (the toolchain image is large). No `FailedCreate` event for `statefulset/h17` or `statefulset/h15`. `kubectl -n e2e-runners get pod h17-0 -o json | jq '{nonroot: .spec.securityContext.runAsNonRoot, user: .spec.securityContext.runAsUser, seccomp: .spec.securityContext.seccompProfile.type, containers: [(.spec.initContainers + .spec.containers)[] | {name, image, ape: .securityContext.allowPrivilegeEscalation, drop: .securityContext.capabilities.drop, privileged: .securityContext.privileged}]}'` → `nonroot: true`, `user: 1000`, `seccomp: RuntimeDefault`; `install-runner` (image `data.forgejo.org/forgejo/runner:13.2.0`) and `runner` (image `data.forgejo.org/oci/node:22-bookworm`) both with `ape: false`, `drop: ["ALL"]`, `privileged: null`. RUNNERS(f17) lists `h17-0` with `labels=host17`; RUNNERS(f15) lists `h15-0` with `labels=host15`.
3. **R15.3 host jobs**: for (N, label) in (f17, host17) and (f15, host15): ADMIN(N); REPO(N, $U, hostci); WORKFLOW(N, $U, hostci, file) with
   ```yaml
   on: [push]
   jobs:
     host:
       runs-on: <label>
       steps:
         - uses: actions/checkout@v4
         - run: echo "uid=$(id -u)" && node --version && cat README.md && echo host-ok
   ```
   RUN-WAIT → `success`; LOGS(N, $U, hostci, RUN, 0) contains `uid=1000` and `host-ok`.
4. **R15.4 Docker executor blocked**: `kubectl -n e2e-runners delete pod acme17-0`. Within 2 min: no pod `acme17-0` exists; an event `FailedCreate` on `statefulset/acme17` contains `violates PodSecurity "restricted:latest"`; `kubectl -n e2e-runners get fjrn acme17 -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'` has `status: False`, `reason: PodSecurityBlocked` and a message that names `"restricted"` and `pod-security.kubernetes.io/enforce=privileged`; one Warning event `PodSecurityBlocked` for `acme17`. Within 2 more min RUNNERS(f17) shows `acme17-0` `offline`.
5. **R15.5 deletion while blocked**: `kubectl -n e2e-runners delete fjrn acme17 h17 h15 --timeout=180s` returns within 3 min; RUNNERS(f17) and RUNNERS(f15) list no runners at all.

## R16 — Cleanup

1. **R16.1**: `kubectl delete namespace e2e-runners --timeout=300s`; then the Garage prefix `e2e-runners-f17/` may stay (shared lab bucket; leave it). Prometheus has no `forgejo_operator_runners_desired{namespace="e2e-runners"}` series within 5 min.
