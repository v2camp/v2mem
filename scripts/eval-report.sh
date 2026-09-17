#!/usr/bin/env bash
set -euo pipefail
# eval-report.sh —— 生成发布评测报告 .mem/reports/<tag>.md。
#
# 用法: eval-report.sh <tag> <sha> <coverage> <recall5> <goldhash>
#   头部含 tag / commit / coverage / recall5 / gold 哈希，便于回溯「这个版本凭什么发」。
if [ $# -ne 5 ]; then
  echo "用法: eval-report.sh <tag> <sha> <coverage> <recall5> <goldhash>" >&2
  exit 2
fi
TAG="$1"; SHA="$2"; COV="$3"; R5="$4"; GH="$5"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$ROOT/.mem/reports"
cat >"$ROOT/.mem/reports/$TAG.md" <<EOF
# v2mem eval 报告 $TAG

- tag: $TAG
- commit: $SHA
- coverage: $COV
- recall5: $R5
- gold_hash: $GH
- date: $(date -u +%Y-%m-%d)
EOF
echo "已生成 $ROOT/.mem/reports/$TAG.md"