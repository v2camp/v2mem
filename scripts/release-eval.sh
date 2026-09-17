#!/usr/bin/env bash
set -euo pipefail
# release-eval.sh —— 发布评测与门禁判据。
#
# 职责（对应 make eval-gate / make release）：
#   1. 读门槛单一真理源 eval-policy.txt 的 recall5_min
#   2. 跑正式版 eval recall（gold 起点集，--gold-dir）取 Recall@5
#   3. 判据：recall < recall5_min → 打印 RED 并非零退出（CI 借此跳过打 tag）
#   4. 非 --gate-only 时推导版本 → 写 .mem/reports/v<tag>.md → 把 tag 打到 stdout
#
# 用法:
#   release-eval.sh [--override major|minor|patch] [--gate-only]
#
# 环境变量:
#   MEM_BIN   跑 eval 的二进制（默认 <仓库>/bin/mem；不存在则现场 go build）
#   GOLD_DIR  金标准目录（默认 <仓库>/gold）
#   REPO_DIR  git 仓库目录（默认 .）

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
GATE_ONLY=0
OVERRIDE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --gate-only) GATE_ONLY=1; shift ;;
    --override)  OVERRIDE="$2"; shift 2 ;;
    *) echo "未知参数: $1（可用 --gate-only / --override major|minor|patch）" >&2; exit 2 ;;
  esac
done

REPO_DIR="${REPO_DIR:-.}"
GOLD_DIR="${GOLD_DIR:-$ROOT/gold}"
MEM_BIN="${MEM_BIN:-$ROOT/bin/mem}"

# ---------- 1) 门槛（单一真理源） ----------
POLICY="$ROOT/eval-policy.txt"
[ -f "$POLICY" ] || { echo "RED: 缺少 $POLICY" >&2; exit 1; }
MIN="$(awk -F= '$1=="recall5_min"{print $2}' "$POLICY" | head -1 | tr -d '[:space:]')"
[ -n "$MIN" ] || { echo "RED: $POLICY 里没有合法 recall5_min" >&2; exit 1; }

# ---------- 2) 准备二进制 ----------
if [ ! -x "$MEM_BIN" ]; then
  (cd "$ROOT" && CGO_ENABLED=0 go build -o "$MEM_BIN" ./cmd/mem)
fi

# ---------- 3) 跑正式版 eval recall（JSON） ----------
TMPJSON="$(mktemp)"
trap 'rm -f "$TMPJSON"' EXIT
if ! "$MEM_BIN" eval recall --gold-dir "$GOLD_DIR" --k 5 --json >"$TMPJSON" 2>/dev/null; then
  echo "RED: eval 失败（--gold-dir $GOLD_DIR）" >&2
  cat "$TMPJSON" >&2 || true
  exit 1
fi
RECALL="$(grep -o '"recall_at_k":[0-9.]*' "$TMPJSON" | head -1 | cut -d: -f2)"
[ -n "$RECALL" ] || { echo "RED: 从 eval 输出解析不到 recall_at_k" >&2; cat "$TMPJSON" >&2; exit 1; }

# ---------- 4) 门禁判据 ----------
if awk -v r="$RECALL" -v m="$MIN" 'BEGIN{ exit !(r >= m) }'; then
  echo "GREEN: Recall@5=$RECALL  ≥  ${MIN}（门槛）"
else
  echo "RED: Recall@5=$RECALL  <  ${MIN}（eval-policy.txt 门槛）—— 不达标不发版" >&2
  exit 1
fi

if [ "$GATE_ONLY" = 1 ]; then
  exit 0
fi

# ---------- 5) 推导版本 + 写报告 + 输出 tag ----------
LAST="$(cd "$REPO_DIR" && git describe --tags --abbrev=0 2>/dev/null || echo v0.1.0)"
TAG="$(bash "$ROOT/scripts/derive-version.sh" --repo "$REPO_DIR" --last "$LAST" ${OVERRIDE:+--override "$OVERRIDE"})"
SHA="$(cd "$REPO_DIR" && git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
GOLDHASH="$(
  python3 - "$GOLD_DIR" <<'PY' 2>/dev/null || echo "n/a"
import glob, hashlib, sys
h = hashlib.sha256()
for f in sorted(glob.glob(sys.argv[1] + "/*.gold.jsonl")):
    h.update(open(f, "rb").read())
print(h.hexdigest())
PY
)"
COV="$(cd "$ROOT" && CGO_ENABLED=0 go test ./... -coverprofile=/tmp/v2mem-rpt-cov.out >/dev/null 2>&1 && go tool cover -func=/tmp/v2mem-rpt-cov.out 2>/dev/null | tail -1 | awk '{print $3}')"
rm -f /tmp/v2mem-rpt-cov.out
COV="${COV:-n/a}"

bash "$ROOT/scripts/eval-report.sh" "$TAG" "$SHA" "$COV" "$RECALL" "$GOLDHASH"
printf '%s\n' "$TAG"