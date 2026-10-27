#!/usr/bin/env bash
set -euo pipefail
: "${TAG:?set an existing tag}"
: "${GITHUB_TOKEN:?workflow token required}"
: "${FORGEJO_REGISTRY_TOKEN:?Forgejo package-write token required}"
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo 'Invalid version tag'; exit 1; }
version="${TAG#v}"
test "$(git rev-parse HEAD)" = "$(git rev-parse "refs/tags/$TAG^{commit}")"
git merge-base --is-ancestor HEAD origin/main
chart_version="$(helm show chart charts/forgejo-operator | sed -n 's/^version: //p')"
chart_app_version="$(helm show chart charts/forgejo-operator | sed -n 's/^appVersion: //p' | tr -d '"')"
test "$chart_version" = "$version"
test "$chart_app_version" = "$TAG"
awk -v tag="$TAG" '$0 == "## " tag {emit=1; next} /^## / && emit {exit} emit {print}' CHANGELOG.md > /tmp/forgejo-release-notes.md
test -s /tmp/forgejo-release-notes.md || { echo 'Missing release notes'; exit 1; }
image="ghcr.io/lictl/forgejo-operator:$TAG"
mirror_image="git.lucas.icu/lictl/forgejo-operator:$TAG"
chart="ghcr.io/lictl/charts/forgejo-operator:$version"
mirror_chart="git.lucas.icu/lictl/charts/forgejo-operator:$version"
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin >/dev/null
echo "$FORGEJO_REGISTRY_TOKEN" | docker login git.lucas.icu -u lictl --password-stdin >/dev/null
if ! command -v crane >/dev/null; then
  GOBIN=/tmp go install github.com/google/go-containerregistry/cmd/crane@v0.20.6
  export PATH="/tmp:$PATH"
fi
existing="$(crane digest "$image" 2>/dev/null || true)"
if [ -z "$existing" ]; then
  docker buildx build --platform linux/amd64,linux/arm64 --push \
    --build-arg VERSION="$TAG" --build-arg VCS_REF="$(git rev-parse HEAD)" \
    --tag "$image" .
  existing="$(crane digest "$image")"
fi
for platform in linux/amd64 linux/arm64; do
  ARCH="$platform" REVISION="$(git rev-parse HEAD)" crane config --platform "$platform" "$image" | python3 -c 'import json,os,sys; x=json.load(sys.stdin)["config"]["Labels"]; assert x["org.opencontainers.image.revision"] == os.environ["REVISION"]'
done
remote="$(crane digest "$mirror_image" 2>/dev/null || true)"
if [ -z "$remote" ]; then crane cp "$image" "$mirror_image"; else test "$remote" = "$existing"; fi
test "$(crane digest "$mirror_image")" = "$existing"
crane manifest "$image" | python3 -c 'import json,sys; d=json.load(sys.stdin); p={(x["platform"]["os"],x["platform"]["architecture"]) for x in d["manifests"] if "platform" in x}; assert {("linux","amd64"),("linux","arm64")} <= p, p'
helm package charts/forgejo-operator -d /tmp
chart_digest="$(crane digest "$chart" 2>/dev/null || true)"
if [ -z "$chart_digest" ]; then
  echo "$GITHUB_TOKEN" | helm registry login ghcr.io -u "$GITHUB_ACTOR" --password-stdin >/dev/null
  helm push "/tmp/forgejo-operator-$version.tgz" oci://ghcr.io/lictl/charts
  chart_digest="$(crane digest "$chart")"
fi
mkdir -p /tmp/forgejo-published-chart
helm pull "oci://ghcr.io/lictl/charts/forgejo-operator" --version "$version" --destination /tmp/forgejo-published-chart
cmp "/tmp/forgejo-operator-$version.tgz" "/tmp/forgejo-published-chart/forgejo-operator-$version.tgz"
remote="$(crane digest "$mirror_chart" 2>/dev/null || true)"
if [ -z "$remote" ]; then crane cp "$chart" "$mirror_chart"; else test "$remote" = "$chart_digest"; fi
test "$(crane digest "$mirror_chart")" = "$chart_digest"
anon_docker="$(mktemp -d)"
for ref in "$image" "$mirror_image" "$chart" "$mirror_chart"; do
  DOCKER_CONFIG="$anon_docker" crane digest "$ref" >/dev/null
done
helm show chart "oci://ghcr.io/lictl/charts/forgejo-operator" --version "$version" >/dev/null
helm show chart "oci://git.lucas.icu/lictl/charts/forgejo-operator" --version "$version" >/dev/null
{
  cat /tmp/forgejo-release-notes.md
  printf '\nController: %s (also %s)\nDigest: %s\n' "$image" "$mirror_image" "$existing"
  printf 'Chart: oci://ghcr.io/lictl/charts/forgejo-operator (also oci://git.lucas.icu/lictl/charts/forgejo-operator), version %s\nDigest: %s\n' "$version" "$chart_digest"
} > /tmp/forgejo-release-body.md
gh release view "$TAG" >/dev/null 2>&1 || gh release create "$TAG" --verify-tag --title "$TAG" --notes-file /tmp/forgejo-release-body.md
