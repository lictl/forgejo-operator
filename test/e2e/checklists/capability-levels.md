<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklists

Shared facts for every scenario:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- `docker` commands must reach the docker engine the local cluster runs on, so that pulled and built images are visible to the cluster; set `DOCKER_CONTEXT` if that is not your current docker context.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`). Do not modify it. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep <your-namespace>`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`), `ForgejoBackup` (`fjb`), `ForgejoRestore` (`fjr`).
- An instance `<name>` in namespace `<ns>` serves HTTP at `http://<name>-http.<ns>.svc.cluster.local:3000` and SSH at `<name>-ssh.<ns>.svc.cluster.local:22`; the Mac resolves these names directly.
- Admin credentials: `kubectl -n <ns> get secret <name>-admin -o jsonpath='{.data.username}' | base64 -d` (same for `password`).
- Images available locally: `codeberg.org/forgejo/forgejo:15`, `:16`, `:16-rootless`.
- Wait with bounded `until` loops (poll every 5-10 s, explicit timeout) or the Monitor tool. Never wait without a timeout.
- Work only in your namespace(s). Do not change operator code or anything outside your namespace.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed). On failure also collect: `kubectl -n <ns> get fj <name> -o yaml` (status part), `kubectl -n <ns> get pods,events --sort-by=.lastTimestamp | tail -30`, relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

---

## L1 — Basic install (namespace `e2e-l1`)

1. **L1.1 install**: create the namespace; apply a Forgejo `l1` with `spec: {}`. Expect `status.phase=Running` and condition `Ready=True` within 6 min. Record the elapsed time.
2. **L1.2 healthz**: `curl -s http://l1-http.e2e-l1.svc.cluster.local:3000/api/healthz` → JSON `"status":"pass"`.
3. **L1.3 admin**: `curl -s -u USER:PASS .../api/v1/user` → `"is_admin":true`.
4. **L1.4 git over HTTP**: `POST /api/v1/user/repos` `{"name":"demo","auto_init":true}`; `git clone http://USER:PASS@l1-http.e2e-l1.svc.cluster.local:3000/USER/demo.git` into a temp dir under your scratch area; add a file, commit, push; `GET /api/v1/repos/USER/demo/contents/<file>` returns it.
5. **L1.5 git over SSH**: `ssh-keygen -t ed25519 -N '' -f <tmp>/key`; `POST /api/v1/user/keys` with the public key; `GIT_SSH_COMMAND="ssh -i <tmp>/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null" git clone ssh://git@l1-ssh.e2e-l1.svc.cluster.local:22/USER/demo.git` succeeds and contains the pushed file.
6. **L1.6 config change rolls pods**: note the pod name and the Deployment's pod-template annotation `forgejo.lucas.icu/config-hash`; `kubectl patch fj l1 --type merge -p '{"spec":{"config":{"ui":{"DEFAULT_THEME":"forgejo-dark"}}}}'`; within 4 min the annotation changes, a new pod is Ready, and `GET /api/v1/settings/ui` returns `"default_theme":"forgejo-dark"`.
7. **L1.7 pod loss**: delete the Forgejo pod; within 4 min a new pod is Ready and repo `demo` still has the pushed file.
8. **L1.8 delete and re-create (Retain)**: `kubectl delete fj l1`; within 2 min the Deployment `l1` is gone while PVC `l1-data`, CNPG Cluster `l1-db`, and Secrets `l1-secrets`, `l1-admin` still exist. Re-apply the same Forgejo `l1` → Running within 6 min, the admin password is unchanged and works, repo `demo` with the file is present.
9. **L1.9 rootless image**: Forgejo `l1r` with `spec.image: codeberg.org/forgejo/forgejo:16-rootless` → Running within 6 min and healthz pass.
10. **L1.10 external database**: create a CNPG cluster yourself:
    ```yaml
    apiVersion: postgresql.cnpg.io/v1
    kind: Cluster
    metadata: {name: extdb, namespace: e2e-l1}
    spec:
      instances: 1
      storage: {size: 1Gi}
      bootstrap: {initdb: {database: forgejo, owner: forgejo}}
    ```
    Wait until it is healthy (`status.readyInstances=1`). Then Forgejo `l1x` with `spec.database: {type: External, external: {host: extdb-rw, database: forgejo, credentialsSecret: {name: extdb-app}}}` → Running within 6 min, healthz pass, and NO CNPG cluster named `l1x-db` exists.
11. **L1.11 printer columns**: `kubectl -n e2e-l1 get fj -o wide` shows Phase, Version (non-empty for `l1`), Ready, Replicas and URL.

Leave the instances in place.

## L2 — Seamless upgrades (namespace `e2e-l2`)

1. **L2.1 install old version**: Forgejo `l2` with `spec.image: codeberg.org/forgejo/forgejo:15` and `spec.upgrade.timeout: 4m` → Running within 6 min; `status.version` starts with `15.`.
2. **L2.2 data**: create repo `demo` (auto_init) and an issue (`POST /api/v1/repos/USER/demo/issues {"title":"keep me"}`).
3. **L2.3 upgrade 15 → 16**: `kubectl patch fj l2 --type merge -p '{"spec":{"image":"codeberg.org/forgejo/forgejo:16"}}'`. Poll `status.upgrade.phase` every 5 s and record the sequence of distinct phases. Expect `Succeeded` within 10 min, `status.version` starting with `16.`, a ForgejoBackup with `spec.reason: PreUpgrade` in phase `Completed`, and repo + issue intact.
4. **L2.4 bad image rolls back**: patch image to `codeberg.org/forgejo/forgejo:99` (does not exist). Record the phase sequence. Expect `status.upgrade.phase=RolledBack` within 12 min, `status.image` = `codeberg.org/forgejo/forgejo:16`, `status.upgrade.failedImage` = the :99 image, condition `Degraded=True` with reason `UpgradeRolledBack`, a ForgejoRestore in phase `Completed`, healthz pass, repo + issue intact, and an event with reason `UpgradeRolledBack` on `l2`.
5. **L2.5 downgrade refused**: patch image to `codeberg.org/forgejo/forgejo:15`. Within 2 min a condition (Degraded or Upgrading) has reason `DowngradeRefused`, and the Deployment's container image is still `:16`.
6. **L2.6 recover**: patch image back to `codeberg.org/forgejo/forgejo:16`. Within 3 min `Degraded` is not True with reason `UpgradeRolledBack` or `DowngradeRefused`, and Ready is True.

## L3 — Full lifecycle: backup, restore, retention, deletion (namespace `e2e-l3`)

1. **L3.1 install + data**: Forgejo `l3` → Running; create repo `demo` (auto_init) and an issue.
2. **L3.2 manual backup**: apply `ForgejoBackup b1` with `spec.forgejoRef.name: l3`. Within 8 min `status.phase=Completed`, `status.databaseSizeBytes > 0`, `status.dataSizeBytes > 0`, `status.path` set.
3. **L3.3 change data**: delete repo `demo` via API; create repo `after-backup`.
4. **L3.4 restore**: apply `ForgejoRestore r1` (`forgejoRef.name: l3`, `backupName: b1`). Poll and record the restore phases and the Forgejo `status.phase`. Expect restore `Completed` within 10 min, the Forgejo phase `Restoring` observed at least once, then: repo `demo` with its issue exists, `after-backup` does not, healthz pass, `git clone` of demo over HTTP works.
5. **L3.5 scheduled backups + retention**: patch `spec.backup: {enabled: true, schedule: "*/2 * * * *", retention: 2}`. Wait ~8 min. Expect at least 3 ForgejoBackups with `spec.reason: Scheduled` were created over time (watch with `kubectl get fjb -w` or poll and record names), at most 2 Completed Scheduled backups remain at the end, condition `BackupHealthy=True`, `status.backup.lastSuccessfulBackupTime` set. Then patch `spec.backup.enabled: false` and confirm the `BackupHealthy` condition disappears within 2 min.
6. **L3.6 invalid restore**: apply `ForgejoRestore r-bad` with `backupName: does-not-exist` → phase `Failed` with a message within 2 min, and Forgejo `l3` stays Ready (its Deployment never goes to 0 replicas).
7. **L3.7 immutability**: `kubectl patch fjb b1 --type merge -p '{"spec":{"includeData":false}}'` is rejected with a message about the spec being immutable.
8. **L3.8 backup deletion**: `kubectl delete fjb b1` completes within 3 min (finalizer ran its cleanup Job).
9. **L3.9 retainPolicy Delete**: Forgejo `l3d` with `spec.storage.retainPolicy: Delete` → Running; delete it; within 4 min no PVC, CNPG Cluster or Secret whose name starts with `l3d` remains in the namespace.

## L4 — Deep insights (namespace `e2e-l4`)

Find the Prometheus and Grafana services with `kubectl -n monitoring get svc` (kube-prometheus-stack release `kps`; Prometheus on 9090, Grafana on 80, Grafana login admin/admin).

1. **L4.1 install**: Forgejo `l4` with `spec.monitoring: {grafanaDashboard: {enabled: true}, statsInterval: 1m}` → Running.
2. **L4.2 status insights**: within 3 min `status.health.healthy=true` with check `database:ping` = pass, `status.version` non-empty, `status.stats.users >= 1`; conditions include `Ready`, `DatabaseReady`, `Healthy`, `StorageHealthy`.
3. **L4.3 monitoring objects**: ServiceMonitor `l4`, PrometheusRule `l4` and ConfigMap `l4-dashboard` exist in e2e-l4.
4. **L4.4 scraping**: within 4 min Prometheus `up{namespace="e2e-l4"}` returns 1 and `gitea_users{namespace="e2e-l4"}` has a sample (`/api/v1/query`).
5. **L4.5 rules**: Prometheus `/api/v1/rules` contains rules from `l4`'s PrometheusRule (e.g. names starting with `Forgejo`).
6. **L4.6 operator metrics**: Prometheus `forgejo_operator_instance_ready{namespace="e2e-l4",name="l4"}` = 1.
7. **L4.7 dashboard**: Grafana `/api/search?query=e2e-l4` (basic auth admin:admin) returns a dashboard.
8. **L4.8 failure detection**: in pod `l4-db-1` run `psql -U postgres -d forgejo -c "ALTER ROLE forgejo NOLOGIN"` and `psql -U postgres -c "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename='forgejo'"`. Within 4 min the instance shows the problem: `Healthy` condition False or Unknown (message mentions the database), or `status.health.healthy=false`, or `Ready=False`. Then `ALTER ROLE forgejo LOGIN` and expect `Healthy=True` and `Ready=True` within 6 min.
9. **L4.9 events**: `kubectl -n e2e-l4 get events --field-selector involvedObject.name=l4` lists events from the Forgejo controller.

## L5 — Auto pilot (namespace `e2e-l5`)

1. **L5.1 HA**: Forgejo `l5ha` with `spec.highAvailability.enabled: true` and `spec.replicas: 2` → within 8 min Deployment `l5ha-valkey` ready, 2 Forgejo server pods Ready, PodDisruptionBudget `l5ha` exists, and Secret `l5ha-config` key `app.ini` contains `ADAPTER = redis` under `[cache]`, `PROVIDER = redis` under `[session]` and `TYPE = redis` under `[queue]`.
2. **L5.2 shared sessions**: with pod IPs A and B (`kubectl get pods -o wide`), log in through A: `curl -c jar -b jar http://A:3000/user/login` and extract the `_csrf` hidden input; `curl -c jar -b jar -X POST http://A:3000/user/login -d "_csrf=...&user_name=USER&password=PASS"`; then `curl -b jar -o /dev/null -w '%{http_code}' http://B:3000/user/settings` must return 200 (a login redirect is 303/302).
3. **L5.3 HPA**: patch `spec.autoscaling.horizontal: {enabled: true, minReplicas: 2, maxReplicas: 3}` → within 4 min HPA `l5ha` exists and `kubectl get hpa l5ha` shows numeric CPU targets (not `<unknown>`).
4. **L5.4 config rollback**: Forgejo `l5` (single replica, `spec.resources.limits.memory: 1Gi`) → Running; wait until `status.autoPilot.lastKnownGoodConfigHash` is set. Patch `spec.config: {server: {PROTOCOL: https, ENABLE_ACME: "true"}}` (this makes Forgejo exit at startup). Within 7 min expect `Degraded=True` reason `ConfigRolledBack`, `status.autoPilot.rejectedConfigHash` set, `Ready=True` again, and an event `ConfigRolledBack`. Then remove the bad config (`kubectl patch fj l5 --type json -p '[{"op":"remove","path":"/spec/config"}]'`) and expect `Degraded` no longer `ConfigRolledBack` and `status.configHash` changed again within 5 min.
5. **L5.5 OOM right-sizing**: patch `l5` with `spec.resources: {requests: {memory: 48Mi}, limits: {memory: 48Mi}}` and `spec.autoscaling.vertical: {enabled: true, maxAllowed: {memory: 1Gi}}`. Within 10 min expect a `status.autoPilot.recentActions` entry of type `ResourcesScaled`, `status.autoPilot.appliedResources.limits.memory` larger than 48Mi, and the pod Ready.
6. **L5.6 hung process**: find the running Forgejo container of `l5`: `docker ps --filter "label=io.kubernetes.pod.namespace=e2e-l5" --filter "label=io.kubernetes.container.name=forgejo" --format '{{.ID}} {{.Names}}'` (pick the one for an `l5-` pod). Freeze it: `docker kill --signal=SIGSTOP <id>`. Within 10 min expect the pod to be replaced (new pod name, Ready) and an autopilot action (e.g. `PodRestarted`) or event explaining it. If it is not replaced, `SIGCONT` the container at the end.
7. **L5.7 storage pressure**: Forgejo `l5s` with `spec.storage.size: 1Gi` → Running; `status.storage.volumes` contains claim `l5s-data` with `usedBytes > 0`; `StorageHealthy=True`. Fill the volume: `kubectl exec deploy/l5s -c forgejo -- dd if=/dev/zero of=/data/fill.bin bs=1M count=880`. Within 8 min expect `StorageHealthy=False` with reason `ExpansionUnsupported` (local-path cannot expand) and a Warning event. Then `rm /data/fill.bin` and expect `StorageHealthy=True` within 12 min.
8. **L5.8 tuning**: `status.autoPilot.tuning` of `l5` includes a `database.MAX_OPEN_CONNS` entry and an `env.GOMEMLIMIT` entry.
