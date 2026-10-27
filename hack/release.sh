#!/usr/bin/env bash
# Both CI hosts run this script from the same reviewed tag and publish locally.
set -euo pipefail
: "${TAG:?set an existing tag}"
: "${REGISTRY_HOST:?set the local registry}"
: "${REGISTRY_TOKEN:?package-write credential required on this CI host}"
: "${REGISTRY_USER:?registry username required}"
[[ "$REGISTRY_HOST" == ghcr.io || "$REGISTRY_HOST" == git.lucas.icu ]]
[[ "$TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo 'Invalid version tag'; exit 1; }
version="${TAG#v}"
revision="$(git rev-parse HEAD)"
test "$revision" = "$(git rev-parse "refs/tags/$TAG^{commit}")"
git merge-base --is-ancestor HEAD origin/main
awk -v tag="$TAG" '$0 == "## " tag {emit=1; next} /^## / && emit {exit} emit {print}' CHANGELOG.md > /tmp/forgejo-release-notes.md
test -s /tmp/forgejo-release-notes.md || { echo 'Missing reviewed release notes'; exit 1; }
export SOURCE_DATE_EPOCH="$(git show -s --format=%ct HEAD)"
tmp="$(mktemp -d)"
builder="forgejo-release-$$"
cleanup() { docker buildx rm "$builder" >/dev/null 2>&1 || true; rm -rf "$tmp"; }
trap cleanup EXIT

# Use the same verified Helm and Go toolchain on both CI hosts.
curl -fsSL https://get.helm.sh/helm-v4.2.4-linux-amd64.tar.gz -o "$tmp/helm.tar.gz"
echo "c306b46f719b0a4da32d0f78ee21bf90ce8d602f15b22ab753f0674d1670a7f3  $tmp/helm.tar.gz" | sha256sum -c -
tar -xzf "$tmp/helm.tar.gz" -C "$tmp" linux-amd64/helm
export PATH="$tmp/linux-amd64:$tmp:$PATH"
export GOTOOLCHAIN=go1.26.9
GOBIN="$tmp" go install github.com/google/go-containerregistry/cmd/crane@v0.20.6
test "$(helm show chart charts/forgejo-operator | sed -n 's/^version: //p')" = "$version"
test "$(helm show chart charts/forgejo-operator | sed -n 's/^appVersion: //p' | tr -d '\042')" = "$TAG"
python3 hack/normalize-source-times.py . "$SOURCE_DATE_EPOCH"

image="$REGISTRY_HOST/lictl/forgejo-operator:$TAG"
chart="$REGISTRY_HOST/lictl/charts/forgejo-operator:$version"
echo "$REGISTRY_TOKEN" | docker login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin >/dev/null
echo "$REGISTRY_TOKEN" | helm registry login "$REGISTRY_HOST" -u "$REGISTRY_USER" --password-stdin >/dev/null
remote_digest() {
  if crane digest "$1" 2>"$tmp/remote-error"; then return 0; fi
  if grep -Eiq 'MANIFEST_UNKNOWN|NAME_UNKNOWN|404|not found' "$tmp/remote-error"; then return 1; fi
  cat "$tmp/remote-error" >&2
  return 2
}

# This one-time exception is limited to the unreleased rc.1 artifacts. Future
# versions remain immutable, and an unexpected digest is never replaced.
replaces_rc1_image() {
  [ "$TAG" = v0.1.0-rc.1 ] || return 1
  case "$REGISTRY_HOST:$1" in
    ghcr.io:sha256:a1de7fb19864bd6bd5d5c33606a666a7e31b2788ebefc905451bea0e3b2f3ee1|git.lucas.icu:sha256:821e9f6f5e902a6b8e72c0a9ff86145637688a7a5a0136ac828aff3bd79fbbcb) return 0 ;;
  esac
  return 1
}
replaces_rc1_chart() {
  [ "$TAG" = v0.1.0-rc.1 ] && [ "$1" = sha256:6ba004f46e3ddd395d799b82653927d4e1d4d6caf03d34a9481b487c4cf2d15e ]
}

existing=""
if existing="$(remote_digest "$image")"; then :; else
  result=$?
  test "$result" -eq 1 || exit "$result"
fi
if [ -z "$existing" ] || replaces_rc1_image "$existing"; then
  if [ -n "$existing" ]; then
    test "$(remote_digest "$image")" = "$existing" || { echo 'Image changed during rc.1 replacement'; exit 1; }
  fi
  docker buildx create --name "$builder" --driver docker-container \
    --driver-opt image=docker.io/moby/buildkit@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea --use >/dev/null
  docker buildx inspect --bootstrap >/dev/null
  docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 \
    --provenance=mode=min --build-arg SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
    --build-arg VERSION="$TAG" --build-arg VCS_REF="$revision" \
    --output "type=image,name=$image,push=true,rewrite-timestamp=true,oci-mediatypes=true,compression=gzip,compression-level=6" .
fi
for platform in linux/amd64 linux/arm64; do
  crane config --platform "$platform" "$image" |
    REVISION="$revision" python3 -c 'import json,os,sys; c=json.load(sys.stdin); assert c["config"]["Labels"]["org.opencontainers.image.revision"] == os.environ["REVISION"]'
done
crane manifest "$image" | python3 -c 'import json,sys; m=json.load(sys.stdin); p={(d["platform"]["os"],d["platform"]["architecture"]) for d in m["manifests"] if "platform" in d}; assert {("linux","amd64"),("linux","arm64")} <= p, p'

helm package charts/forgejo-operator -d "$tmp"
archive="$tmp/forgejo-operator-$version.tgz"
normalized="$tmp/normalized-$version.tgz"
go run -mod=readonly hack/normalize-chart.go "$archive" "$normalized" "$SOURCE_DATE_EPOCH"
chart_existing=""
if chart_existing="$(remote_digest "$chart")"; then :; else
  result=$?
  test "$result" -eq 1 || exit "$result"
fi
if [ -n "$chart_existing" ] && ! replaces_rc1_chart "$chart_existing"; then
  mkdir -p "$tmp/published"
  helm pull "oci://$REGISTRY_HOST/lictl/charts/forgejo-operator" --version "$version" --destination "$tmp/published"
  cmp "$normalized" "$tmp/published/forgejo-operator-$version.tgz"
else
  if [ -n "$chart_existing" ]; then
    test "$(remote_digest "$chart")" = "$chart_existing" || { echo 'Chart changed during rc.1 replacement'; exit 1; }
  fi
  helm push "$normalized" "oci://$REGISTRY_HOST/lictl/charts"
fi

# A release is not complete until both sites are public and independently verified.
anon_config="$(mktemp -d)"
DOCKER_CONFIG="$anon_config" crane digest "$image" >/dev/null
DOCKER_CONFIG="$anon_config" crane digest "$chart" >/dev/null
rm -rf "$anon_config"
printf 'Controller %s %s\nChart %s %s\n' "$image" "$(crane digest "$image")" "$chart" "$(crane digest "$chart")"
