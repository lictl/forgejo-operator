#!/usr/bin/env bash
# A fresh Kind cluster and kubeconfig for every run, including failed runs.
set -euo pipefail
kind_bin=${KIND:-kind}
cluster=${KIND_CLUSTER:-forgejo-operator-test-e2e}
node_image=${KIND_NODE_IMAGE:-kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5}
command -v "$kind_bin" >/dev/null
clusters=$("$kind_bin" get clusters)
if grep -Fxq "$cluster" <<< "$clusters"; then
  echo "Refusing to reuse or delete existing Kind cluster: $cluster" >&2
  exit 1
fi
scratch=$(mktemp -d)
create_attempted=false
# Set this before any operation whose cleanup could update kubeconfig.
export KUBECONFIG="$scratch/config" KUBECTL_KUBERC=false
cleanup() {
  result=$?
  trap - EXIT
  if (( result != 0 )) && [[ "$create_attempted" == true ]]; then
    KUBECONFIG="$scratch/config" "$kind_bin" export logs "$scratch/logs" --name "$cluster" >/dev/null 2>&1 || true
    echo "Failed E2E diagnostics: $scratch/logs" >&2
  fi
  delete_result=0
  if [[ "$create_attempted" == true ]]; then
    KUBECONFIG="$scratch/config" "$kind_bin" delete cluster --name "$cluster" || delete_result=$?
  fi
  if (( delete_result != 0 )); then
    echo "Could not delete Kind cluster '$cluster'. Preserved recovery files: $scratch" >&2
    printf 'Retry cleanup: KUBECONFIG=%q %q delete cluster --name %q\n' "$scratch/config" "$kind_bin" "$cluster" >&2
    # Keep the original test failure when both tests and cleanup failed.
    if (( result == 0 )); then result=$delete_result; fi
    exit "$result"
  fi
  rm -f "$scratch/config" "$scratch/isolated"
  if (( result == 0 )); then rm -rf "$scratch"; fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
# This name did not exist at preflight. Clean up even a failed create: Kind can
# leave nodes behind if its final kubeconfig export fails.
create_attempted=true
# --kubeconfig prevents Kind from merging into the caller's default kubeconfig.
"$kind_bin" create cluster --name "$cluster" --image "$node_image" --kubeconfig "$scratch/config" --wait 120s
KUBECONFIG="$scratch/config" kubectl config view --minify --flatten --context "kind-$cluster" > "$scratch/isolated"
export KUBECONFIG="$scratch/isolated" KIND="$kind_bin" KIND_CLUSTER="$cluster" KUBECTL_KUBERC=false
export FORGEJO_E2E_ISOLATED_CLUSTER="$cluster"
go test -tags=e2e ./test/e2e/ -count=1 -timeout=45m -v -ginkgo.v
