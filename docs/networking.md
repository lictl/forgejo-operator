# Networking

How clients reach Forgejo: the Services the operator always creates, an optional Ingress, optional
[Gateway API](https://gateway-api.sigs.k8s.io/) routes, and TLS certificates from
[cert-manager](https://cert-manager.io). How the operator works inside is in
[design.md](design.md); SSH host keys are in [ssh.md](ssh.md).

## Entry points

| | Services | Ingress (`spec.ingress`) | Gateway API (`spec.gateway`) |
|---|---|---|---|
| Objects | `<name>-http` (port 3000), `<name>-ssh` (port 22) | Ingress `<name>` | HTTPRoute `<name>`; TCPRoute `<name>-ssh` with `spec.gateway.ssh` |
| HTTP | ClusterIP, NodePort or LoadBalancer (`spec.server.http`) | host and path prefix | host names and path prefix, on Gateways that already exist (`parentRefs`) |
| Git over SSH | `spec.server.ssh.service`, e.g. a LoadBalancer | no (Ingress is HTTP only) | through a TCP listener of a Gateway |
| TLS | – | `tlsSecretName`, or the cert-manager Secret | the Gateway's HTTPS listener; the cert-manager Secret can be referenced from it |
| Long requests | – | the controller's own timeouts | `rules[].timeouts.request: 0s` by default |
| Needs | – | an Ingress controller | the Gateway API CRDs and a Gateway controller |

The routes and certificates are optional integrations: the operator creates them only when the
cluster serves their kinds, uses unstructured objects (no Go dependency on Gateway API or
cert-manager), and reports a missing CRD in a condition instead of failing. A failed API discovery
request is not taken for a missing CRD: the pass fails and is retried. It never creates a
Gateway: Gateways belong to the platform, routes to the application.

The operator creates these objects, but updates or deletes only the ones the instance controls.
An HTTPRoute, TCPRoute, Certificate or ReferenceGrant that already has the name the operator
would use and belongs to someone else (your own, or another controller's) is neither taken over
nor changed, and the condition reports `NameConflict` (see [`RoutesReady`](#routesready) and
[`TLSReady`](#tlsready)). The exception is an object without a controller that carries the
instance's labels: one left behind when an earlier instance of the same name was deleted with
orphaning, which is adopted.

## Public identity

Forgejo writes its URLs (clone URLs, links in mails, OAuth redirects) from `app.ini`. The operator
derives them from the spec; `spec.server.rootURL` and `spec.config` override them.

| `app.ini` | Value, first that applies |
|-----------|---------------------------|
| `DOMAIN` | `spec.server.domain`; `spec.ingress.host`; the first `spec.gateway.hostnames` entry that is not a wildcard; the host of `spec.server.rootURL`; the `<name>-http` Service's DNS name |
| `ROOT_URL` | `spec.server.rootURL`; `<scheme>://<ingress.host><ingress.path>`; `<scheme>://<domain or first gateway host name><gateway.path>`; `<scheme>://<domain>/`; `http://<name>-http.<ns>.svc.<cluster domain>:3000/` |
| scheme | `https` when `spec.tls` is set, or the Ingress has a TLS Secret (`tlsSecretName`, or the cert-manager Secret); else `http` |
| `SSH_DOMAIN` | `DOMAIN`, unless that is the HTTP Service's name: then the `<name>-ssh` Service's DNS name. When SSH has an address of its own (e.g. a LoadBalancer `<name>-ssh` Service next to an HTTP Gateway), set it with `spec.config: {server: {SSH_DOMAIN: <ssh host>}}`, which wins over the default |
| `SSH_PORT` | `spec.server.ssh.externalPort`; else, with `spec.gateway.ssh`, the `port` that every one of its `parentRefs` names; else the SSH Service port (22). The operator does not read Gateways: with parent references by `sectionName` only (or with different ports), set `externalPort` to the Gateway listener's port |
| `[security] REVERSE_PROXY_TRUSTED_PROXIES` | `spec.server.trustedProxies`; loopback only (`127.0.0.0/8,::1/128`) when `spec.server.http.type` is `LoadBalancer` or `NodePort`; else the loopback and private ranges (`127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,100.64.0.0/10,fc00::/7`) |

`status.url` is the effective `ROOT_URL`. Changing any of these changes `app.ini` and rolls the
pods.

`spec.server.trustedProxies` decides whose `X-Real-IP` and `X-Forwarded-For` Forgejo believes.
Forgejo reads `X-Real-IP` first and takes `X-Forwarded-For` only after its last `", "` (comma
and space). A trusted proxy must therefore never pass a client's own `X-Real-IP` through:

- **Gateway API:** the operator's HTTPRoute always removes it (Envoy Gateway passes it through).
- **Ingress:** the operator cannot change the controller's headers. ingress-nginx and Traefik set
  `X-Real-IP` themselves; Envoy-based controllers such as Contour pass a client's through, so
  remove it in the controller's configuration, or set `trustedProxies` to loopback only.
- **`LoadBalancer` or `NodePort` HTTP Service:** clients reach Forgejo directly, and with the
  default `externalTrafficPolicy: Cluster` kube-proxy shows them with a node's private address.
  The default is therefore loopback only. Set `trustedProxies` to your proxies' pod network when
  an Ingress or Gateway serves the instance too.

Narrow the list to the pod network of your ingress controller or gateway when other workloads in
the private ranges can reach Forgejo directly.

## Gateway API

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata: {name: git, namespace: forge}
spec:
  server:
    domain: git.example.com          # HTTPRoute host name, DOMAIN, SSH_DOMAIN
    ssh: {externalPort: 2222}        # the Gateway's TCP listener port, shown in clone URLs
  gateway:
    parentRefs:                      # the HTTPS listener only (sectionName), see below
      - {name: eg, namespace: envoy-gateway-system, sectionName: https-git}
    ssh:
      parentRefs:
        - {name: eg, namespace: envoy-gateway-system, sectionName: ssh}
  tls:
    certManager:
      issuerRef: {name: letsencrypt, kind: ClusterIssuer}
    gatewayNamespaces: [envoy-gateway-system]
```

A parent reference without `sectionName` (or `port`) attaches the route to every listener of the
Gateway that matches its host names, plain HTTP ones included: Forgejo would then also answer on
port 80, so passwords and tokens could travel in clear text, and since its route names a host, it
would win over a catch-all HTTP-to-HTTPS redirect the platform runs on that listener. With
`spec.tls`, name the HTTPS listeners. To redirect `http://` for this host yourself, attach a route
of your own to the HTTP listener:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata: {name: git-redirect, namespace: forge}
spec:
  parentRefs: [{name: eg, namespace: envoy-gateway-system, sectionName: http}]
  hostnames: [git.example.com]
  rules:
    - filters: [{type: RequestRedirect, requestRedirect: {scheme: https, statusCode: 301}}]
```

### HTTPRoute `<name>`

| Field | Rendered as | Why |
|-------|-------------|-----|
| `parentRefs` (required) | `spec.parentRefs`; `group`/`kind` default to the Gateway, `namespace` to the instance's | the Gateways, or listeners (`sectionName`, `port`), to attach to. Their listeners must allow routes from the instance's namespace (`allowedRoutes`). With `spec.tls`, name the HTTPS listeners (see above) |
| `hostnames` | `spec.hostnames`; default: `DOMAIN` and the host of `ROOT_URL`, where they are DNS names and not the `<name>-http` Service's | without any, there is no HTTPRoute and `RoutesReady=False/NoHostnames`: a route without host names would take every host name of its listeners, other applications' on a shared Gateway too. To serve every host name (e.g. on a Gateway of its own), write that HTTPRoute yourself |
| `path` (`/`) | `PathPrefix` match; another prefix is replaced by `/` (`URLRewrite`, extended support) and is part of `ROOT_URL`. The API server refuses what Gateway API's `HTTPPathMatch` refuses: characters other than `A-Za-z0-9-._~!$&'()*+,;=:@/` and percent escapes, `//`, `/./`, `/../`, `%2F` and a trailing `/.` or `/..` | Forgejo expects requests without its sub-path |
| `requestTimeout` (`0s`) | `rules[].timeouts.request`; `""` leaves it out | Gateways otherwise end requests after their own default (Envoy Gateway: 15 s), which cuts clones, pushes and downloads that take longer, also slow clients |
| `removeRequestHeaders` | a `RequestHeaderModifier` filter that removes `X-Real-IP` and these headers | `X-Real-IP` is always removed: Envoy Gateway passes a client's own through, and Forgejo prefers it to `X-Forwarded-For`, so any client could choose the address Forgejo logs |
| `annotations` | the routes' annotations | |

The backend is Service `<name>-http`. The route is owned by the instance and deleted with it, and
when `spec.gateway` is removed. An HTTPRoute `<name>` that exists without belonging to the
instance (a route of your own, or another controller's) is left as it is, and
`RoutesReady=False/NameConflict` says so; the TCPRoute is still reconciled.

### TCPRoute `<name>-ssh`

With `spec.gateway.ssh`, TCPRoute `<name>-ssh` sends a TCP listener's connections to Service
`<name>-ssh`. A TCP listener has no host names, so every instance needs a listener (a port) of its
own; `sectionName` picks it. The operator uses TCPRoute `v1` (Gateway API 1.6 and later, also the
standard channel) and else `v1alpha2` (the experimental channel of older bundles). When the cluster
serves neither, the HTTPRoute is still created and `RoutesReady` says so; SSH then works through
the `<name>-ssh` Service only (e.g. `spec.server.ssh.service.type: LoadBalancer`). Removing
`spec.gateway.ssh`, or disabling SSH, deletes the TCPRoute.

The host keys clients see through the Gateway are the instance's own (`status.ssh.hostKeys`):
TCPRoute forwards TCP, it does not terminate SSH.

### `RoutesReady`

Reported while `spec.gateway` is set, from each route's `status.parents`, for every parent of its
spec (an entry of the whole Gateway does not count for one of its listeners):

| Status | Reason | Meaning |
|--------|--------|---------|
| True | `Accepted` | Every route is `Accepted` by every parent and its `ResolvedRefs` is True |
| Unknown | `Pending` | A parent has not reported on the route's current generation yet (is a Gateway controller running for its GatewayClass?) |
| False | `NotAccepted` | A parent rejects the route; the message has the controller's reason, e.g. `NoMatchingParent` (no listener of that name), `NotAllowedByListeners` (`allowedRoutes`) |
| False | `RefsNotResolved` | A backend does not resolve, e.g. `EndpointsNotFound` while no Forgejo pod is ready (Envoy Gateway) |
| False | `NoHostnames` | No host name for the HTTPRoute (see `hostnames`); there is no HTTPRoute. The TCPRoute is still created |
| False | `GatewayAPIUnavailable` | The cluster does not serve HTTPRoute `gateway.networking.k8s.io/v1`; nothing was created |
| False | `TCPRouteUnavailable` | `spec.gateway.ssh` is set, but no TCPRoute version is served |
| False | `NameConflict` | HTTPRoute `<name>` or TCPRoute `<name>-ssh` exists but does not belong to the instance (the message names its controller, if any); the operator neither takes it over nor changes it. Delete or rename it to let the operator create its own. The other route is still reconciled |

A Warning event (the reason) is recorded when the condition turns False or changes its reason, a
Normal event `RoutesReady` when it turns True. The operator does not watch routes: it reads them on
every pass (at least every 60 s), and every 15 s while they are not ready.

## TLS

`spec.tls` says that Forgejo is served over HTTPS: `ROOT_URL` becomes `https`. On its own
(`tls: {}`) it fits a Gateway that terminates TLS with a certificate of its own, e.g. a wildcard.

### cert-manager

With `spec.tls.certManager`, the operator creates Certificate `<name>-tls` (owned by the instance),
and cert-manager keeps the key pair in Secret `<name>-tls`. A Certificate `<name>-tls` that
exists without belonging to the instance is not taken over (`TLSReady=False/NameConflict`):

- `issuerRef`: an `Issuer` in the instance's namespace (the default kind) or a `ClusterIssuer`;
  `group` for external issuers.
- `dnsNames`: by default the public host names: `DOMAIN` and the host of `ROOT_URL` (so also the
  host of `spec.server.rootURL`), `spec.ingress.host` and the HTTPRoute host names (IP addresses
  and the Service's in-cluster name are skipped). Without any, `TLSReady=False/NoDNSNames`.
- **Ingress:** `spec.ingress.tlsSecretName` defaults to `<name>-tls`.
- **Gateway:** the Gateway's HTTPS listener references the Secret. A Gateway in another namespace
  may only do so when a ReferenceGrant in the instance's namespace allows it:
  `spec.tls.gatewayNamespaces: [<gateway namespace>]` makes the operator create ReferenceGrant
  `<name>-tls`, which lets Gateways in those namespaces read Secret `<name>-tls` and nothing else.

  ```yaml
  listeners:
    - name: https-git
      port: 443
      protocol: HTTPS
      hostname: git.example.com
      tls:
        mode: Terminate
        certificateRefs: [{name: git-tls, namespace: forge}]
      allowedRoutes: {namespaces: {from: All}}
  ```

  Alternatively, cert-manager's Gateway support (Helm values `config.apiVersion:
  controller.config.cert-manager.io/v1alpha1`, `config.kind: ControllerConfiguration`,
  `config.gatewayAPI.enabled: true`) issues certificates for annotated Gateways itself; then use
  `spec.tls: {}` here.

Removing `spec.tls.certManager` deletes the Certificate (cert-manager leaves the Secret);
removing `gatewayNamespaces` deletes the ReferenceGrant.

**cert-manager annotations on the Ingress.** cert-manager's ingress-shim creates a Certificate
for every TLS Secret of an Ingress that carries a `cert-manager.io/issuer` or
`cert-manager.io/cluster-issuer` annotation. With `spec.tls.certManager` that Secret is
`<name>-tls`, so the shim would create Certificate `<name>-tls` itself, controlled by the
Ingress, and the operator reports `TLSReady=False/NameConflict` with a hint. Use one or the
other: remove the annotation from `spec.ingress.annotations` and let `spec.tls.certManager`
request the certificate (then delete the shim's Certificate), or drop `spec.tls.certManager`
and keep the annotation.

### `TLSReady`

Reported while `spec.tls.certManager` or `spec.tls.gatewayNamespaces` is set:

| Status | Reason | Meaning |
|--------|--------|---------|
| True | `Ready` | The Certificate is `Ready` (the message names the DNS names, `notAfter` and the ReferenceGrant) |
| Unknown | `Pending` | cert-manager is issuing it, or has not looked at its current generation yet |
| False | `CertificateNotReady` | cert-manager reports a problem (its reason and message follow) |
| False | `CertManagerUnavailable` | The cluster does not serve Certificate `cert-manager.io/v1`; nothing was created |
| False | `NoDNSNames` | No DNS name to request |
| False | `ReferenceGrantUnavailable` | `gatewayNamespaces` is set, but the cluster does not serve ReferenceGrant |
| False | `NameConflict` | Certificate or ReferenceGrant `<name>-tls` exists but does not belong to the instance; it is neither taken over nor changed. For a Certificate controlled by the Ingress, the message says to remove the ingress-shim annotation (see above) |

## Permissions

`spec.ingress`, `spec.gateway` and `spec.tls` make the operator's ClusterRole create objects on
behalf of whoever may edit a `Forgejo`: an Ingress, HTTPRoutes, TCPRoutes, ReferenceGrants and
cert-manager Certificates, always in the instance's namespace. For the Gateway API and
cert-manager kinds the operator holds only `get`, `create`, `update` and `delete` (it reads them
live and never lists, watches or patches them); a ReferenceGrant it creates grants only Secret
`<name>-tls`, and only to Gateways in `spec.tls.gatewayNamespaces`. Route attachment stays
governed by the Gateways themselves: their listeners' `allowedRoutes` and host names decide
whether a route from the instance's namespace is accepted. A Certificate is not limited that way:
`spec.tls.certManager` can request any `dnsNames` from any `ClusterIssuer` (or an `Issuer` of the
namespace). Restrict that with cert-manager's [approver-policy](https://cert-manager.io/docs/policy/approval/approver-policy/),
or grant write access to `Forgejo` objects (e.g. the ClusterRole
`forgejo-operator-forgejo-editor-role`) only to users trusted with the cluster's issuers. The operator has no namespace or issuer allow-list of its own.
All of the operator's permissions are listed in [permissions.md](permissions.md).

## Envoy Gateway (reference setup)

Tested with Envoy Gateway v1.9.2 (its chart installs the Gateway API v1.6.1 experimental-channel
CRDs, which serve TCPRoute `v1` and `v1alpha2`), cert-manager v1.21.2 and Kubernetes 1.36. Check
Envoy Gateway's compatibility matrix for your Kubernetes version.

```bash
helm upgrade --install eg oci://docker.io/envoyproxy/gateway-helm --version v1.9.2 \
  -n envoy-gateway-system --create-namespace --wait
helm upgrade --install cert-manager jetstack/cert-manager --version v1.21.2 \
  -n cert-manager --create-namespace --set crds.enabled=true --wait
```

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
    - name: https-git
      port: 443
      protocol: HTTPS
      hostname: git.example.com
      tls: {mode: Terminate, certificateRefs: [{name: git-tls, namespace: forge}]}
      allowedRoutes: {namespaces: {from: All}}
    - {name: ssh, port: 2222, protocol: TCP, allowedRoutes: {namespaces: {from: All}}}
---
apiVersion: gateway.envoyproxy.io/v1alpha1
kind: ClientTrafficPolicy
metadata: {name: eg, namespace: envoy-gateway-system}
spec:
  targetRefs: [{group: gateway.networking.k8s.io, kind: Gateway, name: eg}]
  path:
    escapedSlashesAction: KeepUnchanged   # Envoy decodes %2F by default; Forgejo needs it as sent
  headers:
    earlyRequestHeaders:
      remove: [X-Forwarded-For]           # only on a Gateway that clients reach directly
```

- **Timeouts:** Envoy Gateway ends every request after 15 s unless the route says otherwise; the
  operator's `timeouts.request: 0s` lifts that for Forgejo's route only.
- **Escaped slashes:** without `escapedSlashesAction: KeepUnchanged`, Envoy decodes `%2F` in paths,
  which breaks Forgejo URLs that carry a slash inside a path segment (branch names, package names).
- **Client addresses:** Envoy appends the client's address to `X-Forwarded-For` with a bare comma
  (`198.51.100.9,192.168.1.10`). Forgejo splits that header at `", "` only, so when a client sends
  its own `X-Forwarded-For`, Forgejo records the whole string as its address. On a Gateway that
  clients reach directly, `earlyRequestHeaders.remove: [X-Forwarded-For]` drops the client's header
  before Envoy adds the real address. Behind a load balancer that sets `X-Forwarded-For` itself,
  keep the header and configure `clientIPDetection` instead. `X-Real-IP` is removed by the route.
- **SSH client addresses (optional):** a `BackendTrafficPolicy` with `proxyProtocol: {version: V2}`
  targeting TCPRoute `<name>-ssh` sends the PROXY protocol header, and
  `spec.config: {server: {SSH_SERVER_USE_PROXY_PROTOCOL: "true"}}` makes Forgejo read it. Forgejo
  then refuses connections without that header, also through the `<name>-ssh` Service.
- **Addresses:** Envoy Gateway creates a LoadBalancer Service
  `envoy-<gateway namespace>-<gateway name>-<hash>` in `envoy-gateway-system`; the Gateway's
  `status.addresses` has its address.

## Traefik

Traefik's Kubernetes Gateway provider serves the HTTPRoute (tested with Traefik 3.7.16):

- It ignores HTTPRoute `timeouts`, so `requestTimeout` has no effect. Its entryPoints end reading
  a request body after `readTimeout` (60 s by default), which cuts long pushes and LFS uploads: set
  `ports.web.transport.respondingTimeouts.readTimeout` and the same for `websecure` (Helm values)
  to `0` or a long duration.
- It replaces `X-Forwarded-For` and `X-Real-IP` with what it saw, so the client cannot choose its
  address; removing `X-Real-IP` in the route does no harm.
- TCPRoute needs `providers.kubernetesGateway.experimentalChannel: true` and TCPRoute `v1alpha2`.
  On standard-channel CRDs (k3s ships them), Traefik before 3.8 fails its whole Gateway provider
  with the experimental channel enabled. Until then, serve SSH through the `<name>-ssh` Service
  (e.g. `type: LoadBalancer` on its own address or port) and leave `spec.gateway.ssh` out. Clone
  URLs then need that address: set `spec.config: {server: {SSH_DOMAIN: <its host name>}}`, and
  `spec.server.ssh.externalPort` when its port is not 22.
- Gateway listener ports are Traefik entryPoint ports (8000 and 8443 by default); an SSH listener
  needs an entryPoint of its own in Traefik's static configuration (`ports.ssh` in Helm values).
- k3s's bundled Traefik owns ports 80 and 443 through ServiceLB; another Gateway controller needs
  other ports or addresses there.

## Troubleshooting

| Symptom | Look at |
|---------|---------|
| `RoutesReady=Unknown/Pending` for minutes | `kubectl get gatewayclass` (Accepted?), the Gateway's `status.listeners`, the Gateway controller's logs |
| `RoutesReady=False/NotAccepted` | the listener names (`sectionName`), `allowedRoutes` of the listeners, and the route: `kubectl get httproute <name> -o yaml` |
| `RoutesReady=False/NoHostnames` | set `spec.gateway.hostnames`, or `spec.server.domain` or `spec.server.rootURL` to a DNS name |
| Forgejo answers on `http://` although `spec.tls` is set | a `parentRefs` entry without `sectionName` attaches to the plain HTTP listener too |
| 504 after 15 s through Envoy Gateway | `requestTimeout` is not `0s`, or another policy sets a timeout |
| HTTPS listener `ResolvedRefs=False/RefNotPermitted` | `spec.tls.gatewayNamespaces` lacks the Gateway's namespace |
| `TLSReady=False/CertificateNotReady` | `kubectl describe certificate <name>-tls`, its CertificateRequest and the issuer |
| Clone URLs show the wrong host or port | `DOMAIN`, `SSH_DOMAIN`, `SSH_PORT` in the table above; `status.url`. SSH served on an address of its own needs `SSH_DOMAIN` in `spec.config`; a TCP listener named by `sectionName` only needs `spec.server.ssh.externalPort` |
| `RoutesReady` or `TLSReady` `False/NameConflict` | an object of that name that is not the instance's: `kubectl get httproute,tcproute,certificate,referencegrant -n <ns> -o wide` and its `ownerReferences`; delete or rename it, or remove the Ingress's cert-manager annotation |
