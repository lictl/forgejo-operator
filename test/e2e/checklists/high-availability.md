<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklist: high availability (namespace `e2e-ha`)

What is checked (design in `docs/high-availability.md`): with `spec.highAvailability.enabled`
the server pods (Deployment `<name>`) run an app.ini without cron tasks (`app-web.ini`) and one
cron runner pod (StatefulSet `<name>-cron`, pod `<name>-cron-0`, never two at once) runs them; Valkey is a three-pod StatefulSet
`<name>-valkey` with a Sentinel in each pod, which Forgejo reaches through `redis+sentinel://`;
sessions stay in PostgreSQL; HA pods get default topology spread constraints; status, the
`ValkeyReady` condition, metrics and alerts report the cron runner and Valkey. Forgejo 17 is
covered in full, Forgejo 15 on its core path (H9).

Shared facts:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- `docker` commands must reach the docker engine the local cluster runs on, so that pulled and built images are visible to the cluster; set `DOCKER_CONTEXT` if that is not your current docker context.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`) with the high-availability build already deployed. Do not modify or redeploy it. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep e2e-ha`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`), `ForgejoBackup` (`fjb`), `ForgejoRestore` (`fjr`).
- The local cluster is a **single node** with `local-path` (ReadWriteOnce) storage. All replicas share the node's mount of the data volume, so multi-replica works; topology spreading is `ScheduleAnyway` and must not leave pods Pending. The node has no `topology.kubernetes.io/zone` label, so no zone constraint is expected.
- An instance `<name>` serves HTTP at `http://<name>-http.e2e-ha.svc.cluster.local:3000`; the Mac resolves service names and reaches pod IPs directly.
- Variables used below (set them per instance; passwords only contain URL-safe characters):
  ```sh
  NS=e2e-ha; N=<name>
  U=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.username}' | base64 -d)
  P=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.password}' | base64 -d)
  B=http://$N-http.$NS.svc.cluster.local:3000
  W=<a fresh temp dir under your scratch area>
  SEL="app.kubernetes.io/instance=$N"
  ```
  Server pods: `-l $SEL,app.kubernetes.io/component=server`; cron runner: `-l $SEL,app.kubernetes.io/component=cron`; Valkey: `-l $SEL,app.kubernetes.io/component=valkey`. **Always include the instance label**: several instances share the namespace.
- Never print the Valkey password (`<name>-secrets` key `valkey-password`) or the admin password into your evidence; mask them (`sed -E 's/:[0-9a-f]{48}@/:***@/; s/sentinelpassword=[0-9a-f]+/sentinelpassword=***/'`).
- Events: `kubectl -n $NS get events --field-selector involvedObject.name=$N,reason=<Reason> -o jsonpath='{range .items[*]}{.type} {.reason}: {.message}{"\n"}{end}'`.
- Prometheus: `http://kps-kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090` (`/api/v1/query?query=...` with `curl -sG --data-urlencode`, `/api/v1/alerts`). Operator metrics are scraped every 30-60 s; counters reset when the operator restarts.
- Images in the image store of the docker engine the local cluster runs on (no registry needed): `codeberg.org/forgejo/forgejo:15`, `codeberg.org/forgejo/forgejo:17`, `docker.io/valkey/valkey:8-alpine` (check with `docker images`; pull a missing one with `docker pull <image>`).
- Repository root: read only; do not edit files, do not run git commands that change state in it (cloning test repositories into `$W` is fine).
- Wait with bounded `until` loops (poll every 2-10 s, explicit timeout) or the Monitor tool. Never wait without a timeout. A fresh HA install takes 1-8 minutes.
- Work only in namespace `e2e-ha`. Other namespaces belong to other work: do not touch them.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed). On failure also collect: `kubectl -n e2e-ha get fj <name> -o yaml` (status part), `kubectl -n e2e-ha get pods,events --sort-by=.lastTimestamp | tail -30`, relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

Reusable procedures (referenced below):
- **CRON-COUNTS(name)**: for every server and cron pod of the instance, ask that pod (not the Service) for its cron tasks; the counts are kept per process:
  ```sh
  for pod in $(kubectl -n $NS get pods -l "$SEL,app.kubernetes.io/component in (server,cron)" --field-selector=status.phase=Running -o jsonpath='{range .items[*]}{.metadata.name}={.status.podIP}{" "}{end}'); do
    printf '%s ' "${pod%=*}"
    curl -s -m 5 -u "$U:$P" "http://${pod#*=}:3000/api/v1/admin/cron?limit=50" | python3 -c 'import json,sys; d=json.load(sys.stdin); print("total", sum(t["exec_times"] for t in d), "scheduled", sum(1 for t in d if not t["next"].startswith("0001")), "archive_cleanup", [t["exec_times"] for t in d if t["name"]=="archive_cleanup"])'
  done
  ```
  A pod with cron tasks disabled reports `total 0 scheduled 0`; the cron runner reports `scheduled` > 0 and growing totals.
- **HTTP-PUSH(name, repo, file)**: `curl -s -o /dev/null -w '%{http_code}' -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/user/repos -d '{"name":"<repo>","auto_init":true}'` (201, or 409 when it exists already); `git clone -q http://$U:$P@$N-http.$NS.svc.cluster.local:3000/$U/<repo>.git $W/<repo>-<n>` into a fresh directory; write `<file>`, `git -C ... add`, `git -C ... -c user.email=e2e@example.com -c user.name=e2e commit -qm <file>`, `git -C ... push -q`; then `curl -s -u "$U:$P" $B/api/v1/repos/$U/<repo>/contents/<file>` returns JSON with `"name":"<file>"`.
- **LOGIN(name, jar)**: a browser session in cookie jar `<jar>`: `curl -s -c <jar> -b <jar> -o $W/login.html $B/user/login`; if `$W/login.html` contains `name="_csrf" value="X"`, add `--data-urlencode "_csrf=X"` below; `curl -s -c <jar> -b <jar> -o /dev/null -w '%{http_code}' --data-urlencode "user_name=$U" --data-urlencode "password=$P" $B/user/login` → `303`. **SESSION-OK(jar)**: `curl -s -b <jar> -o /dev/null -w '%{http_code} %{redirect_url}' $B/user/settings` → `200` (an anonymous request gets `303` to `/user/login`). Never log in again with the jar after LOGIN: the point is that the same cookie keeps working.
- **SENTINEL(name, i, command...)**: `kubectl -n $NS exec $N-valkey-<i> -c sentinel -- sh -c 'valkey-cli --no-auth-warning -p 26379 <command>'` (the container authenticates with its `REDISCLI_AUTH` env). **VALKEY(name, i, command...)**: the same with `-c valkey` and port 6379.
- **MASTER(name)**: the Valkey primary as the Sentinels see it: `kubectl -n $NS get fj $N -o jsonpath='{.status.highAvailability.valkey.master}'` (operator view) and SENTINEL(name, i, `sentinel get-master-addr-by-name forgejo`) on a pod `i` that is not the primary (first line: `<pod>.<name>-valkey.e2e-ha.svc.cluster.local`).
- **CLONE-LOOP(name, seconds)**: in the background, clone `$U/demo` over HTTP into a fresh directory as fast as possible for `<seconds>`, deleting each clone, and log each failure with a timestamp (`date +%T`) and git's stderr; at the end print `clones ok=<n> failed=<m>`.
- **WATCH-WORKLOAD(name)**: every 2-3 s print one line, only when it changes: `date +%T`, `.status.upgrade.phase` and restore phase where relevant, `kubectl get deploy $N -o jsonpath='{.spec.replicas}/{.status.readyReplicas}/{.spec.strategy.type}/{.spec.template.spec.containers[0].image}'`, the same with `kubectl get sts $N-cron` (`{.spec.replicas}/{.status.readyReplicas}/{.spec.template.spec.containers[0].image}`), and the pods `kubectl -n $NS get pods -l "$SEL,app.kubernetes.io/component in (server,cron)" --no-headers | awk '{print $1":"$3}'`.

---

## H0 — Prerequisites

1. **H0.1 images**: the three images of the shared facts are present (pull missing ones). Record `docker image inspect codeberg.org/forgejo/forgejo:17 --format '{{index .Config.Labels "org.opencontainers.image.version"}}'` (and `:15`).
2. **H0.2 CRD**: `kubectl get crd forgejos.forgejo.lucas.icu -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.highAvailability.properties}'` contains `cronRunner`, `cronRunnerReady` and `valkey`; `...spec.properties.highAvailability.properties.valkey.properties` contains `topologySpreadConstraints`.
3. **H0.3 name limit**: `kubectl apply --dry-run=server -f -` of a Forgejo in namespace `default` named with 46 `a`s and `spec: {highAvailability: {enabled: true}}` is refused with a message containing `at most 45 characters`; the same with 45 `a`s is accepted (dry run only, nothing is created).
4. **H0.4 reserved name suffixes**: with `kubectl apply --dry-run=server` in namespace `default`: a Forgejo named `git-valkey` is refused with `must not end in '-valkey'`; a Forgejo named `git-cron` is accepted; a ForgejoRunner (`spec: {forgejoRef: {name: git}, executor: Host}`) named `git-cron` or `git-valkey` is refused with `must not end in '-valkey' or '-cron'`, one named `cron-ci` is accepted.
5. **H0.5 namespace**: if `e2e-ha` exists from an earlier run, delete it and wait until it is gone (timeout 5 min); `kubectl create namespace e2e-ha`.

## H1 — HA instance on Forgejo 17 (`ha17`)

Apply:

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata: {name: ha17, namespace: e2e-ha}
spec:
  image: codeberg.org/forgejo/forgejo:17
  replicas: 3
  highAvailability: {enabled: true}
  database: {cnpg: {instances: 1, storage: {size: 2Gi}}}
  storage: {size: 2Gi}
  resources:
    requests: {cpu: 50m, memory: 192Mi}
    limits: {memory: 768Mi}
  config:
    cron.archive_cleanup: {ENABLED: "true", RUN_AT_START: "true", SCHEDULE: "@every 1m"}
```

(`cron.archive_cleanup` is a frequent, harmless task, explicitly enabled in `spec.config`: the server pods must still not run it.)

1. **H1.1 running**: within 8 min `status.phase=Running`, condition `Ready=True`, `status.readyReplicas=3`, `status.version` starts with `17.`; `kubectl -n e2e-ha get deploy ha17` → `3/3`; `kubectl -n e2e-ha get sts ha17-cron ha17-valkey` → `1/1` and `3/3`; no Deployment `ha17-cron` exists. Record the elapsed time.
2. **H1.2 objects**:
   - `kubectl -n e2e-ha get sts ha17-cron -o jsonpath='{.spec.replicas} {.spec.podManagementPolicy} {.spec.updateStrategy.type} {.spec.selector.matchLabels.app\.kubernetes\.io/component} {.spec.template.metadata.labels.app\.kubernetes\.io/component} {.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}'` → `1 Parallel RollingUpdate cron cron Forgejo/ha17`; its pod is `ha17-cron-0`.
   - `kubectl -n e2e-ha get deploy ha17 -o jsonpath='{.spec.strategy.type} {.spec.strategy.rollingUpdate.maxUnavailable} {.spec.strategy.rollingUpdate.maxSurge}'` → `RollingUpdate 0 1`.
   - The HTTP Service routes only to server pods: `kubectl -n e2e-ha get endpointslices -l kubernetes.io/service-name=ha17-http -o jsonpath='{range .items[*].endpoints[*]}{.targetRef.name}{"\n"}{end}'` lists exactly the 3 server pods and not `ha17-cron-0`; the same for `ha17-ssh`.
   - `kubectl -n e2e-ha get svc ha17-valkey -o jsonpath='{.spec.clusterIP} {.spec.publishNotReadyAddresses} {.spec.ports[*].port}'` → `None true 6379 26379`.
   - `kubectl -n e2e-ha get pdb ha17 ha17-valkey -o custom-columns=N:.metadata.name,MIN:.spec.minAvailable,MAX:.spec.maxUnavailable` → `ha17` min 1, `ha17-valkey` max 1. No PDB for `ha17-cron`.
3. **H1.3 two app.ini variants**: `kubectl -n e2e-ha get secret ha17-config -o jsonpath='{.data}'` has exactly the keys `app.ini` and `app-web.ini`. Decode both (mask secrets):
   - `app-web.ini` has `[cron]` with `ENABLED = false` and `RUN_AT_START = false`, and `[cron.archive_cleanup]` with `ENABLED = false`, `RUN_AT_START = false`, `SCHEDULE = @every 1m`.
   - `app.ini` has `[cron.archive_cleanup]` with `ENABLED = true`, `RUN_AT_START = true`, and no `[cron]` section (only `[cron.update_checker] ENABLED = false`).
   - Both: `[session] PROVIDER = db` (no redis there); `[cache] ADAPTER = redis` and `HOST = redis+sentinel://:***@ha17-valkey-0.ha17-valkey.e2e-ha.svc.cluster.local:26379,ha17-valkey-1...,ha17-valkey-2...:26379/0?mastername=forgejo&sentinelpassword=***`; `[queue] TYPE = redis`, `CONN_STR` the same with `/2?`; `[server] GRACEFUL_HAMMER_TIME = 50s`; `[indexer] ISSUE_INDEXER_TYPE = db`. Apart from the `[cron*]` sections the two files are identical: piping each decoded file through `awk '/^\[/{skip=($0 ~ /^\[cron/)} !skip'` gives two files that `diff` reports as equal.
   - `kubectl -n e2e-ha get deploy ha17 -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].secret}'` has `items` `[{"key":"app-web.ini","path":"app.ini"}]`; the same for `sts ha17-cron` has no `items`. Both containers run `/app/gitea/gitea web --config /etc/forgejo/app.ini` (same path).
4. **H1.4 one cron runner**: at least 2 min after H1.1, CRON-COUNTS(ha17): each of the 3 server pods `total 0 scheduled 0 archive_cleanup [0]`; the cron pod `scheduled` ≥ 10, `total` ≥ 5, `archive_cleanup` ≥ 2. Wait 70 s and run it again: the cron pod's `archive_cleanup` grew by ≥ 1, every server pod still `total 0`.
5. **H1.5 healthz and admin**: `curl -s $B/api/healthz` → `"status": "pass"` with `cache:ping` and `database:ping` `pass`; `curl -s -u "$U:$P" $B/api/v1/user` → `"is_admin":true`.
6. **H1.6 status and condition**: `kubectl -n e2e-ha get fj ha17 -o jsonpath='{.status.highAvailability}'`: `cronRunner` is `ha17-cron-0`, `cronRunnerReady: true`; `valkey.master` = `ha17-valkey-0` (fresh group), `valkey.members` has 3 entries, all `ready: true`, one `role: master` and two `role: replica`, `valkey.sentinels: 3`. Condition `ValkeyReady` = `True/Available`, message `primary ha17-valkey-0, 2/2 replicas in sync, 3/3 Sentinels answered`.
7. **H1.7 graceful shutdown**: `kubectl -n e2e-ha get deploy ha17 -o jsonpath='{.spec.template.spec.terminationGracePeriodSeconds} {.spec.template.spec.containers[0].lifecycle.preStop.exec.command}'` → `75 ["/bin/sleep","10"]`; the cron runner has grace 75 and no `lifecycle`.
8. **H1.8 alerts and metrics**: `kubectl -n e2e-ha get prometheusrule ha17 -o yaml` contains `ForgejoValkeyNoMaster` (severity `critical`, `for: 2m`), `ForgejoValkeyFailedOver`, `ForgejoValkeyDegraded` (`for: 15m`) and `ForgejoCronRunnerDown` (`for: 15m`). Within 3 min Prometheus returns `forgejo_operator_valkey_master_available{namespace="e2e-ha",name="ha17"}` = 1, `forgejo_operator_valkey_ready_members{...}` = 3 and `forgejo_operator_cron_runner_ready{...}` = 1.
9. **H1.9 data for later checks**: HTTP-PUSH(ha17, demo, http.txt); `curl -s -o /dev/null -w '%{http_code}' -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/repos/$U/demo/issues -d '{"title":"keep me HA"}'` → 201.

## H2 — Topology spread constraints

1. **H2.1 server pods**: `kubectl -n e2e-ha get deploy ha17 -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'` is exactly one constraint: `maxSkew 1`, `topologyKey kubernetes.io/hostname`, `whenUnsatisfiable ScheduleAnyway`, `labelSelector.matchLabels` = `{app.kubernetes.io/component: server, app.kubernetes.io/instance: ha17, app.kubernetes.io/name: forgejo}` (no zone constraint: `kubectl get nodes -L topology.kubernetes.io/zone` shows no zone).
2. **H2.2 cron runner next to a server pod**: the data volume is `ReadWriteOnce` (`local-path`), so `sts ha17-cron`'s template has no `topologySpreadConstraints`, and `.spec.template.spec.affinity.podAffinity.requiredDuringSchedulingIgnoredDuringExecution` is exactly one term: `topologyKey: kubernetes.io/hostname`, `labelSelector.matchLabels` = `{app.kubernetes.io/component: server, app.kubernetes.io/instance: ha17, app.kubernetes.io/name: forgejo}`. `ha17-cron-0` runs on the node of a server pod. `Degraded` is not `HARequiresSharedStorage` (one node).
3. **H2.3 Valkey**: `kubectl -n e2e-ha get sts ha17-valkey -o jsonpath='{.spec.template.spec.topologySpreadConstraints}'`: one constraint, hostname, `ScheduleAnyway`, `matchLabels` with `app.kubernetes.io/component: valkey`.
4. **H2.4 degrades gracefully**: every pod of `ha17` is `Running` on the cluster's only node (`kubectl -n e2e-ha get pods -l $SEL -o wide`), none `Pending`, no `FailedScheduling` event in the namespace.
5. **H2.5 override**: `kubectl -n e2e-ha patch fj ha17 --type merge -p '{"spec":{"podTemplate":{"topologySpreadConstraints":[{"maxSkew":2,"topologyKey":"kubernetes.io/hostname","whenUnsatisfiable":"ScheduleAnyway","labelSelector":{"matchLabels":{"app.kubernetes.io/instance":"ha17"}}}]}}}'`. Within 1 min both `ha17` and `sts ha17-cron` templates carry exactly that constraint (`maxSkew` 2), and the cron runner keeps its `podAffinity`. Wait until `ha17` is `3/3` and `ha17-cron` `1/1` again (≤ 5 min), then remove it: `kubectl -n e2e-ha patch fj ha17 --type json -p '[{"op":"remove","path":"/spec/podTemplate/topologySpreadConstraints"}]'`; within 1 min the defaults of H2.1/H2.2 are back; wait for `3/3` and `1/1`.

## H3 — Valkey Sentinel topology

1. **H3.1 quorum**: SENTINEL(ha17, i, `sentinel ckquorum forgejo`) for i = 0, 1, 2 → `OK 3 usable Sentinels. Quorum and failover authorization can be reached`.
2. **H3.2 master view**: SENTINEL(ha17, 1, `sentinel master forgejo | paste - -`) → `flags master`, `num-slaves 2`, `num-other-sentinels 2`, `quorum 2`, `down-after-milliseconds 5000`, `failover-timeout 60000`, and `ip` = MASTER(ha17) (a host name `ha17-valkey-<i>.ha17-valkey.e2e-ha.svc.cluster.local`, not an IP).
3. **H3.3 agreement**: `sentinel get-master-addr-by-name forgejo` returns the same first line on all three Sentinels, and its pod equals `status.highAvailability.valkey.master`.
4. **H3.4 replication**: VALKEY(ha17, <master>, `info replication | grep -E "^(role|connected_slaves)"`) → `role:master`, `connected_slaves:2`; on the other two: `role:slave` and `master_link_status:up`. VALKEY(ha17, <master>, `info keyspace`) lists `db0` (the cache).
5. **H3.5 no password in arguments**: with `PW=$(kubectl -n e2e-ha get secret ha17-secrets -o jsonpath='{.data.valkey-password}' | base64 -d)`: `kubectl -n e2e-ha get sts ha17-valkey -o yaml | grep -c "$PW"` → 0 and `kubectl -n e2e-ha get pods -l $SEL,app.kubernetes.io/component=valkey -o yaml | grep -c "$PW"` → 0 (the password reaches the containers only through `secretKeyRef`); `kubectl -n e2e-ha get events | grep -c "$PW"` → 0; `kubectl -n e2e-ha get fj ha17 -o yaml | grep -c "$PW"` → 0. Do not print `$PW`.

## H4 — Valkey primary failover

Preparation: LOGIN(ha17, `$W/jar`) and SESSION-OK → 200. Create repo `failover` (HTTP-PUSH(ha17, failover, start.txt)). In the background start (a) a **push loop**: every 2 s commit a new file `f<n>.txt` in a clone of `failover` and `git push`, logging `date +%T` and the result of each push, and (b) a **health loop**: every 1 s `curl -s -m 3 -o /dev/null -w '%{http_code}' $B/api/healthz` with a timestamp. Record `kubectl -n e2e-ha get fj ha17 -o jsonpath='{.status.conditions[?(@.type=="Ready")].lastTransitionTime}'`.

1. **H4.1 graceful failover**: note M0=MASTER(ha17); `kubectl -n e2e-ha delete pod <M0> --wait=false` at time T0. Poll `sentinel get-master-addr-by-name forgejo` every 1 s on a pod other than M0: a different primary M1 within 15 s (expected about 2 s: the primary's preStop hook asks for the failover). Record the seconds.
2. **H4.2 the old pod rejoins**: within 2 min `<M0>` is Running `2/2` again and `kubectl -n e2e-ha logs <M0> -c config` ends with `starting as a replica of <M1>.ha17-valkey.e2e-ha.svc.cluster.local`; SENTINEL ckquorum OK again.
3. **H4.3 forced failover**: wait until `ValkeyReady` is `True/Available` again, note M1=MASTER(ha17), `kubectl -n e2e-ha delete pod <M1> --grace-period=0 --force` at T1 (no preStop hook runs). A primary other than M1 within 20 s (Sentinel's down-after is 5 s; the recreated pod also forces a failover from its init container before it starts). Within 2 min the recreated `<M1>` pod's `config` log says `starting as a replica of ...` — never `starting as the primary`.
4. **H4.4 operator report**: within 1 min of each failover `status.highAvailability.valkey.master` is the new primary and `lastFailoverTime` is later than T0 (then T1); events `ValkeyFailedOver` (type Warning) `the Valkey primary moved from <M0> to <M1>` and `... from <M1> to <M2>`; within 2 min `ValkeyReady` is `True/Available` with 3 Ready members.
5. **H4.5 session survives**: SESSION-OK(`$W/jar`) → `200` (same cookie, no new login).
6. **H4.6 git push during and after**: stop the loops 30 s after T1. Every push either succeeded or failed within 10 s after T0 or T1 (at most one failed push per failover); after the loops, HTTP-PUSH(ha17, failover, after.txt) succeeds, and `GET $B/api/v1/repos/$U/failover/contents` lists every `f<n>.txt` whose push succeeded.
7. **H4.7 Forgejo stayed up**: the health loop shows at most 3 consecutive non-200 answers around T0 and T1 (expected none); the `Ready` condition's `lastTransitionTime` of `fj ha17` is unchanged; `curl -s -u "$U:$P" $B/api/v1/repos/$U/demo/issues?state=all` still lists `keep me HA`.
8. **H4.8 metrics and alert**: within 3 min Prometheus `forgejo_operator_valkey_failovers_total{namespace="e2e-ha",name="ha17"}` ≥ 2 and `/api/v1/alerts` has `ForgejoValkeyFailedOver` with label `forgejo="ha17"` in state `firing`. `ForgejoValkeyNoMaster` is not firing.

## H5 — Server pod loss during a clone loop

1. **H5.1 graceful delete**: CLONE-LOOP(ha17, 60); after 10 s `kubectl -n e2e-ha delete pod <first server pod of ha17> --wait=false` (select with `-l $SEL,app.kubernetes.io/component=server`). Result: `failed=0` and `ok` ≥ 50 (measured: 268 ok, 0 failed).
2. **H5.2 forced delete**: wait until `ha17` is `3/3`; CLONE-LOOP(ha17, 60); after 10 s `kubectl -n e2e-ha delete pod <a server pod of ha17> --grace-period=0 --force`. Result: at most 2 failed clones, all within 10 s after the kill (measured: 0 failed).
3. **H5.3 cron runner untouched**: `ha17-cron-0` is the same pod as before H5.1 (`metadata.uid` and `restartCount` 0), `status.highAvailability.cronRunner` unchanged, and within 2 min `ha17` is `3/3`.

## H6 — Scaling 3 → 1 → 3 keeps exactly one cron runner

Start a sampler that every 2 s records the number of cron runner pods that are `Running` without `deletionTimestamp` (`kubectl -n e2e-ha get pods -l $SEL,app.kubernetes.io/component=cron -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}'`, count lines whose second field is empty), and the server pod count. Changing `spec.replicas` changes `[database] MAX_OPEN_CONNS` (auto-tuning), so every pod, the cron runner included, is replaced once per step.

1. **H6.1 scale down**: `kubectl -n e2e-ha scale fj ha17 --replicas=1`. Within 5 min: `ha17` `1/1`, `ha17-cron` `1/1`, one non-terminating server pod; `kubectl -n e2e-ha get pdb ha17` is NotFound (no PDB at one replica); `app-web.ini` of `ha17-config` has `MAX_OPEN_CONNS = 30` ((100 - 10) / (1 + 1 surge + 1 cron runner)).
2. **H6.2 cron runner at one replica**: CRON-COUNTS(ha17) after 70 s: the server pod `total 0 scheduled 0`, the cron pod `scheduled` ≥ 10 and `archive_cleanup` ≥ 1.
3. **H6.3 scale up**: `kubectl -n e2e-ha scale fj ha17 --replicas=3`. Within 5 min: `ha17` `3/3`, `ha17-cron` `1/1`, PDB `ha17` exists again (`minAvailable: 1`), `MAX_OPEN_CONNS = 18` ((100 - 10) / (3 + 1 + 1)).
4. **H6.4 never two runners**: over the whole of H6 the sampler never saw more than one running, non-terminating cron pod; after each step exactly one, and `status.highAvailability.cronRunner` names it. CRON-COUNTS(ha17): all 3 server pods `total 0`.

## H7 — Backup and restore with HA

1. **H7.1 backup**: HTTP-PUSH(ha17, restore-me, r.txt). Apply `ForgejoBackup ha17-before` (`spec: {forgejoRef: {name: ha17}, includeData: true, reason: Manual}`); within 5 min `status.phase=Completed`. Then create repo `after-backup` (POST `/api/v1/user/repos`, 201).
2. **H7.2 restore stops every Forgejo pod**: apply `ForgejoRestore ha17-restore` (`spec: {forgejoRef: {name: ha17}, backupName: ha17-before}`) and WATCH-WORKLOAD(ha17) plus `kubectl -n e2e-ha get fjr ha17-restore -o jsonpath='{.status.phase}'`. While the restore is `ScalingDown`/`Restoring`, both `deploy ha17` and `sts ha17-cron` have `spec.replicas` 0, and once the `ha17-restore-restore-*` Job pod exists no non-terminating server or cron pod is listed. Valkey keeps running (the `ha17-valkey-*` pods are not restarted: same names and ages).
3. **H7.3 migration hold**: during `ScalingUp`, `kubectl -n e2e-ha get deploy ha17 -o jsonpath='{.metadata.annotations.forgejo\.lucas\.icu/migration-hold} {.spec.replicas} {.spec.strategy.type}'` → `ha17-restore 1 Recreate`, and `sts ha17-cron` has 0 replicas.
4. **H7.4 back to HA**: within 10 min the restore is `Completed`, `ha17` `3/3`, `ha17-cron` `1/1`, `fj ha17` `Running`/`Ready=True`.
5. **H7.5 data**: `GET $B/api/v1/repos/$U/after-backup` → 404; `restore-me` has `r.txt`; `demo` has `http.txt`. CRON-COUNTS(ha17) after 70 s: server pods `total 0`, the cron pod's `archive_cleanup` ≥ 1.

## H8 — One replica without HA (`solo`)

First create a decoy StatefulSet `solo-cron` that is not the operator's: `kubectl -n e2e-ha create -f -` of `apiVersion: apps/v1, kind: StatefulSet, metadata: {name: solo-cron}, spec: {replicas: 1, selector: {matchLabels: {app: decoy}}, template: {metadata: {labels: {app: decoy}}, spec: {containers: [{name: c, image: registry.k8s.io/pause:3.10}]}}}` (pull the image if needed). Then apply Forgejo `solo` (namespace `e2e-ha`) with `image: codeberg.org/forgejo/forgejo:17`, `database: {cnpg: {instances: 1, storage: {size: 1Gi}}}`, `storage: {size: 1Gi}`, the resources of `ha17`, and no `replicas` or `highAvailability`.

1. **H8.1 running**: within 8 min `Running`, `Ready=True`.
2. **H8.2 no cron runner, no Valkey**: `kubectl -n e2e-ha get deploy,sts,svc,pdb -l app.kubernetes.io/instance=solo -o name` lists only `deployment.apps/solo`, `service/solo-http` and `service/solo-ssh` (no `solo-cron`, no `solo-valkey`, no PDB); `kubectl -n e2e-ha get secret solo-config -o jsonpath='{.data}'` has only the key `app.ini`; `status.highAvailability` is absent and there is no `ValkeyReady` condition.
3. **H8.3 its pod runs the cron tasks**: CRON-COUNTS(solo) (`SEL=app.kubernetes.io/instance=solo`): the single pod has `scheduled` ≥ 10 and `total` > 0.
4. **H8.4 pod spec**: `Recreate` strategy, no `lifecycle`, `terminationGracePeriodSeconds` 75, no `topologySpreadConstraints`; the decoded `app.ini` has `GRACEFUL_HAMMER_TIME = 50s` and `[session] PROVIDER = db`, and no `[cache]`/`[queue]` redis settings.
5. **H8.5 no HA alerts**: `kubectl -n e2e-ha get prometheusrule solo -o yaml` contains none of `ForgejoValkeyNoMaster`, `ForgejoValkeyFailedOver`, `ForgejoValkeyDegraded`, `ForgejoCronRunnerDown`.
6. **H8.6 a foreign `<name>-cron` is ignored**: the decoy `sts solo-cron` still has `spec.replicas` 1, no `ownerReferences`, and its pod `solo-cron-0` (label `app=decoy`) is Running with `restartCount` 0. Apply `ForgejoBackup solo-b` (`spec: {forgejoRef: {name: solo}, includeData: true, reason: Manual}`, `Completed` within 5 min), then `ForgejoRestore solo-r` (`spec: {forgejoRef: {name: solo}, backupName: solo-b}`): it reaches `Completed` within 10 min (no `ScaleDownTimeout`: the decoy is not counted as a Forgejo pod), `solo` is `Running`/`Ready=True` again, and the decoy is still untouched. Delete the decoy afterwards.

## H9 — Forgejo 15 with HA and the upgrade 15 → 17 (`ha15`)

Apply `ha15`: the manifest of H1 with `name: ha15` and `image: codeberg.org/forgejo/forgejo:15`.

1. **H9.1 running on 15**: within 8 min `Running`, `Ready=True`, `status.version` starts with `15.`; `ha15` `3/3`, `ha15-cron` `1/1`, `ha15-valkey` `3/3`; `ValkeyReady=True/Available`; healthz `pass` with `cache:ping` pass.
2. **H9.2 cron split on 15**: `ha15-config` has `app.ini` and `app-web.ini` with the same expectations as H1.3. CRON-COUNTS(ha15) at least 2 min after H9.1: server pods `total 0 scheduled 0`, the cron pod `archive_cleanup` ≥ 2 and `scheduled` ≥ 10.
3. **H9.3 data**: HTTP-PUSH(ha15, demo, http.txt); create issue `keep me 15` (201).
4. **H9.4 upgrade**: start WATCH-WORKLOAD(ha15) (including `.status.upgrade.phase`), then `kubectl -n e2e-ha patch fj ha15 --type merge -p '{"spec":{"image":"codeberg.org/forgejo/forgejo:17"}}'`. Expect `Succeeded` within 12 min, with:
   - `BackingUp`: `deploy ha15` and `sts ha15-cron` both at `spec.replicas` 0, and no server or cron pod left before the pre-upgrade `ForgejoBackup` (`kubectl -n e2e-ha get fjb -l forgejo.lucas.icu/instance=ha15`) is created; that backup ends `Completed`.
   - `RollingOut`/`Verifying`: `ha15` `1` replica, `Recreate`, image `:17`; `ha15-cron` 0 replicas; exactly one Forgejo pod exists (record its name, R).
   - `Succeeded`: within 3 min `ha15` `3/3` with `RollingUpdate`, `ha15-cron` `1/1`, both on `:17`.
5. **H9.5 migrated once**: `kubectl -n e2e-ha logs <R> -c bootstrap | grep -c 'Migration\['` > 0 (e.g. `Migration[v16a_...]`); for every other current server pod and the cron pod the same count is 0.
6. **H9.6 result**: `status.version` starts with `17.`, `Ready=True`, healthz `pass`, repo `demo` has `http.txt`, the issue `keep me 15` exists, HTTP-PUSH(ha15, after, a.txt) works. CRON-COUNTS(ha15) after 70 s: server pods `total 0`, the cron pod (on `:17`) `archive_cleanup` ≥ 1. Events of `ha15` include `UpgradeSucceeded`.

## H10 — Turning HA off

On `ha15`: LOGIN(ha15, `$W/jar15`), SESSION-OK → 200. Then `kubectl -n e2e-ha patch fj ha15 --type merge -p '{"spec":{"replicas":1,"highAvailability":{"enabled":false}}}'`.

1. **H10.1 cron runner removed**: within 1 min `sts ha15-cron` is NotFound and an event `CronRunnerDeleted` exists; `ha15-config` has only `app.ini`.
2. **H10.2 Valkey removed after the soak**: `ValkeyRetained` events appear first (the non-HA app.ini must run healthy for a minute and become the last known good one); within 5 min `kubectl -n e2e-ha get sts,svc,pdb ha15-valkey` are all NotFound and an event `ValkeyDeleted` exists; `status.highAvailability` and the `ValkeyReady` condition are gone; `ha15` is `1/1`, `Recreate`, `Ready=True`.
3. **H10.3 session and cron**: SESSION-OK(`$W/jar15`) → `200` (sessions are in the database); CRON-COUNTS(ha15) after 70 s: the single pod `scheduled` ≥ 10.

## H12 — At most one cron runner (`ha17`, after H7)

1. **H12.1 graceful delete**: start a sampler that every 1 s records every cron pod of `ha17` with its uid, phase and `deletionTimestamp` (`kubectl -n e2e-ha get pods -l $SEL,app.kubernetes.io/component=cron -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.uid} {.status.phase} {.metadata.deletionTimestamp}{"\n"}{end}'`). Note the uid U0 of `ha17-cron-0`, `kubectl -n e2e-ha delete pod ha17-cron-0 --wait=false`. Within 3 min `ha17-cron-0` is Running and Ready with another uid; the sampler never saw two cron pods at once (the new pod appears only after U0 is gone), and `status.highAvailability.cronRunner` is `ha17-cron-0` again.
2. **H12.2 a pod that does not go away blocks the replacement**: `kubectl -n e2e-ha patch pod ha17-cron-0 --type merge -p '{"metadata":{"finalizers":["e2e.example.com/hold"]}}'`, then `kubectl -n e2e-ha delete pod ha17-cron-0 --wait=false` at T2 (simulates a pod whose deletion never completes, as on a lost node). For 3 min: the old pod object stays (deletionTimestamp set), no other cron pod is created, `status.highAvailability.cronRunner` is empty and `cronRunnerReady` false. Within 4 min of T2 a Warning event `CronRunnerStuck` names `ha17-cron-0` and contains `delete pod ha17-cron-0 --force --grace-period=0`.
3. **H12.3 recovery**: `kubectl -n e2e-ha patch pod ha17-cron-0 --type json -p '[{"op":"remove","path":"/metadata/finalizers"}]'`. Within 3 min a new `ha17-cron-0` (another uid) is Running and Ready, `cronRunnerReady: true`; the sampler of H12.1 (still running) never saw two cron pods. CRON-COUNTS(ha17) after 70 s: the cron pod `archive_cleanup` ≥ 1, server pods `total 0`.

## H11 — Cleanup

1. **H11.1**: `kubectl delete namespace e2e-ha` and wait until it is gone (timeout 10 min). Remove `$W`.
