#!/usr/bin/env bash
set -euo pipefail
# derive-version.sh —— 按 conventional commits 推导下一个语义化版本号。
#
# 用法:
#   derive-version.sh --repo <git 目录> --last <tag> [--override major|minor|patch]
#
# 输出:
#   下一版号 vX.Y.Z 到 stdout。无 override 时按「自 --last 以来的最高档提交」推导：
#     - 含 `!`（feat!/fix!/BREAKING）→ major（破坏性变更）
#     - feat: / feat(type):       → minor
#     - 其余（fix/docs/chore/refactor/perf 等）→ patch
#   --override 显式指定升档，忽略提交历史。
#
# 档位优先级 major > minor > patch。同一提交同时触发多档时取最高档。

REPO="."
LAST=""
OVERRIDE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repo)    REPO="$2";    shift 2 ;;
    --last)    LAST="$2";    shift 2 ;;
    --override) OVERRIDE="$2"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done
[ -n "$LAST" ] || { echo "缺少 --last <tag>" >&2; exit 2; }

# 收集自上次 tag 以来的提交 subject（逐行）。仓库无 tag 范围或为空时拿不到提交，
# 统一回退 patch（至少让首个版本能从 v0.0.0/v0.1.0 起正常递增）。
LOGS="$(cd "$REPO" && git log --pretty=%s "$LAST"..HEAD 2>/dev/null || true)"

MAJOR=0; MINOR=0; PATCH=0
if [ -n "$LOGS" ]; then
  while IFS= read -r line; do
    case "$line" in
      *'!'*|BREAKING*) MAJOR=1 ;;   # ! 或 BREAKING 前缀 → major
      feat*)           MINOR=1 ;;
      fix*|docs*|chore*|refactor*|perf*|build*|ci*|test*) PATCH=1 ;;
    esac
  done <<< "$LOGS"
fi

if [ -n "$OVERRIDE" ]; then
  BUMP="$OVERRIDE"
elif [ "$MAJOR" = 1 ]; then
  BUMP=major
elif [ "$MINOR" = 1 ]; then
  BUMP=minor
else
  BUMP=patch
fi

# 解析 --last 的 vX.Y.Z（容忍无 v 前缀）。
base="${LAST#v}"
major="${base%%.*}"; rest="${base#*.}"; minor="${rest%%.*}"; patch="${rest##*.}"
major=$((10#${major:-0})); minor=$((10#${minor:-0})); patch=$((10#${patch:-0}))

case "$BUMP" in
  major) major=$((major+1)); minor=0; patch=0 ;;
  minor) minor=$((minor+1)); patch=0 ;;
  patch) patch=$((patch+1)) ;;
  *) echo "非法 --override: $BUMP（可选 major|minor|patch）" >&2; exit 2 ;;
esac

echo "v$major.$minor.$patch"