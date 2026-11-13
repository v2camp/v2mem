#!/usr/bin/env bash
# gofmt 门禁：仓库内任何 .go 未格式化即非零退出。
#
# 为什么单独成脚本：Makefile 里内联 `test -z "$(gofmt -l .)"` 无法被测试，
# 而门禁本身失效过一次（16 个文件尾换行漂移进 main）。可测的最低单位是脚本。
#
# 用法：scripts/fmt-check.sh [目录，默认 .]
set -euo pipefail

dir="${1:-.}"

if ! command -v gofmt >/dev/null 2>&1; then
	echo "fmt-check: 找不到 gofmt（需要 Go 工具链在 PATH 里）" >&2
	exit 2
fi

out="$(gofmt -l "$dir")"
if [ -n "$out" ]; then
	echo "fmt-check: 以下文件未格式化，跑 gofmt -w 修正：" >&2
	echo "$out" >&2
	exit 1
fi

echo "fmt-check: OK（$dir 下所有 .go 已格式化）"
