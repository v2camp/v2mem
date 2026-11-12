#!/usr/bin/env bash
# 交叉编译 install.sh 会下载的五个预编译文件，并生成 SHA256 清单。
# 名字是契约：改这里必须同步 scripts/install.sh 的 BIN_NAME 与 scripts/publish-release.sh。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT="${1:-$ROOT/dist}"
mkdir -p "$OUT"
cd "$ROOT" || exit 1

assets=(
	mem-darwin-amd64
	mem-darwin-arm64
	mem-linux-amd64
	mem-linux-arm64
	mem-windows-amd64.exe
)

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

for name in "${assets[@]}"; do
	if [[ ! -s "$OUT/$name" ]]; then
		printf '缺少产物 %s\n' "$OUT/$name" >&2
		exit 1
	fi
done

# SHA256 清单：Linux/CI 用 sha256sum，macOS 本地用 shasum，两者输出格式一致（<hex>  <name>）。
# install.sh 只取目标平台那一行比对，故清单必须按文件名可检索。
if command -v sha256sum >/dev/null 2>&1; then
	hash_cmd=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
	hash_cmd=(shasum -a 256)
else
	printf '缺少 sha256sum / shasum，无法生成 checksums.txt\n' >&2
	exit 1
fi
(
	cd "$OUT" || exit 1
	"${hash_cmd[@]}" "${assets[@]}" >checksums.txt
)
if [[ ! -s "$OUT/checksums.txt" ]]; then
	printf 'checksums.txt 生成失败\n' >&2
	exit 1
fi

printf '✓ assets in %s\n' "$OUT"
