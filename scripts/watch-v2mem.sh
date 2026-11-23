#!/usr/bin/env bash
# v2mem 运行观测巡检
#
# 定位：安装校验通过后，本脚本负责回答「稳态运行是否符合预期」。
# 判据的真理源是 docs/ops-watch.md，本文件是它的可执行形态——两者必须同步改。
#
# 用法:
#   scripts/watch-v2mem.sh                 # 默认 12h 窗口
#   scripts/watch-v2mem.sh --window 24h
#   scripts/watch-v2mem.sh --json          # 机器可读
#   scripts/watch-v2mem.sh --quiet         # 只输出非 PASS 项
#   scripts/watch-v2mem.sh --no-gc         # 不执行 R15 的候选过期，只判级
#
# 只读：不写库、不改配置。唯一的写入是 ~/.v2mem/watch-state.json 的基线快照。
# 唯一例外是 R15：候选超期时执行 `mem gc --candidates` 做 pending → expired 的状态迁移
# （行仍在库、不删 memories）；存在待删的已采纳行时转人工，不执行。`--no-gc` 可关掉。
# R3 只统计读侧 hook（prompt-submit / session-start）的 ms；写侧 stop / obs-append
# 共用同一个 Stop 计时器，计入会污染「检索延迟」，详见 docs/ops-watch.md §3。
# R10 给这部分写侧耗时独立判级（只取 stop，避免与 obs-append 双计）；R11 只读
# mem.db-wal 的文件大小，绝不修改或删除 -wal。R12 诊断写侧自动决策链路，只读
# audit.jsonl 与 mem eval activity --json，不调用 distill/accept/reject/gc。
# R13 取读侧审计 inject_bytes 的最大值（**字节**，不是 rune）：宿主对 hook stdout
# 有硬上限，越线会整块换占位且仍报成功，R2 的命中率看不出来。
# R14 只量 evidence 归档目录的落盘量（文件数 + 字节），不折算成观测条数：gc 之后
# 行数会降而文件数不降，两者本就会脱钩。归档的准出路径是 mem gc --obs → --evidence
# （后者只删「已无观测引用」的段），脚本侧仍只读不回收。
# R15 在候选超期时执行 pending → expired 的状态迁移（行仍在库，不删行）；--no-gc 可关。
# 判据定义以 docs/ops-watch.md §3 为准。
# 退出码: 0=无 FAIL  1=有 FAIL

set -uo pipefail

MEM_BIN="${MEM_BIN:-$(command -v mem || echo "$HOME/go/bin/mem")}"
V2MEM_DIR="${V2MEM_DIR:-$HOME/.v2mem}"
WB_DIR="${WORKBUDDY_CONFIG_DIR:-$HOME/.workbuddy}"
WINDOW="12h"
JSON_OUT=0
QUIET=0
GC_DISABLED=0

while [ $# -gt 0 ]; do
  case "$1" in
    --window) WINDOW="${2:-12h}"; shift 2 ;;
    --json)   JSON_OUT=1; shift ;;
    --quiet)  QUIET=1; shift ;;
    --no-gc)  GC_DISABLED=1; shift ;;
    -h|--help) sed -n '2,28p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "未知参数: $1" >&2; exit 2 ;;
  esac
done

window_sec() {
  case "$1" in
    *h) echo $(( ${1%h} * 3600 )) ;;
    *d) echo $(( ${1%d} * 86400 )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *)  echo "$1" ;;
  esac
}

NOW=$(date +%s)
CUTOFF=$(( NOW - $(window_sec "$WINDOW") ))

# ---------- 采集 ----------
# 结果项累积为 "ID|级别|标题|说明"
RESULTS=()
add() { RESULTS+=("$1|$2|$3|$4"); }

# I1 二进制
# STATS_RAW 留一份原始输出：R2 要按工程算库内条数，只解析一次，避免为每个
# prompt 再开一次库。PROJ_TABLE 是 "project=count" 的多行表，缺「按工程」行
# （旧版 stats 或 stub）时为空，R2 自动退回全局口径。
STATS_RAW=""
PROJ_TABLE=""
if "$MEM_BIN" stats >/dev/null 2>&1; then
  STATS_RAW=$("$MEM_BIN" stats 2>/dev/null)
  TOTAL=$(printf '%s\n' "$STATS_RAW" | awk '/^总条数/{print $2}')
  TOTAL=${TOTAL:-0}
  PROJ_TABLE=$(printf '%s\n' "$STATS_RAW" | awk '
    /^按工程/ { for (i = 2; i <= NF; i++) { if (split($i, a, "=") == 2) print a[1]"="a[2] } }')
  add I1 PASS "二进制可执行" "mem stats 正常，库内 ${TOTAL} 条"
else
  TOTAL=0
  add I1 FAIL "二进制不可执行" "$MEM_BIN stats 失败"
fi

# proj_total <project>：该工程库内记忆条数。未出现在「按工程」里就是 0 条。
proj_total() {
  local want="$1" n
  n=$(printf '%s\n' "$PROJ_TABLE" | awk -F= -v p="$want" '$1 == p { print $2; exit }')
  case "$n" in
    ''|*[!0-9]*) printf '0' ;;
    *)           printf '%s' "$n" ;;
  esac
}

# I2 hooks 四个事件
HOOK_EVENTS=0
if [ -f "$WB_DIR/settings.json" ]; then
  HOOK_EVENTS=$(jq -r '
    [.hooks // {} | to_entries[] | select(any(.value[]?.hooks[]?; (.command // "") | contains("mem hook"))) | .key] | unique | length
  ' "$WB_DIR/settings.json" 2>/dev/null || echo 0)
fi
case "$HOOK_EVENTS" in
  4) add I2 PASS "钩子事件齐全" "SessionStart/UserPromptSubmit/Stop/SessionEnd 全部指向 mem hook" ;;
  0) add I2 FAIL "钩子缺失" "$WB_DIR/settings.json 未找到 mem hook 条目" ;;
  *) add I2 WARN "钩子不全" "仅 ${HOOK_EVENTS}/4 个事件挂载" ;;
esac

# I3 MCP 批准记录
if jq -e 'to_entries[] | select(.key | endswith("::v2mem"))' "$WB_DIR/mcp-approvals.json" >/dev/null 2>&1; then
  add I3 PASS "MCP 已批准" "mcp-approvals.json 有 v2mem 记录"
else
  add I3 WARN "MCP 未批准" "连接器管理页未开启 v2mem 开关；仅影响结构化工具，不影响 hook 注入"
fi

# I4 MCP 进程
MCP_PROCS=$(pgrep -f "mem mcp" 2>/dev/null | wc -l | tr -d ' ')
if [ "$MCP_PROCS" -ge 1 ]; then
  add I4 PASS "MCP 进程存活" "${MCP_PROCS} 个 mem mcp 进程"
else
  add I4 WARN "MCP 进程未运行" "空闲时无进程属正常；若本次会话要用 MCP 工具则需重启会话"
fi

# I5 Trae 钩子执行环境：Sandbox 会"读得到、写不进"（审计/观测为 0）
TRAE_HOOKS="$HOME/.trae-cn/hooks.json"
if [ -f "$TRAE_HOOKS" ] && grep -q '# v2mem' "$TRAE_HOOKS" 2>/dev/null; then
  HOOK_LOG=""
  for d in "$HOME/Library/Application Support/Trae CN/logs" \
           "$HOME/Library/Application Support/Trae/logs" \
           "$HOME/.config/Trae CN/logs" \
           "$HOME/.config/Trae/logs"; do
    [ -d "$d" ] || continue
    f=$(ls -t "$d"/*/Modular/agent-hooks.log 2>/dev/null | head -1)
    if [ -n "$f" ]; then HOOK_LOG="$f"; break; fi
  done
  if [ -z "$HOOK_LOG" ]; then
    add I5 INFO "Trae 钩子尚未触发" "未找到 agent-hooks.log；面板启用并新建会话后再查"
  else
    HOOK_ENV=$(awk -v RS='--- Hook Execution Start ---' '
      {
        cmd=0; env=""
        n=split($0, a, "\n")
        for (i=1; i<=n; i++) {
          if (a[i] ~ /Command:/ && a[i] ~ /--harness traecode/) cmd=1
          if (cmd && a[i] ~ /ExecEnv:/) {
            e=a[i]; sub(/.*ExecEnv: /, "", e); gsub(/[^A-Za-z]/, "", e); env=e
          }
        }
      }
      END { print env }
    ' "$HOOK_LOG" 2>/dev/null)
    case "$HOOK_ENV" in
      Host)    add I5 PASS "Trae 钩子本地运行" "ExecEnv=Host，审计/观测可写" ;;
      Sandbox) add I5 WARN "Trae 钩子在沙箱运行" "读侧注入可用，但 ~/.v2mem 禁写；面板运行方式改「本地自动运行」" ;;
      *)       add I5 INFO "Trae 钩子环境未知" "日志未匹配到 traecode 执行块：${HOOK_LOG##*/}" ;;
    esac
  fi
else
  add I5 INFO "未接入 Trae" "$TRAE_HOOKS 无 v2mem 条目，跳过"
fi

# ---------- 运行态：解析 audit.jsonl ----------
AUDIT="$V2MEM_DIR/audit.jsonl"
A_EVENTS=0; A_PROMPT=0; A_PROMPT_EMPTY=0; A_SS=0; A_OBS=0; A_ADD=0; A_MS=0
A_READ_N=0; A_WRITE_MS=0; A_WRITE_N=0

if [ -f "$AUDIT" ]; then
  # 必须用 -s：默认流式模式下 [select(...)] 会对每一行各自收集，导致只有最后一行生效
  eval "$(jq -sr --argjson c "$CUTOFF" '
    map(select(.ts >= $c)) as $e
    | "A_EVENTS=\($e | length)",
      "A_PROMPT=\([$e[] | select(.event=="prompt-submit")] | length)",
      "A_PROMPT_EMPTY=\([$e[] | select(.event=="prompt-submit" and .empty==true)] | length)",
      "A_SS=\([$e[] | select(.event=="session-start")] | length)",
      "A_OBS=\([$e[] | select(.event=="obs-append")] | length)",
      "A_ADD=\([$e[] | select(.event=="manual-add" or .event=="write")] | length)",
      "A_READ_N=\([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms != null and .ms > 0)] | length)",
      "A_MS=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms != null and .ms > 0) | .ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_WRITE_N=\([$e[] | select(.event=="stop" and .ms != null and .ms > 0)] | length)",
      "A_WRITE_MS=\(([$e[] | select(.event=="stop" and .ms != null and .ms > 0) | .ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_SEG_HAS=\([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and (.claim_ms != null or .evidence_ms != null or .maint_ms != null))] | length)",
      "A_SEG_OPEN=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .open_ms != null) | .open_ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_SEG_SEARCH=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .search_ms != null) | .search_ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_SEG_SEARCH_N=\([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .search_ms != null)] | length)",
      "A_SEG_CLAIM=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .claim_ms != null) | .claim_ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_SEG_EVID=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .evidence_ms != null) | .evidence_ms] | if length>0 then (add/length|floor) else 0 end))",
      "A_SEG_MAINT=\(([$e[] | select((.event=="prompt-submit" or .event=="session-start") and .ms > 0 and .maint_ms != null) | .maint_ms] | if length>0 then (add/length|floor) else 0 end))"
  ' "$AUDIT" 2>/dev/null)"
fi
A_EVENTS=${A_EVENTS:-0}; A_PROMPT=${A_PROMPT:-0}; A_PROMPT_EMPTY=${A_PROMPT_EMPTY:-0}
A_SS=${A_SS:-0}; A_OBS=${A_OBS:-0}; A_ADD=${A_ADD:-0}; A_MS=${A_MS:-0}
A_READ_N=${A_READ_N:-0}; A_WRITE_MS=${A_WRITE_MS:-0}; A_WRITE_N=${A_WRITE_N:-0}
A_SEG_HAS=${A_SEG_HAS:-0}; A_SEG_OPEN=${A_SEG_OPEN:-0}; A_SEG_SEARCH=${A_SEG_SEARCH:-0}
A_SEG_CLAIM=${A_SEG_CLAIM:-0}; A_SEG_EVID=${A_SEG_EVID:-0}; A_SEG_MAINT=${A_SEG_MAINT:-0}
A_SEG_SEARCH_N=${A_SEG_SEARCH_N:-0}

# R12 写侧事件单独采集：不复用上面的 eval 字符串插值 —— 插值内部再嵌字符串字面量
# 会触发 jq INVALID_CHARACTER。这里输出 TSV 后由 read 拆列，表达式里没有插值。
A_DISTILL=0; A_DISTILL_PROPOSAL=0; A_DISTILL_FAIL=0
A_DISTILL_EMPTY=0; A_DECIDE_A=0; A_DECIDE_R=0
if [ -f "$AUDIT" ]; then
  W12=$(jq -sr --argjson c "$CUTOFF" '
    map(select(.ts >= $c)) as $e
    | [$e[] | select(.event=="distill")] as $d
    | [ ($d|length),
        ([$d[] | select(.source=="proposal")]|length),
        ([$d[] | (.failed // false) as $f | (.mode // "") as $m
                | select($f == true or ($m | test("^(fail|partial|invalid|backlog)$")))]|length),
        ([$d[] | (.mode // "") as $m | select($m | test("^(empty|noop)$"))]|length),
        ([$e[] | select(.event=="accept")]|length),
        ([$e[] | select(.event=="reject")]|length) ]
    | @tsv' "$AUDIT" 2>/dev/null)
  if [ -n "${W12:-}" ]; then
    IFS=$'\t' read -r A_DISTILL A_DISTILL_PROPOSAL A_DISTILL_FAIL A_DISTILL_EMPTY A_DECIDE_A A_DECIDE_R <<< "$W12"
  fi
fi
A_DISTILL=${A_DISTILL:-0}; A_DISTILL_PROPOSAL=${A_DISTILL_PROPOSAL:-0}
A_DISTILL_FAIL=${A_DISTILL_FAIL:-0}; A_DISTILL_EMPTY=${A_DISTILL_EMPTY:-0}
A_DECIDE_A=${A_DECIDE_A:-0}; A_DECIDE_R=${A_DECIDE_R:-0}

# R13 注入体量单独采集：同样用 TSV 后 read 拆列，不往上面的 eval 插值里再嵌字面量。
# 三列 = 最大值 / 带字段的样本数 / 读侧记录总数（不论有没有字段）。
# 字段缺失（升级前的旧记录）用 (.inject_bytes // null) != null 判定，不能当 0：
# 当 0 会拉低最大值、掩盖越线。A_INJ_READ 用来区分「窗口内真无读侧记录」与「全是旧记录」——
# 读侧事件有三个，只数 session-start / prompt-submit 会漏掉只有 pre-compact 的窗口。
A_INJ_MAX=0; A_INJ_N=0; A_INJ_READ=0
if [ -f "$AUDIT" ]; then
  W13=$(jq -sr --argjson c "$CUTOFF" '
    map(select(.ts >= $c)) as $e
    | [$e[] | select(.event=="session-start" or .event=="prompt-submit" or .event=="pre-compact")] as $r
    | [ ($r | map(select((.inject_bytes // null) != null) | .inject_bytes)
              | if length > 0 then max else 0 end),
        ($r | map(select((.inject_bytes // null) != null)) | length),
        ($r | length) ]
    | @tsv' "$AUDIT" 2>/dev/null)
  if [ -n "${W13:-}" ]; then
    IFS=$'\t' read -r A_INJ_MAX A_INJ_N A_INJ_READ <<< "$W13"
  fi
fi
A_INJ_MAX=${A_INJ_MAX:-0}; A_INJ_N=${A_INJ_N:-0}; A_INJ_READ=${A_INJ_READ:-0}

# R12 诊断文案：mem eval activity --json 只读 audit.jsonl（cmdEvalActivity 不开库）。
# 读不到或不可解析时只省略文案，R12 仍按上面的审计计数独立判级。
ACT_VERDICT=""
SINCE="$WINDOW"
case "$SINCE" in
  ''|*[!0-9]*) : ;;
  *) SINCE="${SINCE}s" ;;
esac
ACT_JSON=$( "$MEM_BIN" eval activity --since "$SINCE" --json 2>/dev/null )
if printf '%s' "$ACT_JSON" | jq -e 'type == "object"' >/dev/null 2>&1; then
  ACT_VERDICT=$(printf '%s' "$ACT_JSON" | jq -r '(.verdict // "") | gsub("[\"|]"; "/") | .[0:48]' 2>/dev/null)
fi
ACT_VERDICT=${ACT_VERDICT:-}

# R1 钩子是否被真实触发
if [ "$A_EVENTS" -ge 1 ]; then
  add R1 PASS "钩子已触发" "窗口内 ${A_EVENTS} 个事件（session-start ${A_SS} / prompt-submit ${A_PROMPT}）"
else
  add R1 FAIL "钩子零事件" "窗口内无审计记录；先查 settings.json 路径是否为 $WB_DIR"
fi

# R2 检索命中率（仅 prompt-submit 有意义；session-start 的 empty 恒 false）
#
# 口径按 project 分组，与 R8「跨工程不相加」同源：库是全局的、project 是列。
# 旧实现用全局 EMPTY/A_PROMPT 算，只要窗口内提问集中在「库内 0 条」的工程上，
# 就会得到稳定但无意义的 FAIL——空注入是冷启动的设计预期，不是检索失效。
# 这里把「该工程库内 <20 条」的样本单列为冷启动剔除，只在有料工程上判级。
# 剔除不等于注入为空：规则段与观测注入不看 memories 条数，所以冷启动 INFO 会把
# 实际的注入次数一并报出（见下方 R2_COLD_INJ），否则「确实注入了」会被读成
# 「没有可注入内容」，把检索失效藏在 INFO 背后。
PROMPT_BY_PROJ=""
if [ -f "$AUDIT" ]; then
  PROMPT_BY_PROJ=$(jq -sr --argjson c "$CUTOFF" '
    map(select(.ts >= $c and .event=="prompt-submit")) as $p
    | $p | group_by(.project // "")
    | map([(.[0].project // ""), (length), ([.[] | select(.empty==true)] | length)] | @tsv)
    | .[]' "$AUDIT" 2>/dev/null)
fi

if [ "$A_PROMPT" -gt 0 ]; then
  R2_N=0; R2_EMPTY=0; R2_COLD=0; R2_COLD_INJ=0; R2_COLD_PROJ=""
  if [ -n "$PROMPT_BY_PROJ" ]; then
    while IFS=$'\t' read -r pj tot emp; do
      case "${tot:-}${emp:-}" in ''|*[!0-9]*) continue ;; esac
      # 旧审计没有 project 字段：按全局口径判断，避免把「未知工程」一律当冷启动。
      if [ -z "$pj" ]; then pc=$TOTAL; else pc=$(proj_total "$pj"); fi
      if [ "$pc" -lt 20 ]; then
        R2_COLD=$((R2_COLD + tot))
        # 冷启动样本仍可能注入（规则段与观测不依赖 memories 条数），
        # 只报「空注入属设计预期」会把「确实注入了」读成「没有可注入内容」。
        R2_COLD_INJ=$((R2_COLD_INJ + tot - emp))
        R2_COLD_PROJ="$R2_COLD_PROJ${R2_COLD_PROJ:+, }${pj:-(未标注)}"
        continue
      fi
      R2_N=$((R2_N + tot)); R2_EMPTY=$((R2_EMPTY + emp))
    done <<< "$PROMPT_BY_PROJ"
  else
    # 分组拿不到（jq 失败 / 空审计）：退回全局口径，宁可旧行为也不静默判 PASS。
    R2_N=$A_PROMPT; R2_EMPTY=$A_PROMPT_EMPTY
  fi

  if [ "$R2_N" -eq 0 ]; then
    # 冷启动样本的注入次数要如实报出：库内 <20 条只说明「不按 30% 判级」，
    # 不说明「一定注入为空」。规则段 / 观测注入不受 memories 条数约束。
    if [ "$R2_COLD_INJ" -gt 0 ]; then
      R2_COLD_TAIL="其中 ${R2_COLD_INJ} 次仍注入（来自规则段/观测，非记忆库命中）、$((R2_COLD - R2_COLD_INJ)) 次为空；样本同源，不计命中率"
    else
      R2_COLD_TAIL="空注入属设计预期，不计命中率"
    fi
    add R2 INFO "窗口内提问全部来自冷启动工程" \
      "prompt-submit ${A_PROMPT} 次全落在库内 <20 条的工程（${R2_COLD_PROJ:-未标注}），${R2_COLD_TAIL}"
  else
    HIT_RATE=$(( (R2_N - R2_EMPTY) * 100 / R2_N ))
    COLD_NOTE=""
    if [ "$R2_COLD" -gt 0 ]; then
      COLD_NOTE="；已剔除冷启动工程 ${R2_COLD} 次（${R2_COLD_PROJ}）"
    fi
    if [ "$HIT_RATE" -ge 30 ]; then
      add R2 PASS "检索命中率 ${HIT_RATE}%" "有效样本 ${R2_N} 次，空 ${R2_EMPTY} 次${COLD_NOTE}"
    elif [ "$HIT_RATE" -ge 10 ]; then
      add R2 WARN "检索命中率偏低 ${HIT_RATE}%" "有效样本 ${R2_N} 次，空 ${R2_EMPTY} 次${COLD_NOTE}；该工程库内 ≥20 条仍大量空命中，考虑补记忆或查同义词表"
    else
      add R2 FAIL "检索基本不命中 ${HIT_RATE}%" "有效样本 ${R2_N} 次，空 ${R2_EMPTY} 次${COLD_NOTE}；检查中文分词与 FTS 索引"
    fi
  fi
else
  add R2 INFO "窗口内无提问" "窗口内没有 prompt-submit 事件，无法评估命中率"
fi

# R3 检索延迟：判 search_ms（检索+渲染），写侧 Stop 单独展示但不判级。
# 总 ms 含进程启动、开库、去重事务、证据落盘、维护，与「检索」不同名不同事——
# 用它判级会把宿主负载与 fsync 算成检索退化（旧口径的 FAIL 均由此产生），
# 故只作 detail 观察。没有 search_ms 的旧记录按无样本记 INFO，不当 0 判 PASS。
if [ "$A_SEG_SEARCH_N" -gt 0 ]; then
  R3_NOTE="带 search_ms 的读侧样本 ${A_SEG_SEARCH_N}/${A_READ_N} 次；总耗时均值 ${A_MS}ms（含启动/开库/证据/维护，不判级）"
  # 有分段字段的窗口才展开：旧记录没有这三段，硬算「其余」会把整段 ms 都算成空白
  # 变量名一律用花括号收口：macOS 自带 bash 3.2 在 set -u 下会把紧邻的
  # 全角分号首字节并进变量名（"$R3_NOTE；" → R3_NOTE\xEF unbound variable）。
  if [ "$A_SEG_HAS" -gt 0 ]; then
    R3_NOTE="${R3_NOTE}；其余分段均值 开库 ${A_SEG_OPEN} / 去重 ${A_SEG_CLAIM} / 证据 ${A_SEG_EVID} / 维护 ${A_SEG_MAINT} / 未归类 $((A_MS - A_SEG_OPEN - A_SEG_SEARCH - A_SEG_CLAIM - A_SEG_EVID - A_SEG_MAINT))（启动+stdout+审计）"
  fi
  R3_NOTE="${R3_NOTE}；写侧 Stop ${A_WRITE_MS}ms 由 R10 单独判级，不计入本判据"
  if [ "$A_SEG_SEARCH" -lt 50 ]; then
    add R3 PASS "检索延迟 ${A_SEG_SEARCH}ms" "低于 50ms 目标线；${R3_NOTE}"
  elif [ "$A_SEG_SEARCH" -lt 200 ]; then
    add R3 WARN "检索延迟 ${A_SEG_SEARCH}ms" "高于典型值 ~9ms，观察是否库膨胀或锁竞争（分因见 docs/ops-watch.md §5 R3）；${R3_NOTE}"
  else
    add R3 FAIL "检索延迟 ${A_SEG_SEARCH}ms" "超过 200ms，检查库膨胀与 WAL 写锁竞争（分因见 docs/ops-watch.md §5 R3）；${R3_NOTE}"
  fi
elif [ "$A_MS" -gt 0 ]; then
  add R3 INFO "无分段样本" "读侧 ${A_READ_N} 次都没有 search_ms 字段（二进制早于分段埋点，需 make build）；总耗时均值 ${A_MS}ms 仅作观察"
else
  add R3 INFO "无延迟样本" "窗口内没有带 ms 的读侧记录"
fi

# R4 写侧活动
if [ "$A_ADD" -ge 1 ]; then
  add R4 PASS "有写入活动" "窗口内新增 ${A_ADD} 条"
elif [ "$TOTAL" -lt 20 ]; then
  add R4 INFO "窗口内无写入" "冷启动期，写侧稀疏属预期"
else
  add R4 WARN "窗口内无写入" "库已有 ${TOTAL} 条但 12h 无新增；确认是否有值得落库的决策/坑点"
fi

# R5 观测落库
if [ "$A_OBS" -ge 1 ]; then
  add R5 PASS "观测已落库" "obs-append ${A_OBS} 次"
else
  add R5 INFO "窗口内无观测" "Stop/SessionEnd 未产出 obs-append，可能窗口内无长会话"
fi

# R6 库规模对比上次快照。TOTAL 取自 mem stats「总条数」，口径是全库各数据表之和
# （按表分布见同命令的「按表」行；memories 为空但观测/候选有料时不再误报 0）。
STATE="$V2MEM_DIR/watch-state.json"
PREV_TOTAL=""
if [ -f "$STATE" ]; then
  PREV_TOTAL=$(jq -r '.total // empty' "$STATE" 2>/dev/null)
fi
if [ -n "$PREV_TOTAL" ]; then
  if [ "$TOTAL" -ge "$PREV_TOTAL" ]; then
    add R6 PASS "库规模 ${PREV_TOTAL} → ${TOTAL}" "未下降"
  else
    add R6 WARN "库规模下降 ${PREV_TOTAL} → ${TOTAL}" "非 gc 操作导致的减少需关注"
  fi
else
  add R6 INFO "首次基线" "已记录基线 ${TOTAL} 条"
fi
printf '{"ts":%s,"total":%s,"window":"%s"}\n' "$NOW" "$TOTAL" "$WINDOW" > "$STATE" 2>/dev/null

# R7 spool 积压
#
# 计数口径必须与 internal/evidence.isSpoolSegment 一致：`.jsonl.processing` 是
# DrainSpool 已认领、尚未读完的段（认领靠 rename，成功才删）。只数 `*.jsonl`
# 会在「认领后进程被杀 / 恢复失败」时得到 0，把积压误报成 PASS。
#
# 第二个坑是文案：只要看到文件就说「下次 SessionStart 会补写」，可重试本身可能
# 一直在失败（2026-11-22 实测：同一个 seg 从 02:58 活到 15:04，期间 SessionStart
# 的 drain 记了 failed=1）。所以窗口内若有 spool-drain 记录的 failed>0，
# 必须说「重试已跑但没消化」，否则读报告的人会以为它会自愈。
SPOOL_FILES=0
SPOOL_STUCK=0
if [ -d "$V2MEM_DIR/spool" ]; then
  SPOOL_FILES=$(find "$V2MEM_DIR/spool" -name '*.jsonl' -type f 2>/dev/null | wc -l | tr -d ' ')
  SPOOL_STUCK=$(find "$V2MEM_DIR/spool" -name '*.jsonl.processing' -type f 2>/dev/null | wc -l | tr -d ' ')
fi
SPOOL_TOTAL=$(( ${SPOOL_FILES:-0} + ${SPOOL_STUCK:-0} ))

# 窗口内 spool-drain 的三元组：尝试次数 / 已回放条数 / 未消化条数（取最大）。
A_SPOOL_TRY=0; A_SPOOL_DRAINED=0; A_SPOOL_FAILED=0
if [ -f "$AUDIT" ]; then
  W7=$(jq -sr --argjson c "$CUTOFF" '
    [ .[] | select(.ts >= $c and .event=="obs-append" and (.mode // "") == "spool-drain") ] as $d
    | [ ($d | length),
        ([$d[] | .count // 0]  | add // 0),
        ([$d[] | .failed // 0] | max // 0) ]
    | @tsv' "$AUDIT" 2>/dev/null)
  if [ -n "${W7:-}" ]; then
    IFS=$'\t' read -r A_SPOOL_TRY A_SPOOL_DRAINED A_SPOOL_FAILED <<< "$W7"
  fi
fi
A_SPOOL_TRY=${A_SPOOL_TRY:-0}; A_SPOOL_DRAINED=${A_SPOOL_DRAINED:-0}
A_SPOOL_FAILED=${A_SPOOL_FAILED:-0}

if [ "$SPOOL_TOTAL" -eq 0 ]; then
  add R7 PASS "spool 无积压" "写库链路正常"
else
  STUCK_NOTE=""
  if [ "$SPOOL_STUCK" -gt 0 ]; then
    STUCK_NOTE="；其中 ${SPOOL_STUCK} 个是已认领未完成（*.jsonl.processing），不是写侧新失败"
  fi
  if [ "$A_SPOOL_FAILED" -gt 0 ]; then
    R7_TAIL="窗口内回放尝试 ${A_SPOOL_TRY} 次、回放 ${A_SPOOL_DRAINED} 条、失败 ${A_SPOOL_FAILED} 条：重试已跑但没消化，别按「下次会补写」等；按 §5 R7 查锁竞争（busy_timeout）与库权限，先用库副本对照能否补上${STUCK_NOTE}"
  else
    R7_TAIL="窗口内无失败回放记录，下次 SessionStart 会补写；若持续增长说明写库失败${STUCK_NOTE}"
  fi
  if [ "$SPOOL_TOTAL" -le 3 ]; then
    add R7 WARN "spool 有 ${SPOOL_TOTAL} 个文件" "$R7_TAIL"
  else
    add R7 FAIL "spool 积压 ${SPOOL_TOTAL} 个文件" "写库持续失败，检查库文件权限与磁盘${STUCK_NOTE}"
  fi
fi

# R8 候选积压 / R9 载荷残留：统一从 candidates ls --json 采集，避免文本行数口径。
# 按 project 分组，窗口值取各工程计数的**最大值**（阈值 5/15 不变）：
# 跨工程候选不相加——别的项目候选留给它们自己的会话就近裁决，
# 全局累加会把合法产物误诊成脏数据。见 docs/ops-watch.md §3 R8 / design-write-governance.md §12。
#
# 口径（2026-11-20 起）：R8 数的是「**现行准入判据下仍可裁决**的待处理数」，
# 不是 pending 行数。原因：助手准入判据 2026-11-17 才落到写侧，此前入库的行按定义
# 都没过这道门，于是 pending 里混着大量过程叙述（一台 cron 的进度汇报就有 17 条）。
# 把这批算进「待办」，R8 会长期 FAIL 且处置不可执行——它不是没人裁决，是不可裁决。
# 判据只约束新行、不能追认旧行，所以在读侧重算一遍：--triage 由 internal/distill.Reviewable
# 统一给每条打 reviewable（与写侧同一套规则，不在脚本里另写正则）。
# 旧行不删不改，`mem candidates ls --triage` 照常可见，并按 R15 的 TTL 自然过期；
# 不可裁决的条数单独在 detail 里报出来，不做静默丢弃。
CAND_JSON=$( "$MEM_BIN" candidates ls --json --triage --limit 100000 -db "$V2MEM_DIR/mem.db" 2>/dev/null )
# 脚本可能先于二进制更新（钩子跑的是装好的 ~/.local/bin/mem）。老二进制不认 --triage，
# 此时退回纯 pending 口径并在 detail 说明，而不是把 R8 判成「候选读取失败」——
# 后者会让人去查库权限，把版本错配误诊成故障。
CAND_TRIAGE=1
if ! printf '%s' "$CAND_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
  if CAND_JSON=$( "$MEM_BIN" candidates ls --json --limit 100000 -db "$V2MEM_DIR/mem.db" 2>/dev/null ) \
     && printf '%s' "$CAND_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
    CAND_TRIAGE=0
  fi
fi
if printf '%s' "$CAND_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
  CAND_READ_FAIL=0
else
  CAND_JSON='[]'
  CAND_READ_FAIL=1
fi
CAND_PENDING=$(printf '%s' "$CAND_JSON" | jq -c '[.[] | select(.status == "pending")]' 2>/dev/null || echo '[]')
CAND=$(printf '%s' "$CAND_PENDING" | jq -r 'length' 2>/dev/null || echo 0)
CAND=${CAND:-0}
# 缺 reviewable 键（旧二进制 / 测试夹具）按「可裁决」处理：不可裁决统计以 false 为唯一依据。
CAND_REVIEWABLE=$(printf '%s' "$CAND_PENDING" | jq -c '[.[] | select(.reviewable != false)]' 2>/dev/null || echo '[]')
CAND_REV=$(printf '%s' "$CAND_REVIEWABLE" | jq -r 'length' 2>/dev/null || echo 0)
CAND_REV=${CAND_REV:-0}
CAND_EXHAUST=$(( CAND - CAND_REV ))
[ "$CAND_EXHAUST" -lt 0 ] && CAND_EXHAUST=0
CAND_MAX=$(printf '%s' "$CAND_REVIEWABLE" | jq -r '[group_by(.project)[] | length] | max // 0' 2>/dev/null || echo 0)
CAND_MAX=${CAND_MAX:-0}
# detail 以 | 分隔 RESULTS，且会被拼进 JSON：项目名里的 | 与 " 一律替换掉。
CAND_GROUPS=$(printf '%s' "$CAND_REVIEWABLE" | jq -r '
  group_by(.project) | map({p:(.[0].project // ""), n:length}) | sort_by(-.n)
  | map((if .p == "" then "(全局)" else .p end) + "=" + (.n|tostring))
  | join(", ") | gsub("[\"|]"; "/")' 2>/dev/null || echo '')
# 不可裁决的按工程报数，避免「总量降了但不知降在哪」。
CAND_EXHAUST_TXT=""
if [ "$CAND_EXHAUST" -gt 0 ]; then
  CAND_EXHAUST_TXT="；另有 ${CAND_EXHAUST} 条按现行准入判据不可裁决（过程叙述/助手偏好，不计入积压，随 R15 TTL 过期）"
fi
if [ "${CAND_TRIAGE:-1}" -eq 0 ]; then
  CAND_EXHAUST_TXT="；二进制不支持 --triage，本窗口按 pending 行数计（重建 mem 后恢复可裁决口径）"
fi
# 越线时直接给出「最积压工程的前 3 条」，省掉裁决前的一次 candidates ls。
# 只给 id 前半段 + 正文前 40 字：够认出是哪件事，又不把 detail 撑长。
# 取数集与积压计数同源（CAND_REVIEWABLE）：不可裁决的行列进来只会误导裁决。
CAND_TODO=$(printf '%s' "$CAND_REVIEWABLE" | jq -r '
  (group_by(.project) | max_by(length) // [])[0:3]
  | map(((.id // "")[0:8]) + ":" + (((.content // "")[0:40]) | gsub("[\\n\"|*#]"; "/")))
  | join(" / ")' 2>/dev/null || echo '')

if [ "$CAND_READ_FAIL" -eq 1 ]; then
  add R8 WARN "候选读取失败" "candidates ls --json 不可解析；按 WARN 观察，勿据此判 PASS"
elif [ "$CAND_MAX" -le 5 ]; then
  add R8 PASS "候选待处理 ${CAND_MAX}" "隔离区未积压；按工程：${CAND_GROUPS:-无}${CAND_EXHAUST_TXT}"
elif [ "$CAND_MAX" -le 15 ]; then
  add R8 WARN "候选积压 ${CAND_MAX}" "按工程：${CAND_GROUPS}；在对应工程会话内用 MCP candidates_ls / accept / reject 就近裁决${CAND_EXHAUST_TXT}${CAND_TODO:+；可先裁决：$CAND_TODO}"
else
  add R8 FAIL "候选积压 ${CAND_MAX}" "按工程：${CAND_GROUPS}；某工程长期未裁决，检索质量会受影响${CAND_EXHAUST_TXT}${CAND_TODO:+；可先裁决：$CAND_TODO}"
fi

# R9 候选内容质量：结构化载荷残留不是原子事实，说明蒸馏输入未按准入过滤。
# 检测式有两种形态，缺一不可：
#   - transcript 原始 JSON：{"id"…} / {"type"…}（旧版 grep 口径，保持等价）；
#   - 工具调用载荷的文本形态：「Grep {」「Shell {」等工具名 + 对象字面量开头。
#     2026-11-15 发现本机 2 条 11-02 存量候选（`Shell {"command"…` / `Grep {"pattern"…`）
#     只有后一种形态，旧检测式判 PASS，属漏判——载荷残留正是 R9 要拦的东西。
#
# 新鲜度：R9 统计的是**全部 pending**，不按 created_at 切片（存量残留不清就一直计数）。
# 只报条数会把 14 天前的存量说成「单窗口内产生」，把历史遗留误诊成「过滤刚失效」，
# 诱导去改本来正常的准入逻辑。故把命中集拆成「窗口内新增 / 存量」两段报出来：
# 窗口内新增 >0 才能断言过滤失效；全为存量时只说清理，不提过滤。
NOISE_STAT=$(printf '%s' "$CAND_PENDING" | jq -r --argjson cut "$CUTOFF" --argjson now "$NOW" '
  [ .[] | (.content // "") as $c
       | select(($c | test("\\{\"(id|parentId|callId|type)\""))
                or ($c | test("^\\s*(Grep|Shell|Read|Bash|Edit|Write|Glob|WebFetch|TodoWrite|MultiEdit)\\s*\\{")))
       | (.created_at // 0) ] as $ts
  | if ($ts | length) == 0 then "0 0 -1"
    else "\($ts | length) \([$ts[] | select(. > $cut)] | length) "
         + "\(((($now - ([$ts[] | select(. > 0)] | min // $now)) / 86400) | floor))"
    end' 2>/dev/null || echo "0 0 -1")
read -r NOISE NOISE_FRESH NOISE_AGE <<<"${NOISE_STAT:-0 0 -1}"
case "$NOISE" in ''|*[!0-9]*) NOISE=0 ;; esac
case "$NOISE_FRESH" in ''|*[!0-9]*) NOISE_FRESH=0 ;; esac
case "$NOISE_AGE" in ''|*[!0-9-]*) NOISE_AGE=-1 ;; esac
NOISE_OLD=$(( NOISE - NOISE_FRESH ))
if [ "$NOISE_AGE" -lt 0 ]; then
  NOISE_AGE_TXT="创建时间不可读"
elif [ "$NOISE_AGE" -eq 0 ]; then
  NOISE_AGE_TXT="最早为今日"
else
  NOISE_AGE_TXT="最早 ${NOISE_AGE} 天前"
fi
NOISE_OLD_TXT=""
if [ "$NOISE_OLD" -gt 0 ]; then NOISE_OLD_TXT="（另有存量 ${NOISE_OLD} 条，${NOISE_AGE_TXT}）"; fi
if [ "$CAND_READ_FAIL" -eq 1 ]; then
  add R9 WARN "候选内容未校验" "pending 内容未读取；按 WARN 观察"
elif [ "$NOISE" -eq 0 ]; then
  add R9 PASS "候选内容无载荷残留" "pending ${CAND} 条，未发现 transcript JSON 或工具载荷"
elif [ "$NOISE_FRESH" -gt 0 ]; then
  # 窗口内确有新增：这才是「蒸馏输入没按 proposalEligible 过滤」的证据。
  if [ "$NOISE" -le 3 ]; then
    add R9 WARN "候选含载荷残留 ${NOISE} 条" "窗口内新增 ${NOISE_FRESH} 条${NOISE_OLD_TXT}；用 mem reject 清理并核查 distill 输入过滤"
  else
    add R9 FAIL "候选含载荷残留 ${NOISE} 条" "窗口内新增 ${NOISE_FRESH} 条${NOISE_OLD_TXT}，蒸馏输入未过滤 pointer 观测；先修准入，再逐条 mem reject"
  fi
elif [ "$NOISE" -le 3 ]; then
  add R9 WARN "候选含载荷残留 ${NOISE} 条" "全为存量（${NOISE_AGE_TXT}），非本窗口新增；用 mem reject 清理即可，过滤逻辑暂无失效证据"
else
  add R9 FAIL "候选含载荷残留 ${NOISE} 条" "全为存量（${NOISE_AGE_TXT}），非本窗口新增；先逐条 mem reject，再复查是否仍有新增"
fi

# R10 写侧延迟：stop 与 obs-append 共用同一个 Stop 计时器，只取 stop 避免双计。
# 阈值来自 Stop hook 同步超时 20s 的 5% / 15%，见 docs/ops-watch.md §3。
if [ "$A_WRITE_N" -gt 0 ]; then
  R10_NOTE="stop ${A_WRITE_N} 次均值；宿主 Stop hook timeout=20s，阈值取 5% / 15%"
  if [ "$A_WRITE_MS" -lt 1000 ]; then
    add R10 PASS "写侧延迟 ${A_WRITE_MS}ms" "低于 1000ms；${R10_NOTE}"
  elif [ "$A_WRITE_MS" -le 3000 ]; then
    add R10 WARN "写侧延迟 ${A_WRITE_MS}ms" "1000–3000ms，查单次 Stop 的 transcript 补写量（瓶颈是逐条归档 fsync）；WAL 由 R11 单独判；${R10_NOTE}"
  else
    add R10 FAIL "写侧延迟 ${A_WRITE_MS}ms" ">3000ms，接近同步 hook 超时预算；${R10_NOTE}"
  fi
else
  add R10 INFO "无写侧样本" "窗口内没有带 ms 的 stop 事件，无法评估写侧延迟"
fi

# R11 WAL 高水位：只读文件大小，绝不修改或删除 -wal。
#
# 帧数由文件大小折算，量的是**文件高水位**，不是积压量：SQLite 只在
# wal_checkpoint(TRUNCATE) 或最后一个连接关闭时才收缩 -wal 文件，被动 checkpoint
# 只把页回写主库、文件本身不缩。故「帧数 > 1000」只说明历史写过这么多，不等于
# 数据滞留。2026-11-20 实证：仅取主库文件（不含 -wal）的快照含 5249/5264 条观测
# （99.7%，最新到 11-19 20:29），checkpoint 实际一直在推进；同期 -wal 稳定 4.0MB。
# 判「是否真滞留」看 §5 的 (busy,log,checkpointed) 三元组，不要据此推断读侧延迟。
WAL_FILE="$V2MEM_DIR/mem.db-wal"
WAL_BYTES=0
if [ -f "$WAL_FILE" ]; then
  WAL_BYTES=$(wc -c < "$WAL_FILE" 2>/dev/null | tr -d ' ')
fi
WAL_BYTES=${WAL_BYTES:-0}
WAL_FRAMES=$(( WAL_BYTES / 4096 ))
if [ "$WAL_FRAMES" -le 1000 ]; then
  add R11 PASS "WAL 高水位 ${WAL_FRAMES} 帧" "${WAL_BYTES} 字节，未超 wal_autocheckpoint=1000"
elif [ "$WAL_FRAMES" -le 2000 ]; then
  add R11 WARN "WAL 高水位 ${WAL_FRAMES} 帧" "${WAL_BYTES} 字节，超过 autocheckpoint 水位。帧数取自文件大小，不代表数据滞留（SQLite 只在 TRUNCATE 或最后一个连接关闭时收缩 -wal）；先按 §5 取 (busy,log,checkpointed) 三元组，log 与 checkpointed 相等即无滞留，确有滞留再 TRUNCATE"
else
  add R11 FAIL "WAL 高水位 ${WAL_FRAMES} 帧" "${WAL_BYTES} 字节，超过阈值 2 倍。先按 §5 取三元组判滞留，确需回收再 wal_checkpoint(TRUNCATE)，不要手工删 -wal"
fi

# R12 写侧自动决策链路：观测 → 蒸馏 → 候选 → 显式裁决。
# 判级只看「自动段」是否推进；裁决活性与 eval activity 的 verdict 只作诊断信息，
# 零裁决由 R8 判积压，避免同一现象重复告警。阈值与依据见 docs/ops-watch.md §3 R12。
A_DISTILL_RULES=$(( A_DISTILL - A_DISTILL_PROPOSAL ))
D12="观测 ${A_OBS} / 蒸馏 ${A_DISTILL}（proposal ${A_DISTILL_PROPOSAL} · rules ${A_DISTILL_RULES}） / 合法空 ${A_DISTILL_EMPTY} / 失败 ${A_DISTILL_FAIL} / 裁决 accept ${A_DECIDE_A} reject ${A_DECIDE_R}"
if [ -n "$ACT_VERDICT" ]; then D12="${D12}；结论: ${ACT_VERDICT}"; fi
if [ "$A_OBS" -eq 0 ]; then
  add R12 INFO "无观测样本" "窗口内无 obs-append，无法评估写侧决策链路（无样本，非冷启动降级）；${D12}"
elif [ "$A_DISTILL_FAIL" -gt 3 ]; then
  add R12 FAIL "蒸馏失败 ${A_DISTILL_FAIL} 次" "${D12}；按 §5 R12 查 distill_runs 的 error 与 attempt，不要重跑 distill 掩盖"
elif [ "$A_DISTILL" -eq 0 ]; then
  add R12 WARN "自动蒸馏未推进" "${D12}；有观测但窗口内 0 条 distill；按 §8.1 核对 MEM_DISTILL_SPAWN / MEM_PROPOSAL_HOOK / MEM_NO_DISTILL，确认是否按 §8.4 有意关闭"
elif [ "$A_DISTILL_FAIL" -ge 1 ]; then
  add R12 WARN "蒸馏失败 ${A_DISTILL_FAIL} 次" "${D12}；查 distill_runs 的 error 与 evidence_bundles 是否 backlog"
else
  add R12 PASS "写侧决策链路推进" "${D12}"
fi

# R13 注入体量：宿主对 hook stdout 的硬上限会把超限内容整块换成约 2KB 占位且**仍返回成功**，
# 用户侧表现为「记忆没出现」而不是报错——R2 只测查得到，测不到送不进去。阈值依据见 §3 R13。
R13_RUNES=$(( A_INJ_MAX / 3 ))
if [ "$A_INJ_N" -eq 0 ]; then
  if [ "$A_INJ_READ" -gt 0 ]; then
    add R13 INFO "无注入体量样本" "读侧记录均无 inject_bytes 字段（升级前的旧记录），无法评估注入体量；新记录自带该字段"
  else
    add R13 INFO "无注入体量样本" "窗口内无读侧记录（session-start / prompt-submit / pre-compact），无法评估注入体量"
  fi
elif [ "$A_INJ_MAX" -le 3600 ]; then
  add R13 PASS "注入体量 ${A_INJ_MAX} 字节" "本次最大 ${A_INJ_MAX} 字节，≈${R13_RUNES} rune（按 3 字节/rune 估）；读侧样本 ${A_INJ_N} 次，未超 3600 字节"
elif [ "$A_INJ_MAX" -le 8000 ]; then
  add R13 WARN "注入体量 ${A_INJ_MAX} 字节" "本次最大 ${A_INJ_MAX} 字节，落在 3601–8000，≈${R13_RUNES} rune（按 3 字节/rune 估）；读侧样本 ${A_INJ_N} 次，查预算变量是否被调高"
elif [ "$A_INJ_MAX" -gt 8000 ]; then
  add R13 FAIL "注入体量 ${A_INJ_MAX} 字节" "本次最大 ${A_INJ_MAX} 字节，超过 8000，≈${R13_RUNES} rune（按 3 字节/rune 估）；读侧样本 ${A_INJ_N} 次。宿主可能整块替换为占位且仍报成功，记忆会静默消失"
fi

# R14 归档体量：evidence 是只写事实源（VALUE §4.1），一条观测一份 .jsonl.gz。
# 准出路径是 mem gc --obs 之后再 mem gc --evidence（只删已无观测引用的段与文件）；
# 本脚本只读，不做回收。故只量落盘量（文件数 + 字节），不折算成「多少条观测」：
# gc 之后行数降而文件数不降，两者本就会脱钩。
# 阈值与「为何不设 FAIL」见 docs/ops-watch.md §3 R14。
EV_DIR="$V2MEM_DIR/evidence"
EV_FILES=0; EV_BYTES=0
if [ -d "$EV_DIR" ]; then
  EV_FILES=$(find "$EV_DIR" -type f 2>/dev/null | wc -l | tr -d ' ')
  EV_KB=$(du -sk "$EV_DIR" 2>/dev/null | awk '{print $1}')
  EV_BYTES=$(( ${EV_KB:-0} * 1024 ))
fi
EV_FILES=${EV_FILES:-0}; EV_BYTES=${EV_BYTES:-0}
EV_MIB=$(( EV_BYTES / 1048576 ))
DB_BYTES=$( "$MEM_BIN" stats --json 2>/dev/null | jq -r '.size_bytes // 0' 2>/dev/null )
DB_BYTES=${DB_BYTES:-0}
DB_MIB=$(( ${DB_BYTES:-0} / 1048576 ))
if [ ! -d "$EV_DIR" ]; then
  add R14 INFO "无归档目录" "${EV_DIR} 不存在；未捕获过观测，或来自未启用证据归档的旧版本"
elif [ "$EV_FILES" -eq 0 ]; then
  add R14 PASS "归档为空" "evidence 目录无归档文件（主库 ${DB_MIB}MiB）；尚无观测落盘"
elif [ "$EV_BYTES" -le 536870912 ]; then
  add R14 PASS "归档 ${EV_MIB}MiB" "${EV_FILES} 个归档 / ${EV_MIB}MiB（主库 ${DB_MIB}MiB），未超 512MiB 水位"
else
  add R14 WARN "归档 ${EV_MIB}MiB" "${EV_FILES} 个归档 / ${EV_MIB}MiB（主库 ${DB_MIB}MiB），超过 512MiB 水位；归档只增不减，按 §5 R14 裁决保留策略，勿删目录"
fi

# R15 候选 TTL 执行：pending 超过 14 天仍未转 expired，说明过期机制从来没被跑过。
# 为什么需要单独一条判据：GCCandidates 在代码里写死 14 天 TTL（`internal/store/candidates.go`），
# 但唯一执行者是手工 `mem gc --candidates`。2026-11-17 实测本机 2 条 11-02 候选已超期
# 14.8 天仍是 pending，crontab / LaunchAgents 均无调度——TTL 存在却形同虚设，
# 后果是 pending 只增不减、R8 长期 FAIL，且这个失效是**静默的**（不报错，只是永远不生效）。
#
# 自主边界（重要）：只执行「状态迁移」pending → expired——行仍在库、`--status expired` 可查、
# 不删 memories、不影响 R6。不执行「行删除」：GCCandidates 还带一条 DELETE（删 90 天以上的
# accepted 行），所以先查是否存在待删的已采纳行，**有就不执行**，转人工——
# 删除不可逆，属 design-write-governance.md §12.5 明确要人判的那一类。
#
# 顺序要求：R15 必须排在 R8/R9 之后。R8 反映的是过期前的真实积压，先采集再处置，
# 否则会把自己刚清空的结果当成「没有积压」。
TTL_DAYS=14
TTL_ACCEPTED_DAYS=90
STALE_PENDING=$(printf '%s' "$CAND_PENDING" | jq -r --argjson now "$NOW" --argjson ttl $(( TTL_DAYS * 86400 )) '
  [ .[] | select(($now - (.created_at // 0)) > $ttl) ] | length' 2>/dev/null || echo 0)
case "$STALE_PENDING" in ''|*[!0-9]*) STALE_PENDING=0 ;; esac

if [ "$CAND_READ_FAIL" -eq 1 ]; then
  add R15 WARN "候选 TTL 未核查" "candidates ls --json 不可解析；按 WARN 观察，勿据此判 PASS"
elif [ "$STALE_PENDING" -eq 0 ]; then
  add R15 PASS "候选 TTL 无超期" "无超过 ${TTL_DAYS} 天的 pending（与 GCCandidates 默认阈值同源）"
elif [ "$GC_DISABLED" -eq 1 ]; then
  add R15 WARN "候选超期 ${STALE_PENDING} 条" "已超 ${TTL_DAYS} 天仍 pending；--no-gc 下不自动过期，按 §5 手工执行 mem gc --candidates"
else
  ACC_JSON=$( "$MEM_BIN" candidates ls --json --status accepted --limit 100000 -db "$V2MEM_DIR/mem.db" 2>/dev/null || echo '[]' )
  ACC_OLD=0
  if printf '%s' "$ACC_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
    ACC_OLD=$(printf '%s' "$ACC_JSON" | jq -r --argjson now "$NOW" --argjson ttl $(( TTL_ACCEPTED_DAYS * 86400 )) '
      [ .[] | select(($now - (.created_at // 0)) > $ttl) ] | length' 2>/dev/null || echo 0)
  fi
  case "$ACC_OLD" in ''|*[!0-9]*) ACC_OLD=0 ;; esac
  if [ "$ACC_OLD" -gt 0 ]; then
    add R15 WARN "候选超期 ${STALE_PENDING} 条·未执行" "另有 ${ACC_OLD} 条 ${TTL_ACCEPTED_DAYS} 天前的已采纳行会被连带删除（不可逆），故不自动执行；确认后跑 mem gc --candidates"
  elif GC_OUT=$( "$MEM_BIN" gc --candidates -db "$V2MEM_DIR/mem.db" 2>&1 ); then
    add R15 WARN "候选超期已过期 ${STALE_PENDING} 条" "已执行 mem gc --candidates（pending → expired，行仍在库，--status expired 可查，未动 memories）；R8 本窗口仍按过期前计数"
  else
    add R15 WARN "候选过期执行失败" "${GC_OUT:-mem gc --candidates 无输出}；按 §5 手工执行并查库权限与磁盘"
  fi
fi

# ---------- 输出 ----------
FAILS=0; WARNS=0
for r in "${RESULTS[@]}"; do
  lvl="${r#*|}"; lvl="${lvl%%|*}"
  [ "$lvl" = FAIL ] && FAILS=$((FAILS+1))
  [ "$lvl" = WARN ] && WARNS=$((WARNS+1))
done

if [ "$JSON_OUT" -eq 1 ]; then
  printf '{"window":"%s","total":%s,"fail":%s,"warn":%s,"items":[' "$WINDOW" "$TOTAL" "$FAILS" "$WARNS"
  first=1
  for r in "${RESULTS[@]}"; do
    [ $first -eq 0 ] && printf ','
    first=0
    printf '%s' "$r" | awk -F'|' '{printf "{\"id\":\"%s\",\"level\":\"%s\",\"title\":\"%s\",\"detail\":\"%s\"}", $1,$2,$3,$4}'
  done
  printf ']}\n'
else
  echo "v2mem 运行观测（窗口 ${WINDOW}）"
  echo "库: ${V2MEM_DIR}/mem.db"
  echo
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r id lvl title detail <<< "$r"
    [ "$QUIET" -eq 1 ] && [ "$lvl" = PASS ] && continue
    printf '  %-4s %-4s %-18s %s\n' "$lvl" "$id" "$title" "$detail"
  done
  echo
  if [ "$FAILS" -gt 0 ]; then
    echo "结论: FAIL（${FAILS} 项）—— 按 docs/ops-watch.md 处置表处理"
  elif [ "$WARNS" -gt 0 ]; then
    echo "结论: WARN（${WARNS} 项）—— 观察，不阻断"
  else
    echo "结论: OK —— 动线运行符合预期"
  fi
fi

[ "$FAILS" -gt 0 ] && exit 1
exit 0
