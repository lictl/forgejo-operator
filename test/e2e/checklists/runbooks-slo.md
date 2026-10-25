<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklist: runbooks and SLOs (namespace `e2e-slo`)

What is checked (design in `docs/observability.md`, "Alerts", "Runbooks" and "Service level
objectives"; runbooks in `docs/runbooks/`): every alert the operator ships links a runbook that
exists in the repository; the promtool rule tests pass; the availability SLI
`forgejo_operator_availability_probes_total` counts one sample per health-check interval, also
while no pod is Ready, and marks restores, upgrades, pauses and the first installation as
planned; an unplanned outage burns the error budget and fires `ForgejoAvailabilityFastBurn`,
recovery ends it, a planned restore does not; the recovery point objective (two backup schedule
intervals) drives `ForgejoBackupTooOld`; the dashboard's SLO panels return data through Grafana.

Two instances run side by side:

| Instance | Spec | Used for |
|----------|------|----------|
| `slo` | No scheduled backups; `autoPilot.selfHealing: {enabled: false, healthCheckInterval: 10s}`; `monitoring.slo: {availabilityTarget: "99.9", window: 1d}`; dashboard on | SLI, burn-rate alerts, planned restore. At 99.9% over one day the burn-rate windows are at their 5m floor (5m/5m, 12m/5m, 48m/5m, 2h24m/12m), so an outage pages within a minute or two and the page ends about 5 minutes after recovery |
| `rpo` | `backup: {enabled: true, schedule: "*/5 * * * *"}` (RPO 10m); default SLO (99.5%, 30d); dashboard on | RPO alert, paused samples |

Shared facts:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- `docker` commands must reach the docker engine the local cluster runs on, so that pulled and built images are visible to the cluster; set `DOCKER_CONTEXT` if that is not your current docker context.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`) with the runbooks/SLO build already deployed, and Prometheus scrapes it (`config/monitoring`). Do not modify, restart or redeploy it: a restart resets the operator's counters, which several checks compare. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep e2e-slo`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`), `ForgejoBackup` (`fjb`), `ForgejoRestore` (`fjr`).
- Prometheus: `http://kps-kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090` (`/api/v1/query` with `curl -s ... --data-urlencode "query=..."`, `/api/v1/rules`, `/api/v1/alerts`). It scrapes the operator every 30 s and evaluates rules every 30 s (the SLO-window group every 3 min). Grafana: `http://kps-grafana.monitoring.svc.cluster.local` with `admin`/`admin`, Prometheus datasource uid `prometheus`. The local cluster must let the host resolve service names directly.
- Helpers used below (define them in every shell):
  ```sh
  NS=e2e-slo
  PROM=http://kps-kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090
  q() { curl -s "$PROM/api/v1/query" --data-urlencode "query=$1" | jq -c '.data.result[] | [.metric, .value[1]]'; }
  # SAMPLES(name): the four availability counters of an instance, as result/planned=value
  samples() { curl -s "$PROM/api/v1/query" --data-urlencode "query=sum by (result, planned) (forgejo_operator_availability_probes_total{namespace=\"$NS\", name=\"$1\"})" | jq -r '[.data.result[] | "\(.metric.result)/\(.metric.planned)=\(.value[1])"] | sort | join(" ")'; }
  # ALERTS(name): the instance's alerts as name[long]=state
  alerts() { curl -s "$PROM/api/v1/alerts" | jq -r --arg n "$1" '[.data.alerts[] | select(.labels.namespace=="e2e-slo" and .labels.forgejo==$n) | "\(.labels.alertname)[\(.labels.long // "")]=\(.state)"] | sort | join(" ")'; }
  RB=https://github.com/lictl/forgejo-operator/blob/main/docs/runbooks
  ```
- Counters reset only when the operator restarts; compare increments (value now minus value before), never absolute values across a restart. `samples` reflects the last scrape (up to 30 s old).
- Repository root: read only. Do not edit files and do not run git commands that change state. `make test-rules` rewrites `test/prometheus/rules*/` with identical content; `git status --short test/prometheus` must stay empty.
- Wait with bounded `until` loops (poll every 5-15 s, explicit timeout) or the Monitor tool. Never wait without a timeout. A fresh install takes 1-8 minutes.
- Work only in namespace `e2e-slo`. Other namespaces belong to other work: do not touch them.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed, with timestamps from `date -u +%T` where timing matters). On failure also collect: `kubectl -n e2e-slo get fj <name> -o yaml` (status part), `kubectl -n e2e-slo get pods,events --sort-by=.lastTimestamp | tail -30`, `kubectl -n e2e-slo get prometheusrule <name> -o yaml`, relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

Reusable procedures (referenced below):
- **OUTAGE-START(name)**: revoke the database login of Forgejo and drop its connections:
  `kubectl -n $NS exec <name>-db-1 -c postgres -- psql -U postgres -d forgejo -c "ALTER ROLE forgejo NOLOGIN" -c "SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename='forgejo'"`. Forgejo's `/api/healthz` fails (`database:ping`) and its pod turns NotReady within about 30 s.
- **OUTAGE-END(name)**: `kubectl -n $NS exec <name>-db-1 -c postgres -- psql -U postgres -d forgejo -c "ALTER ROLE forgejo LOGIN"`. Forgejo reconnects on its own; the pod is Ready again within about 30 s.
- **GRAFANA-SLO(name)**: run every query of the "Service level objectives" row of the instance's dashboard through Grafana's `/api/ds/query` (as the panels do), with the dashboard's constants substituted. Write this to `$W/grafana_slo.py` (`$W`: a fresh temp dir under your scratch area) and run `python3 -I $W/grafana_slo.py e2e-slo <name>`:
  ```python
  import base64, json, sys, urllib.request
  ns, name = sys.argv[1], sys.argv[2]
  G = "http://kps-grafana.monitoring.svc.cluster.local"
  AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
  def call(path, body=None):
      req = urllib.request.Request(G + path, data=json.dumps(body).encode() if body else None,
                                   headers={"Authorization": AUTH, "Content-Type": "application/json"})
      return json.load(urllib.request.urlopen(req, timeout=30))
  d = call(f"/api/dashboards/uid/forgejo-{ns}-{name}")["dashboard"]
  consts = {v["name"]: v["query"] for v in d["templating"]["list"] if v["type"] == "constant"}
  print("links:", [l["url"] for l in d.get("links", [])])
  inrow = False
  for p in d["panels"]:
      if p["type"] == "row":
          inrow = p["title"] == "Service level objectives"
          continue
      if not inrow:
          continue
      for t in p["targets"]:
          e = t["expr"].replace("$__rate_interval", "2m")
          for k, v in consts.items():
              e = e.replace("$" + k, v)
          r = call("/api/ds/query", {"from": "now-5m", "to": "now", "queries": [
              {"refId": "A", "datasource": {"uid": "prometheus", "type": "prometheus"}, "expr": e, "instant": True}]})
          frames = r["results"]["A"].get("frames", [])
          vals = [f["data"]["values"][1][-1] for f in frames if len(f["data"]["values"]) > 1 and f["data"]["values"][1]]
          print(f"{p['title']}\tvalues={vals}\t{e}")
  ```

---

## O0 — Prerequisites

1. **O0.1 build deployed**: `kubectl get crd forgejos.forgejo.lucas.icu -o json | jq -c '.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.monitoring.properties | {slo: .slo.properties | map_values(.default), runbook: (.runbookBaseURL.pattern != null)}'` → `{"slo":{"availabilityTarget":"99.5","window":"30d"},"runbook":true}`. `kubectl -n forgejo-operator-system get deploy forgejo-operator-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="RUNBOOK_BASE_URL")].name}'` → `RUNBOOK_BASE_URL`. `kubectl -n forgejo-operator-system get prometheusrule forgejo-operator-controller-manager-rules` exists.
2. **O0.2 namespace**: `kubectl create namespace e2e-slo`.
3. **O0.3 earlier runs** (before O2): Prometheus keeps the samples of earlier runs of this checklist for its retention, and they still count: the SLI is keyed by namespace and instance name, across operator restarts and across deleting and recreating an instance (by design, `docs/observability.md`, "Availability objective"), and every run uses the same names. Record, per instance, the unplanned down samples an earlier run left in the instance's SLO window, and when the earlier samples end:
   ```sh
   q 'sum by (name) (increase(forgejo_operator_availability_probes_total{namespace="e2e-slo", name="slo", result="down", planned="false"}[1d]))'   # H_slo
   q 'sum by (name) (increase(forgejo_operator_availability_probes_total{namespace="e2e-slo", name="rpo", result="down", planned="false"}[30d]))'  # H_rpo
   q 'max by (name) (max_over_time(timestamp(forgejo_operator_availability_probes_total{namespace="e2e-slo"})[1d:1m]))'                            # T_H (unix time)
   ```
   No output or `0` means no earlier history (`H=0`). This check only records `H_slo`, `H_rpo` and `T_H` (O4.3 and O7.2 depend on them) and always passes. Observed in a rerun 67 min after an earlier run: `H_slo` about 11, `H_rpo` `0`.

## O1 — Rule tests and runbooks in the repository

1. **O1.1 make test-rules**: in the repository root run `make test-rules; echo "exit=$?"` (if the current docker context has no daemon: `DOCKER_CONTEXT=<context of the local cluster's docker engine> make test-rules`). Expect `exit=0`; the `go test` lines `ok`; twelve `Checking rules/... ` / `Checking unit-rules/...` blocks (`basic`, `backups`, `remote`, `ha`, `probes`, `operator` in each directory) each followed by `SUCCESS: <n> rules found`; then seven `SUCCESS` lines, one per `test/prometheus/*_test.yaml`: `backups_test.yaml basic_test.yaml database_upgrade_test.yaml ha_test.yaml operator_test.yaml probes_test.yaml remote_test.yaml`, and no `FAILED`. Afterwards `git status --short test/prometheus` prints nothing.
2. **O1.2 runbook, coverage and dashboard tests**: `go test ./internal/observability/ -count=1 -v -run 'TestPrometheusRuleFiles|TestEveryAlertHasARunbook|TestEveryAlertIsUnitTested|TestDashboardSLOPanels|TestRunbookBaseURL|TestSLOWindowsAndBudget|TestBackupRPO|TestAvailabilitySamplesDoNotHideOutages|TestAvailabilitySamplesFollowTime' 2>&1 | grep -E '^(--- |ok|FAIL)'` → every listed test `--- PASS`, package `ok`.
3. **O1.3 index**: `ls docs/runbooks/*.md | wc -l` → `28` (27 alerts and `README.md`); every one of them except `README.md` is linked from `docs/runbooks/README.md`: `for f in docs/runbooks/Forgejo*.md; do b=$(basename $f); grep -q "($b)" docs/runbooks/README.md || echo "unlinked $b"; done` prints nothing.

## O2 — Instances

Apply:

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata: {name: slo, namespace: e2e-slo}
spec:
  autoPilot:
    selfHealing: {enabled: false, healthCheckInterval: 10s}
  monitoring:
    grafanaDashboard: {enabled: true}
    slo: {availabilityTarget: "99.9", window: 1d}
---
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata: {name: rpo, namespace: e2e-slo}
spec:
  backup: {enabled: true, schedule: "*/5 * * * *"}
  monitoring:
    grafanaDashboard: {enabled: true}
```

1. **O2.1 running**: within 8 min both have `status.phase=Running` and `Ready=True` (`kubectl -n $NS get fj`). Record the time `T_RUN` when both are Running.
2. **O2.2 objects**: `kubectl -n $NS get prometheusrule slo rpo` and `kubectl -n $NS get configmap slo-dashboard rpo-dashboard` exist.
3. **O2.3 SLO refused**: `kubectl -n $NS patch fj slo --type merge --dry-run=server -p '{"spec":{"monitoring":{"slo":{"availabilityTarget":"100"}}}}'` fails with an error naming `spec.monitoring.slo.availabilityTarget`; the same with `{"window":"90m"}` names `spec.monitoring.slo.window`, and with `{"runbookBaseURL":"docs/runbooks"}` (under `monitoring`) names `spec.monitoring.runbookBaseURL`.

## O3 — Every deployed alert links an existing runbook

1. **O3.1 instance rules**: in the repository root:
   ```sh
   for n in slo rpo; do kubectl -n $NS get prometheusrule $n -o json | jq -r '.spec.groups[].rules[] | select(.alert) | "\(.alert) \(.labels.severity) \(.annotations.runbook_url)"' | sort -u | while read a s u; do
     if [ "$u" = "$RB/$a.md" ] && [ -f docs/runbooks/$a.md ] && [ -n "$s" ]; then echo "OK $n $a $s"; else echo "BAD $n $a $s $u"; fi; done; done
   ```
   Expect no `BAD` line, 15 `OK` lines for `slo` and 17 for `rpo`. The alerts of `slo`: ForgejoAutoPilotActive, ForgejoAvailabilityFastBurn (critical), ForgejoAvailabilitySlowBurn, ForgejoBackupFailed, ForgejoDatabaseUpgradeFailed (rendered for CNPG, the default database type), ForgejoDown (critical), ForgejoHealthCheckFailing, ForgejoHealthProbeUnreachable, ForgejoNotReady, ForgejoOperatorMetricsMissing, ForgejoRunnersOffline, ForgejoUpgradeFailed, ForgejoVersionAdvisory, ForgejoVersionUnsupported, ForgejoVolumeAlmostFull; `rpo` has these plus ForgejoBackupTooOld and ForgejoBackupMissing. All severities are `critical` or `warning`.
2. **O3.2 operator rules**: the same loop over `kubectl -n forgejo-operator-system get prometheusrule forgejo-operator-controller-manager-rules -o json` → `OK` for ForgejoOperatorDown (critical), ForgejoOperatorReconcileErrors and ForgejoOperatorWorkQueueBacklog, no `BAD`.
3. **O3.3 SLO rule shape**: `kubectl -n $NS get prometheusrule slo -o json | jq -c '[.spec.groups[] | {name, interval, n: (.rules|length)}]'` → `[{"name":"forgejo.e2e-slo.slo","interval":null,"n":13},{"name":"forgejo-slo.e2e-slo.slo","interval":null,"n":12},{"name":"forgejo-slo-window.e2e-slo.slo","interval":"3m","n":3}]`. `kubectl -n $NS get prometheusrule slo -o json | jq -r '.spec.groups[].rules[] | select(.alert|test("Burn")?) | "\(.alert) \(.labels.long)/\(.labels.short) \(.labels.severity)"'` → `ForgejoAvailabilityFastBurn 5m/5m critical`, `ForgejoAvailabilityFastBurn 12m/5m critical`, `ForgejoAvailabilitySlowBurn 48m/5m warning`, `ForgejoAvailabilitySlowBurn 2h24m/12m warning`. For `rpo` (30d window) the same query gives `1h/5m`, `6h/30m`, `1d/2h`, `3d/6h`.
4. **O3.4 RPO rules**: `kubectl -n $NS get prometheusrule rpo -o json | jq -r '.spec.groups[].rules[] | select(.alert=="ForgejoBackupTooOld" or .alert=="ForgejoBackupMissing") | "\(.alert) for=\(.for) \(.expr)"'` → ForgejoBackupTooOld `for=5m` with an expr ending in `> 600`; ForgejoBackupMissing `for=10m`.
5. **O3.5 loaded**: within 3 min `curl -s $PROM/api/v1/rules | jq -r '.data.groups[] | select(.name|test("e2e-slo")) | "\(.name) \(.interval) \([.rules[].health]|unique)"'` lists the six groups (`forgejo`, `forgejo-slo`, `forgejo-slo-window` for each instance), intervals 30 and 180, health `["ok"]` (a `forgejo-slo-window` group may show `["unknown"]` until its first 3-min evaluation).
6. **O3.6 runbook base override**: `kubectl -n $NS patch fj rpo --type merge -p '{"spec":{"monitoring":{"runbookBaseURL":"https://wiki.example.com/ops/"}}}'`; within 2 min every `runbook_url` of `kubectl -n $NS get prometheusrule rpo -o json` starts with `https://wiki.example.com/ops/Forgejo` and ends in `.md` (no `//` after `ops`), and the dashboard ConfigMap links it: `kubectl -n $NS get configmap rpo-dashboard -o json | jq -r '.data[]' | jq -r '.links[0].url'` → `https://wiki.example.com/ops/README.md`. Revert with `kubectl -n $NS patch fj rpo --type json -p '[{"op":"remove","path":"/spec/monitoring/runbookBaseURL"}]'`; within 2 min the URLs start with `$RB/` again.

## O4 — The availability SLI counts up samples

1. **O4.1 series**: within 3 min after `T_RUN`, `samples slo` and `samples rpo` each list four series `down/false`, `down/true`, `up/false`, `up/true` (all exported from the first reconcile). `down/false=0` for both; `down/true` ≥ 1 for both (the installation before the first successful probe is planned).
2. **O4.2 rate**: `q 'sum by (name) (increase(forgejo_operator_availability_probes_total{namespace="e2e-slo", result="up", planned="false"}[3m]))'` (at least 4 min after `T_RUN`) → `slo` between 12 and 24 (one sample every 10 s), `rpo` between 4 and 8 (every 30 s).
3. **O4.3 recording rules**: `q 'forgejo_slo:availability_burn_rate{namespace="e2e-slo", name="slo"}'` → four series, `window` `5m`, `12m`, `48m`, `2h24m`. With `H_slo=0` (O0.3) all are `0`. With `H_slo>0` every window that starts after `T_H` (now minus the window later than `T_H`) is `0`, while a window reaching back before `T_H` holds the earlier run's samples and may be above 0; `samples slo` still shows `down/false=0` (this run sampled no downtime). Within 4 min: `q 'forgejo_slo:availability_objective{namespace="e2e-slo"}'` → `slo` `0.999` (`window="1d"`) and `rpo` `0.995` (`window="30d"`); `q 'forgejo_slo:availability_error_budget_remaining{namespace="e2e-slo"}'` → `1` for an instance with `H=0`, below `1` for one with `H>0`. Observed in a rerun with `H_slo` about 11, 67 min after the earlier samples ended: `5m`, `12m`, `48m` `0`, `2h24m` `77.2`, budget of `slo` `-76.3`, of `rpo` `1`.
4. **O4.4 no alert**: `alerts slo` and `alerts rpo` print nothing (empty).

## O5 — An unplanned outage burns the budget and pages

1. **O5.1 outage**: record `B5=$(samples slo)`, then OUTAGE-START(slo) and record `T5=$(date -u +%T)`. Within 60 s `kubectl -n $NS get fj slo -o jsonpath='{.status.conditions[?(@.type=="Healthy")].reason}'` is `HealthCheckFailed`, then `NoReadyReplicas` once the pod is NotReady (`kubectl -n $NS get pods -l app.kubernetes.io/instance=slo` shows `0/1`).
2. **O5.2 down samples although no pod is Ready**: 90 s after `T5`, `samples slo` shows `down/false` at least 4 higher than in `B5`, and `up/false` at most 2 higher; `down/true` unchanged. 60 s later `down/false` has grown by at least 3 more (sampling continues while probing is suspended).
3. **O5.3 fast burn**: poll `alerts slo` every 15 s. Within 3 min of `T5`, `ForgejoAvailabilityFastBurn[5m]=firing` and `ForgejoAvailabilityFastBurn[12m]=firing` appear (observed: firing 48 s after the outage started; there is no `for`, so the alert goes straight to firing). `ForgejoAvailabilitySlowBurn[48m]` and `[2h24m]` fire as well. Record the time of the first firing state. `q 'forgejo_slo:availability_burn_rate{namespace="e2e-slo", name="slo", window="5m"}'` > 14.4.
4. **O5.4 alert content**: `curl -s $PROM/api/v1/alerts | jq '.data.alerts[] | select(.labels.alertname=="ForgejoAvailabilityFastBurn" and .labels.forgejo=="slo" and .labels.long=="5m")'` has labels `severity: critical`, `slo: availability`, `short: 5m`, `namespace: e2e-slo`, annotation `runbook_url` = `$RB/ForgejoAvailabilityFastBurn.md`, and a `description` containing `99.9% over 1d` and `Page: act now.`.
Keep the outage for about 2 minutes in total (from `T5`).

## O6 — Recovery ends the page

1. **O6.1 recover**: OUTAGE-END(slo) and record `T6=$(date -u +%T)`. Within 3 min `Ready=True`, Healthy `HealthCheckPassed`, and `up/false` of `samples slo` grows again while `down/false` stops growing.
2. **O6.2 fast burn ends**: poll `alerts slo` every 15 s. Both `ForgejoAvailabilityFastBurn` alerts and `ForgejoAvailabilitySlowBurn[48m]` disappear within 8 min of `T6` (their short window is 5m; observed: 5 min after `T6`). Record the time.
3. **O6.3 slow burn ends**: `ForgejoAvailabilitySlowBurn[2h24m]` (short window 12m) disappears within 16 min of `T6` (observed: 12 min). Wait for it: O8 starts only when `alerts slo` contains no `Burn` alert.
4. **O6.4 budget spent**: `q 'forgejo_slo:availability_error_budget_remaining{namespace="e2e-slo", name="slo"}'` (allow 4 min for the 3-min group) is below `0`: about 2 minutes of downtime exceed the 86 s budget of a 99.9% day. `q 'forgejo_slo:availability_error_ratio{namespace="e2e-slo", name="slo", window="1d"}'` is above `0.001`.

## O7 — The dashboard's SLO panels render

1. **O7.1 slo**: GRAFANA-SLO(slo). Expect the `links:` line `['https://github.com/lictl/forgejo-operator/blob/main/docs/runbooks/README.md']`, and for the panels: `Availability ($slo_window)` one value between 0.5 and 0.999 (below the objective after O5), `Objective` `[0.999]`, `Error budget remaining ($slo_window)` one value below 0, `Burn rate` four values, `Availability samples` at least two series (up and down) with at least one non-zero value. `Recovery point age` and `Recovery point age vs. objective` return no values for `slo` (no backups); that is expected.
2. **O7.2 rpo**: GRAFANA-SLO(rpo) (after at least one scheduled backup, i.e. 5 min after `T_RUN`). Expect `Availability ($slo_window)` `[1]`, `Objective` `[0.995]`, `Error budget remaining` `[1]`, `Burn rate` seven values (windows 5m, 30m, 1h, 2h, 6h, 1d, 3d), each `0` (also a window that holds only planned samples: they count as available; with `H_rpo>0` from O0.3 availability and budget are below 1 and a window reaching back before `T_H` may be above 0, as in O4.3), `Recovery point age` one value between 0 and 600, `Recovery point age vs. objective` a `copy=local` value and the objective `600`.
3. **O7.3 panel queries match the rules**: every `forgejo_slo:` metric in the printed queries is recorded by the instance's PrometheusRule (`kubectl -n $NS get prometheusrule rpo -o json | jq -r '[.spec.groups[].rules[].record // empty] | unique[]'` lists `forgejo_slo:availability_burn_rate`, `forgejo_slo:availability_error_budget_remaining`, `forgejo_slo:availability_error_ratio`, `forgejo_slo:availability_objective`, `forgejo_slo:backup_recovery_point_age_seconds`, `forgejo_slo:backup_recovery_point_objective_seconds`).

## O8 — A planned restore does not burn the budget

Start only when `alerts slo` shows no `Burn` alert (O6.3).

1. **O8.1 backup**: `printf 'apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoBackup\nmetadata: {name: b1}\nspec: {forgejoRef: {name: slo}}\n' | kubectl -n $NS apply -f -`; within 5 min `kubectl -n $NS get fjb b1 -o jsonpath='{.status.phase}'` → `Completed`.
2. **O8.2 restore**: record `B8=$(samples slo)` and `T8`, then apply `printf 'apiVersion: forgejo.lucas.icu/v1alpha1\nkind: ForgejoRestore\nmetadata: {name: r1}\nspec: {forgejoRef: {name: slo}, backupName: b1}\n' | kubectl -n $NS apply -f -`. Poll every 5 s and record the distinct restore phases (`kubectl -n $NS get fjr r1 -o jsonpath='{.status.phase}'`) and the Forgejo phase; expect `ScalingDown`/`Restoring`/`ScalingUp` and `Completed` within 10 min, the Forgejo phase `Restoring` at least once, and Healthy reason `Maintenance` at least once. Record `T8C` (Completed).
3. **O8.3 planned samples**: 2 min after `T8C`, compare `samples slo` with `B8`: `down/true` grew by at least 1 (the restore was sampled, as planned; observed +2 for a restore of 27 s), `down/false` did not grow at all, `up/false` grows again after `T8C`.
4. **O8.4 no burn alert**: poll `alerts slo` every 15 s from `T8` until 6 min after `T8C`: no `ForgejoAvailabilityFastBurn` or `ForgejoAvailabilitySlowBurn` in any state, and no `ForgejoDown` or `ForgejoNotReady` firing (phase `Restoring` suppresses them). `q 'forgejo_slo:availability_burn_rate{namespace="e2e-slo", name="slo", window="5m"}'` stays `0` throughout (planned samples count as available, so the ratio is defined).

## O9 — RPO: the backup age alert follows the schedule

1. **O9.1 baseline**: `rpo` has at least one `Completed` scheduled backup (`kubectl -n $NS get fjb -l forgejo.lucas.icu/instance=rpo`). `q 'forgejo_slo:backup_recovery_point_objective_seconds{namespace="e2e-slo", name="rpo"}'` → `600`; `q 'forgejo_slo:backup_recovery_point_age_seconds{namespace="e2e-slo", name="rpo"}'` → one series `copy="local"`, value below 600. `alerts rpo` contains no `ForgejoBackupTooOld`.
2. **O9.2 stop backups**: wait until a scheduled backup has just completed (`status.backup.lastSuccessfulBackupName` changes to the newest `rpo-scheduled-<yyyymmddhhmm00>`), then at once `kubectl -n $NS patch fj rpo --type merge -p '{"spec":{"paused":true}}'`. Record `TL=$(kubectl -n $NS get fj rpo -o jsonpath='{.status.backup.lastSuccessfulBackupTime}')` and `B9=$(samples rpo)`.
3. **O9.3 paused samples are planned**: 2 min later `kubectl -n $NS get fj rpo` shows `PHASE Paused`, and `samples rpo` shows `up/true` grown by at least 2 against `B9` while `up/false` and `down/false` stayed (the pod still serves; no probe while paused, the sample follows pod readiness).
4. **O9.4 RPO alert**: no new ForgejoBackup of `rpo` appears while paused. `ForgejoBackupTooOld` is absent from `alerts rpo` until `TL`+10m, `pending` after that, and `firing` within `TL`+17m (observed: pending at `TL`+10m30s, firing at `TL`+15m19s). The firing alert has `severity: warning`, `runbook_url` = `$RB/ForgejoBackupTooOld.md` and summary `The last successful backup of Forgejo e2e-slo/rpo is older than its RPO of 10m`. During the whole pause `alerts rpo` has no `Burn` alert.
5. **O9.5 recovery**: `kubectl -n $NS patch fj rpo --type merge -p '{"spec":{"paused":false}}'` and record `T9`. A new scheduled backup completes within 6 min of `T9` (the ticks missed while paused are caught up with one backup right away; observed: completed 5 s after `T9`), `forgejo_slo:backup_recovery_point_age_seconds{name="rpo"}` drops below 600, `ForgejoBackupTooOld` disappears from `alerts rpo` within 2 min after that backup completed (observed: 30 s after `T9`), and `up/false` of `samples rpo` grows again (no longer planned).

## O10 — Cleanup

1. **O10.1 delete**: `kubectl delete namespace e2e-slo --wait=false`, then wait (timeout 6 min) until `kubectl get namespace e2e-slo` returns NotFound. Record whether anything was left behind.
2. **O10.2 series gone**: within 3 min after the namespace is gone, `q 'forgejo_operator_availability_probes_total{namespace="e2e-slo"}'` returns nothing (allow one more scrape interval), and `curl -s $PROM/api/v1/rules | jq -r '.data.groups[].name' | grep e2e-slo` prints nothing.
