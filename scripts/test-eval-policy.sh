#!/usr/bin/env bash
set -euo pipefail
# 自检：eval-policy.txt 存在且含合法 recall5_min 键（发布门禁的单一真理源）。
# 另守 hook_recall5_min（只被 scripts/eval-hook-ab.sh 读，不进发布门禁）与它依赖的夹具：
# 键被删/改名时评测会静默退回「无门槛」，所以在这里钉住。
# 无 Go 依赖，纯 shell 断言。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
[ -f "$ROOT/eval-policy.txt" ] || { echo "missing eval-policy.txt" >&2; exit 1; }
grep -q '^recall5_min=0.60$' "$ROOT/eval-policy.txt" || { echo "bad recall5_min" >&2; exit 1; }
grep -q '^hook_recall5_min=0\.60$' "$ROOT/eval-policy.txt" || { echo "bad hook_recall5_min" >&2; exit 1; }
[ -f "$ROOT/eval/sandbox/hook-prompts.jsonl" ] || { echo "missing eval/sandbox/hook-prompts.jsonl" >&2; exit 1; }
echo "eval-policy ok"
