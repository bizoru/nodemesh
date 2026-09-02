#!/usr/bin/env bash
# Cross-compile nodemesh for all four nodes into dist/.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
export CGO_ENABLED=0

# Versión inyectada en el binario, visible en /api/nodes y en el dashboard.
# git describe da "v1.0.0" en un tag limpio y "v1.0.0-3-gabc1234" tres commits
# después, así que se ve de un vistazo si un nodo corre algo sin etiquetar.
VER="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
LDFLAGS="-s -w -X main.version=${VER}"
echo "compilando versión ${VER}"
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-darwin-amd64 .
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-linux-arm64 .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-windows-amd64.exe .
ls -lh dist/
