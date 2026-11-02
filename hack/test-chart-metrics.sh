#!/usr/bin/env bash
# Verify the operator's rendered metrics contract without requiring a cluster.
set -euo pipefail

cd "$(dirname "$0")/.."
render_dir="$(mktemp -d)"
trap 'rm -rf "$render_dir"' EXIT
chart=charts/forgejo-operator
monitor=templates/prometheus/controller-manager-metrics-monitor.yaml

reject_match() {
  if grep -Eq "$1" "$2"; then
    echo "Unexpected rendered configuration matching $1 in $2" >&2
    exit 1
  fi
}

helm template forgejo-operator "$chart" > "$render_dir/default.yaml"
reject_match '^kind: ServiceMonitor$' "$render_dir/default.yaml"

for secure in true false; do
  helm template forgejo-operator "$chart" --show-only "$monitor" \
    --set prometheus.enabled=true --set metrics.secure="$secure" > "$render_dir/monitor.yaml"
  grep -q '^    honorLabels: true$' "$render_dir/monitor.yaml"
  reject_match 'metrics-server-cert' "$render_dir/monitor.yaml"
  if [ "$secure" = true ]; then
    grep -q '^    scheme: https$' "$render_dir/monitor.yaml"
    grep -q '^    bearerTokenFile:' "$render_dir/monitor.yaml"
    grep -q '^      insecureSkipVerify: true$' "$render_dir/monitor.yaml"
  else
    grep -q '^    scheme: http$' "$render_dir/monitor.yaml"
    reject_match 'tlsConfig:|bearerTokenFile:' "$render_dir/monitor.yaml"
  fi
done

# Reject even a disabled stale option instead of silently ignoring user intent.
for enabled in true false; do
  if helm template forgejo-operator "$chart" --set certManager.enabled="$enabled" \
    > "$render_dir/removed-option.log" 2>&1; then
    echo 'Removed certManager chart option was accepted' >&2
    exit 1
  fi
  grep -q certManager "$render_dir/removed-option.log"
done
