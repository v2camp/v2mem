#!/usr/bin/env bash
set -euo pipefail
# 自检：eval-policy.txt 存在且含合法 recall5_min 键（发布门禁的单一真理源）。
# 无 Go 依赖，纯 shell 断言。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[ -f "$ROOT/eval-policy.txt" ] || { echo "missing eval-policy.txt" >&2; exit 1; }
grep -q '^recall5_min=0.60$' "$ROOT/eval-policy.txt" || { echo "bad recall5_min" >&2; exit 1; }
echo "eval-policy ok"