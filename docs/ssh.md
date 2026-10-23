# SSH

Forgejo serves Git over SSH with its built-in SSH server (port 2222 in the pod, Service
`<name>-ssh` on port 22, `spec.server.ssh`; through a Gateway with `spec.gateway.ssh`, see
[networking.md](networking.md#tcproute-name-ssh)). This page is about its **host keys**: the keys
clients pin in `known_hosts`. When they change, every client fails with "REMOTE HOST
IDENTIFICATION HAS CHANGED". How it works inside is in [design.md](design.md#ssh-host-keys).

## Stable host keys

Forgejo itself keeps its host key on the data volume (`/data/gitea/ssh/gitea.rsa`), so a
rebuilt volume, or a new instance that a backup is restored into, gets a new key. The operator
keeps the keys in the Secret **`<name>-ssh-host-keys`** instead, and never rotates them. Each
pod copies the keys when it starts (init container `ssh-host-keys`, into memory) and serves that
copy until it ends, also across container restarts:

| Instance | Keys |
|----------|------|
| Default | ed25519, ECDSA P-256 and RSA 3072, generated once into `<name>-ssh-host-keys` |
| `<name>-ssh-host-keys` exists already (kept by `retainPolicy: Retain`, or created by you) | Used as it is |
| `spec.server.ssh.hostKeys.existingSecret` set | Yours, see below |

The Secret follows `spec.storage.retainPolicy` like `<name>-secrets`: with `Retain` (the
default) it outlives the `Forgejo` resource, and a `Forgejo` recreated with the same name serves
the same keys.

## Publishing the fingerprints

```console
$ kubectl get fj git -o jsonpath='{range .status.ssh.hostKeys[*]}{.type}{"\t"}{.fingerprint}{"\n"}{end}'
ssh-ed25519            SHA256:yXEOVfL6gmHac9bBOWfHCEzyp7SNNJNyywvo0KA3pK4
ecdsa-sha2-nistp256    SHA256:xu0Zbqe2I7vbj8Bf19Ln0Vzfs7jY15nc4eatrf3rjKo
ssh-rsa                SHA256:M8e9hPunZeAWO87k/gKTl/vnoTC1z++PFJm9kKEtDl0

$ kubectl get fj git -o jsonpath='{range .status.ssh.hostKeys[*]}git.example.com {.publicKey}{"\n"}{end}'   # known_hosts lines
```

`status.ssh` also names the Secret (`hostKeysSecretName`) and where the keys came from
(`hostKeysSource`: `Generated`, or `ExistingSecret` for `existingSecret` and a
`<name>-ssh-host-keys` you created).

## Bringing your own keys

```bash
ssh-keygen -q -t ed25519 -N '' -f ssh_host_ed25519_key
kubectl create secret generic git-ssh-keys --from-file=ssh_host_ed25519_key
```

```yaml
spec:
  server:
    ssh:
      hostKeys:
        existingSecret: git-ssh-keys
```

The Secret holds one or more of `ssh_host_ed25519_key`, `ssh_host_ecdsa_key` and
`ssh_host_rsa_key` (private keys without a passphrase, OpenSSH or PEM format; other entries are
ignored). Forgejo serves one key per type. A Secret named `<name>-ssh-host-keys` that you create
before the instance is used as it is, too.

To add a key type to the operator's Secret, or to replace a key on purpose, edit the Secret: the
operator checks the keys, then the pods roll and serve the new set. Running pods never read the
Secret again, so an edit reaches them only through that rollout. An edit the operator cannot use
(a public key instead of the private one, a key with a passphrase, data encoded twice) is not
rolled out: see `SSHHostKeysUnavailable` below. The operator itself never adds, changes or
removes keys.

With `[server] SSH_SERVER_HOST_KEYS` in `spec.config`, or SSH disabled, the operator leaves host
keys alone.

## Backups and disaster recovery

Every backup holds a copy of the keys in `<backup>/ssh-host-keys/` (listed in `manifest.json`),
and every remote backup holds the Secret of host keys in its encrypted `identity.json`. A restore
does not change the instance's keys.

- **Same cluster.** With `retainPolicy: Retain` the Secret survives the deletion of the
  `Forgejo`; recreate it with the same name and restore (see
  [backup-restore.md](backup-restore.md#restoring-into-a-new-instance-of-the-same-cluster)).
- **Another namespace or cluster, from a remote backup.** `spec.restoreFrom` of the new
  `Forgejo` creates `<name>-ssh-host-keys` from the backup before the keys would be generated,
  so clients see the same fingerprints (see
  [backup-restore.md](backup-restore.md#disaster-recovery)).
- **Another cluster, or another instance name, from local backup files.** A backup can only be
  restored into the instance it was taken of (`spec.forgejoRef`), but its keys can serve any
  instance. Create a Secret from the backup copy **before** the `Forgejo`, then the instance
  (then restore, for the same name in another cluster):

  ```bash
  kubectl create secret generic git-ssh-host-keys --from-file=./before-migration/ssh-host-keys/
  ```

  Name it `<name>-ssh-host-keys`, or give any name and set
  `spec.server.ssh.hostKeys.existingSecret`.

## When something is wrong

| Symptom | Meaning |
|---------|---------|
| `Degraded=SSHHostKeysUnavailable` | The Secret the pods serve keys from is missing or unusable (the message says why), and the operator does not generate new keys. Running pods keep serving the keys they have. New pods cannot start: they stay in `ContainerCreating` while the Secret is missing, and their init container `ssh-host-keys` fails while it holds other keys. So the operator leaves the Deployment and its `app.ini` as they are: spec changes wait, self-healing does not restart pods, vertical sizing does not resize them, and upgrades and `ForgejoRestore`s wait (`Pending`). A pod replaced from outside still does not start (a node drain, an eviction, `kubectl rollout restart`, an HPA scale-up): with one replica, Forgejo is down, HTTP included, until the Secret is fixed. Restore it from a backup's `ssh-host-keys/`, or create it with the keys you want to serve. |
| Init container `ssh-host-keys` fails with "holds other SSH host keys" | The pod got other keys from the Secret than the ones the operator checked and publishes in `status.ssh.hostKeys`. Right after an edit of the Secret this resolves itself once the operator rolls out the new keys; otherwise `Degraded=SSHHostKeysUnavailable` says what is wrong with the Secret. |
