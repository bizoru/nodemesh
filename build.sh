#!/usr/bin/env bash
# Cross-compile nodemesh for all four nodes into dist/.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
export CGO_ENABLED=0
GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/nodemesh-darwin-arm64 .
GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/nodemesh-darwin-amd64 .
GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/nodemesh-linux-amd64 .
GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/nodemesh-linux-arm64 .
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/nodemesh-windows-amd64.exe .
ls -lh dist/
