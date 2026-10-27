#!/usr/bin/env bash
# Read-only comparison after both independent release jobs complete.
set -euo pipefail
: "${TAG:?set existing release tag}"
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]
version="${TAG#v}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
export DOCKER_CONFIG="$tmp/docker"
export HELM_REGISTRY_CONFIG="$tmp/helm.json"
mkdir -p "$DOCKER_CONFIG"
if ! command -v crane >/dev/null; then
  GOBIN="$tmp" GOTOOLCHAIN=go1.26.9 go install github.com/google/go-containerregistry/cmd/crane@v0.20.6
  export PATH="$tmp:$PATH"
fi
github_image="ghcr.io/lictl/forgejo-operator:$TAG"
forgejo_image="git.lucas.icu/lictl/forgejo-operator:$TAG"
github_chart="ghcr.io/lictl/charts/forgejo-operator:$version"
forgejo_chart="git.lucas.icu/lictl/charts/forgejo-operator:$version"
for platform in linux/amd64 linux/arm64; do
  left="$(crane digest --platform "$platform" "$github_image")"
  right="$(crane digest --platform "$platform" "$forgejo_image")"
  printf '%s image manifest: %s (GHCR) %s (Forgejo)\n' "$platform" "$left" "$right"
  test "$left" = "$right" || { echo "Independent $platform images differ" >&2; exit 1; }
done
echo "Image indexes, including site-specific provenance:"
crane digest "$github_image"
crane digest "$forgejo_image"
for host in ghcr.io git.lucas.icu; do
  mkdir -p "$tmp/$host"
  helm pull "oci://$host/lictl/charts/forgejo-operator" --version "$version" --destination "$tmp/$host"
done
cmp "$tmp/ghcr.io/forgejo-operator-$version.tgz" "$tmp/git.lucas.icu/forgejo-operator-$version.tgz"
echo "Chart archive SHA256:"
sha256sum "$tmp/ghcr.io/forgejo-operator-$version.tgz"
left="$(crane digest "$github_chart")"
right="$(crane digest "$forgejo_chart")"
printf 'Chart OCI manifests: %s (GHCR) %s (Forgejo)\n' "$left" "$right"
test "$left" = "$right" || { echo 'Chart OCI manifests differ' >&2; exit 1; }
echo 'Independent artifacts match at the required payload levels.'
