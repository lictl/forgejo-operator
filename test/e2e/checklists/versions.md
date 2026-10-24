<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklist: Forgejo version support (namespace `e2e-versions`)

Shared facts:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- `docker` commands must reach the docker engine the local cluster runs on, so that pulled and built images are visible to the cluster; set `DOCKER_CONTEXT` if that is not your current docker context.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`) with the version-support build already deployed. Do not modify or redeploy it. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep e2e-versions`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`), `ForgejoBackup` (`fjb`), `ForgejoRestore` (`fjr`).
- An instance `<name>` in namespace `e2e-versions` serves HTTP at `http://<name>-http.e2e-versions.svc.cluster.local:3000` and SSH at `<name>-ssh.e2e-versions.svc.cluster.local:22`; the local cluster must let the host resolve these names directly.
- Admin credentials (passwords only contain URL-safe characters, so `http://USER:PASS@host` works):
  ```sh
  NS=e2e-versions; N=<name>
  U=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.username}' | base64 -d)
  P=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.password}' | base64 -d)
  B=http://$N-http.$NS.svc.cluster.local:3000
  ```
- Images in the image store of the docker engine the local cluster runs on (no registry needed): `codeberg.org/forgejo/forgejo:14`, `:15`, `:15-rootless`, `:16`, `:17`, `:17-rootless`. Check first with `docker images | grep codeberg.org/forgejo/forgejo`; pull a missing one with `docker pull <image>`.
- Version facts on 2026-10-22 (the operator classifies with the current date): supported are **15 (LTS)** and **17**; `:15` is 15.0.10, `:16` is 16.0.6 (`Deprecated` until 2026-10-29, `EndOfLife` from that day on), `:17` is 17.0.0 (supported, with an upstream advisory: the condition message says "use 17.0.1 or newer"), `:14` is 14.0.5 (`EndOfLife`).
- The condition is read with `kubectl -n $NS get fj $N -o jsonpath='{.status.conditions[?(@.type=="VersionSupported")]}'` (fields `status`, `reason`, `message`).
- Events: `kubectl -n $NS get events --field-selector involvedObject.name=$N,reason=<Reason> -o jsonpath='{range .items[*]}{.type} {.reason}: {.message}{"\n"}{end}'`.
- Prometheus: `http://kps-kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090` (`/api/v1/query?query=...` with `--data-urlencode`, `/api/v1/rules`, `/api/v1/alerts`). Operator metrics are scraped every 30-60 s.
- Repository root: the repository root (read only; do not edit files, do not run git commands that change state).
- Wait with bounded `until` loops (poll every 5-10 s, explicit timeout) or the Monitor tool. Never wait without a timeout. Several instances may run at once; a fresh install can take up to 8 min while others start.
- Work only in namespace `e2e-versions`. Other namespaces belong to other work: do not touch them.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed). On failure also collect: `kubectl -n e2e-versions get fj <name> -o yaml` (status part), `kubectl -n e2e-versions get pods,events --sort-by=.lastTimestamp | tail -30`, relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

Reusable procedures (referenced below):
- **HTTP-PUSH(name, repo, file)**: `curl -s -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/user/repos -d '{"name":"<repo>","auto_init":true}'` (HTTP 201); `git clone http://$U:$P@$N-http.e2e-versions.svc.cluster.local:3000/$U/<repo>.git <tmpdir>/<repo>` into a fresh temp dir under your scratch area; create `<file>` with some text, `git -c user.email=e2e@example.com -c user.name=e2e commit -am`-style add+commit, `git push`; then `curl -s -u "$U:$P" $B/api/v1/repos/$U/<repo>/contents/<file>` returns JSON with `"name":"<file>"`. A failed push hook shows up as `remote: ... error` / `pre-receive hook declined` in the push output.
- **SSH-PUSH(name, repo, file)**: `ssh-keygen -t ed25519 -N '' -f <tmp>/key` (once); `curl -s -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/user/keys -d "{\"title\":\"e2e\",\"key\":\"$(cat <tmp>/key.pub)\"}"` (201; a 422 saying the key is already in use means it was added to this instance before, which is fine); with `GIT_SSH_COMMAND="ssh -i <tmp>/key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes"` clone `ssh://git@$N-ssh.e2e-versions.svc.cluster.local:22/$U/<repo>.git` into a fresh dir, add and commit `<file>`, `git push` over SSH; `GET $B/api/v1/repos/$U/<repo>/contents/<file>` returns it.
- **ISSUE(name, repo, title)**: `curl -s -u "$U:$P" -H 'Content-Type: application/json' -X POST $B/api/v1/repos/$U/<repo>/issues -d '{"title":"<title>"}'` (201). Verify later with `GET $B/api/v1/repos/$U/<repo>/issues?state=all` containing the title.
- **UPGRADE-WATCH(name)**: poll `kubectl -n $NS get fj $N -o jsonpath='{.status.upgrade.phase}'` every 5 s and record the sequence of distinct phases until `Succeeded`, `Failed` or `RolledBack` (timeout 12 min).

---

## V0 — Prerequisites

1. **V0.1 images**: all six images listed in the shared facts are present (pull missing ones). Record `docker image inspect <image> --format '{{index .Config.Labels "org.opencontainers.image.version"}}'` for each (expect 14.0.5, 15.0.10, 15.0.10, 16.0.6, 17.0.0, 17.0.0).
2. **V0.2 CRD default and printer column**: `kubectl get crd forgejos.forgejo.lucas.icu -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.image.default}'` → `codeberg.org/forgejo/forgejo:15`; `kubectl get crd forgejos.forgejo.lucas.icu -o jsonpath='{.spec.versions[0].additionalPrinterColumns[*].name}'` contains `Support`.
3. **V0.3 namespace**: `kubectl create namespace e2e-versions`.

## V1 — Fresh install of the default image (15, root)

Apply Forgejo `v15` with `spec: {}` (no image).

1. **V1.1 default image**: right after apply, `kubectl -n e2e-versions get fj v15 -o jsonpath='{.spec.image}'` → `codeberg.org/forgejo/forgejo:15`.
2. **V1.2 running**: within 8 min `status.phase=Running`, condition `Ready=True`, `status.version` starts with `15.`. Record the elapsed time.
3. **V1.3 healthz**: `curl -s $B/api/healthz` → JSON with `"status": "pass"` (any whitespace).
4. **V1.4 admin login**: `curl -s -u "$U:$P" $B/api/v1/user` → `"is_admin":true`.
5. **V1.5 HTTP push**: HTTP-PUSH(v15, demo, http.txt) succeeds without hook errors.
6. **V1.6 SSH push**: SSH-PUSH(v15, demo, ssh.txt) succeeds; `ssh.txt` is returned by the contents API.
7. **V1.7 VersionSupported**: condition `status=True`, `reason=Supported`, message contains `current LTS release` and `2027-07-15`. `kubectl -n e2e-versions get fj v15 -o wide` shows `Supported` in the SUPPORT column. No event with reason `UnsupportedVersion` or `VersionAdvisory` for `v15`.
8. **V1.8 rendered app.ini**: `kubectl -n e2e-versions get secret v15-config -o jsonpath='{.metadata.annotations.forgejo\.lucas\.icu/config-profile}'` → `internal-token`; `kubectl -n e2e-versions get secret v15-config -o jsonpath='{.data.app\.ini}' | base64 -d` contains a line starting `INTERNAL_TOKEN = ` under `[security]` and does NOT contain `INTERNAL_LISTENER_PATH`.
9. **V1.9 data for the upgrade**: ISSUE(v15, demo, "keep me 15").

## V2 — Fresh install 15-rootless

Forgejo `v15r` with `spec.image: codeberg.org/forgejo/forgejo:15-rootless`.

1. **V2.1 running**: within 8 min `Running`, `Ready=True`, `status.version` starts with `15.`.
2. **V2.2 healthz + admin**: as V1.3 and V1.4.
3. **V2.3 HTTP push**: HTTP-PUSH(v15r, demo, http.txt).
4. **V2.4 SSH push**: SSH-PUSH(v15r, demo, ssh.txt).
5. **V2.5 VersionSupported**: `True/Supported`.

## V3 — Fresh install 17 (root)

Forgejo `v17` with `spec.image: codeberg.org/forgejo/forgejo:17`.

1. **V3.1 running**: within 8 min `Running`, `Ready=True`, `status.version` starts with `17.`.
2. **V3.2 healthz + admin**: as V1.3 and V1.4.
3. **V3.3 HTTP push**: HTTP-PUSH(v17, demo, http.txt).
4. **V3.4 SSH push**: SSH-PUSH(v17, demo, ssh.txt).
5. **V3.5 VersionSupported**: `status=True`, `reason=Supported`, message starts with `Forgejo 17 is supported: the latest release` and contains `2027-01-28` and `The running patch release 17.0.0 is older than recommended: use 17.0.1 or newer` (the 17.0.0 advisory; it appears once `status.version` is set, allow 2 min). Exactly one event `VersionAdvisory` (type Warning) for `v17`; no `UnsupportedVersion` event.
6. **V3.6 rendered app.ini**: same expectations as V1.8 (profile `internal-token`, INTERNAL_TOKEN present, no INTERNAL_LISTENER_PATH).

## V4 — Fresh install 17-rootless

Forgejo `v17r` with `spec.image: codeberg.org/forgejo/forgejo:17-rootless`.

1. **V4.1 running**: within 8 min `Running`, `Ready=True`, `status.version` starts with `17.`.
2. **V4.2 healthz + admin**: as V1.3 and V1.4.
3. **V4.3 HTTP push**: HTTP-PUSH(v17r, demo, http.txt).
4. **V4.4 SSH push**: SSH-PUSH(v17r, demo, ssh.txt).
5. **V4.5 VersionSupported**: `True/Supported`.
6. **V4.6 free resources**: delete Forgejo `v15r` and `v17r` (`kubectl -n e2e-versions delete fj v15r v17r`); within 3 min their Deployments are gone.

## V5 — Deprecated version (16)

Forgejo `v16` with `spec.image: codeberg.org/forgejo/forgejo:16` and `spec.upgrade.timeout: 6m`.

1. **V5.1 running**: within 8 min `Running`, `Ready=True`, `status.version` starts with `16.`.
2. **V5.2 condition**: `VersionSupported` has `status=False`, `reason=Deprecated`, message starts with `Forgejo 16 is outside this operator's support policy` and contains `2026-10-29` and `Upgrade to 17 (17.0.1 or newer) before 2026-10-29`. (If today is 2026-10-29 or later: `reason=EndOfLife` and message contains `reached its upstream end of life`; record which.) The SUPPORT column of `kubectl get fj -o wide` shows the reason. Running is not blocked: `Ready=True`.
3. **V5.3 event**: exactly one event with reason `UnsupportedVersion`, type `Warning`, for `v16`, whose message equals the condition message (check the event's `count`/`series` too: it must not repeat). Wait 2 min and confirm no second one appeared (it is only emitted when the classification changes; `status.version` becoming known does not change it).
4. **V5.4 metric**: within 3 min Prometheus `forgejo_operator_version_supported{namespace="e2e-versions",name="v16"}` returns exactly one series, value `0`, labels `major="16"` and `support="Deprecated"` (or `EndOfLife`, as in V5.2); `forgejo_operator_version_end_of_life_timestamp_seconds{namespace="e2e-versions",name="v16"}` = `1793232000` (2026-10-29T00:00:00Z). Also `forgejo_operator_version_supported{namespace="e2e-versions",name="v15"}` = `1` with `support="Supported"`.
5. **V5.5 data for the upgrade**: HTTP-PUSH(v16, demo, http.txt); ISSUE(v16, demo, "keep me 16").

## V6 — Alert rule

1. **V6.1 PrometheusRule**: `kubectl -n e2e-versions get prometheusrule v16 -o yaml` contains an alert `ForgejoVersionUnsupported` with `for: 1h`, `severity: warning` and expr `max by (namespace, name, major, support) (forgejo_operator_version_supported{namespace="e2e-versions", name="v16"}) == 0`.
2. **V6.2 loaded**: Prometheus `/api/v1/rules` lists `ForgejoVersionUnsupported` in group `forgejo.e2e-versions.v16` (allow up to 3 min for the rule reload).
3. **V6.3 pending**: within 5 min Prometheus `/api/v1/alerts` contains an alert `ForgejoVersionUnsupported` with labels `forgejo="v16"`, `major="16"` and state `pending` (it fires only after 1h; do not wait for that). No such alert exists for `forgejo="v15"`.

## V7 — Upgrade 16 → 17

1. **V7.1 upgrade**: `kubectl -n e2e-versions patch fj v16 --type merge -p '{"spec":{"image":"codeberg.org/forgejo/forgejo:17"}}'`; UPGRADE-WATCH(v16). Expect `Succeeded` within 12 min; the sequence includes `BackingUp`.
2. **V7.2 backup**: a ForgejoBackup with `spec.reason: PreUpgrade` for `v16` (`kubectl -n e2e-versions get fjb -l forgejo.lucas.icu/instance=v16`) is `Completed`.
3. **V7.3 result**: within 2 min after Succeeded: `status.image` = `codeberg.org/forgejo/forgejo:17`, `status.version` starts with `17.`, `Ready=True`, healthz pass.
4. **V7.4 data intact**: repo `demo` has `http.txt`, and the issue "keep me 16" exists. HTTP-PUSH works again on a new repo (HTTP-PUSH(v16, after, a.txt)).
5. **V7.5 supported again**: `VersionSupported` = `True/Supported` within 2 min; an event `SupportedVersion` (type Normal) exists for `v16`; Prometheus `forgejo_operator_version_supported{namespace="e2e-versions",name="v16"}` now has one series with `major="17"`, `support="Supported"`, value 1 (allow 3 min), and no series with `major="16"` remains.
6. **V7.6 events**: events for `v16` include `MajorUpgrade` (message contains `from 16 to 17`), `UpgradeStarted`, `PreUpgradeBackupCompleted` and `UpgradeSucceeded`.

## V8 — Upgrade 15 → 17, skipping 16

1. **V8.1 upgrade**: on `v15` (from V1, with repo `demo` and the issue "keep me 15"): patch `spec.image` to `codeberg.org/forgejo/forgejo:17`; UPGRADE-WATCH(v15). Expect `Succeeded` within 12 min. It must NOT be refused (no `UpgradePathUnsupported`).
2. **V8.2 event**: an event `MajorUpgrade` for `v15` whose message contains `from 15 to 17, skipping 16`.
3. **V8.3 backup**: a `PreUpgrade` ForgejoBackup for `v15` is `Completed`.
4. **V8.4 result + data**: `status.version` starts with `17.`, `Ready=True`, healthz pass, repo `demo` has `http.txt` and `ssh.txt`, issue "keep me 15" exists; SSH-PUSH(v15, demo, after.txt) works.
5. **V8.5 condition**: `VersionSupported` = `True/Supported` (message mentions 17).

## V9 — Downgrade 17 → 15 refused

1. **V9.1 refuse**: on `v15` (now 17): patch `spec.image` to `codeberg.org/forgejo/forgejo:15`. Within 2 min condition `Upgrading` has `reason=DowngradeRefused` and `Degraded=True` with `reason=DowngradeRefused`; `kubectl -n e2e-versions get deploy v15 -o jsonpath='{.spec.template.spec.containers[0].image}'` is still `:17`; no new ForgejoBackup was created for `v15`; an event `DowngradeRefused` (Warning) exists; `Ready=True`.
2. **V9.2 recover**: patch `spec.image` back to `codeberg.org/forgejo/forgejo:17`; within 3 min `Degraded` is not True with reason `DowngradeRefused`, `Ready=True`, `VersionSupported=True`.

## V10 — End-of-life version and the upgrade path rule (14)

Forgejo `v14` with `spec.image: codeberg.org/forgejo/forgejo:14` and `spec.upgrade.timeout: 6m`.

1. **V10.1 running**: within 8 min `Running`, `Ready=True`, `status.version` starts with `14.` (an end-of-life version still runs).
2. **V10.2 condition**: `VersionSupported` = `status=False`, `reason=EndOfLife`, message contains `reached its upstream end of life` and `Upgrade to 15 (LTS, 15.0.10 or newer) or 17 (17.0.1 or newer, through 15 first) now`; one `UnsupportedVersion` Warning event; Prometheus `forgejo_operator_version_supported{namespace="e2e-versions",name="v14"}` = 0 with `support="EndOfLife"` (allow 3 min).
3. **V10.3 path refused**: patch `spec.image` to `codeberg.org/forgejo/forgejo:17`. Within 2 min: `Upgrading` reason `UpgradePathUnsupported` with a message containing `Upgrade to Forgejo 15 first`; `Degraded=True` reason `UpgradePathUnsupported`; one `UpgradePathUnsupported` Warning event; the Deployment image is still `:14`; `status.upgrade` is empty (no upgrade started) and no ForgejoBackup with reason PreUpgrade exists for `v14`; `Ready=True`.
4. **V10.4 via 15**: patch `spec.image` to `codeberg.org/forgejo/forgejo:15`; UPGRADE-WATCH(v14) → `Succeeded` within 12 min; `status.version` starts with `15.`; `Degraded` is not True (reason `AsExpected` or absent); `VersionSupported=True/Supported`.
5. **V10.5 free resources**: delete Forgejo `v14` and `v16`.

## V11 — Backup and restore round trip on 17

On `v17` (from V3).

1. **V11.1 data**: ISSUE(v17, demo, "keep me 17"); repo `demo` has `http.txt` and `ssh.txt` from V3.
2. **V11.2 backup**: apply `ForgejoBackup b17` (`spec.forgejoRef.name: v17`). Within 8 min `status.phase=Completed`, `status.databaseSizeBytes > 0`, `status.dataSizeBytes > 0`.
3. **V11.3 change data**: `DELETE $B/api/v1/repos/$U/demo` (204); create repo `after-backup` (auto_init).
4. **V11.4 restore**: apply `ForgejoRestore r17` (`forgejoRef.name: v17`, `backupName: b17`). Record the restore phases and the Forgejo `status.phase` (expect `Restoring` at least once). Restore `Completed` within 10 min.
5. **V11.5 result**: within 3 min after Completed: repo `demo` with `http.txt`, `ssh.txt` and the issue "keep me 17" exists; `after-backup` does not (404); healthz pass; `git clone` of demo over HTTP works; `status.version` starts with `17.`; `VersionSupported=True/Supported`; `Ready=True`.

## V12 — Future versions and other unit-tested behaviour (no cluster test)

A made-up image tag cannot run, so `Future` (an image newer than the operator's table), the version policy on later dates, strict-duration warnings and the prepared Forgejo 18 rendering are verified by unit tests.

1. **V12.1 unit tests**: in the repository root run
   `go test ./internal/forgejo/ ./internal/upgrade/ ./internal/core/ ./internal/autopilot/ -count=1 -v -run 'TestClassify|TestSupportPolicyFollowsTheCalendar|TestTraits|TestDirectUpgrade|TestReportVersionSupport|TestUpgradeGateAcrossMajors|TestConfigFollowsForgejoMajor|TestConfigWarnsAboutDurationsForgejo17Refuses|TestBootstrapScriptPasswordFromStdin|TestConfigGuardMatchesLKGProfile' 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)'`.
   Expect every listed test `--- PASS` and every package `ok`. Record the `--- PASS` lines.
2. **V12.2 cluster part**: record as skipped ("Future needs an image newer than the table; covered by TestClassify/TestReportVersionSupport/TestUpgradeGateAcrossMajors").

## V13 — Upstream version check

1. **V13.1 tool**: in the repository root run `go run ./hack/forgejo-versions; echo "exit=$?"` (needs internet access to codeberg.org, no credentials). Expect `exit=0`, a matrix with rows `15 yes ... verified Supported 15.0.10 15.0.10, 15.0.10-rootless`, `16 ... verified Deprecated` (or `EndOfLife` from 2026-10-29), `17 ... verified Supported 17.0.x`, `18 ... upcoming Future`, and no line starting with `STALE`. If codeberg.org is unreachable (`exit=2`), record that instead of a failure.

## V14 — Cleanup

1. **V14.1 delete**: `kubectl delete namespace e2e-versions --wait=false`, then wait (timeout 6 min) until `kubectl get namespace e2e-versions` returns NotFound. Record whether anything was left behind (e.g. a namespace stuck on finalizers: list `kubectl -n e2e-versions get fj,fjb,fjr,cluster.postgresql.cnpg.io,pvc` and report).
2. **V14.2 metrics gone**: within 3 min after the namespace is gone, Prometheus `forgejo_operator_version_supported{namespace="e2e-versions"}` returns no series (allow for one more scrape interval).
