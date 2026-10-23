#!/usr/bin/env bash
# Installs the local S3 lab (Garage) into namespace s3-lab of the current kubectl context and
# creates the buckets "forgejo" (object storage) and "forgejo-backups" (remote backups).
# Idempotent: the Secrets are generated once and kept. See docs/object-storage.md#local-lab.
set -euo pipefail
NS=s3-lab
HERE=$(cd "$(dirname "$0")" && pwd)

kubectl get ns "$NS" >/dev/null 2>&1 || kubectl create ns "$NS"
if ! kubectl -n "$NS" get secret garage >/dev/null 2>&1; then
  access="GK$(openssl rand -hex 12)"
  secret="$(openssl rand -hex 32)"
  # Garage reads GARAGE_RPC_SECRET and GARAGE_DEFAULT_{ACCESS,SECRET}_KEY from its environment.
  kubectl -n "$NS" create secret generic garage \
    --from-literal=GARAGE_RPC_SECRET="$(openssl rand -hex 32)" \
    --from-literal=GARAGE_DEFAULT_ACCESS_KEY="$access" \
    --from-literal=GARAGE_DEFAULT_SECRET_KEY="$secret"
  # The same key under the names spec.objectStorage.s3.credentialsSecret expects by default;
  # copy it into the namespace of a Forgejo instance.
  kubectl -n "$NS" create secret generic s3-credentials \
    --from-literal=AWS_ACCESS_KEY_ID="$access" \
    --from-literal=AWS_SECRET_ACCESS_KEY="$secret"
fi
kubectl apply -f "$HERE/garage.yaml"
kubectl -n "$NS" rollout status deploy/garage --timeout=180s
# The optional HTTPS endpoint needs cert-manager and its ClusterIssuer lab-ca.
if kubectl get clusterissuer lab-ca >/dev/null 2>&1; then
  kubectl apply -f "$HERE/garage-tls.yaml"
  kubectl -n "$NS" wait certificate/garage-tls --for=condition=Ready --timeout=120s
  kubectl -n "$NS" rollout status deploy/garage-tls --timeout=180s
fi

# A second bucket for remote backups, created with the default key (it may create buckets).
key=$(kubectl -n "$NS" get secret s3-credentials -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)
sec=$(kubectl -n "$NS" get secret s3-credentials -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)
endpoint=http://garage.$NS.svc.cluster.local:3900
for i in $(seq 1 30); do
  code=$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 "aws:amz:garage:s3" --user "$key:$sec" "$endpoint/forgejo?list-type=2&max-keys=1" || true)
  [ "$code" = 200 ] && break
  sleep 2
done
code=$(curl -s -o /dev/null -w '%{http_code}' --aws-sigv4 "aws:amz:garage:s3" --user "$key:$sec" -X PUT "$endpoint/forgejo-backups")
echo "PUT /forgejo-backups: HTTP $code (200 created, 409 exists)"
curl -s --aws-sigv4 "aws:amz:garage:s3" --user "$key:$sec" "$endpoint/" | tr '<' '\n' | sed -n 's/^Name>//p'
