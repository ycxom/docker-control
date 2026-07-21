#!/bin/sh
set -eu

# Compatibility wrapper. Installation logic lives in the fused Go binary.
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
case "$(uname -m)" in
    x86_64|amd64)
        BINARY="$ROOT/dist/docker-control-v3.4.0-linux-amd64"
        ;;
    aarch64|arm64)
        BINARY="$ROOT/dist/docker-control-v3.4.0-linux-arm64"
        ;;
    *)
        echo "unsupported architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

if [ ! -x "$BINARY" ]; then
    echo "executable not found: $BINARY" >&2
    exit 1
fi

exec "$BINARY" install --home "$(pwd -P)" "$@"
