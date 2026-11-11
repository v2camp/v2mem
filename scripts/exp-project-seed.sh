#!/usr/bin/env bash
set -uo pipefail
# exp-project-seed.sh —— 「工程记忆覆盖度」的前后测（C 臂 / T 臂）。
#
# 要回答的问题：某个工程检索命中率为 0，是因为**检索实现**不行，还是因为该工程
# 在库里一条记忆都没有？做法是把生产库拍成两份快照，只给 T 臂灌入该工程的结论型
# 种子，然后用**真实钩子路径**把该工程的历史 query 逐条重放，比较注入率。
#
# 为什么不用 `mem search` 而是 `mem hook`：用户看到的记忆是注入出来的，检索对了
# 但被质量门/预算/去重挡掉，`mem search` 看不见（同 scripts/eval-hook-ab.sh 的理由）。
# 这里两臂都走 `hook --event prompt-submit`，唯一变量是「库里有没有这个工程的记忆」。
#
# 为什么必须两种查询都拍快照、且只动副本：
#   * 生产库 ~/.v2mem/mem.db 与 ~/.v2mem/audit.jsonl 全程只读（.backup 走 SQLite
#     备份 API，不碰 -wal 一致性）；
#   * 两臂各自独立 audit 文件，灌库用 `mem add --no-audit`，避免合成样本污染个人审计。
#
# 为什么 query 取自 read_traces 而不是 audit.jsonl：read_traces 是检索层逐次记录的
# 结果（含 gate_reason），能精确排除 `gate_reason=session_start` 那批——那是开场类码表
# 的检索，不是用户提问，混进分母会把「工程覆盖」问题稀释成「开场也空手」。
#
# 为什么两臂要 --dedup-window 0：同一 session_id 连续触发会被去重抑制，而重放时每条
# query 都是独立样本，让去重生效等于人为制造「注入丢失」。
#
# 输出（默认 $TMPD，--out 可指定）：
#   queries.tsv   重放用的 query 全集（去重、保留原文）
#   c.tsv / t.tsv 逐条结果：序号、empty、inject_bytes、hashes
#   seeds.tsv     T 臂种子 id → 前 40 字，用于归因「哪条种子被注入」
#
# 用法：
#   ./scripts/exp-project-seed.sh                      # 默认 training-products
#   ./scripts/exp-project-seed.sh --project P --seed FILE --dir DIR
#
# 退出码：0=两臂都跑完  2=参数/环境错  3=跑数过程中有 hook 失败

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MEM_BIN="${MEM_BIN:-$ROOT/bin/mem}"
PROJECT="training-products"
SEED=""
PROJECT_DIR=""
OUT=""
LIMIT=3
MAX_CHARS=1200
DB="${MEM_DB:-$HOME/.v2mem/mem.db}"
QUERIES_FILE=""

while [ $# -gt 0 ]; do
  case "$1" in
    --project) PROJECT="${2:-}"; shift 2 ;;
    --seed) SEED="${2:-}"; shift 2 ;;
    --dir) PROJECT_DIR="${2:-}"; shift 2 ;;
    --out) OUT="${2:-}"; shift 2 ;;
    --queries) QUERIES_FILE="${2:-}"; shift 2 ;;
    --limit) LIMIT="${2:-3}"; shift 2 ;;
    --max-chars) MAX_CHARS="${2:-1200}"; shift 2 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

TMPD="${OUT:-$(mktemp -d)}"
mkdir -p "$TMPD"

[ -n "$SEED" ] || SEED="$ROOT/eval/exp/${PROJECT}.seed.jsonl"
# worktree 里通常没有 bin/mem（产物在主仓库），现场构建到临时目录，避免拿旧二进制量新口径。
if [ ! -x "$MEM_BIN" ]; then
  echo "→ $MEM_BIN 不可执行，现场构建" >&2
  ( cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMPD/mem" ./cmd/mem ) || {
    echo "RED: 构建失败" >&2; exit 2; }
  MEM_BIN="$TMPD/mem"
fi
[ -f "$SEED" ] || { echo "RED: 找不到种子文件 $SEED" >&2; exit 2; }
[ -f "$DB" ] || { echo "RED: 找不到库 $DB" >&2; exit 2; }
command -v jq >/dev/null || { echo "RED: 需要 jq" >&2; exit 2; }
command -v sqlite3 >/dev/null || { echo "RED: 需要 sqlite3" >&2; exit 2; }

# 工程目录：钩子按 cwd 推 project，目录名必须与 project 一致，否则 T 臂灌的种子
# 根本不在作用域里（scope=current 会把 cross 丢掉），实验会假阴性。
if [ -z "$PROJECT_DIR" ]; then
  for d in "$HOME/Code"/*; do
    [ -d "$d" ] || continue
    if [ "$(basename "$d")" = "$PROJECT" ]; then PROJECT_DIR="$d"; break; fi
  done
fi
[ -n "$PROJECT_DIR" ] && [ -d "$PROJECT_DIR" ] || {
  echo "RED: 找不到工程目录（用 --dir 指定）: $PROJECT" >&2; exit 2; }

# ---------- 1) 快照 ----------
# 为什么用 cp 而不是 `sqlite3 .backup`：库是 WAL 模式，`.backup` 在只读打开时产出的
# 单文件副本没有 -wal/-shm，后续 `sqlite3 -readonly` 打不开（实测 error 14）；
# 而 `mem add` 需要可写。cp 三件套（db/-wal/-shm）+ 首次打开时由 SQLite 自身做 WAL
# 恢复，是这里唯一既能读又能写的组合。副本仅用于实验，生产库全程只被读过一次。
cp "$DB" "$TMPD/base.db" || { echo "RED: 快照失败" >&2; exit 2; }
[ -f "$DB-wal" ] && cp "$DB-wal" "$TMPD/base.db-wal"
[ -f "$DB-shm" ] && cp "$DB-shm" "$TMPD/base.db-shm"
Q() { sqlite3 "$1" "$2"; }   # 副本一律可写打开：只读打开 WAL 库会 error 14
Q "$TMPD/base.db" "PRAGMA quick_check;" | grep -qx ok || { echo "RED: 快照完整性检查未通过" >&2; exit 2; }
# 先 checkpoint 把 -wal 折进主文件，两臂才可能是「同一份干净基线」的单文件副本；
# 否则 cp 只带主文件，最新提交还留在 -wal 里，两臂看到的库不一致。
Q "$TMPD/base.db" "PRAGMA wal_checkpoint(TRUNCATE);" >/dev/null
cp "$TMPD/base.db" "$TMPD/c.db"
cp "$TMPD/base.db" "$TMPD/t.db"
BASE_N="$(Q "$TMPD/base.db" "SELECT COUNT(*) FROM memories;")"

# ---------- 2) query 全集 ----------
# 一行一条 JSON：query 里有换行（多段 prompt 很常见），用 TSV 读会把一条拆成多条，
# 分母悄悄变大而两臂条数对不上（实测 77 条 vs 71 行）。JSONL 不受换行影响。
if [ -n "$QUERIES_FILE" ]; then
  cp "$QUERIES_FILE" "$TMPD/queries.jsonl"
else
python3 - "$TMPD/base.db" "$PROJECT" >"$TMPD/queries.jsonl" <<'PYEOF'
import json, sqlite3, sys
db, proj = sys.argv[1], sys.argv[2]
con = sqlite3.connect(db)
rows = con.execute(
    "SELECT DISTINCT raw_query FROM read_traces "
    "WHERE project = ? AND raw_query <> '' AND gate_reason <> 'session_start' "
    "ORDER BY created_at", (proj,))
for (q,) in rows:
    print(json.dumps({"q": q}, ensure_ascii=False))
PYEOF
fi
QN="$(wc -l <"$TMPD/queries.jsonl" | tr -d ' ')"
[ "$QN" -gt 0 ] || { echo "RED: 该工程没有可重放的 query" >&2; exit 2; }

# ---------- 3) T 臂灌种子 ----------
SEEDED=0
while IFS=$'\t' read -r kind src ch content; do
  [ -z "${content:-}" ] && continue
  [ "$SEEDED" -eq 0 ] || sleep 1   # last_seen_at 是秒级，隔 1 秒让排序确定（同 eval-hook-ab）
  "$MEM_BIN" add --db "$TMPD/t.db" --no-audit --kind "$kind" --project "$PROJECT" \
    --source "$src" --channel "$ch" --no-scrub "$content" >/dev/null 2>"$TMPD/seed.err" || {
      echo "RED: 灌库失败（${content:0:30}…）" >&2; cat "$TMPD/seed.err" >&2; exit 2; }
  SEEDED=$((SEEDED + 1))
done < <(jq -r 'select((.layer // "memory") == "memory")
  | [ (.kind // "fact"), (.source // "human"), (.channel // "import"), .content ] | @tsv' "$SEED")
[ "$SEEDED" -gt 0 ] || { echo "RED: 种子里没有 memory 行" >&2; exit 2; }
T_N="$(sqlite3 "$TMPD/t.db" "SELECT COUNT(*) FROM memories;")"
ADDED=$((T_N - BASE_N))
# 种子按内容去重（`mem add` 相同内容覆盖而不是新增）：部署后重跑同一份种子会得到 ADDED=0，
# 那不是失败，是「两臂已无差异」——实验退化成 C vs C，必须显式拒绝，免得给出 0 差值当结论。
[ "$ADDED" -gt 0 ] || {
  echo "RED: 该工程已包含这些种子（ADDED=0），两臂将无差异；换种子或先按文档回退" >&2; exit 2; }
[ "$ADDED" -eq "$SEEDED" ] ||   echo "WARN: 仅 ${ADDED}/${SEEDED} 条为新写入（其余已在库中），差值按 ${ADDED} 条计" >&2

"$MEM_BIN" export --db "$TMPD/t.db" 2>/dev/null | jq -r '[.id, (.content[0:40] | gsub("\n"; " "))] | @tsv' \
  >"$TMPD/seeds.tsv"

# ---------- 4) 逐条重放 ----------
# 环境变量必须清：宿主会把 CODEBUDDY_PROJECT_DIR / CLAUDE_PROJECT_DIR 指到自己工作区，
# 钩子优先读它们，不清就会把所有 query 都算到宿主工程名下（实测踩过：项目被记成 v2mem）。
run_arm() {
  local arm="$1" db="$2" audit="$TMPD/$1.audit.jsonl" i=0
  : >"$audit"
  : >"$TMPD/$arm.tsv"
  while IFS= read -r jline; do
    [ -z "$jline" ] && continue
    q="$(printf '%s' "$jline" | jq -r .q)"
    [ -z "$q" ] && continue
    i=$((i + 1))
    local payload
    payload="$(jq -nc --arg p "$q" --arg sid "exp-$arm-$i" --arg cwd "$PROJECT_DIR" \
      '{hook_event_name:"UserPromptSubmit", prompt:$p, session_id:$sid, cwd:$cwd}')"
    ( cd "$PROJECT_DIR" && printf '%s' "$payload" | \
      env -u CODEBUDDY_PROJECT_DIR -u CLAUDE_PROJECT_DIR -u CLAUDE_CODE_PROJECT_DIR \
        "$MEM_BIN" hook --event prompt-submit --db "$db" --audit "$audit" \
          --dedup-window 0 --limit "$LIMIT" --max-chars "$MAX_CHARS" --harness traecode \
          >/dev/null 2>>"$TMPD/$arm.err" ) || { echo "RED: $arm 第 $i 条 hook 失败" >&2; return 3; }
    local line
    line="$(tail -1 "$audit")"
    printf '%s\t%s\t%s\t%s\n' "$i" \
      "$(printf '%s' "$line" | jq -r 'if .empty then "true" else "false" end')" \
      "$(printf '%s' "$line" | jq -r '.inject_bytes // 0')" \
      "$(printf '%s' "$line" | jq -r '(.hashes // []) | join(",")')" >>"$TMPD/$arm.tsv"
  done <"$TMPD/queries.jsonl"
  return 0
}

run_arm c "$TMPD/c.db" || exit 3
run_arm t "$TMPD/t.db" || exit 3

# ---------- 5) 汇总 ----------
summary() {
  awk -F'\t' -v arm="$1" '
    { n++; if ($2=="false") inj++; b+=$3; if ($3>mx) mx=$3 }
    END { printf "%s\t%d\t%d\t%.1f\t%.1f\t%d\n", arm, n, inj, n?inj*100/n:0, n?b/n:0, mx }
  ' "$2"
}
{
  echo -e "arm\tn\tinjected\tinj_pct\tavg_bytes\tmax_bytes"
  summary C "$TMPD/c.tsv"
  summary T "$TMPD/t.tsv"
} | column -t -s $'\t'

echo
BASE_P="$(Q "$TMPD/base.db" "SELECT COUNT(*) FROM memories WHERE project='$PROJECT';")"
echo "库: 全库 ${BASE_N} 条，该工程 ${BASE_P} 条；T 臂 +${SEEDED} 条种子"
echo "query: ${QN} 条（project=${PROJECT}，已排除 session_start）"
echo "产物目录: $TMPD"
