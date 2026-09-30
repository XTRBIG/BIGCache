#!/usr/bin/env bash
# Cross-compiles release binaries into dist/ and packages them.
#   scripts/build-release.sh [VERSION]
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}
VERSION=${VERSION#v}
rm -rf dist && mkdir -p dist
export CGO_ENABLED=0
for target in windows/amd64 windows/arm64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*}; arch=${target#*/}
  out="dist/${os}-${arch}"
  mkdir -p "$out"
  exe=bigcache; [ "$os" = windows ] && exe=bigcache.exe
  GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "$out/$exe" ./cmd/bigcache
  cp README.md LICENSE "$out/"
  if [ "$os" = windows ]; then
    cp examples/config.windows.json "$out/config.example.json"
    (cd dist && zip -qr "bigcache-${VERSION}-${os}-${arch}.zip" "${os}-${arch}")
  else
    cp examples/config.json "$out/config.example.json"
    cp examples/bigcache.service examples/attach@.service "$out/" 2>/dev/null || true
    tar -C dist -czf "dist/bigcache-${VERSION}-${os}-${arch}.tar.gz" "${os}-${arch}"
  fi
  echo "built $out/$exe"
done
(cd dist && sha256sum *.zip *.tar.gz > SHA256SUMS.txt)
echo "version ${VERSION}: $(ls dist/*.zip dist/*.tar.gz | wc -l) archives in dist/"
