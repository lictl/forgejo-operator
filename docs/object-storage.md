# Object storage

`spec.objectStorage` keeps Forgejo's files in an S3-compatible bucket instead of on the data
volume: LFS objects, attachments, avatars, packages, Actions logs and artifacts, and the cache of
archive downloads. Git repositories stay on the volume. The code is in
`internal/core/objectstorage.go` (rendering, the check, the pod) and `internal/s3` (the operator's
S3 client).

```yaml
apiVersion: forgejo.lucas.icu/v1alpha1
kind: Forgejo
metadata:
  name: git
spec:
  objectStorage:
    s3:
      endpoint: https://s3.eu-central-1.amazonaws.com   # scheme://host[:port], no path
      bucket: acme-forgejo
      region: eu-central-1                              # default us-east-1
      prefix: git                                       # optional: keys start with "git/"
      credentialsSecret:
        name: forgejo-s3                                # keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
    # subsystems: {packages: false}                     # everything goes to the bucket by default
    # serveDirect: true                                 # downloads redirect to the bucket
```

```console
$ kubectl create secret generic forgejo-s3 \
    --from-literal=AWS_ACCESS_KEY_ID=... --from-literal=AWS_SECRET_ACCESS_KEY=...
$ kubectl get fj git -o jsonpath='{.status.conditions[?(@.type=="ObjectStorageReady")]}'
{"reason":"BucketReachable","status":"True","message":"Bucket acme-forgejo at https://s3.eu-central-1.amazonaws.com is reachable with the credentials of Secret \"forgejo-s3\"",...}
```

Object storage is meant for new instances. Turning it on for an instance that already has files
does not move them: Forgejo then looks for them in the bucket. Copy them first with Forgejo's
`forgejo migrate-storage` (or `rclone`), or keep those subsystems on the volume.

## What goes where

Each subsystem has its own prefix below `spec.objectStorage.s3.prefix`, the same on Forgejo 15
and 17:

| `subsystems` field | Files | Object prefix | app.ini section |
|--------------------|-------|---------------|-----------------|
| `lfs` | Git LFS objects | `lfs/` | `[lfs]` |
| `attachments` | attachments of issues, pull requests and releases | `attachments/` | `[attachment]` |
| `avatars` | user and organization avatars | `avatars/` | `[avatar]` |
| `repoAvatars` | repository avatars | `repo-avatars/` | `[repo-avatar]` |
| `repoArchives` | the cache of archive downloads (zip, tar.gz, bundle) | `repo-archive/` | `[repo-archive]` |
| `packages` | the package registry | `packages/` | `[packages]` |
| `actionsLogs` | Forgejo Actions logs | `actions_log/` | `[storage.actions_log]` |
| `actionsArtifacts` | Forgejo Actions artifacts | `actions_artifacts/` | `[actions.artifacts]` |

Actions jobs run on the runners of a `ForgejoRunner` (see [actions-runners.md](actions-runners.md));
their logs and artifacts reach Forgejo over its API and land here.

Every subsystem defaults to `true`. One set to `false` stays on the data volume, in its usual
directory (LFS in `/data/git/lfs`, the others under `/data/gitea`).

These stay on the data volume in any case: the Git repositories (wikis included), the search
indexes and queues (unless high availability moves them to the database and Valkey), the JWT and
OAuth signing keys, Git's home directory, `custom/` and temporary files. A multi-replica instance
therefore still needs a `ReadWriteMany` volume.

## The bucket

| Field | Default | Meaning |
|-------|---------|---------|
| `endpoint` | required | `http(s)://host[:port]` of the S3 API. A short host name is resolved from the instance's namespace, as in its pods. `https` uses TLS. |
| `bucket` | required | The bucket. Forgejo creates it when it starts if it is missing and the key may create buckets. |
| `region` | `us-east-1` | The region requests are signed for. It must be the server's region (`garage` for Garage's default configuration). |
| `pathStyle` | unset | `true`: `https://host/bucket/key`; `false`: `https://bucket.host/key` (virtual-hosted style). Unset: virtual-hosted style for Amazon S3, Google Cloud Storage and Alibaba Cloud OSS endpoints, path style for every other endpoint. |
| `prefix` | none | Prepended to every key, e.g. to share a bucket between instances. |
| `tls.caSecret` | none | `{name, key: ca.crt}`: a Secret key with the PEM certificate(s) of a private CA that signed the server's certificate. Trusted in addition to the image's system CAs. |
| `tls.insecureSkipVerify` | `false` | Accept any certificate. For tests only. |
| `credentialsSecret` | required | `{name, accessKeyIDKey: AWS_ACCESS_KEY_ID, secretAccessKeyKey: AWS_SECRET_ACCESS_KEY}` in the instance's namespace. |

The type of `s3` (`S3Connection`) is meant to be reused by other features that write to a
bucket, such as copying backups off the cluster.

The access key needs to list the bucket (Forgejo checks that it exists at start, and the operator
lists it) and to read, write and delete objects: on AWS, `s3:ListBucket` on the bucket and
`s3:GetObject`, `s3:PutObject`, `s3:DeleteObject` on `<bucket>/<prefix>*`, plus
`s3:CreateBucket` if Forgejo should create the bucket. Forgejo creates objects with multipart
uploads when it does not know their size (Actions logs, archives).

**Forgejo does not start without its bucket.** At start it checks the bucket, creates it when it
is missing, and stops with a fatal error (the pod crash-loops) when either fails, for example with
wrong credentials, an unknown CA or an unreachable endpoint.

Other `[storage]` keys can be set with `spec.config`, which wins over the operator's values, e.g.
`MINIO_CHECKSUM_ALGORITHM: md5` for Cloudflare R2 and Backblaze B2, which refuse the default
`x-amz-checksum-*` headers.

## How Forgejo is configured

The operator renders one `[storage]` section with the bucket. A subsystem's own section only
overrides `SERVE_DIRECT`, `MINIO_BASE_PATH` and `MINIO_BUCKET` of it, so every subsystem stores
its files there, below `<prefix>/<subsystem>/`. A subsystem kept on the volume gets
`STORAGE_TYPE = local` in its own section, which makes that section its target:

```ini
[storage]
MINIO_BASE_PATH = git/
MINIO_BUCKET = acme-forgejo
MINIO_BUCKET_LOOKUP = auto        ; path or dns with pathStyle true or false
MINIO_ENDPOINT = s3.eu-central-1.amazonaws.com
MINIO_INSECURE_SKIP_VERIFY = false
MINIO_LOCATION = eu-central-1
MINIO_USE_SSL = true
SERVE_DIRECT = false
STORAGE_TYPE = minio

[packages]                        ; subsystems: {packages: false}
STORAGE_TYPE = local
```

`[lfs] PATH` is only rendered while LFS stays on the volume.

**Forgejo 15 and 17** read these settings the same way (`modules/setting/storage.go` is identical),
and the operator renders the same `app.ini` for both. Two differences in behaviour:

- Forgejo 17 sends its S3 requests through the proxy of `[proxy]` when `PROXY_ENABLED` is true
  (hosts in `PROXY_HOSTS`, others through `HTTP(S)_PROXY` and `NO_PROXY` of the environment).
  Forgejo 15 never uses a proxy for S3. If you enable `[proxy]` on 17, keep the S3 endpoint out
  of `PROXY_HOSTS` or add it to `NO_PROXY` (`spec.env`) unless the proxy should carry it.
- With `serveDirect`, Forgejo 15 also redirects avatars to the bucket; Forgejo 17 always serves
  avatars itself.

## Credentials

The access key never appears in `app.ini`, in pod arguments, logs, events or status:

- `app.ini` has no `MINIO_ACCESS_KEY_ID`. Forgejo (minio-go) then takes the key from the
  environment: the `forgejo` and `bootstrap` containers get `AWS_ACCESS_KEY_ID` and
  `AWS_SECRET_ACCESS_KEY` as `secretKeyRef`s to `credentialsSecret`, so the pod spec holds only
  references. (Forgejo has no file or `*_URI` variant of these settings.)
- The operator reads the Secret to check the bucket and to compute a digest of the key and the CA
  certificate, which it puts on the pod template (annotation
  `forgejo.lucas.icu/object-storage`). Environment variables are only read when a
  container starts, so **changing the Secret rolls the pods** within a minute (the operator
  re-reads it on every pass, at least every 60 s).
- Forgejo's admin page masks the key, and Forgejo does not log it.

While the Secret, one of its keys, or the CA Secret is missing, `ObjectStorageReady` is
`False/CredentialsUnavailable` and the operator neither writes `app.ini` nor creates or changes the
Deployment (pods could not start), as with missing database credentials. `Ready` says
`Waiting for spec.objectStorage: ...`.

A private CA (`tls.caSecret`) is mounted read-only at `/etc/forgejo-s3-ca` in the same two
containers, and `SSL_CERT_DIR=/etc/ssl/certs:/etc/forgejo-s3-ca` adds it to the trust store next to
the system CAs. It is trusted for all of Forgejo's outgoing TLS connections, not only S3.

## serveDirect

With `serveDirect: true` (`[storage] SERVE_DIRECT`), Forgejo answers downloads with a redirect to a
presigned URL of the object, valid for 5 minutes, instead of streaming the file itself: LFS (web,
API and the batch API that `git lfs` uses), attachments, archive downloads, packages, Actions
artifacts and, on Forgejo 15, avatars. Uploads always go through Forgejo.

The URL is signed for the endpoint as Forgejo knows it (`endpoint`), so **clients must reach that
host name and trust its certificate**. An in-cluster endpoint such as
`http://garage.s3-lab.svc.cluster.local:3900` only works for clients inside the cluster (and, on
a local cluster, the Mac). Use an endpoint that is public, or leave `serveDirect` off.

## Status

`ObjectStorageReady` reports a check the operator makes from its own pod with the instance's
credentials: it lists at most one key of the bucket (`ListObjectsV2`, signed with AWS Signature
Version 4), with a 5 s timeout. It checks again every minute while that works, every 30 s while it
fails, and at once when the spec, the credentials or the CA change.

| Status | Reason | Meaning |
|--------|--------|---------|
| True | `BucketReachable` | The bucket answered the listing. |
| False | `CredentialsUnavailable` | `credentialsSecret` (or a key of it) or the CA Secret is missing; nothing is deployed until it is there. |
| False | `InvalidSpec` | The endpoint cannot be parsed. |
| False | `BucketNotFound` | The bucket does not exist. Forgejo creates it when it starts if the key may create buckets, and does not start otherwise. |
| False | `AccessDenied` | The server refused the key (wrong key or secret, missing permission). The message has the S3 error code. |
| False | `TLSError` | The TLS handshake failed, e.g. a certificate signed by an unknown CA (set `tls.caSecret`), or an `https` endpoint that speaks plain HTTP. |
| False | `RequestFailed` | Another answer, e.g. a wrong region (the message names the bucket's region when the server tells it). |
| False | `UnreachableFromOperator` | The operator's pod could not connect. Only reported (see below). |
| (absent) | | `spec.objectStorage` is not set. |

A Warning event `ObjectStorageUnavailable` repeats a failure at most hourly.

The check is advisory: apart from missing Secrets, the operator renders `app.ini` and deploys
Forgejo whatever it finds, because Forgejo's pods may reach an endpoint that the operator's pod
cannot. But Forgejo does not start without its bucket, so while the endpoint answers with a
failure (`ObjectStorageReady` is `False` with any reason except `UnreachableFromOperator`):

- autopilot replaces no pods: no health-based restarts, no restarts for a file system resize, no
  vertical resizes, and a crash loop is not blamed on `app.ini` (no config rollback; only crashes
  after the bucket became usable again count);
- an upgrade does not start (`Upgrading=False/WaitingForObjectStorage`).

`UnreachableFromOperator` holds none of this: no answer from the operator's pod says nothing about
Forgejo's pods (an egress NetworkPolicy of the operator's namespace, an operator running outside
the cluster, a VPC endpoint only the nodes use), and holding would switch off self-healing, the
config rollback and upgrades for as long as that lasts. When the pods cannot reach the bucket
either, they do not become ready, which `Ready` and `Healthy` report.

Network requirement: the operator's pod should reach the S3 endpoint (see
[design.md](design.md#network-requirements)). Allow it in NetworkPolicies and egress firewalls as
you do for Forgejo's pods; otherwise `ObjectStorageReady` stays `False/UnreachableFromOperator`
and the bucket is not checked.

## Backups

`ForgejoBackup` still dumps the database and archives the data volume (see
[backup-restore.md](backup-restore.md)). The files of the subsystems in the bucket are not on the
volume, so **they are not in the backup**. `manifest.json` records where they are:

```json
"objectStorage": {"endpoint": "https://s3.eu-central-1.amazonaws.com", "bucket": "acme-forgejo",
                  "prefix": "git/", "subsystems": ["lfs", "attachments", ...]}
```

The bucket's durability is the job of the object store and its configuration, which are yours:

- Turn on versioning (or replication, Object Lock) where the store supports it, so that deleted
  or overwritten objects can be brought back, and a lifecycle rule that expires old versions.
  Garage has neither versioning nor Object Lock.
- A restore does not touch the bucket. Restoring an older database leaves objects that were
  created later (harmless, `forgejo doctor` can find them), and objects that were deleted after
  the backup are missing unless versioning keeps them. Forgejo deletes objects itself: deleting
  a repository removes its issue and release attachments (and LFS objects no other repository
  uses) from the bucket at once. Restoring a backup to undo such a deletion brings the
  repository's git data and database rows back, but without versioning its attachments answer
  HTTP 500 (`serve content: unable to read`) and its LFS files are missing.
- To keep a copy elsewhere, copy the bucket after the dump, e.g. with
  `rclone sync s3:<bucket>/<prefix> <remote>:...`.
- Remote backups (`spec.backup.remote`, see
  [backup-restore.md](backup-restore.md#remote-backups)) copy the database, the data volume and
  the instance's identity off the cluster, but not this bucket. After a disaster, point
  `spec.objectStorage` of the instance recreated with `spec.restoreFrom` at the same bucket and
  prefix: the restored database references the objects that are there.

## Local lab

`hack/s3-lab/setup.sh` installs [Garage](https://garagehq.deuxfleurs.fr) v2.4.1 (single node,
`dxflrs/garage:v2.4.1`) into namespace `s3-lab` of the current kubectl context. It is idempotent
and creates:

- Secret `garage`: the RPC secret and the default access key, generated once with `openssl`;
  Garage creates its layout, the key and the bucket `forgejo` from them at start (`garage server
  --single-node --default-bucket`).
- Secret `s3-credentials`: the same key under `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`,
  to copy into the namespace of an instance.
- Bucket `forgejo-backups`, for backup tests, created with the same key.
- With cert-manager and its ClusterIssuer `lab-ca` (the entry-points lab), an HTTPS endpoint:
  nginx terminates TLS for `garage-tls.s3-lab.svc.cluster.local` and passes the requests to
  Garage; the CA is the key `ca.crt` of Secret `s3-lab/garage-tls`.

| | |
|---|---|
| Endpoint | `http://garage.s3-lab.svc.cluster.local:3900`, or `https://garage-tls.s3-lab.svc.cluster.local` |
| Region | `garage` |
| Buckets | `forgejo`, `forgejo-backups` |
| Addressing | path style always works; virtual-hosted style needs host names below `s3.garage.test` (`root_domain`), which only resolve with `hostAliases` |

```console
$ hack/s3-lab/setup.sh
$ kubectl -n s3-lab get secret s3-credentials -o json \
    | jq '{apiVersion, kind, type, data, metadata: {name: .metadata.name}}' | kubectl -n <ns> apply -f -
```

```yaml
spec:
  objectStorage:
    s3:
      endpoint: http://garage.s3-lab.svc.cluster.local:3900
      bucket: forgejo
      region: garage
      pathStyle: true
      prefix: <instance>               # several instances can share the bucket
      credentialsSecret: {name: s3-credentials}
```

From the Mac (the local cluster resolves `*.svc.cluster.local` and routes to Services), `curl`
signs requests itself:

```console
$ KEY=$(kubectl -n s3-lab get secret s3-credentials -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
$ SEC=$(kubectl -n s3-lab get secret s3-credentials -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)
$ curl -s --aws-sigv4 aws:amz:garage:s3 --user "$KEY:$SEC" \
    'http://garage.s3-lab.svc.cluster.local:3900/forgejo?list-type=2&prefix=<instance>/'
$ kubectl -n s3-lab get secret garage-tls -o jsonpath='{.data.ca\.crt}' | base64 -d > lab-ca.crt
$ curl -s --cacert lab-ca.crt --aws-sigv4 aws:amz:garage:s3 --user "$KEY:$SEC" \
    'https://garage-tls.s3-lab.svc.cluster.local/forgejo?list-type=2&max-keys=10'
```

`rclone` runs in a pod of the lab, with the key from the Secret (`env_auth` reads
`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`), so that it never shows up in the pod spec. Docker
containers on the Mac do not resolve `*.svc.cluster.local`.

```console
$ kubectl -n s3-lab run rclone --rm -i --restart=Never --image=rclone/rclone:1.75.2 --overrides='{"spec":{"containers":[{
    "name":"rclone","image":"rclone/rclone:1.75.2","args":["lsf","-R","lab:forgejo"],
    "envFrom":[{"secretRef":{"name":"s3-credentials"}}],
    "env":[{"name":"RCLONE_CONFIG_LAB_TYPE","value":"s3"},{"name":"RCLONE_CONFIG_LAB_PROVIDER","value":"Other"},
           {"name":"RCLONE_CONFIG_LAB_ENV_AUTH","value":"true"},{"name":"RCLONE_CONFIG_LAB_ENDPOINT","value":"http://garage:3900"},
           {"name":"RCLONE_CONFIG_LAB_REGION","value":"garage"}]}]}}'
```

The presigned URLs of `serveDirect` can be fetched from the Mac as well; for the HTTPS endpoint,
`git lfs` needs the lab CA in the system keychain.

Remove the lab with `kubectl delete ns s3-lab`.
