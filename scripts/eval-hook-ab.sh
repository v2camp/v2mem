#!/usr/bin/env bash
set -uo pipefail
# eval-hook-ab.sh —— 钩子注入路径的 ON/OFF 双盲评测。
#
# 为什么需要它：现有门禁（make eval-gate → scripts/release-eval.sh）只跑
# `mem eval recall`（search 路径），完全不跑 `mem hook`（注入路径）。而生产环境里
# 用户看到的记忆是**注入**出来的：检索对了、渲染/预算/质量门把东西挤掉了，
# 现有门禁一点都看不见。本脚本补的就是这一段。
#
# 两臂 + 一个只报告的 PROD 臂（同一个临时库、同一批 query、同样 k=5）：
#   OFF   `mem search --db <tmp> --limit 5 --scope all --json`，逐条 query 取回的记忆 id。
#         这就是「只做检索、不做注入」，也就是现有 eval-gate 量的那条路径。
#   ON    每条 query 用**同一个 session_id** 依次跑两个事件，命中取两事件 hashes 的并集：
#           ① `mem hook --event session-start`   —— 开场注入（硬规则 / 全局 / 本工程三段）
#           ② `mem hook --event prompt-submit`   —— 提问注入（真实钩子）
#         并集 = 提问那一刻会话里**累计**的记忆，这才是模型真正看得见的东西。
#         为什么必须跑开场：开场已经注入过的规则，prompt-submit 会有意不再重复注入
#         （notes 去重，见 cmd/mem/hook.go 的 filterRulesCoveredByNotes）—— 那是设计行为。
#         只跑 prompt-submit 会把「开场已给过」记成「注入丢失」，纯属评测构造出来的伪丢失。
#         两个事件的贡献**分开报告**（开场贡献 / 提问贡献 / 并集）：并集只说明「有没有」，
#         分开才知道是哪一段给的；否则以后再出现丢失，仍然定位不到阶段。
#         注入 id 一律从审计读（audit.jsonl 的 hashes = 实际注入的记忆 id，见
#         cmd/mem/hook.go 的审计写入），不解析渲染文本 —— 文本是给人看的、没有 id。
#         这样 ON 臂量的是生产真实路径，不是对它的模拟。
#   PROD  同 ON 臂但 --limit 3（生产默认值），只报告不判级，用来看「按生产配置实际注入几条」。
#
# 为什么要自己建临时库：`mem eval recall --sandbox` 内部 os.MkdirTemp 建库、灌 seed、
# 跑完即删，库路径不对外暴露（cmd/mem/eval_sandbox.go 的 runSandboxRecall），两臂没法
# 共用它。本脚本用 `mem add` 把 seed.jsonl 灌进自己的临时库，OFF 与 ON 都打这个库。
#
# 为什么两臂的 recall 必须由同一个函数算（count_hits）：命中判定是「gold 前缀 vs 实际
# hash 前缀」。若一边用 Go 侧 eval.Score、另一边用 shell 算，差值里就混进了打分口径的
# 区别，没法解释。同一函数 + 同一批 query + 同一库 ⇒ parity 只反映「注入这一段」。
#
# 为什么 ON 臂必须显式给 --limit 5：生产默认是 3，而 OFF 臂是 k=5。
# 不给 5 就是拿 top3 和 top5 比，差值里混进了「条数」这个变量，不再是注入保真度。
#
# 另一个只打印的参照值：`mem eval recall --sandbox eval/sandbox`（7 条金标准、发布门禁
# 口径）。它的分母不是这批夹具、分数也不是 count_hits 算的，所以**不参与 parity**，
# 只用来让人看见「现有门禁眼里的世界」长什么样。
#
# 判据（eval-policy.txt 的 hook_recall5_min，只被本脚本读取）：
#   为什么用绝对值而不是「和 OFF 臂比」：夹具是合成的小样本，parity 受供给侧判定
#   （gate/质量门/去重/预算）影响，是个诊断量而不是质量结论。差值仍然打印，因为它是
#   本评测最想让人看见的那个数：负值越大，说明「检索找到了、注入丢了」越严重。
#
# 退出码: 0=达标  1=未达标或执行失败  2=参数/环境错
#
# 环境变量:
#   MEM_BIN         跑评测的二进制（默认 <仓库>/bin/mem；不存在则现场 go build）
#   HOOK_AB_BUDGET  默认预算（默认 1200，与 cmd/mem/hook.go 的 defaultHookBudget 一致）
#
# 本脚本只碰临时库（mktemp -d）。夹具是合成的：它能回答「注入有没有把检索找到的
# 东西弄丢」，**不能**回答「检索质量好不好」—— 那是 L2 金标准与真实会话样本的事。

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
FIXTURE="$ROOT/eval/sandbox/hook-prompts.jsonl"
SANDBOX="$ROOT/eval/sandbox"
POLICY="$ROOT/eval-policy.txt"
K=5
ON_LIMIT=5        # 与 OFF 臂的 k 对齐，见上方注释
PROD_LIMIT=3      # cmd/mem/hook.go 的 defaultHookLimit
BUDGET="${HOOK_AB_BUDGET:-1200}"
VERBOSE=0

while [ $# -gt 0 ]; do
  case "$1" in
    --budget) BUDGET="${2:-}"; shift 2 || { echo "RED: --budget 需要数值" >&2; exit 2; } ;;
    --verbose) VERBOSE=1; shift ;;
    -h|--help) sed -n '2,54p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "RED: 未知参数: $1（可用 --budget N / --verbose）" >&2; exit 2 ;;
  esac
done

case "$BUDGET" in
  ''|*[!0-9]*) echo "RED: --budget 必须是正整数，收到: $BUDGET" >&2; exit 2 ;;
esac
[ "$BUDGET" -gt 0 ] || { echo "RED: --budget 必须 > 0" >&2; exit 2; }

command -v jq >/dev/null 2>&1 || { echo "RED: 缺少 jq（本脚本用它解析审计与夹具）" >&2; exit 2; }
[ -f "$FIXTURE" ] || { echo "RED: 缺少夹具 $FIXTURE" >&2; exit 2; }
[ -f "$POLICY" ] || { echo "RED: 缺少 $POLICY" >&2; exit 2; }

# 门槛键只被本脚本读取：发布门禁（make eval-gate）仍只看 recall5_min。
MIN="$(awk -F= '$1=="hook_recall5_min"{print $2}' "$POLICY" | head -1 | tr -d '[:space:]')"
case "$MIN" in
  ''|*[!0-9.]*) echo "RED: $POLICY 里没有合法 hook_recall5_min（当前: ${MIN:-空}）" >&2; exit 2 ;;
esac

# ---------- 二进制 ----------
# 取法与 scripts/release-eval.sh 对齐：优先 <仓库>/bin/mem，不存在则现场构建。
# 找不到就报错退出，绝不静默回落到 PATH 上某个来路不明的 mem（版本不对会让数字失真）。
#
# 新鲜度：只在「不存在」时构建是不够的 —— bin/mem 存在但比源码旧时，会把旧二进制的
# 行为当成本次改动的事实，这是本仓库反复踩过的坑（构建产物不进 git，看不出过期）。
# 所以默认路径上再比一次时间：任一 *.go / go.mod 比它新就重建。
# MEM_BIN 由用户显式指定时不重建（尊重显式意图），但打印说明，免得有人以为校验过。
DEFAULT_MEM_BIN="$ROOT/bin/mem"
USER_MEM_BIN="${MEM_BIN:-}"
MEM_BIN="${USER_MEM_BIN:-$DEFAULT_MEM_BIN}"
if [ -z "$USER_MEM_BIN" ]; then
  STALE=""
  if [ -x "$MEM_BIN" ]; then
    # 剪掉 .git / bin / dist / temp：只看会真正影响二进制的源码，避免被产物目录带偏。
    STALE="$(find "$ROOT" \( -name .git -o -name bin -o -name dist -o -name temp \) -prune \
      -o -name '*.go' -newer "$MEM_BIN" -print -quit 2>/dev/null)"
    if [ -z "$STALE" ] && [ "$ROOT/go.mod" -nt "$MEM_BIN" ]; then STALE="$ROOT/go.mod"; fi
  fi
  if [ ! -x "$MEM_BIN" ] || [ -n "$STALE" ]; then
    if [ -x "$MEM_BIN" ]; then
      echo "→ $MEM_BIN 比源码旧（$STALE 更新），重建" >&2
    else
      echo "→ $MEM_BIN 不存在，现场构建" >&2
    fi
    mkdir -p "$(dirname "$MEM_BIN")"
    ( cd "$ROOT" && CGO_ENABLED=0 go build -o "$MEM_BIN" ./cmd/mem ) || {
      echo "RED: 构建 $MEM_BIN 失败" >&2; exit 2; }
  fi
else
  echo "→ 使用用户指定的 MEM_BIN=${MEM_BIN}（不校验新鲜度）" >&2
fi
[ -x "$MEM_BIN" ] || { echo "RED: MEM_BIN 不可执行: $MEM_BIN" >&2; exit 2; }

# ---------- 环境隔离 ----------
# 本机配置必须是「结果的外生变量」，不能是「结果的一部分」：
#   * MEM_* ：蒸馏/候选/notes 等开关会改变检索与注入内容；
#   * *_PROJECT_DIR：hook 的工程判定优先读这些变量，本机残留的 CLAUDE_PROJECT_DIR
#     会让 project 变成别的名字，夹具里 project=sandbox 的记忆会被当成跨工程而丢弃。
# MEM_BIN 已捕获到变量里，环境变量本身清掉无妨（子进程不需要它）。
for v in $(env | sed -n 's/^\(MEM_[A-Za-z0-9_]*\)=.*/\1/p'); do
  [ "$v" = "MEM_BIN" ] || unset "$v"
done
unset CLAUDE_PROJECT_DIR TRAE_PROJECT_DIR CODEBUDDY_PROJECT_DIR \
      QODER_PROJECT_DIR QODERCN_PROJECT_DIR CURSOR_PROJECT_DIR 2>/dev/null

TMPBASE="${TMPDIR:-/tmp}"; TMPBASE="${TMPBASE%/}"
TMPD="$(mktemp -d "$TMPBASE/v2mem-hook-ab.XXXXXX")" || { echo "RED: mktemp 失败" >&2; exit 2; }
trap 'rm -rf "$TMPD"' EXIT
DB="$TMPD/mem.db"
# 工程名取自目录名：hook 用 stdin 的 cwd 推断 project（detectProjectAt 取 basename），
# 夹具里所有记忆都是 project=sandbox，所以工作目录必须叫 sandbox。
WORK="$TMPD/sandbox"
mkdir -p "$WORK"

EXEC_ERR="$TMPD/exec-errors.txt"
: > "$EXEC_ERR"

say() { printf '%s\n' "$*"; }

# ---------- 1) 灌临时库 ----------
# 只灌 layer=memory 的行。观测行（layer=observation）跳过：CLI 没有写入观测的命令，
# 且 prompt-submit 只查 memory 层（store.SearchQuery.Layer 默认），对两条臂都没有影响。
#
# 每条之间隔 1 秒：ON 臂的开场段（session-start 的「本工程记忆」）按
# `salience DESC, last_seen_at DESC, updated_at DESC, id ASC` 排序（internal/store.List），
# 种子全是同一个 salience、又在同一秒内灌入时，排序会落到 `id ASC` —— 而 id 是
# crypto/rand 生成的（internal/store.newID），于是开场每次注入的 5 条都不一样，
# ON 臂的并集召回会随运行抖动（实测三次三套结果）。last_seen_at 是 INTEGER 秒，
# 隔 1 秒就能让它严格递增：开场集合因此唯一确定 = 最近灌入的 5 条，与生产里
# 「同 salience 时先给最近见过的」的语义也是一致的。
MEM_ROWS="$(jq -r 'select((.layer // "memory") == "memory") | .content' "$SANDBOX/seed.jsonl" \
  | wc -l | tr -d ' ')"
SEEDED=0
while IFS=$'\t' read -r kind proj src ch content; do
  [ -z "${content:-}" ] && continue
  [ "$SEEDED" -eq 0 ] || sleep 1
  # --no-audit 是硬要求：`mem add` 默认会把每条写入记进 ~/.v2mem/audit.jsonl，
  # 评测灌库不该在个人审计里留下 8 条合成样本（也不能碰用户的个人目录）。
  "$MEM_BIN" add --db "$DB" --no-audit --kind "$kind" --project "$proj" \
    --source "$src" --channel "$ch" --no-scrub "$content" >/dev/null 2>"$TMPD/seed.err" || {
    echo "RED: 灌库失败（${content}）" >&2; cat "$TMPD/seed.err" >&2; exit 2; }
  SEEDED=$((SEEDED + 1))
done < <(jq -r 'select((.layer // "memory") == "memory")
  | [ (.kind // "fact"), (.project // ""), (.source // "human"), (.channel // ""), .content ]
  | @tsv' "$SANDBOX/seed.jsonl")
[ "$SEEDED" -gt 0 ] || { echo "RED: seed.jsonl 没有灌进任何记忆" >&2; exit 2; }
[ "$SEEDED" -eq "$MEM_ROWS" ] || {
  echo "RED: 灌库条数($SEEDED) 与 seed.jsonl 的 memory 行数($MEM_ROWS) 不一致" >&2; exit 2; }

# id → content_hash 前 12 位。夹具的 gold 用 hash 前缀（与 recall.gold.jsonl 同口径），
# 而审计里的 hashes 是**记忆 id** —— 这层映射是 ON 臂能做哈希前缀匹配的唯一途径。
"$MEM_BIN" export --db "$DB" >"$TMPD/export.jsonl" 2>"$TMPD/export.err" || {
  echo "RED: mem export 失败" >&2; cat "$TMPD/export.err" >&2; exit 2; }
jq -r '[.id, (.content_hash[0:12])] | @tsv' "$TMPD/export.jsonl" >"$TMPD/id2hash.tsv"
# 不用 bash 关联数组：macOS 自带的是 bash 3.2，`declare -A` 不存在（实测报
# "invalid option"）。查表用 awk 扫小文件，条数是个位数到几十，代价可忽略。
h12_of() { awk -F'\t' -v id="$1" '$1==id {print $2; exit}' "$TMPD/id2hash.tsv"; }
MAPN="$(wc -l < "$TMPD/id2hash.tsv" | tr -d ' ')"
[ "$MAPN" -eq "$SEEDED" ] || {
  echo "RED: export 条数($MAPN) 与灌库条数($SEEDED) 不一致" >&2; exit 2; }

# ---------- 2) 夹具自检 ----------
# 三类问题必须在跑之前就拦住，否则会伪装成「注入丢了东西」：
#   * 坏 JSON / 缺 query；* gold 前缀在库里不存在（--gold 会静默剔除，分母被篡改）
CASES=0
while IFS= read -r line; do
  [ -z "$line" ] && continue
  echo "$line" | jq -e . >/dev/null 2>&1 || { echo "RED: 夹具不是合法 JSON: $line" >&2; exit 2; }
  q="$(echo "$line" | jq -r '.query // ""')"
  [ -n "$q" ] || { echo "RED: 夹具缺 query: $line" >&2; exit 2; }
  for g in $(echo "$line" | jq -r '(.gold // []) | .[]'); do
    hit="$(awk -F'\t' -v g="$g" 'index($2, g) == 1 {print 1; exit}' "$TMPD/id2hash.tsv")"
    [ "$hit" = 1 ] || { echo "RED: 夹具 gold 前缀不在库里: ${g}（query=${q}）" >&2; exit 2; }
  done
  CASES=$((CASES + 1))
done < "$FIXTURE"
[ "$CASES" -gt 0 ] || { echo "RED: 夹具为空" >&2; exit 2; }

DIST="$(jq -r '.case // "unknown"' "$FIXTURE" | sort | uniq -c | awk '{printf "%s=%s ", $2, $1}' | sed 's/ $//')"

# ---------- 3) 参照值：发布门禁口径 ----------
# `mem eval recall --sandbox` 自己建临时库（runSandboxRecall 里 os.MkdirTemp，跑完即删），
# 库路径不对外暴露，**没法**让 ON 臂复用它的库。所以它只用来打印一个「现有门禁看到的世界」，
# 不参与 parity，也不当判据 —— 它的分母是 recall.gold.jsonl 的 7 条，不是这批夹具。
OFF_JSON="$("$MEM_BIN" eval recall --sandbox "$SANDBOX" --k "$K" --json 2>"$TMPD/off.err")"
[ -n "$OFF_JSON" ] || { echo "RED: 沙箱 eval 失败（--sandbox ${SANDBOX}）" >&2; cat "$TMPD/off.err" >&2; exit 1; }
OFF_SANDBOX="$(echo "$OFF_JSON" | jq -r '.report.recall_at_k // empty')"
[ -n "$OFF_SANDBOX" ] || { echo "RED: 解析不到 eval recall 的 recall_at_k" >&2; echo "$OFF_JSON" >&2; exit 1; }
OFF_SANDBOX="$(awk -v v="$OFF_SANDBOX" 'BEGIN{printf "%.4f", v}')"

# ---------- 3b) 两臂共用的打分函数 ----------
# 命中判定与 internal/eval 同口径：gold 是 content_hash 前缀，命中即前缀匹配。
# **OFF 与 ON 必须用这一个函数**，否则差值里混进了「打分口径不同」，没法解释。
count_hits() { # $1=空格分隔 gold 前缀  $2=逗号分隔 实际 hash 前缀 → 打印命中条数
  local g ih n=0
  for g in $1; do
    for ih in ${2//,/ }; do
      case "$ih" in "$g"*) n=$((n + 1)); break ;; esac
    done
  done
  printf '%s' "$n"
}

# ---------- 3c) 审计记录 → hash 前缀 ----------
# 开场与提问各写一条审计记录，hashes 是记忆 id，这里统一换成 content_hash 前 12 位。
hashes_csv() { # $1=审计记录 JSON → 逗号分隔 hash 前缀
  local csv="" id h
  for id in $(printf '%s' "$1" | jq -r '(.hashes // []) | .[]'); do
    h="$(h12_of "$id")"
    [ -n "$h" ] || h="unknown"
    csv="${csv:+$csv,}$h"
  done
  printf '%s' "$csv"
}

# 两事件命中集合的并集（按 hash 前缀去重）：模拟「会话累计上下文」。
csv_union() { # $1,$2 逗号分隔 hash 前缀 → 去重后的并集
  local csv="" x
  for x in $(printf '%s,%s' "$1" "$2" | tr ',' ' '); do
    case ",$csv," in *",$x,"*) continue ;; esac
    csv="${csv:+$csv,}$x"
  done
  printf '%s' "$csv"
}

# ---------- 4) ON / PROD / OFF 臂 ----------
# 结果 TSV 的列（两臂同一格式，parity 只比 $7/$8）：
#   1 arm  2 idx  3 case  4 limit  5 max_chars  6 gold_n
#   7 并集命中数  8 并集 ratio  9 并集注入 hash 前缀
#  10 prompt-submit inject_bytes  11 status  12 query
#  13 开场命中数 14 开场 ratio  15 提问命中数 16 提问 ratio
#  17 开场 inject_bytes  18 开场注入 hash 前缀  19 提问注入 hash 前缀
# 13-19 是为「分开报告开场贡献 / 提问贡献」加的分阶段列：只给并集的话，
# 以后看不出并集里每一条是哪个事件给的，归因就断了。
#
# row_fail <TSV> <臂> <idx> <case> <limit> <max_chars> <gold_n> <status> <query>
# 失败行也写满 19 列：汇总/断言按列号取值，列数不一致会把后面的行读错位。
row_fail() {
  printf '%s\t%s\t%s\t%s\t%s\t%s\t0\t0\t-\t0\t%s\t%s\t0\t0\t0\t0\t0\t-\t-\n' \
    "$2" "$3" "$4" "$5" "$6" "$7" "$8" "$9" >>"$1"
}

# run_arm <臂名> <limit> <审计文件> <结果TSV>
# 每条 query 用**独立 session_id**：hook 的幂等去重键含（事件 + session_id + prompt + cwd），
# 同一 session 连跑 10 次会让后 9 次被判成重复触发、审计里只有 suppressed、没有 hashes。
# 选「独立 session」而不是 `--dedup-window 0`，是为了让 ON 臂保留生产的去重逻辑。
# 同一 session 内开场只跑一次（幂等去重会挡住第二次），这也正是真实会话的形状。
run_arm() {
  local arm="$1" limit="$2" audit="$3" out="$4"
  : > "$audit"; : > "$out"
  local idx=0 line q ck mc golds gold_n sid pl_start pl_prompt rec st_rec
  local st_csv ps_csv un_csv st_hits ps_hits hits st_ratio ps_ratio ratio st_bytes bytes
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    idx=$((idx + 1))
    q="$(echo "$line" | jq -r '.query')"
    ck="$(echo "$line" | jq -r '.case // "unknown"')"
    mc="$(echo "$line" | jq -r --argjson d "$BUDGET" '.max_chars // $d')"
    golds="$(echo "$line" | jq -r '(.gold // []) | .[]')"
    gold_n="$(echo "$line" | jq -r '(.gold // []) | length')"
    sid="ab-${arm}-${idx}"

    # (a) 开场：session-start 没有 prompt，只有 session_id + cwd
    pl_start="$(jq -nc --arg s "$sid" --arg c "$WORK" '{session_id:$s, cwd:$c}')"
    if ! printf '%s' "$pl_start" | "$MEM_BIN" hook --event session-start \
        --db "$DB" --audit "$audit" --limit "$limit" --max-chars "$mc" \
        >"$TMPD/stdout.start" 2>"$TMPD/stderr.start"; then
      # 钩子的设计红线是「绝不阻断宿主、异常也返回 0」。非零退出说明二进制或参数
      # 本身有问题（不是被评测的行为），必须单独记下来，不能算成「注入丢失」。
      printf '%s\t%s\t%s\n' "$arm" "$idx" "$q" >>"$EXEC_ERR"
      row_fail "$out" "$arm" "$idx" "$ck" "$limit" "$mc" "$gold_n" "hook_exit_nonzero_start" "$q"
      continue
    fi
    st_rec="$(jq -c --arg s "$sid" 'select(.session_id == $s and .event == "session-start" and ((.suppressed // false) | not))' "$audit" | tail -1)"
    if [ -z "$st_rec" ]; then
      printf '%s\t%s\t%s\n' "$arm" "$idx" "$q" >>"$EXEC_ERR"
      row_fail "$out" "$arm" "$idx" "$ck" "$limit" "$mc" "$gold_n" "audit_missing_start" "$q"
      continue
    fi
    st_bytes="$(printf '%s' "$st_rec" | jq -r '.inject_bytes // 0')"
    st_csv="$(hashes_csv "$st_rec")"

    # (b) 提问：真实钩子，同一 session_id
    pl_prompt="$(jq -nc --arg p "$q" --arg s "$sid" --arg c "$WORK" \
      '{prompt:$p, session_id:$s, cwd:$c}')"
    if ! printf '%s' "$pl_prompt" | "$MEM_BIN" hook --event prompt-submit \
        --db "$DB" --audit "$audit" --limit "$limit" --max-chars "$mc" \
        >"$TMPD/stdout" 2>"$TMPD/stderr"; then
      printf '%s\t%s\t%s\n' "$arm" "$idx" "$q" >>"$EXEC_ERR"
      row_fail "$out" "$arm" "$idx" "$ck" "$limit" "$mc" "$gold_n" "hook_exit_nonzero" "$q"
      continue
    fi
    rec="$(jq -c --arg s "$sid" 'select(.session_id == $s and .event == "prompt-submit" and ((.suppressed // false) | not))' "$audit" | tail -1)"
    if [ -z "$rec" ]; then
      printf '%s\t%s\t%s\n' "$arm" "$idx" "$q" >>"$EXEC_ERR"
      row_fail "$out" "$arm" "$idx" "$ck" "$limit" "$mc" "$gold_n" "audit_missing" "$q"
      continue
    fi
    bytes="$(printf '%s' "$rec" | jq -r '.inject_bytes // 0')"
    ps_csv="$(hashes_csv "$rec")"

    # (c) 打分：并集是 ON 臂的口径，开场/提问各自单算只用于归因。
    un_csv="$(csv_union "$st_csv" "$ps_csv")"
    hits="$(count_hits "$golds" "${un_csv:--}")"
    st_hits="$(count_hits "$golds" "${st_csv:--}")"
    ps_hits="$(count_hits "$golds" "${ps_csv:--}")"
    ratio="$(awk -v h="$hits" -v n="$gold_n" 'BEGIN{ if (n > 0) printf "%.4f", h/n; else printf "0" }')"
    st_ratio="$(awk -v h="$st_hits" -v n="$gold_n" 'BEGIN{ if (n > 0) printf "%.4f", h/n; else printf "0" }')"
    ps_ratio="$(awk -v h="$ps_hits" -v n="$gold_n" 'BEGIN{ if (n > 0) printf "%.4f", h/n; else printf "0" }')"

    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\tok\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$arm" "$idx" "$ck" "$limit" "$mc" "$gold_n" "$hits" "$ratio" "${un_csv:--}" "$bytes" "$q" \
      "$st_hits" "$st_ratio" "$ps_hits" "$ps_ratio" "$st_bytes" "${st_csv:--}" "${ps_csv:--}" >>"$out"
  done < "$FIXTURE"
}

# run_search_arm <结果TSV>
# OFF 臂（parity 的基线）：同一个临时库、同一批 query、同样 k=5，用 `mem search` 取 id。
# 这是「只做检索、不做注入」的意思 —— 也就是现有 eval-gate 量的那条路径。
# `--no-audit` 是硬要求：不加会把评测样本写进 ~/.v2mem/audit.jsonl，污染个人审计。
run_search_arm() {
  local out="$1"
  : > "$out"
  local idx=0 line q ck mc golds gold_n hits ratio ids inj_csv id h
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    idx=$((idx + 1))
    q="$(echo "$line" | jq -r '.query')"
    ck="$(echo "$line" | jq -r '.case // "unknown"')"
    mc="$(echo "$line" | jq -r --argjson d "$BUDGET" '.max_chars // $d')"
    golds="$(echo "$line" | jq -r '(.gold // []) | .[]')"
    gold_n="$(echo "$line" | jq -r '(.gold // []) | length')"

    ids="$("$MEM_BIN" search --db "$DB" --limit "$K" --scope all --json --no-audit "$q" 2>"$TMPD/search.err" \
      | jq -r 'if type == "array" then (.[] | .id) else empty end')"
    inj_csv=""
    for id in $ids; do
      h="$(h12_of "$id")"
      [ -n "$h" ] || h="unknown"
      inj_csv="${inj_csv:+$inj_csv,}$h"
    done
    hits="$(count_hits "$golds" "${inj_csv:--}")"
    ratio="$(awk -v h="$hits" -v n="$gold_n" 'BEGIN{ if (n > 0) printf "%.4f", h/n; else printf "0" }')"
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\tok\t%s\n' \
      "off" "$idx" "$ck" "$K" "$mc" "$gold_n" "$hits" "$ratio" "${inj_csv:--}" "0" "$q" >>"$out"
  done < "$FIXTURE"
}

AUDIT_ON="$TMPD/audit-on.jsonl"
AUDIT_PROD="$TMPD/audit-prod.jsonl"
ON_TSV="$TMPD/on.tsv"
PROD_TSV="$TMPD/prod.tsv"
OFF_TSV="$TMPD/off.tsv"
run_search_arm "$OFF_TSV"
run_arm on "$ON_LIMIT" "$AUDIT_ON" "$ON_TSV"
run_arm prod "$PROD_LIMIT" "$AUDIT_PROD" "$PROD_TSV"

# 工程判定守卫：project 不是 sandbox 说明环境里有残留变量，夹具的记忆会被当跨工程丢弃，
# 数字会全线归零且看不出去原因。
PJ="$(jq -r 'select(.project != null) | .project' "$AUDIT_ON" 2>/dev/null | head -1)"
if [ -n "$PJ" ] && [ "$PJ" != "sandbox" ]; then
  echo "RED: hook 判定的工程是 \"$PJ\" 而不是 sandbox —— 夹具 gold 会全部错配。" >&2
  echo "     通常是本机残留的 *_PROJECT_DIR / MEM_* 环境变量。" >&2
  exit 2
fi

# ---------- 5) 汇总 ----------
# 汇总用 awk：bash 没有浮点。分母是「gold 非空」的用例（与 --gold 的口径一致）。
# $2=ratio 列号：8=并集（ON 臂口径） 14=开场 16=提问。三者的分母相同、可逐项对照。
summarize() { # <tsv> [ratio 列号] → 打印 "recall n"
  awk -F'\t' -v c="${2:-8}" '$6 > 0 { s += $c; n++ } END { if (n > 0) printf "%.4f %d\n", s/n, n; else printf "0.0000 0\n" }' "$1"
}
read -r ON_RECALL ON_N <<<"$(summarize "$ON_TSV" 8)"
read -r OPEN_RECALL _ <<<"$(summarize "$ON_TSV" 14)"
read -r PS_RECALL _ <<<"$(summarize "$ON_TSV" 16)"
read -r PROD_RECALL PROD_N <<<"$(summarize "$PROD_TSV" 8)"
read -r OFF_RECALL OFF_N <<<"$(summarize "$OFF_TSV" 8)"

BYTES_MAX="$(awk -F'\t' '$10 > m { m = $10 } END { print m+0 }' "$ON_TSV")"
ST_BYTES_MAX="$(awk -F'\t' '$17 > m { m = $17 } END { print m+0 }' "$ON_TSV")"
PROD_BYTES_MAX="$(awk -F'\t' '$10 > m { m = $10 } END { print m+0 }' "$PROD_TSV")"

# 客套/无检索意图的判据落在 **prompt-submit** 上：开场段不经 query gate、按工程整段转储，
# 对任何提问（包括「谢谢」）都会注入 —— 那是会话基线，不是「往上下文里灌噪声」。
MISS_TOTAL="$(awk -F'\t' '$3 == "miss"' "$ON_TSV" | wc -l | tr -d ' ')"
MISS_OK="$(awk -F'\t' '$3 == "miss" && $19 == "-"' "$ON_TSV" | wc -l | tr -d ' ')"
MISS_OPEN="$(awk -F'\t' '$3 == "miss" && $18 != "-"' "$ON_TSV" | wc -l | tr -d ' ')"
# 预算压力测的是 prompt-submit 的按条裁剪，故看 $15（提问命中）而不是并集。
BUDGET_TOTAL="$(awk -F'\t' '$3 == "budget-stress"' "$ON_TSV" | wc -l | tr -d ' ')"
BUDGET_OK="$(awk -F'\t' '$3 == "budget-stress" && $15 >= 1' "$ON_TSV" | wc -l | tr -d ' ')"
# 开场真正补上的用例：并集有 gold，而单看提问没有。
OPEN_ONLY="$(awk -F'\t' '$6 > 0 && $13 > $15' "$ON_TSV" | wc -l | tr -d ' ')"
EXEC_ERRORS="$(wc -l < "$EXEC_ERR" | tr -d ' ')"

PARITY="$(awk -v a="$ON_RECALL" -v b="$OFF_RECALL" 'BEGIN{ printf "%+.1f", (a-b)*100 }')"
OPEN_GAIN="$(awk -v a="$ON_RECALL" -v b="$PS_RECALL" 'BEGIN{ printf "%+.1f", (a-b)*100 }')"

# ---------- 6) 输出 ----------
say "v2mem 钩子注入路径 ON/OFF 评测"
say "  夹具    ${FIXTURE}（$CASES 条：${DIST}）"
say "  临时库  $TMPD/mem.db（灌入 $SEEDED 条；不读 ~/.v2mem/mem.db）"
say "  OFF 臂   mem search --db <tmp> --limit $K --scope all    Recall@$K=${OFF_RECALL}（$OFF_N 条可评分；只检索不注入）"
say "  ON  臂   session-start → prompt-submit（同一 sid，命中取并集） --limit $ON_LIMIT --max-chars $BUDGET   Recall@$K=${ON_RECALL}（$ON_N 条可评分）"
say "             ├ 开场贡献 session-start   Recall@$K=${OPEN_RECALL}"
say "             └ 提问贡献 prompt-submit   Recall@$K=${PS_RECALL}（并集比只算提问 ${OPEN_GAIN}pt；$OPEN_ONLY 条用例是开场补上的）"
say "  PROD 臂  同上但 --limit ${PROD_LIMIT}（生产默认）              Recall@$PROD_LIMIT=${PROD_RECALL}（只报告，不判级）"
say "  参照值   mem eval recall --sandbox eval/sandbox（7 条金标准，发布门禁口径）  Recall@5=${OFF_SANDBOX}"
say "  注入体量  ON bytes_max: 开场=${ST_BYTES_MAX} prompt-submit=${BYTES_MAX}（审计 inject_bytes，字节不是 rune）；PROD bytes_max=$PROD_BYTES_MAX"
say "  ON − OFF（同一打分函数，仅 detail 不判级）: parity=$PARITY"
say ""
say "  结构断言  客套不注入（prompt-submit）$MISS_OK/${MISS_TOTAL}；预算压力下 prompt-submit 仍保住至少一条 gold $BUDGET_OK/$BUDGET_TOTAL"
if [ "$MISS_TOTAL" -gt 0 ]; then
  say "            （客套 case 的开场段 ${MISS_OPEN}/${MISS_TOTAL} 有注入：开场不经 query gate、按工程整段转储，属预期基线，不计入该断言）"
fi
say ""

# 注入阶段丢失：OFF 命中了、ON 少了 —— 这才是「注入把检索找到的东西弄丢」的直接证据。
# 两臂同一打分函数、同一批 query、同一个库，所以逐条可比。ON 取的是并集；括号里是
# 分阶段命中数，用来判断丢在开场还是丢在提问。
INJ_LOST="$(awk -F'\t' '
  NR == FNR { oh[$2] = $7; og[$2] = $6; ock[$2] = $3; oq[$2] = $12; next }
  { if (og[$2] + 0 > 0 && oh[$2] + 0 > $7 + 0) {
      printf "  · #%s [%s] OFF %s/%s → ON(并集) %s/%s（开场 %s、提问 %s）    query: %s\n", $2, ock[$2], oh[$2], og[$2], $7, $6, $13, $15, oq[$2]
      n++
    } }
  END { exit !(n > 0) }' "$OFF_TSV" "$ON_TSV" 2>/dev/null)"
if [ -n "$INJ_LOST" ]; then
  say "  注入阶段丢失（OFF 命中、ON 少了）："
  printf '%s\n' "$INJ_LOST"
  say ""
fi

# 结构断言失败项（与上面的「注入阶段丢失」不重复：那里讲召回损失，这里讲行为违约）。
ASSERT_FAIL=0
while IFS=$'\t' read -r arm idx ck lim mc gn hits ratio inj bytes st q \
    st_hits st_ratio ps_hits ps_ratio st_bytes st_inj ps_inj; do
  [ -z "${ck:-}" ] && continue
  failed=0
  [ "$ck" = "miss" ] && [ "$ps_inj" != "-" ] && failed=1
  [ "$ck" = "budget-stress" ] && [ "$ps_hits" -lt 1 ] && failed=1
  [ "$st" != "ok" ] && failed=1
  [ "$failed" = 1 ] || continue
  ASSERT_FAIL=$((ASSERT_FAIL + 1))
  golds_desc="$(jq -r --arg q "$q" 'select(.query == $q) | (.gold // []) | join(",")' "$FIXTURE" | head -1)"
  printf '  ✗ ON #%s [%s] limit=%s budget=%s 期望 gold=[%s] 并集 %s/%s（开场 %s、提问 %s）注入=[%s] bytes=开场%s/提问%s 状态=%s\n    query: %s\n' \
    "$idx" "$ck" "$lim" "$mc" "${golds_desc:--}" "${hits:-0}" "${gn:-0}" "${st_hits:-0}" "${ps_hits:-0}" \
    "${inj}" "${st_bytes:-0}" "${bytes:-0}" "$st" "$q"
done < "$ON_TSV"
[ "$ASSERT_FAIL" -eq 0 ] || say ""

# --verbose 时逐条列出 ON 臂：并集与分阶段命中数并排，方便看「这条是谁给的」。
if [ "$VERBOSE" = 1 ]; then
  say "  ON 臂逐条（并集 = 开场 ∪ 提问；两个数是各自命中的 gold 条数，可能重叠）："
  awk -F'\t' '{ printf "    #%s [%s] 并集 %s/%s　开场 %s、提问 %s　注入=[%s] bytes=开场%s/提问%s\n", \
    $2, $3, $7, $6, $13, $15, $9, $17, $10 }' "$ON_TSV"
  say ""
fi

# PROD 臂只报告不判级；--verbose 时逐条列出，方便对照「生产 3 条上限少放了什么」。
if [ "$VERBOSE" = 1 ]; then
  say "  PROD 臂逐条（--limit ${PROD_LIMIT}，仅供参考）："
  awk -F'\t' '{ printf "    #%s [%s] 并集 %s/%s（开场 %s、提问 %s）注入=[%s] bytes=开场%s/提问%s\n", \
    $2, $3, $7, $6, $13, $15, $9, $17, $10 }' "$PROD_TSV"
  say ""
fi

if [ "$EXEC_ERRORS" -gt 0 ]; then
  say "执行失败（不是被评测的行为，先修环境）："
  while IFS=$'\t' read -r a i q; do say "  ✗ $a #$i ${q}（详见上方 stderr 分支）"; done < "$EXEC_ERR"
fi

# 判级：绝对门槛 + 结构断言。parity 不参与 —— 见文件头的理由。
VERDICT_OK=1
awk -v r="$ON_RECALL" -v m="$MIN" 'BEGIN{ exit !(r >= m) }' || VERDICT_OK=0
[ "$MISS_OK" = "$MISS_TOTAL" ] || VERDICT_OK=0
[ "$BUDGET_OK" = "$BUDGET_TOTAL" ] || VERDICT_OK=0
[ "$EXEC_ERRORS" -eq 0 ] || VERDICT_OK=0

if [ "$VERDICT_OK" = 1 ]; then
  say "GREEN: 注入 Recall@$K=$ON_RECALL ≥ ${MIN}（hook_recall5_min）  n=$ON_N  parity=${PARITY}（开场 ${OPEN_RECALL} / 提问 ${PS_RECALL}）"
else
  # 逐条说清是哪一项没过：只说「RED」的话，下一次改动的人要自己再猜一遍。
  say "RED: 注入路径评测未达标  n=$ON_N  parity=${PARITY}（开场 ${OPEN_RECALL} / 提问 ${PS_RECALL}）" >&2
  awk -v r="$ON_RECALL" -v m="$MIN" 'BEGIN{ exit !(r >= m) }' \
    || echo "  Recall@$K=$ON_RECALL < 门槛 hook_recall5_min=$MIN" >&2
  [ "$MISS_OK" = "$MISS_TOTAL" ] || echo "  客套/无检索意图的 query 被 prompt-submit 注入了内容：$MISS_OK/${MISS_TOTAL}（应为 $MISS_TOTAL/${MISS_TOTAL}）" >&2
  [ "$BUDGET_OK" = "$BUDGET_TOTAL" ] || echo "  预算不足时 prompt-submit 整块弃答：$BUDGET_OK/${BUDGET_TOTAL}（应为 $BUDGET_TOTAL/${BUDGET_TOTAL}）" >&2
  [ "$EXEC_ERRORS" -eq 0 ] || echo "  执行失败 $EXEC_ERRORS 条（不是被评测的行为，见上方明细）" >&2
fi

say ""
say "  注：$CASES 条合成夹具只回答「注入这一步有没有把检索已找到的东西弄丢」，"
say "      不是检索质量结论，也不能替代 L2 的真实会话金标准。"
# 机器可读摘要（单行，便于脚本/CI 抓取）。on_recall5 是并集口径：
# 看「有没有」用它，看「是谁给的」用 open/ps 两个分阶段值。
say "HOOK_AB on_recall5=$ON_RECALL open_recall5=$OPEN_RECALL ps_recall5=$PS_RECALL open_gain=${OPEN_GAIN}pt open_only_cases=$OPEN_ONLY off_search5=$OFF_RECALL off_sandbox5=$OFF_SANDBOX parity=${PARITY}pt n=$ON_N cases=$CASES budget=$BUDGET bytes_max=$BYTES_MAX st_bytes_max=$ST_BYTES_MAX prod_recall3=$PROD_RECALL miss_abstain=$MISS_OK/$MISS_TOTAL budget_keep=$BUDGET_OK/$BUDGET_TOTAL exec_errors=$EXEC_ERRORS"

[ "$VERDICT_OK" = 1 ] || exit 1
exit 0
