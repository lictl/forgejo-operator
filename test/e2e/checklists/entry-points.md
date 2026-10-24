<!-- Manual end-to-end checklist executed by an agent against a local cluster (see .claude/workflows/).
     Ids are stable; the CI slice will port these checks to Ginkgo specs. -->

# forgejo-operator end-to-end checklist: entry points (namespace `e2e-entry`)

SSH host keys, Gateway API routes (Envoy Gateway), cert-manager TLS and the Ingress path. Design
and user documentation: [docs/networking.md](../../../docs/networking.md),
[docs/ssh.md](../../../docs/ssh.md).

Shared facts:
- `LOCAL_KUBE_CONTEXT` is the kube context of the local test cluster. Never point it at a real cluster.
- `export KUBECONFIG=$OPERATOR_KUBECONFIG` in every shell that runs kubectl or helm. Create it once with `kubectl config view --minify --flatten --context "$LOCAL_KUBE_CONTEXT" > "$OPERATOR_KUBECONFIG"` so that it holds only that context; never use another context, in particular never a real cluster such as `k3s`.
- The operator runs in-cluster (namespace `forgejo-operator-system`, deployment `forgejo-operator-controller-manager`, image `forgejo-operator:dev`) with the entry-points build already deployed. Do not modify or redeploy it. You may read its logs: `kubectl -n forgejo-operator-system logs deploy/forgejo-operator-controller-manager --since=30m | grep e2e-entry`.
- API group/kinds: `forgejo.lucas.icu/v1alpha1` — `Forgejo` (short `fj`), `ForgejoBackup` (`fjb`), `ForgejoRestore` (`fjr`).
- Lab components, already installed (do not reinstall, upgrade or uninstall them):
  - **Envoy Gateway v1.9.2** (Helm release `eg`, chart `oci://docker.io/envoyproxy/gateway-helm`) in `envoy-gateway-system`. Its chart installed the Gateway API **v1.6.1 experimental-channel** CRDs: TCPRoute serves `v1` and `v1alpha2`, ReferenceGrant `v1` and `v1beta1`.
  - **cert-manager v1.21.2** (Helm release `cert-manager`, `crds.enabled=true`) in `cert-manager`, with ClusterIssuers `selfsigned` and `lab-ca` (a CA whose certificate is in Secret `cert-manager/lab-ca`, key `ca.crt`).
  - **Traefik 3.7.16** (Helm release `traefik`, chart `traefik/traefik` 42.1.2) in `traefik`, used as an Ingress controller only: IngressClass `traefik`, ClusterIP Service `traefik` (ports 80, 443), its CRD and Gateway providers disabled.
  - GatewayClass `eg` and Gateway `eg` in `envoy-gateway-system` with listeners `http` (80, HTTP, any host), `https-gw` (443, HTTPS, host `gw.entry.test`, certificate `Secret e2e-entry/gw-tls`, which the operator's cert-manager Certificate creates) and `ssh` (2222, TCP), all `allowedRoutes: {namespaces: {from: All}}`; ClientTrafficPolicy `eg` with `path.escapedSlashesAction: KeepUnchanged` and `headers.earlyRequestHeaders.remove: [X-Forwarded-For]`. The manifest is in E0.3.
- Reaching the Gateway from the host: Envoy Gateway serves Gateway `eg` through the LoadBalancer Service `envoy-envoy-gateway-system-eg-<hash>` in `envoy-gateway-system`; the local cluster must give it an EXTERNAL-IP that the host reaches directly (192.168.139.2 when this checklist was written), which is also the Gateway's `status.addresses[0].value`. The Service's DNS name `envoy-envoy-gateway-system-eg-<hash>.envoy-gateway-system.svc.cluster.local` must resolve from the host too. The `*.entry.test` names resolve nowhere: send `Host:` headers for HTTP, `curl --resolve gw.entry.test:443:$GW` for HTTPS and `git -c http.curloptResolve=gw.entry.test:443:$GW` for git. Do not edit `/etc/hosts`.
- Reaching Traefik: its ClusterIP (`$TIP` below) and `traefik.traefik.svc.cluster.local` must be reachable from the host.
- An instance `<name>` serves HTTP at `http://<name>-http.e2e-entry.svc.cluster.local:3000` and SSH at `<name>-ssh.e2e-entry.svc.cluster.local:22`; the local cluster must let the host resolve these names directly.
- Variables, in every shell (`$W` is a fresh directory under your scratch area):
  ```sh
  export KUBECONFIG=$OPERATOR_KUBECONFIG
  NS=e2e-entry; W=<your scratch dir>/entry; mkdir -p $W
  GW=$(kubectl -n envoy-gateway-system get gateway eg -o jsonpath='{.status.addresses[0].value}')
  TIP=$(kubectl -n traefik get svc traefik -o jsonpath='{.spec.clusterIP}')
  H=gw.entry.test
  creds() { N=$1; U=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.username}' | base64 -d); P=$(kubectl -n $NS get secret $N-admin -o jsonpath='{.data.password}' | base64 -d); }
  TLS=(--cacert $W/lab-ca.crt --resolve $H:443:$GW)                         # curl over HTTPS through the Gateway
  GITTLS=(-c http.sslCAInfo=$W/lab-ca.crt -c http.curloptResolve=$H:443:$GW)  # git over HTTPS through the Gateway
  ```
  These are bash arrays: run the checks with bash (`bash -c '...'`, or `bash` scripts). Admin passwords only contain URL-safe characters, so `https://$U:$P@host/...` works.
- Wait with bounded `until` loops (poll every 5-10 s, explicit timeout) or the Monitor tool. Never wait without a timeout. A fresh install can take up to 8 min.
- Work only in namespace `e2e-entry`. You may read objects in `envoy-gateway-system`, `cert-manager` and `traefik`; the only change allowed there is re-applying the E0.3 manifest. Do not edit files in the repository and do not run git commands that change state.
- For every check record: id, passed (true/false), evidence (the decisive command(s) and the observed output, trimmed). On failure also collect: `kubectl -n e2e-entry get fj <name> -o yaml` (status part), `kubectl -n e2e-entry get pods,events --sort-by=.lastTimestamp | tail -30`, `kubectl -n e2e-entry get httproute,tcproute,certificate,referencegrant -o yaml` where relevant, the Gateway's `status.listeners`, and relevant operator log lines. Continue with the remaining checks when one fails, unless it is impossible.

Reusable procedures (referenced below):
- **FP(name)**: the fingerprints in status and the ones the SSH Service serves, each sorted:
  ```sh
  kubectl -n $NS get fj <name> -o jsonpath='{range .status.ssh.hostKeys[*]}{.fingerprint}{"\n"}{end}' | sort
  ssh-keyscan -p 22 <name>-ssh.$NS.svc.cluster.local 2>/dev/null | ssh-keygen -lf - | awk '{print $2}' | sort
  ```
  Both lists have the same three lines. Right after a pod start `ssh-keyscan` may print nothing; retry for up to 60 s.
- **PROBE(headers...)**: a request through the Gateway that Forgejo logs, and the client address Forgejo logged for it:
  ```sh
  T=probe-$(date +%s%N); curl -s -o /dev/null -w '%{http_code}\n' "${TLS[@]}" <headers> "https://$H/explore/repos?probe=$T"
  sleep 2; kubectl -n $NS logs deploy/gw -c forgejo --since=2m | grep "$T"
  ```
  The log line reads `... router: completed GET /explore/repos?probe=<T> for <address>, 200 OK ...`; `<address>` is what Forgejo takes as the client's address.
- **NOROLL(name)**: record `kubectl -n $NS get deploy <name> -o jsonpath='{.metadata.generation}'` before a patch; "no rollout" means it is unchanged 60 s after the patch.

---

## E0 — Prerequisites

1. **E0.1 CRDs**: `kubectl get crd httproutes.gateway.networking.k8s.io tcproutes.gateway.networking.k8s.io referencegrants.gateway.networking.k8s.io certificates.cert-manager.io` lists all four. `kubectl get crd tcproutes.gateway.networking.k8s.io -o jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version} {.metadata.annotations.gateway\.networking\.k8s\.io/channel} {range .spec.versions[*]}{.name}={.served} {end}'` → `v1.6.1 experimental v1=true v1alpha2=true`.
2. **E0.2 lab components**: every pod in `envoy-gateway-system`, `cert-manager` and `traefik` is `Running` and Ready; `kubectl get clusterissuer selfsigned lab-ca` shows `READY True` for both; `helm list -A` shows releases `eg` (chart `gateway-helm-v1.9.2`), `cert-manager` (`cert-manager-v1.21.2`) and `traefik` (`traefik-42.1.2`); `kubectl get ingressclass traefik -o jsonpath='{.spec.controller}'` → `traefik.io/ingress-controller`; `curl -s -o /dev/null -w '%{http_code}' http://$TIP/` → `404`.
3. **E0.3 Gateway**: apply (idempotent):
   ```yaml
   apiVersion: gateway.networking.k8s.io/v1
   kind: GatewayClass
   metadata: {name: eg}
   spec:
     controllerName: gateway.envoyproxy.io/gatewayclass-controller
   ---
   apiVersion: gateway.networking.k8s.io/v1
   kind: Gateway
   metadata: {name: eg, namespace: envoy-gateway-system}
   spec:
     gatewayClassName: eg
     listeners:
     - {name: http, port: 80, protocol: HTTP, allowedRoutes: {namespaces: {from: All}}}
     - name: https-gw
       port: 443
       protocol: HTTPS
       hostname: gw.entry.test
       tls:
         mode: Terminate
         certificateRefs: [{name: gw-tls, namespace: e2e-entry}]
       allowedRoutes: {namespaces: {from: All}}
     - {name: ssh, port: 2222, protocol: TCP, allowedRoutes: {namespaces: {from: All}}}
   ---
   apiVersion: gateway.envoyproxy.io/v1alpha1
   kind: ClientTrafficPolicy
   metadata: {name: eg, namespace: envoy-gateway-system}
   spec:
     targetRefs: [{group: gateway.networking.k8s.io, kind: Gateway, name: eg}]
     path: {escapedSlashesAction: KeepUnchanged}
     headers:
       earlyRequestHeaders: {remove: [X-Forwarded-For]}
   ```
   Within 2 min the Gateway is `Programmed=True` and `$GW` is non-empty; record it and `kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=eg -o wide` (TYPE `LoadBalancer`, EXTERNAL-IP = `$GW`). Listeners `http` and `ssh` are `Programmed=True` (`kubectl -n envoy-gateway-system get gateway eg -o jsonpath='{range .status.listeners[*]}{.name}: {range .conditions[*]}{.type}={.status}/{.reason} {end}{"\n"}{end}'`); `https-gw` is `ResolvedRefs=False/RefNotPermitted` until E3 creates its Secret and ReferenceGrant (expected; record it). ClientTrafficPolicy `eg` has `Accepted=True` (`kubectl -n envoy-gateway-system get ctp eg -o jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}'`). `curl -s -o /dev/null -w '%{http_code}' http://$GW/` → `404` (no route yet).
4. **E0.4 operator build**: `kubectl get crd forgejos.forgejo.lucas.icu -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.gateway.properties.requestTimeout.default} {.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.gateway.properties.removeRequestHeaders.maxItems}'` → `0s 15` (no default list: the route always removes `X-Real-IP`, plus up to 15 headers); the CRD's `spec.properties` has `tls` and `server.properties` has `trustedProxies`; `kubectl get crd forgejos.forgejo.lucas.icu -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.ssh.properties.hostKeysSource.enum}'` → `["Generated","ExistingSecret"]`. `kubectl get clusterrole forgejo-operator-manager-role -o jsonpath='{.rules[?(@.apiGroups[0]=="gateway.networking.k8s.io")].resources} {.rules[?(@.apiGroups[0]=="gateway.networking.k8s.io")].verbs} {.rules[?(@.apiGroups[0]=="cert-manager.io")].resources} {.rules[?(@.apiGroups[0]=="cert-manager.io")].verbs}'` → `["httproutes","referencegrants","tcproutes"] ["create","delete","get","update"] ["certificates"] ["create","delete","get","update"]` (read live, never listed, watched or patched; see docs/networking.md#permissions).
5. **E0.5 namespace, CA and path validation**: `kubectl create namespace e2e-entry`; `kubectl -n cert-manager get secret lab-ca -o jsonpath='{.data.ca\.crt}' | base64 -d > $W/lab-ca.crt`; `openssl x509 -in $W/lab-ca.crt -noout -subject` → `subject=CN=forgejo-operator lab CA`. `ssh-keygen -q -t ed25519 -N '' -f $W/key` (client key for E2.9). The CRD refuses Gateway API paths that `HTTPPathMatch` refuses (nothing is created): `for p in /a//b /a/%2F /git/; do printf 'apiVersion: forgejo.lucas.icu/v1alpha1\nkind: Forgejo\nmetadata: {name: pathcheck, namespace: e2e-entry}\nspec: {gateway: {parentRefs: [{name: eg}], path: "%s"}}\n' "$p" | kubectl apply --dry-run=server -f -; done` → `spec.gateway.path: Invalid value: "/a//b": must not contain '//' (Gateway API HTTPPathMatch)`, `spec.gateway.path: Invalid value: "/a/%2F": must not contain '%2F' (Gateway API HTTPPathMatch)`, `forgejo.forgejo.lucas.icu/pathcheck created (server dry run)`.
6. **E0.6 instances**: apply all three at once (they start in parallel):
   ```yaml
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: Forgejo
   metadata: {name: keys, namespace: e2e-entry}
   spec: {}
   ---
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: Forgejo
   metadata: {name: gw, namespace: e2e-entry}
   spec:
     server:
       domain: gw.entry.test
       ssh: {externalPort: 2222}
     gateway:
       parentRefs:
       - {name: eg, namespace: envoy-gateway-system, sectionName: https-gw}
       ssh:
         parentRefs:
         - {name: eg, namespace: envoy-gateway-system, sectionName: ssh}
     tls:
       certManager:
         issuerRef: {name: lab-ca, kind: ClusterIssuer}
       gatewayNamespaces: [envoy-gateway-system]
   ---
   apiVersion: forgejo.lucas.icu/v1alpha1
   kind: Forgejo
   metadata: {name: ing, namespace: e2e-entry}
   spec:
     ingress: {enabled: true, className: traefik, host: ing.entry.test}
     tls:
       certManager:
         issuerRef: {name: lab-ca, kind: ClusterIssuer}
   ```
   Right after apply, `kubectl -n e2e-entry get fj gw -o jsonpath='{.spec.gateway.path} {.spec.gateway.requestTimeout} [{.spec.gateway.removeRequestHeaders}]'` → `/ 0s []` (API defaults; no extra headers). Within 8 min all three are `status.phase=Running` with `Ready=True`; record the elapsed times.

## E1 — SSH host keys (instance `keys`)

1. **E1.1 generated**: `kubectl -n $NS get fj keys -o jsonpath='{.status.ssh.hostKeysSource} {.status.ssh.hostKeysSecretName} {.status.ssh.hostKeys[*].type}'` → `Generated keys-ssh-host-keys ssh-ed25519 ecdsa-sha2-nistp256 ssh-rsa`; each `fingerprint` starts with `SHA256:`, each `publicKey` with its type; there is no `status.ssh.message`. Secret `keys-ssh-host-keys` has the annotation `forgejo.lucas.icu/ssh-host-keys-source: Generated`, the label `app.kubernetes.io/managed-by: forgejo-operator`, no `ownerReferences` (default `retainPolicy: Retain`) and exactly the data keys `ssh_host_ecdsa_key`, `ssh_host_ed25519_key`, `ssh_host_rsa_key`. A Normal event `SSHHostKeysGenerated` exists for `keys`. Record the three fingerprints as **FP0**.
2. **E1.2 served from the Secret**: `kubectl -n $NS get secret keys-config -o jsonpath='{.data.app\.ini}' | base64 -d | grep SSH_SERVER_HOST_KEYS` → `SSH_SERVER_HOST_KEYS = /etc/forgejo-ssh/ssh_host_ed25519_key,/etc/forgejo-ssh/ssh_host_ecdsa_key,/etc/forgejo-ssh/ssh_host_rsa_key`. The Deployment `keys` has a volume `ssh-host-keys` from Secret `keys-ssh-host-keys` (items: the three keys) and an emptyDir `ssh-host-keys-copy` with `medium: Memory`. Its first init container is `ssh-host-keys`: it mounts `ssh-host-keys` read-only at `/etc/forgejo-ssh-secret` and `ssh-host-keys-copy` at `/etc/forgejo-ssh`, and has env `SSH_HOST_KEYS_DIGEST` equal to the pod-template annotation `forgejo.lucas.icu/ssh-host-keys`. Container `forgejo` mounts only `ssh-host-keys-copy`, read-only at `/etc/forgejo-ssh`; `bootstrap` mounts neither. `kubectl -n $NS logs deploy/keys -c ssh-host-keys` → `SSH host keys copied from Secret keys-ssh-host-keys: ssh_host_ed25519_key ssh_host_ecdsa_key ssh_host_rsa_key (digest <the annotation>)`. `kubectl -n $NS get jobs` → `No resources found` (the operator runs no Jobs for host keys).
3. **E1.3 clients see them**: FP(keys) → both lists equal FP0.
4. **E1.4 pod deletion**: record `kubectl -n $NS get secret keys-ssh-host-keys -o jsonpath='{.data}' | shasum`; delete the server pod (`kubectl -n $NS delete pod -l app.kubernetes.io/instance=keys,app.kubernetes.io/component=server`); within 4 min a new pod is Ready; FP(keys) → both lists equal FP0; the Secret's data hash is unchanged.
5. **E1.5 backup copy**: apply `ForgejoBackup keys-b1` (`spec.forgejoRef.name: keys`, `spec.includeData: false`); within 5 min `status.phase=Completed`. Read its files with a pod next to the backup volume:
   ```yaml
   apiVersion: v1
   kind: Pod
   metadata: {name: backup-shell, namespace: e2e-entry}
   spec:
     securityContext: {runAsUser: 1000, runAsGroup: 1000, fsGroup: 1000, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
     containers:
     - name: shell
       image: codeberg.org/forgejo/forgejo:15
       command: ["sleep", "infinity"]
       securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
       volumeMounts: [{name: backups, mountPath: /backups, readOnly: true}]
     volumes:
     - name: backups
       persistentVolumeClaim: {claimName: keys-backups, readOnly: true}
   ```
   `kubectl -n $NS exec backup-shell -- ls /backups/keys-b1/ssh-host-keys` → `ssh_host_ecdsa_key ssh_host_ed25519_key ssh_host_rsa_key`; `kubectl -n $NS exec backup-shell -- cat /backups/keys-b1/manifest.json` contains `"sshHostKeys": {"directory": "ssh-host-keys", "secret": "keys-ssh-host-keys", "files": ["ssh_host_ecdsa_key", "ssh_host_ed25519_key", "ssh_host_rsa_key"]}`. Copy them out: `mkdir -p $W/keys-b1; for k in ssh_host_ed25519_key ssh_host_ecdsa_key ssh_host_rsa_key; do kubectl -n $NS exec backup-shell -- cat /backups/keys-b1/ssh-host-keys/$k > $W/keys-b1/$k; done; chmod 600 $W/keys-b1/*`; `for k in $W/keys-b1/*; do ssh-keygen -lf $k | awk '{print $2}'; done | sort` equals FP0. Delete the pod.
6. **E1.6 restore keeps the keys**: apply `ForgejoRestore keys-r1` (`spec.forgejoRef.name: keys`, `spec.backupName: keys-b1`); within 10 min `status.phase=Completed` (phases `ScalingDown`, `Restoring`, `ScalingUp`, `Completed`) and `keys` is Ready again. FP(keys) → FP0.
7. **E1.7 delete and re-create (Retain)**: record `kubectl -n $NS get secret keys-ssh-host-keys -o jsonpath='{.metadata.uid}'`; `kubectl -n $NS delete fj keys`; within 2 min Deployment `keys` is gone while Secret `keys-ssh-host-keys` still exists with the same `uid` and no `ownerReferences`. Re-apply Forgejo `keys` with `spec: {}`; within 8 min Running and Ready; `status.ssh.hostKeysSource` = `Generated`; FP(keys) → FP0; the Secret has the same `uid` and data hash as in E1.4.
8. **E1.8 another instance name, `existingSecret`**: a backup can only be restored into the instance it was taken of (`ForgejoRestore` refuses `keys-b1` for another name), but its keys can serve any instance. `kubectl -n $NS create secret generic keys-copy --from-file=$W/keys-b1/`; apply Forgejo `keys2` with `spec.server.ssh.hostKeys.existingSecret: keys-copy`. Within 8 min Running; `status.ssh.hostKeysSource` = `ExistingSecret`, `hostKeysSecretName` = `keys-copy`; FP(keys2) → FP0; `kubectl -n $NS get secret keys2-ssh-host-keys` → NotFound (nothing generated); the Deployment `keys2` mounts Secret `keys-copy`.
9. **E1.9 another instance name, pre-created Secret**: `kubectl -n $NS create secret generic keys3-ssh-host-keys --from-file=$W/keys-b1/`; apply Forgejo `keys3` with `spec: {}`. Within 8 min Running; `status.ssh.hostKeysSource` = `ExistingSecret`, `hostKeysSecretName` = `keys3-ssh-host-keys`; FP(keys3) → FP0; the Secret still holds exactly its three keys (data unchanged) and gained the label `app.kubernetes.io/managed-by: forgejo-operator`. Then delete Forgejo `keys2` and `keys3` (frees resources).
10. **E1.10 missing Secret is not regenerated**: NOROLL(keys); `kubectl -n $NS delete secret keys-ssh-host-keys`. Within 90 s `Degraded=True` with reason `SSHHostKeysUnavailable` and a message naming `keys-ssh-host-keys` and containing `does not generate new host keys`; a Warning event `SSHHostKeysUnavailable` exists. After another 60 s: `kubectl -n $NS get secret keys-ssh-host-keys` → still NotFound, no rollout, `status.ssh.hostKeys` still FP0, FP(keys) → FP0. Re-create it: `kubectl -n $NS create secret generic keys-ssh-host-keys --from-file=$W/keys-b1/`; within 90 s `Degraded` is not True, still no rollout (same keys, same digest), and `status.ssh.hostKeysSource` = `ExistingSecret` (re-created without the operator's annotation).

11. **E1.11 a broken edit never reaches the pods; the workload is held**: NOROLL(keys). Break the Secret the way a user might, with the public keys in place of the private ones: `for k in ssh_host_ed25519_key ssh_host_ecdsa_key ssh_host_rsa_key; do ssh-keygen -y -f $W/keys-b1/$k > $W/$k.pub; done; kubectl -n $NS create secret generic keys-ssh-host-keys --from-file=ssh_host_ed25519_key=$W/ssh_host_ed25519_key.pub --from-file=ssh_host_ecdsa_key=$W/ssh_host_ecdsa_key.pub --from-file=ssh_host_rsa_key=$W/ssh_host_rsa_key.pub --dry-run=client -o yaml | kubectl apply -f -`.
    - Within 90 s `Degraded=True/SSHHostKeysUnavailable`, message containing `ssh_host_ed25519_key: ssh: no key found` and `leaves Deployment keys as it is`.
    - Then `kubectl -n $NS patch fj keys --type=merge -p '{"spec":{"config":{"ui":{"DEFAULT_THEME":"forgejo-dark"}}}}'` and apply `ForgejoRestore keys-r2` (`spec.forgejoRef.name: keys`, `spec.backupName: keys-b1`). After 75 s: no rollout; `kubectl -n $NS get secret keys-config -o jsonpath='{.data.app\.ini}' | base64 -d | grep -c DEFAULT_THEME` → `0`; `keys-r2` is `Pending` with a message starting `Waiting for the SSH host keys: Secret keys-ssh-host-keys`; `kubectl -n $NS exec deploy/keys -c forgejo -- head -c 35 /etc/forgejo-ssh/ssh_host_ed25519_key` → `-----BEGIN OPENSSH PRIVATE KEY-----` (the pod's copy; the kubelet has updated the Secret volume meanwhile).
    - Container restart in place: `kubectl -n $NS exec <server pod> -c forgejo -- kill 1`; within 2 min `restartCount` 1 and Ready; FP(keys) → FP0 (without the pod's copy Forgejo would serve a random RSA key here).
    - New pod: delete the server pod; within 60 s the new pod is `Init:Error` or `Init:CrashLoopBackOff`, and the termination message of its init container `ssh-host-keys` (`.status.initContainerStatuses[0].lastState.terminated.message`, or `.state.terminated.message`) contains `holds other SSH host keys` and the digest of the pod-template annotation.
    - Fix: `kubectl -n $NS create secret generic keys-ssh-host-keys --from-file=$W/keys-b1/ --dry-run=client -o yaml | kubectl apply -f -`. Within 90 s `Degraded` is not True; within 10 min `keys-r2` is `Completed`, the Deployment rolled the held change (generation increased, app.ini has `DEFAULT_THEME = forgejo-dark`), `keys` is Ready and FP(keys) → FP0.

## E2 — Gateway API: HTTPRoute and TCPRoute (instance `gw`)

1. **E2.1 HTTPRoute**: `kubectl -n $NS get httproute gw -o yaml`: a controller owner reference to Forgejo `gw`; labels `app.kubernetes.io/instance: gw`, `app.kubernetes.io/managed-by: forgejo-operator`; `spec.parentRefs` = `[{group: gateway.networking.k8s.io, kind: Gateway, name: eg, namespace: envoy-gateway-system, sectionName: https-gw}]`; `spec.hostnames` = `[gw.entry.test]`; one rule with `matches: [{path: {type: PathPrefix, value: /}}]`, `filters: [{type: RequestHeaderModifier, requestHeaderModifier: {remove: [X-Real-IP]}}]`, `backendRefs: [{group: "", kind: Service, name: gw-http, port: 3000, weight: 1}]`, `timeouts: {request: 0s}`.
2. **E2.2 TCPRoute v1**: `kubectl -n $NS get tcproutes.v1.gateway.networking.k8s.io gw-ssh -o yaml` (the `v1` version): `spec.parentRefs` = `[{group: gateway.networking.k8s.io, kind: Gateway, name: eg, namespace: envoy-gateway-system, sectionName: ssh}]`, `spec.rules[0].backendRefs` = `[{group: "", kind: Service, name: gw-ssh, port: 22, weight: 1}]`, a controller owner reference to Forgejo `gw`. The choice between `v1` and `v1alpha2` is unit-tested in E7.
3. **E2.3 RoutesReady**: within 3 min after `gw` is Ready: `kubectl -n $NS get fj gw -o jsonpath='{.status.conditions[?(@.type=="RoutesReady")]}'` → `status: True`, `reason: Accepted`, message `Accepted: HTTPRoute gw on envoy-gateway-system/eg listener https-gw; TCPRoute gw-ssh on envoy-gateway-system/eg listener ssh`. A Normal event `RoutesReady` exists for `gw` (before the first pod was ready, Envoy Gateway may have reported `RefsNotResolved`/`EndpointsNotFound`, with a Warning event `RefsNotResolved`; record it, it is not a failure). The routes' own status agrees: `kubectl -n $NS get httproute gw -o jsonpath='{range .status.parents[*]}{range .conditions[*]}{.type}={.status} {end}{"\n"}{end}'` → `Accepted=True ResolvedRefs=True` (same for `tcproute gw-ssh`). `kubectl -n envoy-gateway-system get gateway eg -o jsonpath='{range .status.listeners[*]}{.name}:{.attachedRoutes} {end}'` → `http:0 https-gw:1 ssh:1` (the HTTPRoute is not on the plain HTTP listener).
4. **E2.4 public identity**: `kubectl -n $NS get secret gw-config -o jsonpath='{.data.app\.ini}' | base64 -d | grep -E '^(DOMAIN|ROOT_URL|SSH_DOMAIN|SSH_PORT|REVERSE_PROXY_TRUSTED_PROXIES) '` → `DOMAIN = gw.entry.test`, `ROOT_URL = https://gw.entry.test/`, `SSH_DOMAIN = gw.entry.test`, `SSH_PORT = 2222`, `REVERSE_PROXY_TRUSTED_PROXIES = 127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7`. `kubectl -n $NS get fj gw -o jsonpath='{.status.url}'` → `https://gw.entry.test/`.
5. **E2.5 UI through the Gateway, HTTPS only**: `curl -s "${TLS[@]}" -o /dev/null -w '%{http_code}\n' https://$H/` → `200`; `curl -s "${TLS[@]}" https://$H/api/healthz` contains `"status": "pass"`. Plain HTTP is not served: `curl -s -o /dev/null -w '%{http_code}\n' -H "Host: $H" http://$GW/` → `404` (the route is attached to listener `https-gw` only, so credentials never travel in clear text and a platform redirect on `http` would apply).
6. **E2.6 git over HTTPS and a large push**: `creds gw`; `curl -s "${TLS[@]}" -u "$U:$P" -H 'Content-Type: application/json' -X POST https://$H/api/v1/user/repos -d '{"name":"demo","auto_init":true}' -o /dev/null -w '%{http_code}\n'` → `201`. `git "${GITTLS[@]}" clone https://$U:$P@$H/$U/demo.git $W/demo`; in it `dd if=/dev/urandom of=big.bin bs=1M count=60`, `git add big.bin`, `git -c user.email=e2e@example.com -c user.name=e2e commit -m big`, `git "${GITTLS[@]}" push origin HEAD` → exit 0, no `RPC failed`/`502`/`504`; record the duration. `curl -s "${TLS[@]}" -u "$U:$P" https://$H/api/v1/repos/$U/demo` has `"clone_url":"https://gw.entry.test/<user>/demo.git"` and `"ssh_url":"ssh://git@gw.entry.test:2222/<user>/demo.git"`.
7. **E2.7 slow request longer than 20 s**: `curl -s "${TLS[@]}" -u "$U:$P" --limit-rate 2M -o /dev/null -w '%{http_code} %{size_download} %{time_total}\n' https://$H/$U/demo/raw/branch/main/big.bin` → `200 62914560 <t>` with `t` > 20 (about 30 s; Envoy Gateway's own default would end it after 15 s) and curl exit code 0. Control: NOROLL(gw); `kubectl -n $NS patch fj gw --type=merge -p '{"spec":{"gateway":{"requestTimeout":"10s"}}}'`; within 60 s `kubectl -n $NS get httproute gw -o jsonpath='{.spec.rules[0].timeouts.request}'` → `10s`; wait 10 s more, then the same download ends after about 10-12 s with fewer than 62914560 bytes (curl exit code 18, or a non-200 code). Restore: `kubectl -n $NS patch fj gw --type=json -p '[{"op":"remove","path":"/spec/gateway/requestTimeout"}]'`; `spec.gateway.requestTimeout` reads `0s` again (API default) and within 60 s so does the route's `timeouts.request`; the download takes > 20 s again with all bytes. No rollout (a route change does not roll Forgejo).
8. **E2.8 a client's X-Real-IP is not trusted**: PROBE(`-H 'X-Real-IP: 203.0.113.7'`) → `200`, and the logged address is not `203.0.113.7` (it is the address Envoy saw, e.g. `192.168.194.1:0` on a local cluster). PROBE(`-H 'X-Forwarded-For: 198.51.100.9'`) → the logged address does not contain `198.51.100.9` (the ClientTrafficPolicy drops a client's X-Forwarded-For before Envoy appends the real one; see docs/networking.md). Control (listing other headers does not drop `X-Real-IP`): NOROLL(gw); `kubectl -n $NS patch fj gw --type=merge -p '{"spec":{"gateway":{"removeRequestHeaders":["X-Forwarded-For"]}}}'`; within 60 s `kubectl -n $NS get httproute gw -o jsonpath='{.spec.rules[0].filters[0].requestHeaderModifier.remove}'` → `["X-Real-IP","X-Forwarded-For"]`; wait 10 s; PROBE(`-H 'X-Real-IP: 203.0.113.7'`) → the logged address is not `203.0.113.7` (it is the Envoy pod's address now, e.g. `192.168.194.10:<port>`, since the route also drops the `X-Forwarded-For` Envoy added). Restore: `kubectl -n $NS patch fj gw --type=json -p '[{"op":"remove","path":"/spec/gateway/removeRequestHeaders"}]'`; within 60 s the route's `remove` is `["X-Real-IP"]` again. No rollout.
9. **E2.9 SSH through the TCPRoute**: `curl -s "${TLS[@]}" -u "$U:$P" -H 'Content-Type: application/json' -X POST https://$H/api/v1/user/keys -d "{\"title\":\"e2e\",\"key\":\"$(cat $W/key.pub)\"}" -o /dev/null -w '%{http_code}\n'` → `201`. Known hosts from status only: `kubectl -n $NS get fj gw -o jsonpath="{range .status.ssh.hostKeys[*]}[$GW]:2222 {.publicKey}{'\n'}{end}" > $W/known_hosts` (three lines). `GIT_SSH_COMMAND="ssh -i $W/key -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$W/known_hosts" git clone ssh://git@$GW:2222/$U/demo.git $W/demo-ssh` → exit 0 (strict host key checking against the status keys) and `$W/demo-ssh/big.bin` is 62914560 bytes. `ssh-keyscan -p 2222 $GW 2>/dev/null | ssh-keygen -lf - | awk '{print $2}' | sort` equals the sorted fingerprints in `status.ssh.hostKeys` of `gw`.

## E3 — cert-manager TLS (instance `gw`)

1. **E3.1 Certificate**: `kubectl -n $NS get certificate gw-tls -o jsonpath='{.spec.secretName} {.spec.dnsNames} {.spec.issuerRef.group}/{.spec.issuerRef.kind}/{.spec.issuerRef.name} {.status.conditions[?(@.type=="Ready")].status} {.metadata.ownerReferences[0].kind}/{.metadata.ownerReferences[0].name}'` → `gw-tls ["gw.entry.test"] cert-manager.io/ClusterIssuer/lab-ca True Forgejo/gw`. Secret `gw-tls` has type `kubernetes.io/tls`; `kubectl -n $NS get secret gw-tls -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -issuer -ext subjectAltName` → issuer `CN=forgejo-operator lab CA`, SAN `DNS:gw.entry.test`.
2. **E3.2 ReferenceGrant**: `kubectl -n $NS get referencegrant gw-tls -o jsonpath='{.spec.from} {.spec.to} {.metadata.ownerReferences[0].name}'` → `[{"group":"gateway.networking.k8s.io","kind":"Gateway","namespace":"envoy-gateway-system"}] [{"group":"","kind":"Secret","name":"gw-tls"}] gw`. The Gateway listener `https-gw` is now `ResolvedRefs=True` and `Programmed=True`, with `attachedRoutes` 1.
3. **E3.3 TLSReady**: `kubectl -n $NS get fj gw -o jsonpath='{.status.conditions[?(@.type=="TLSReady")]}'` → `status: True`, `reason: Ready`, message `Certificate gw-tls is ready: Secret gw-tls for gw.entry.test, valid until <notAfter>. ReferenceGrant gw-tls lets Gateways in envoy-gateway-system read Secret gw-tls`. A Normal event `TLSReady` exists for `gw`.
4. **E3.4 HTTPS validates with the lab CA**: `curl -s "${TLS[@]}" -o /dev/null -w '%{http_code} %{ssl_verify_result}\n' https://$H/` → `200 0`. `echo | openssl s_client -connect $GW:443 -servername $H -CAfile $W/lab-ca.crt 2>/dev/null | grep -E 'issuer=|Verify return code'` → `issuer=CN=forgejo-operator lab CA` and `Verify return code: 0 (ok)`. Without the CA: `curl -s --resolve $H:443:$GW https://$H/ -o /dev/null; echo $?` → `60` (certificate not trusted).
5. **E3.5 ROOT_URL https**: E2.4 showed `ROOT_URL = https://gw.entry.test/`; `curl -s "${TLS[@]}" https://$H/user/login | grep -o 'https://gw.entry.test/[a-z/]*' | head -3` shows links under `https://gw.entry.test/`.

## E4 — Conditions on failures

1. **E4.1 rejected route** (`gw`): NOROLL(gw); `kubectl -n $NS patch fj gw --type=json -p '[{"op":"replace","path":"/spec/gateway/ssh/parentRefs/0/sectionName","value":"nope"}]'`. Within 90 s `RoutesReady` → `status: False`, `reason: NotAccepted`, message containing `TCPRoute gw-ssh on envoy-gateway-system/eg listener nope: not accepted (NoMatchingParent)`; a Warning event `NotAccepted` exists for `gw`; `Ready` stays `True`. Revert with `"value":"ssh"`; within 90 s `RoutesReady=True/Accepted` again and a new Normal event `RoutesReady`. No rollout.
2. **E4.2 certificate that cannot be issued**: apply Forgejo `badtls` with `spec: {server: {domain: bad.entry.test}, tls: {certManager: {issuerRef: {name: missing-issuer, kind: ClusterIssuer}}}}`. Within 3 min Certificate `badtls-tls` exists (`spec.issuerRef.name: missing-issuer`, `spec.dnsNames: [bad.entry.test]`) and is not Ready; `TLSReady` of `badtls` is not True (`Unknown/Pending` while cert-manager keeps trying, or `False/CertificateNotReady`), and its message names `badtls-tls` and cert-manager's reason. `badtls` still becomes `Running` (TLS problems do not stop the instance) and its `status.url` is `https://bad.entry.test/`. Delete Forgejo `badtls`; within 2 min Certificate `badtls-tls` is gone (owned by the instance).

3. **E4.3 no host name, LoadBalancer**: apply Forgejo `nohost` with `spec: {server: {http: {type: LoadBalancer}}, gateway: {parentRefs: [{name: eg, namespace: envoy-gateway-system, sectionName: https-gw}]}}`. Within 90 s `RoutesReady` → `status: False`, `reason: NoHostnames`, message containing `set spec.gateway.hostnames`; a Warning event `NoHostnames` exists for `nohost`; `kubectl -n $NS get httproute nohost` → NotFound (no route that would take every host name of `https-gw`). Once Secret `nohost-config` exists, its app.ini has `REVERSE_PROXY_TRUSTED_PROXIES = 127.0.0.0/8,::1/128` (clients of a LoadBalancer Service arrive with a node's private address). Delete Forgejo `nohost`.

## E5 — Ingress with the cert-manager Secret (instance `ing`, Traefik)

1. **E5.1 objects**: `kubectl -n $NS get ingress ing -o jsonpath='{.spec.ingressClassName} {.spec.tls}'` → `traefik [{"hosts":["ing.entry.test"],"secretName":"ing-tls"}]` although `spec.ingress.tlsSecretName` is unset. Certificate `ing-tls` (`spec.dnsNames: [ing.entry.test]`) is `Ready=True`; `TLSReady=True/Ready`; `ing` has no `RoutesReady` condition; there is no HTTPRoute `ing`, TCPRoute `ing-ssh` or ReferenceGrant `ing-tls`.
2. **E5.2 HTTPS through the Ingress**: `curl -s --cacert $W/lab-ca.crt --resolve ing.entry.test:443:$TIP -o /dev/null -w '%{http_code} %{ssl_verify_result}\n' https://ing.entry.test/` → `200 0`; `curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: ing.entry.test' http://$TIP/` → `200`. app.ini of `ing` has `ROOT_URL = https://ing.entry.test/` and `DOMAIN = ing.entry.test`; `status.url` = `https://ing.entry.test/`.

## E6 — Removing the spec removes the objects (instance `gw`)

1. **E6.1 SSH route only**: `kubectl -n $NS patch fj gw --type=json -p '[{"op":"remove","path":"/spec/gateway/ssh"}]'`; within 60 s `kubectl -n $NS get tcproute gw-ssh` → NotFound while HTTPRoute `gw` remains; `RoutesReady=True` with the message `Accepted: HTTPRoute gw on envoy-gateway-system/eg`. `ssh-keyscan -T 5 -p 2222 $GW` prints no key (nothing routed on that listener).
2. **E6.2 whole gateway**: `kubectl -n $NS patch fj gw --type=json -p '[{"op":"remove","path":"/spec/gateway"}]'`; within 60 s `kubectl -n $NS get httproute gw` → NotFound; `RoutesReady` is gone from `status.conditions`; `curl -s "${TLS[@]}" -o /dev/null -w '%{http_code}\n' https://$H/` → `404`. `status.url` stays `https://gw.entry.test/` (domain and `spec.tls` are still set).
3. **E6.3 TLS**: `kubectl -n $NS patch fj gw --type=json -p '[{"op":"remove","path":"/spec/tls"}]'`; within 60 s Certificate `gw-tls` and ReferenceGrant `gw-tls` are NotFound and `TLSReady` is gone from `status.conditions`; within 4 min app.ini has `ROOT_URL = http://gw.entry.test/` and `status.url` is `http://gw.entry.test/` (the pods roll). The Gateway listener `https-gw` returns to `ResolvedRefs=False` (expected: the lab Gateway still references the Secret).

## E7 — Behaviour covered by unit tests (no cluster)

Clusters without the Gateway API or cert-manager CRDs, TCPRoute `v1alpha2`-only clusters and the condition mapping are verified by unit tests (removing CRDs from the shared lab cluster would break other work on it).

1. **E7.1 unit tests**: in the repository root run
   `go test ./internal/core/ -count=1 -v -run 'TestNoRoutesOrCertificatesWithoutCRDs|TestTCPRouteVersionDiscovery|TestGatewayRendersRoutes|TestGatewayRouteOptions|TestRoutesReadyFollowsRouteStatus|TestRoutesConditionIgnoresStaleStatus|TestRemovingGatewayDeletesRoutes|TestCertManagerCertificate|TestCertificateNeedsDNSNames|TestPublicIdentityWithoutGatewayOrTLS|TestConfigWithGatewayAndTLS|TestNewInstanceGeneratesHostKeys|TestHostKeysSecretFollowsRetainPolicy|TestExistingHostKeysSecretIsReused|TestMissingHostKeysSecretIsNeverRegenerated|TestExistingSecretHostKeys|TestUnmanagedHostKeys|TestParseHostKeys|TestHostKeysScript|TestRouteHostnamesFromPublicHosts|TestNoHTTPRouteWithoutHostnames|TestTrustedProxiesForDirectServices' 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)'`.
   Expect every listed test (and the subtests of `TestTCPRouteVersionDiscovery`: `v1_and_v1alpha2`, `v1alpha2_only`, `standard_channel_without_TCPRoute`) `--- PASS` and the package `ok`. Record the `--- PASS` lines.
2. **E7.2 holds while the host keys are unavailable**: `go test ./internal/upgrade/ ./internal/backup/ ./internal/autopilot/ -count=1 -v -run 'TestUpgradeWaitsForSSHHostKeys|TestRestoreWaitsForSSHHostKeys|TestAnsweredFailuresRestart' 2>&1 | grep -E '^(--- |ok|FAIL)'` → three `--- PASS` lines and three packages `ok`.

## E8 — Cleanup

1. **E8.1 delete**: `kubectl delete namespace e2e-entry --wait=false`, then wait (timeout 6 min) until `kubectl get namespace e2e-entry` returns NotFound. Record anything left behind (e.g. a namespace stuck on finalizers: list `kubectl -n e2e-entry get fj,fjb,fjr,cluster.postgresql.cnpg.io,pvc,certificate,httproute,tcproute`).
2. **E8.2 lab stays**: GatewayClass and Gateway `eg`, ClientTrafficPolicy `eg`, the ClusterIssuers and the Envoy Gateway, cert-manager and Traefik releases stay installed for the next run; the Gateway's `https-gw` listener reports `RefNotPermitted` until an instance `gw` with `spec.tls.gatewayNamespaces` exists again.
