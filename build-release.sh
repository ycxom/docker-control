#!/bin/sh
set -eu

VERSION="${1:-dev}"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
DIST="$ROOT/dist"
mkdir -p "$DIST"
rm -f "$DIST"/docker-control-* "$DIST/SHA256SUMS.txt"

build() {
    os="$1"
    arch="$2"
    extension="$3"
    output="$DIST/docker-control-$VERSION-$os-$arch$extension"
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
        go build -buildvcs=false -trimpath -ldflags="-s -w -X main.version=$VERSION" -o "$output" ./cmd/docker-control
}

cd "$ROOT"
build windows amd64 .exe
build linux amd64 ""
build linux arm64 ""

if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$DIST"/docker-control-* > "$DIST/SHA256SUMS.txt"
fi
