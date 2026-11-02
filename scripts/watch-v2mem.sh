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
#
# 只读：不写库、不改配置。唯一的写入是 ~/.v2mem/watch-state.json 的基线快照。
# 退出码: 0=无 FAIL  1=有 FAIL

set -uo pipefail

MEM_BIN="${MEM_BIN:-$(command -v mem || echo "$HOME/go/bin/mem")}"
V2MEM_DIR="${V2MEM_DIR:-$HOME/.v2mem}"
WB_DIR="${WORKBUDDY_CONFIG_DIR:-$HOME/.workbuddy}"
WINDOW="12h"
JSON_OUT=0
QUIET=0

while [ $# -gt 0 ]; do
  case "$1" in
    --window) WINDOW="${2:-12h}"; shift 2 ;;
    --json)   JSON_OUT=1; shift ;;
    --quiet)  QUIET=1; shift ;;
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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
if "$MEM_BIN" stats >/dev/null 2>&1; then
  TOTAL=$("$MEM_BIN" stats 2>/dev/null | awk '/^总条数/{print $2}')
  TOTAL=${TOTAL:-0}
  add I1 PASS "二进制可执行" "mem stats 正常，库内 ${TOTAL} 条"
else
  TOTAL=0
  add I1 FAIL "二进制不可执行" "$MEM_BIN stats 失败"
fi

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
      "A_MS=\(([$e[] | select(.ms != null and .ms > 0) | .ms] | if length>0 then (add/length|floor) else 0 end))"
  ' "$AUDIT" 2>/dev/null)"
fi
A_EVENTS=${A_EVENTS:-0}; A_PROMPT=${A_PROMPT:-0}; A_PROMPT_EMPTY=${A_PROMPT_EMPTY:-0}
A_SS=${A_SS:-0}; A_OBS=${A_OBS:-0}; A_ADD=${A_ADD:-0}; A_MS=${A_MS:-0}

# R1 钩子是否被真实触发
if [ "$A_EVENTS" -ge 1 ]; then
  add R1 PASS "钩子已触发" "窗口内 ${A_EVENTS} 个事件（session-start ${A_SS} / prompt-submit ${A_PROMPT}）"
else
  add R1 FAIL "钩子零事件" "窗口内无审计记录；先查 settings.json 路径是否为 $WB_DIR"
fi

# R2 检索命中率（仅 prompt-submit 有意义；session-start 的 empty 恒 false）
if [ "$A_PROMPT" -gt 0 ]; then
  HIT_RATE=$(( (A_PROMPT - A_PROMPT_EMPTY) * 100 / A_PROMPT ))
  if [ "$TOTAL" -lt 20 ]; then
    add R2 INFO "检索命中率 ${HIT_RATE}%" "prompt-submit ${A_PROMPT} 次，空 ${A_PROMPT_EMPTY} 次；库仅 ${TOTAL} 条，冷启动期空注入属预期"
  elif [ "$HIT_RATE" -ge 30 ]; then
    add R2 PASS "检索命中率 ${HIT_RATE}%" "prompt-submit ${A_PROMPT} 次，空 ${A_PROMPT_EMPTY} 次"
  elif [ "$HIT_RATE" -ge 10 ]; then
    add R2 WARN "检索命中率偏低 ${HIT_RATE}%" "库 ${TOTAL} 条仍大量空命中，考虑补记忆或查同义词表"
  else
    add R2 FAIL "检索基本不命中 ${HIT_RATE}%" "库 ${TOTAL} 条但几乎全空；检查中文分词与 FTS 索引"
  fi
else
  add R2 INFO "窗口内无提问" "窗口内没有 prompt-submit 事件，无法评估命中率"
fi

# R3 检索延迟
if [ "$A_MS" -gt 0 ]; then
  if [ "$A_MS" -lt 50 ]; then
    add R3 PASS "检索延迟 ${A_MS}ms" "低于 50ms 目标线"
  elif [ "$A_MS" -lt 200 ]; then
    add R3 WARN "检索延迟 ${A_MS}ms" "高于典型值 ~9ms，观察是否库膨胀或锁竞争"
  else
    add R3 FAIL "检索延迟 ${A_MS}ms" "超过 200ms，检查 WAL 大小与并发写入"
  fi
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

# R6 库规模对比上次快照
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
SPOOL_FILES=0
if [ -d "$V2MEM_DIR/spool" ]; then
  SPOOL_FILES=$(find "$V2MEM_DIR/spool" -name '*.jsonl' -type f 2>/dev/null | wc -l | tr -d ' ')
fi
if [ "$SPOOL_FILES" -eq 0 ]; then
  add R7 PASS "spool 无积压" "写库链路正常"
elif [ "$SPOOL_FILES" -le 3 ]; then
  add R7 WARN "spool 有 ${SPOOL_FILES} 个文件" "下次 SessionStart 会补写；若持续增长说明写库失败"
else
  add R7 FAIL "spool 积压 ${SPOOL_FILES} 个文件" "写库持续失败，检查库文件权限与磁盘"
fi

# R8 候选积压
CAND_OUT=$( "$MEM_BIN" candidates ls 2>/dev/null )
if printf '%s' "$CAND_OUT" | grep -q '没有候选'; then
  CAND=0
else
  CAND=$(printf '%s' "$CAND_OUT" | grep -c . )
fi
CAND=${CAND:-0}
if [ "$CAND" -le 5 ]; then
  add R8 PASS "候选待处理 ${CAND}" "隔离区未积压"
elif [ "$CAND" -le 15 ]; then
  add R8 WARN "候选积压 ${CAND}" "考虑 mem accept / mem reject 清理"
else
  add R8 FAIL "候选积压 ${CAND}" "蒸馏产物长期未处理，检索质量会受影响"
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
