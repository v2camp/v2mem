#!/usr/bin/env bash
# 交叉编译 install.sh 会下载的五个预编译文件。
# 名字是契约：改这里必须同步 scripts/install.sh 的 BIN_NAME 与 scripts/publish-release.sh。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/dist}"
mkdir -p "$OUT"
cd "$ROOT" || exit 1

build() {
	goos="$1"
	goarch="$2"
	name="$3"
	printf '→ %s\n' "$name"
	CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" go build -o "$OUT/$name" ./cmd/mem
}

build darwin amd64 mem-darwin-amd64
build darwin arm64 mem-darwin-arm64
build linux amd64 mem-linux-amd64
build linux arm64 mem-linux-arm64
build windows amd64 mem-windows-amd64.exe

for name in mem-darwin-amd64 mem-darwin-arm64 mem-linux-amd64 mem-linux-arm64 mem-windows-amd64.exe; do
	if [[ ! -s "$OUT/$name" ]]; then
		printf '缺少产物 %s\n' "$OUT/$name" >&2
		exit 1
	fi
done

printf '✓ assets in %s\n' "$OUT"
