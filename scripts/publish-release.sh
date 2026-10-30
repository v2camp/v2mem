#!/usr/bin/env bash
# 把 dist 里的预编译文件挂到已有 GitHub Release；没有 Release 就创建。
# 依赖 gh，并且 GH_TOKEN / GITHUB_TOKEN 对仓库有 contents: write。
# 文件名须与 scripts/release-assets.sh、scripts/install.sh 一致。
set -euo pipefail

dist="${1:?dist 目录}"
tag="${2:?tag}"

require_asset() {
	path="$dist/$1"
	if [[ ! -s "$path" ]]; then
		printf '缺少产物 %s\n' "$path" >&2
		exit 1
	fi
}

require_asset mem-darwin-amd64
require_asset mem-darwin-arm64
require_asset mem-linux-amd64
require_asset mem-linux-arm64
require_asset mem-windows-amd64.exe

notes="v2mem ${tag} 预编译二进制（CGO_ENABLED=0）。

- mem-darwin-amd64 / mem-darwin-arm64
- mem-linux-amd64 / mem-linux-arm64
- mem-windows-amd64.exe

安装：curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash
指定版本：curl -fsSL …/scripts/install.sh | bash -s -- --version ${tag}"

if gh release view "$tag" >/dev/null 2>&1; then
	gh release upload "$tag" \
		"$dist/mem-darwin-amd64" \
		"$dist/mem-darwin-arm64" \
		"$dist/mem-linux-amd64" \
		"$dist/mem-linux-arm64" \
		"$dist/mem-windows-amd64.exe" \
		--clobber
else
	gh release create "$tag" \
		"$dist/mem-darwin-amd64" \
		"$dist/mem-darwin-arm64" \
		"$dist/mem-linux-amd64" \
		"$dist/mem-linux-arm64" \
		"$dist/mem-windows-amd64.exe" \
		--title "$tag" \
		--notes "$notes"
fi

printf '✓ release %s\n' "$tag"
