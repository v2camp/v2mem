#!/usr/bin/env bash
# v2mem 一键安装：优先下载预编译二进制（mac/linux/windows），失败回退源码构建。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/wanghui/v2mem/main/scripts/install.sh | bash
#   bash scripts/install.sh --dir ~/bin --version v0.1.0
#
# 选项：
#   --dir DIR    安装目录（默认 ~/.local/bin；需要时可加 sudo）
#   --version V  指定版本 tag（默认 latest）
#   --from-source 跳过下载，直接源码构建
set -euo pipefail

# ---------- 参数 ----------
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
VERSION="latest"
FROM_SOURCE=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--dir)
		INSTALL_DIR="$2"
		shift 2
		;;
	--version)
		VERSION="$2"
		shift 2
		;;
	--from-source)
		FROM_SOURCE=1
		shift
		;;
	*)
		echo "未知选项: $1" >&2
		exit 2
		;;
	esac
done

REPO="wanghui/v2mem"
BASE="https://github.com/$REPO/releases/download"

# ---------- 探测平台 ----------
OS="$(uname | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
x86_64 | amd64) ARCH="amd64" ;;
aarch64 | arm64) ARCH="arm64" ;;
*)
	echo "不支持的架构: $ARCH（当前支持 amd64/arm64）" >&2
	exit 1
	;;
esac

case "$OS" in
darwin | linux) BIN_NAME="mem-$OS-$ARCH" ;;
mingw* | msys* | cygwin*) OS="windows"; BIN_NAME="mem-windows-amd64.exe" ;;
*)
	echo "不支持的平台: $OS" >&2
	exit 1
	;;
esac

install_from_source() {
	if ! command -v go >/dev/null 2>&1; then
		echo "✗ 需要 Go 工具链才能源码安装；请先安装 Go（https://go.dev/dl/），或检查网络后重试预编译下载。" >&2
		return 1
	fi
	echo "→ 回退源码构建（CGO_ENABLED=0）..."
	mkdir -p "$INSTALL_DIR"
	CGO_ENABLED=0 go build -o "$INSTALL_DIR/mem" ./cmd/mem
}

# ---------- 下载预编译二进制 ----------
if [[ "$FROM_SOURCE" -eq 0 && "$OS" != "windows" ]]; then
	URL="$BASE/$VERSION/$BIN_NAME"
	echo "→ 下载 $URL"
	if curl -fsSL --connect-timeout 15 "$URL" -o "$INSTALL_DIR/mem.tmp"; then
		mv "$INSTALL_DIR/mem.tmp" "$INSTALL_DIR/mem"
		chmod +x "$INSTALL_DIR/mem"
	else
		rm -f "$INSTALL_DIR/mem.tmp"
		echo "✗ 预编译下载失败（版本 $VERSION 可能还没有 $OS/$ARCH 产物）。" >&2
		install_from_source
	fi
else
	install_from_source
fi

# ---------- 收尾 ----------
case ":$PATH:" in
*":$INSTALL_DIR:"*) ;;
*)
	echo ""
	echo "提示：$INSTALL_DIR 不在 PATH 中，可执行："
	echo "  echo 'export PATH=\"$INSTALL_DIR:\$PATH\"' >> ~/.zshrc && source ~/.zshrc"
	;;
esac

echo ""
echo "✓ 安装完成：$INSTALL_DIR/mem"
echo "  运行 mem stats 验证，或 mem init 接入你本机的 AI 工具。"
