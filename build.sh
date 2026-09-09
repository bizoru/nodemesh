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

# rigby (HP Stream 7): Windows 8.1 de 32 bits. Go 1.21 subió el mínimo a
# Windows 10; un binario 386 de un toolchain actual SÍ arranca allí (probado),
# pero "funciona aunque nadie lo prueba" no es base para el servicio que tiene
# que ser creíble cuando lo demás se cae, así que este target —y sólo este— se
# compila con 1.20, el último soportado en 8.1. Instalar una vez con:
#   go install golang.org/dl/go1.20.14@latest && go1.20.14 download
GO120="${GO120:-$(command -v go1.20.14 || echo "$HOME/go/bin/go1.20.14")}"
if [ -x "$GO120" ]; then
  GOOS=windows GOARCH=386 "$GO120" build -trimpath -ldflags="${LDFLAGS}" -o dist/nodemesh-windows-386.exe . \
    && echo "windows/386 (Win8.1) compilado con $("$GO120" version | awk '{print $3}')"
else
  echo "AVISO: sin go1.20.14, no se compila windows/386 (rigby se queda sin actualizar)" >&2
fi
ls -lh dist/
