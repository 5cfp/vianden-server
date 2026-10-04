#!/usr/bin/env bash
# Builds release archives of vianden-server into dist/:
#   vianden-server_<version>_linux_amd64.tar.gz   (most servers and VPS)
#   vianden-server_<version>_linux_arm64.tar.gz   (Raspberry Pi 4/5, ARM VPS)
#   vianden-server_<version>_windows_amd64.tar.gz
# Each contains the program, LICENSE, the third-party license notices, and .env.example.
#
# Usage (from the repo root, in bash / Git Bash):  ./scripts/build-release.sh
# Needs: go, and go-licenses (go install github.com/google/go-licenses/v2@v2.0.1)
set -euo pipefail

cd "$(dirname "$0")/.."
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
out=dist
rm -rf "$out"
mkdir -p "$out"

echo "Collecting third-party license notices..."
go-licenses save ./cmd/server --save_path="$out/licenses" --ignore github.com/5cfp/vianden-server >/dev/null 2>&1
mkdir -p "$out/licenses/go"
cp "$(go env GOROOT)/LICENSE" "$out/licenses/go/LICENSE" # the Go standard library is compiled in too

for target in linux/amd64 linux/arm64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  name="vianden-server_${version}_${os}_${arch}"
  dir="$out/$name"
  mkdir -p "$dir"
  ext=""
  [ "$os" = windows ] && ext=".exe"

  echo "Building $name..."
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X github.com/5cfp/vianden-server/internal/buildinfo.Version=$version" \
    -o "$dir/vianden-server$ext" ./cmd/server

  cp LICENSE .env.example "$dir/"
  cp -r "$out/licenses" "$dir/third_party_licenses"
  tar -czf "$out/$name.tar.gz" -C "$out" "$name"
  rm -rf "$dir"
done

rm -rf "$out/licenses"
echo "Done:"
ls -1 "$out"
