#!/usr/bin/env bash
set -euo pipefail
# eval-prod-gate.sh —— 低频（周级 / 灰度签字时）准出对账门禁，不进 12h 巡检。
#
# 包装 `mem eval flow --profile=prod --json`，汇总 docs/design-eval-flows.md §3.1：
#   G8  超 15 天 SLA 仍 pending 的悬空候选必须为 0
#   G9/G10/G11/G12  候选→裁决→记忆→终态对账（G12 准出率 100%）
#   manual_rate  人工介入率（诊断项，不判级）
#
# 只读边界：业务上纯读（cmd/mem/eval_flow.go:31-47、flow_snapshot.go:142-220、
# flow_check.go:69-138 全 SELECT），但 store.Open 有元数据副作用——schemaSQL、
# 每次覆盖 db_meta.written_by（writer_stamp.go:79-105）、ALTER/CREATE INDEX、
# backfillSigs 写 mem_sigs、review 首次补列回填（internal/store/store.go:82/93/98-169）。
# 因此先把 mem.db/-wal/-shm 复制到 mktemp -d 副本再跑，退出即清理；生产库、
# 审计日志、宿主配置全程不碰（acceptance-automation.md:3-4 既有做法）。
#
# 已知假红：candidates.run_id / evidence_bundles.last_run_id 在当前 main 恒空
# （internal/proposal/runner.go:226 赋值被 :239 覆盖），flow_check.go:471-474 的
# G7 对 fail/partial/backlog 窗口要求 LastRunID 非空，会假报「失败无运行账」；
# prod 13/13 只因 16 个 Bundle 全 noop 才未暴露。输出会标注该来源，不把 G7 红
# 直接判成真实故障。
#
# 用法: eval-prod-gate.sh [--db <path>] [--json] [--help]
# 环境: MEM_BIN 可指定 mem 二进制（默认 <仓库>/bin/mem，缺失则临时构建）
# 依赖: bash / jq / mktemp / cp / rm（临时构建时额外需要 go）
# 退出: 0=PASS  1=门禁失败  2=用法或环境错误

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  cat <<'EOF'
用法: eval-prod-gate.sh [--db <path>] [--json] [--help]

低频（周级 / 灰度签字时）准出对账门禁，不进 12h 巡检。
  --db <path>   库路径（默认 ~/.v2mem/mem.db）
  --json        输出机器可读摘要 JSON（stdout 仅一个 JSON 对象）
  --help        显示本帮助并退出 0

判据：G8/G9/G10/G11/G12 全 PASS、profile=prod 且 eval flow 退出 0 → PASS，否则 FAIL。
manual_rate 仅作诊断输出，不判级。
只读：mem.db/-wal/-shm 复制到 mktemp -d 副本后运行，退出即清理。
EOF
}

DB="$HOME/.v2mem/mem.db"
JSON_OUT=0
while [ $# -gt 0 ]; do
  case "$1" in
    --db)
      [ $# -ge 2 ] || { echo "RED: --db 缺参数" >&2; exit 2; }
      DB="$2"; shift 2 ;;
    --json) JSON_OUT=1; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "RED: 未知参数 $1（用 --help 看用法）" >&2; exit 2 ;;
  esac
done

command -v jq >/dev/null 2>&1 || { echo "RED: 缺少 jq" >&2; exit 2; }

# 相对路径按当前目录解析，且库文件必须已存在（避免空库被 prod 降级误判 PASS）
SRC_DIR="$(cd "$(dirname "$DB")" 2>/dev/null && pwd)" || {
  echo "RED: 库目录不存在: $(dirname "$DB")" >&2; exit 2; }
DB="$SRC_DIR/$(basename "$DB")"
[ -f "$DB" ] || { echo "RED: 库不存在: $DB" >&2; exit 2; }

TMPD="$(mktemp -d "${TMPDIR:-/tmp}/v2mem-prod-gate.XXXXXX")"
trap 'rm -rf "$TMPD"' EXIT

# 二进制：优先显式指定 / 已有产物，否则临时构建（不写仓库）
MEM_BIN="${MEM_BIN:-}"
if [ -z "$MEM_BIN" ]; then
  if [ -x "$ROOT/bin/mem" ]; then
    MEM_BIN="$ROOT/bin/mem"
  else
    ( cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMPD/mem" ./cmd/mem ) || {
      echo "RED: 构建 mem 失败" >&2; exit 2; }
    MEM_BIN="$TMPD/mem"
  fi
fi
[ -x "$MEM_BIN" ] || { echo "RED: MEM_BIN 不可执行: $MEM_BIN" >&2; exit 2; }

# ---------- 只读副本：mem.db + -wal + -shm ----------
COPY="$TMPD/mem.db"
cp -p "$DB" "$COPY" || { echo "RED: 复制库失败: $DB" >&2; exit 2; }
for s in -wal -shm; do
  if [ -f "$DB$s" ]; then cp -p "$DB$s" "$COPY$s"; fi
done

RAW="$TMPD/flow.json"
ERR="$TMPD/flow.err"
set +e
"$MEM_BIN" eval flow --db "$COPY" --profile=prod --json >"$RAW" 2>"$ERR"
EVAL_RC=$?
set -e

if ! jq -e 'type == "object" and (.checks | type == "array")' "$RAW" >/dev/null 2>&1; then
  echo "RED: eval flow 未产出合法 JSON（rc=$EVAL_RC）；stderr:" >&2
  cat "$ERR" >&2 || true
  exit 1
fi

# ---------- 汇总：G8/G9/G10/G11/G12 + manual_rate ----------
SUMMARY="$(jq -c --argjson rc "$EVAL_RC" --arg db "$DB" --arg raw "$RAW" '
  [ .checks[]
    | select(.id == "G8" or .id == "G9" or .id == "G10" or .id == "G11" or .id == "G12")
    | {id, pass, detail} ] as $gc
  | {
      result: (if ($gc | length) == 5 and ($gc | map(.pass) | all)
                  and .failed == 0 and .profile == "prod" then "PASS" else "FAIL" end),
      eval_exit_code: $rc,
      profile: .profile,
      passed: .passed,
      failed: .failed,
      gate_checks: $gc,
      gate_passed: ($gc | map(select(.pass)) | length),
      manual_rate: .manual_rate,
      legacy_exempt: .legacy_exempt,
      window_seconds: .window_seconds,
      g7_pass: ([ .checks[] | select(.id == "G7") | .pass ] | .[0] // null),
      known_false_red: {
        check: "G7",
        source: "candidates.run_id / evidence_bundles.last_run_id 在当前 main 恒空（internal/proposal/runner.go:226 赋值被 :239 覆盖）；flow_check.go:471-474 对 fail/partial/backlog 窗口要求 LastRunID 非空，会假报「失败无运行账」；prod 13/13 仅因 16 个 Bundle 全 noop 才未暴露",
        policy: "G7 红须先排除该假红，不直接判真实故障；G7 不在本门禁判据内"
      },
      db: $db,
      raw_json: $raw,
      note: "只读门禁：mem.db/-wal/-shm 复制到 mktemp 副本运行，退出即清理"
    }
' "$RAW")"

RESULT="$(printf '%s' "$SUMMARY" | jq -r '.result')"
MR_PCT="$(printf '%s' "$SUMMARY" | jq -r '.manual_rate' | awk '{printf "%.1f", $1 * 100}')"

if [ "$JSON_OUT" -eq 1 ]; then
  printf '%s\n' "$SUMMARY"
  if [ "$RESULT" = "FAIL" ] && [ -s "$ERR" ]; then
    cat "$ERR" >&2 || true
  fi
else
  printf '[prod-gate] %s  profile=%s  G8-G12=%s/5  passed=%s/%s  manual_rate=%s%%（诊断项，不判级）  legacy_exempt=%s  db=%s\n' \
    "$RESULT" \
    "$(printf '%s' "$SUMMARY" | jq -r '.profile')" \
    "$(printf '%s' "$SUMMARY" | jq -r '.gate_passed')" \
    "$(printf '%s' "$SUMMARY" | jq -r '.passed')" \
    "$(printf '%s' "$SUMMARY" | jq -r '.failed + .passed')" \
    "$MR_PCT" \
    "$(printf '%s' "$SUMMARY" | jq -r '.legacy_exempt')" \
    "$DB"
  printf '%s' "$SUMMARY" | jq -r '.gate_checks[] | "  \(.id)  \(if .pass then "PASS" else "FAIL" end)  \(.detail)"'
  G7_PASS="$(printf '%s' "$SUMMARY" | jq -r '.g7_pass // "n/a"')"
  if [ "$G7_PASS" = "false" ]; then
    printf '  WARN: G7=FAIL —— 先排除已知假红（run_id/last_run_id 恒空，internal/proposal/runner.go:226/239），不直接判真实故障\n'
  else
    printf '  NOTE: G7 已知假红来源已登记（run_id/last_run_id 恒空，internal/proposal/runner.go:226/239）；G7 红不直接判真实故障\n'
  fi
  printf '  raw_json: %s（临时副本，脚本退出即清理）\n' "$RAW"
  if [ "$RESULT" = "FAIL" ] && [ -s "$ERR" ]; then
    printf '  eval flow stderr:\n' >&2
    cat "$ERR" >&2 || true
  fi
fi

if [ "$RESULT" = "PASS" ]; then exit 0; fi
exit 1
