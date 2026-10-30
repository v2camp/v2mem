#!/usr/bin/env bash
# v2mem 一键安装：优先下载预编译二进制，失败则浅克隆后源码构建。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash -s -- --version v0.1.0
#   bash scripts/install.sh --dir ~/bin --version v0.1.0
#
# 选项：
#   --dir DIR        安装目录（默认 ~/.local/bin）
#   --version V      版本 tag（默认 latest）。latest 走
#                    /releases/latest/download/<文件名>，不是 /releases/download/latest/…
#   --from-source    跳过下载，浅克隆 main（或 --version 指定的 tag）后构建
#   --dry-run        只打印将使用的 URL、目录和克隆计划，不下载、不建目录、不编译
#
# 源码构建尊重调用方已有的 GOPROXY / GOSUMDB，不会改写。
# 国内模块下载超时可先：export GOPROXY=https://goproxy.cn,direct
#
# Windows：需要 Git Bash / MSYS（cmd.exe 不能跑本脚本）。只提供 amd64 预编译
# mem-windows-amd64.exe；默认 ~/.local/bin 通常不在 Windows Path 里，请用 --dir。
# 该路径未在 Windows runner 上实测。
#
# 测试可设 V2MEM_INSTALL_OS / V2MEM_INSTALL_ARCH 覆盖 uname（普通安装不必设）。
set -euo pipefail

REPO="${V2MEM_INSTALL_REPO:-v2camp/v2mem}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="latest"
FROM_SOURCE=0
DRY_RUN=0

usage() {
	printf '%s\n' \
		"用法: install.sh [--dir DIR] [--version TAG|latest] [--from-source] [--dry-run]" \
		"" \
		"  latest 下载 https://github.com/${REPO}/releases/latest/download/<asset>" \
		"  指定 tag 下载 https://github.com/${REPO}/releases/download/<tag>/<asset>" \
		"" \
		"源码回退会浅克隆到临时目录再 CGO_ENABLED=0 go build（curl|bash 时没有仓库目录）。" \
		"已有 GOPROXY / GOSUMDB 原样保留。国内超时可 export GOPROXY=https://goproxy.cn,direct"
}

die() {
	printf '✗ %s\n' "$*" >&2
	exit 1
}

need_arg() {
	if [[ $# -lt 2 || -z "${2:-}" ]]; then
		die "$1 需要参数"
	fi
}

while [[ $# -gt 0 ]]; do
	case "$1" in
	--dir)
		need_arg "$@"
		INSTALL_DIR="$2"
		shift 2
		;;
	--version)
		need_arg "$@"
		VERSION="$2"
		shift 2
		;;
	--from-source)
		FROM_SOURCE=1
		shift
		;;
	--dry-run)
		DRY_RUN=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		printf '未知选项: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
done

if [[ -z "$VERSION" ]]; then
	VERSION="latest"
fi

detect_platform() {
	local os arch
	if [[ -n "${V2MEM_INSTALL_OS:-}" ]]; then
		os="$V2MEM_INSTALL_OS"
	else
		os="$(uname | tr '[:upper:]' '[:lower:]')"
	fi
	if [[ -n "${V2MEM_INSTALL_ARCH:-}" ]]; then
		arch="$V2MEM_INSTALL_ARCH"
	else
		arch="$(uname -m)"
	fi

	case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*)
		die "不支持的架构: $arch（当前支持 amd64/arm64）"
		;;
	esac

	case "$os" in
	darwin | linux)
		OS="$os"
		ARCH="$arch"
		BIN_NAME="mem-${os}-${arch}"
		EXE_NAME="mem"
		;;
	mingw* | msys* | cygwin* | windows)
		OS="windows"
		ARCH="$arch"
		if [[ "$arch" != "amd64" ]]; then
			die "Windows 只提供 amd64 预编译（mem-windows-amd64.exe），当前架构是 $arch。请在 amd64 的 Git Bash 中安装，或自行交叉编译。"
		fi
		BIN_NAME="mem-windows-amd64.exe"
		EXE_NAME="mem.exe"
		;;
	*)
		die "不支持的平台: $os"
		;;
	esac
}

asset_url() {
	if [[ "$VERSION" == "latest" ]]; then
		printf 'https://github.com/%s/releases/latest/download/%s' "$REPO" "$BIN_NAME"
	else
		printf 'https://github.com/%s/releases/download/%s/%s' "$REPO" "$VERSION" "$BIN_NAME"
	fi
}

clone_ref() {
	if [[ "$VERSION" == "latest" ]]; then
		printf 'main'
	else
		printf '%s' "$VERSION"
	fi
}

clone_url() {
	printf 'https://github.com/%s.git' "$REPO"
}

print_dry_run() {
	local action="download"
	if [[ "$FROM_SOURCE" -eq 1 ]]; then
		action="source"
	fi
	printf 'V2MEM_DRY_ACTION=%s\n' "$action"
	printf 'V2MEM_DRY_OS=%s\n' "$OS"
	printf 'V2MEM_DRY_ARCH=%s\n' "$ARCH"
	printf 'V2MEM_DRY_BIN=%s\n' "$BIN_NAME"
	printf 'V2MEM_DRY_EXE=%s\n' "$EXE_NAME"
	printf 'V2MEM_DRY_URL=%s\n' "$(asset_url)"
	printf 'V2MEM_DRY_DIR=%s\n' "$INSTALL_DIR"
	printf 'V2MEM_DRY_MKDIR=mkdir -p %s\n' "$INSTALL_DIR"
	printf 'V2MEM_DRY_CLONE_REF=%s\n' "$(clone_ref)"
	printf 'V2MEM_DRY_CLONE_URL=%s\n' "$(clone_url)"
	printf 'V2MEM_DRY_SOURCE=clone\n'
	if [[ -n "${GOPROXY:-}" ]]; then
		printf 'V2MEM_DRY_GOPROXY=%s\n' "$GOPROXY"
	else
		printf 'V2MEM_DRY_GOPROXY=\n'
	fi
	if [[ -n "${GOSUMDB:-}" ]]; then
		printf 'V2MEM_DRY_GOSUMDB=%s\n' "$GOSUMDB"
	else
		printf 'V2MEM_DRY_GOSUMDB=\n'
	fi
}

reject_not_binary() {
	local sig
	if [[ ! -s "$1" ]]; then
		printf '下载结果为空。\n' >&2
		return 1
	fi
	sig="$(head -c 1 "$1" || true)"
	case "$sig" in
	"<" | "{")
		printf '下载结果不是可执行文件（像是错误页）。\n' >&2
		return 1
		;;
	esac
	return 0
}

download_release() {
	local url="$1" dest="$2"
	DL_TMP="${dest}.download.$$"
	printf '→ 下载 %s\n' "$url"
	if ! command -v curl >/dev/null 2>&1; then
		printf '未找到 curl，无法下载预编译产物。\n' >&2
		rm -f "$DL_TMP"
		DL_TMP=""
		return 1
	fi
	if curl -fsSL --connect-timeout 15 --retry 2 --retry-delay 1 -o "$DL_TMP" "$url" \
		&& reject_not_binary "$DL_TMP"; then
		mv -f "$DL_TMP" "$dest"
		DL_TMP=""
		chmod +x "$dest"
		return 0
	fi
	rm -f "$DL_TMP"
	DL_TMP=""
	printf '✗ 预编译下载失败：%s\n' "$url" >&2
	printf '  版本 %s 可能还没有 %s 产物，将改为浅克隆源码构建。\n' "$VERSION" "$BIN_NAME" >&2
	return 1
}

require_go() {
	if command -v go >/dev/null 2>&1; then
		return 0
	fi
	printf '%s\n' \
		"✗ 未找到 go 命令，无法源码构建。" \
		"  安装 Go：https://go.dev/dl/" \
		"  装好后重跑本脚本。国内拉取模块若超时，先执行：" \
		"    export GOPROXY=https://goproxy.cn,direct" \
		"  本脚本不会改写已有的 GOPROXY / GOSUMDB。" >&2
	return 1
}

require_git() {
	if command -v git >/dev/null 2>&1; then
		return 0
	fi
	printf '%s\n' \
		"✗ 未找到 git 命令，无法把仓库克隆到临时目录。" \
		"  curl|bash 时当前目录没有源码，不能直接 go build ./cmd/mem。" \
		"  请先安装 git，或等预编译产物发布后重跑（不加 --from-source）。" >&2
	return 1
}

hint_goproxy() {
	if [[ -n "${GOPROXY:-}" ]]; then
		return 0
	fi
	printf '%s\n' \
		"提示：当前未设置 GOPROXY。若 go build 卡在下载模块（国内网络常见），" \
		"可中断后执行 export GOPROXY=https://goproxy.cn,direct 再重跑" \
		"（已有 GOPROXY / GOSUMDB 会原样传给 go，脚本不会覆盖）。" >&2
}

install_from_source() {
	local dest="$1" ref url src
	require_go || return 1
	require_git || return 1
	ref="$(clone_ref)"
	url="$(clone_url)"
	mkdir -p "$INSTALL_DIR"
	src="$(mktemp -d "${TMPDIR:-/tmp}/v2mem-src.XXXXXX")"
	WORKDIR="$src"
	printf '→ 浅克隆 %s @ %s\n' "$url" "$ref"
	if ! GIT_TERMINAL_PROMPT=0 git clone --depth 1 --branch "$ref" "$url" "$src"; then
		printf '%s\n' \
			"✗ git clone 失败（需要能访问 ${url} 的分支或 tag ${ref}）。" \
			"  Go 模块下载慢时设置 GOPROXY 即可；GitHub 本身不可达时，源码回退也无法完成。" >&2
		return 1
	fi
	hint_goproxy
	printf '→ 源码构建（CGO_ENABLED=0 go build -o %s ./cmd/mem）\n' "$dest"
	(
		cd "$src" || exit 1
		CGO_ENABLED=0 go build -o "$dest" ./cmd/mem
	)
	chmod +x "$dest"
}

finish_path_hint() {
	case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		printf '\n提示：%s 不在 PATH 中。\n' "$INSTALL_DIR"
		if [[ "$OS" == "windows" ]]; then
			printf '  请把该目录加入用户 Path，或用 --dir 指到已在 Path 里的目录。\n'
			printf '  本脚本只在 Git Bash / MSYS 下尝试安装；cmd.exe 不能执行它。\n'
		else
			# shellcheck disable=SC2016 # 用户粘贴片段，$PATH 保持字面量
			printf '  echo '\''export PATH="%s:$PATH"'\'' >> ~/.zshrc && source ~/.zshrc\n' "$INSTALL_DIR"
		fi
		;;
	esac
}

WORKDIR=""
DL_TMP=""
cleanup() {
	if [[ -n "$WORKDIR" ]]; then
		rm -rf "$WORKDIR"
	fi
	if [[ -n "$DL_TMP" ]]; then
		rm -f "$DL_TMP"
	fi
}
trap cleanup EXIT

detect_platform

if [[ "$DRY_RUN" -eq 1 ]]; then
	print_dry_run
	exit 0
fi

DEST="$INSTALL_DIR/$EXE_NAME"

if [[ "$FROM_SOURCE" -eq 0 ]]; then
	mkdir -p "$INSTALL_DIR"
	if ! download_release "$(asset_url)" "$DEST"; then
		install_from_source "$DEST"
	fi
else
	printf '→ 已指定 --from-source，跳过预编译下载。\n'
	install_from_source "$DEST"
fi

finish_path_hint
printf '\n✓ 安装完成：%s\n' "$DEST"
printf '  运行 mem stats 验证，或 mem init 接入你本机的 AI 工具。\n'
