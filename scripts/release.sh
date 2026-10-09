#!/bin/sh
# Build and publish a release from a tag, without GitHub Actions.
#
#   scripts/release.sh v0.1.0
#
# Produces what .github/workflows/release.yml produces, for the two release
# targets (linux/amd64, darwin/arm64): raw executables named
# <binary>_<version>_<goos>_<goarch>, one archive per target with a .sha256,
# and SHA256SUMS over every asset; builds the linux/amd64 image and pushes
# ghcr.io/ziozzang/dorang:<version> and :<major>.<minor>; then creates the
# GitHub release with the assets. The tag must already be pushed.
#
# Needs: go, docker (logged in to ghcr.io with write:packages), gh.
set -eu

tag=${1:?usage: scripts/release.sh vX.Y.Z}
case $tag in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "not a release tag: $tag" >&2; exit 2 ;; esac
ver=${tag#v}
image=ghcr.io/ziozzang/dorang
repo=ziozzang/dorang

git rev-parse -q --verify "refs/tags/$tag" >/dev/null || { echo "no tag $tag" >&2; exit 1; }
git ls-remote --exit-code --tags origin "$tag" >/dev/null || { echo "push $tag first" >&2; exit 1; }
sha=$(git rev-parse "$tag^{commit}")
date=$(date -u +%Y-%m-%dT%H:%M:%SZ)

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/src" "$work/dist"
git archive "$tag" | tar -x -C "$work/src"

for target in linux/amd64 darwin/arm64; do
	goos=${target%/*} goarch=${target#*/}
	bin="$work/bin/$goos-$goarch"
	mkdir -p "$bin"
	ld="-s -w -X main.version=$tag -X main.commit=$(echo "$sha" | cut -c1-12) -X main.buildDate=$date"
	(cd "$work/src" &&
		GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "$ld" -o "$bin/dorang" ./cmd/dorang &&
		GOOS=$goos GOARCH=$goarch CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags "$ld" -o "$bin/dorangctl" ./cmd/dorangctl)
	(cd "$work/dist" &&
		tar -C "$bin" -czf "dorang_${tag}_${goos}_${goarch}.tar.gz" dorang dorangctl &&
		sha256sum "dorang_${tag}_${goos}_${goarch}.tar.gz" >"dorang_${tag}_${goos}_${goarch}.tar.gz.sha256")
	cp "$bin/dorang" "$work/dist/dorang_${ver}_${goos}_${goarch}"
	cp "$bin/dorangctl" "$work/dist/dorangctl_${ver}_${goos}_${goarch}"
done
(cd "$work/dist" && sha256sum $(ls | grep -v '\.sha256$') >SHA256SUMS && cat SHA256SUMS)

minor=$(echo "$ver" | cut -d. -f1,2)
DOCKER_BUILDKIT=${DOCKER_BUILDKIT:-0} docker build --network=host \
	--build-arg VERSION="$tag" --build-arg COMMIT="$sha" --build-arg BUILD_DATE="$date" \
	--label org.opencontainers.image.source="https://github.com/$repo" \
	--label org.opencontainers.image.version="$ver" \
	--label org.opencontainers.image.revision="$sha" \
	-t "$image:$ver" -t "$image:$minor" "$work/src"
[ "$(docker run --rm --network none "$image:$ver" -version)" = "$tag" ] || { echo "the image does not report $tag" >&2; exit 1; }
docker push "$image:$ver"
docker push "$image:$minor"

notes=$(awk -v h="## $ver" 'index($0,h)==1{on=1;next} on&&/^## /{exit} on' "$work/src/CHANGELOG.md")
gh release create "$tag" --repo "$repo" --title "dorang $ver" --notes "${notes:-dorang $ver}" --verify-tag "$work"/dist/*
