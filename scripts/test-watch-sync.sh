#!/usr/bin/env bash
set -euo pipefail
# 守 docs/ops-watch.md §3 判据表 ↔ scripts/watch-v2mem.sh 的同步。
# AGENTS.md / ops-watch.md 都写了「改判据必须同步改两处」，此前只有人工纪律：
# §7 的「判据 ID（I1…R9）」在判据表已扩到 R11 后仍滞留，就是没有检查的实证。
# 本脚本只读，不碰库与配置。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DOC="$ROOT/docs/ops-watch.md"
SCRIPT="$ROOT/scripts/watch-v2mem.sh"
fail() { echo "FAIL watch-sync: $*" >&2; exit 1; }

# ① ID 集合：§3 表格的 I*/R* 与脚本里 add <ID> 的 ID 必须双向一致。
doc_ids="$(sed -n '/^## 3\./,/^## 4\./p' "$DOC" \
  | grep -oE '^\| \*\*[IR][0-9]+\*\*' | grep -oE '[IR][0-9]+' | sort -u)"
# 按 token 抓而不是按行首：I2 的分级写在 case 分支里（`4) add I2 PASS ...`）。
script_ids="$(grep -oE 'add [IR][0-9]+ ' "$SCRIPT" | awk '{print $2}' | sort -u)"
[ -n "$doc_ids" ] || fail "§3 判据表没解析出任何 ID（表格格式变了？）"
[ -n "$script_ids" ] || fail "脚本没解析出任何 add ID（分级写法变了？）"

missing="$(comm -23 <(printf '%s\n' "$doc_ids") <(printf '%s\n' "$script_ids"))"
extra="$(comm -13 <(printf '%s\n' "$doc_ids") <(printf '%s\n' "$script_ids"))"
[ -z "$missing" ] || fail "判据表有、脚本没实现的 ID: $(echo "$missing" | tr '\n' ' ')"
[ -z "$extra" ]   || fail "脚本有、判据表没登记的 ID: $(echo "$extra" | tr '\n' ' ')"

# ② R8 阈值：文档阈值列与脚本分级字面量必须同时存在。
# 分组口径改了，但 5/15 两档不许单侧调整。
r8_row="$(grep -E '^\| \*\*R8\*\*' "$DOC" | head -1)"
[ -n "$r8_row" ] || fail "§3 找不到 R8 行"
printf '%s' "$r8_row" | grep -q '≤5'  || fail "§3 R8 的 PASS 线不是 ≤5"
printf '%s' "$r8_row" | grep -q '>15' || fail "§3 R8 的 FAIL 线不是 >15"
grep -q 'CAND_MAX" -le 5'  "$SCRIPT" || fail "脚本 R8 的 PASS 线不是 ≤5"
grep -q 'CAND_MAX" -le 15' "$SCRIPT" || fail "脚本 R8 的 WARN 上界不是 15"
# ②b R8 计量对象（2026-11-20 起「只数可裁决」，2026-11-26 候选命令并入 review）：
# 文档写了 reviewable 口径，脚本就必须从 review ls 取数并按 reviewable 过滤；
# 单侧改会退回「数 pending 行数」。
printf '%s' "$r8_row" | grep -q 'review ls --state all' \
  || fail "§3 R8 未写明从 review ls --state all 取数（命令面已收编）"
printf '%s' "$r8_row" | grep -q 'reviewable' \
  || fail "§3 R8 未写明只数 reviewable（口径与脚本脱钩）"
grep -q -- 'review ls --state all --json' "$SCRIPT" || fail "脚本 R8 未从 review ls --state all --json 取数"
grep -q 'select(.reviewable != false)' "$SCRIPT" || fail "脚本 R8 未按 reviewable 过滤"
grep -q '另有 \${CAND_EXHAUST} 条' "$SCRIPT" || fail "脚本 R8 未报出不可裁决条数"

# ③ ID 枚举漂移：§7 等处的「I1…Rn」/「R1…Rn」必须与 §3 的实际最大 R 一致。
# 实证：判据表已扩到 R11 时，§7 仍写「I1…R9」，长期无人发现。
max_r="$(printf '%s\n' "$doc_ids" | grep -oE 'R[0-9]+' | sed 's/R//' | sort -n | tail -1)"
declared="$(grep -oE 'I1…R[0-9]+|R1…R[0-9]+' "$DOC" | grep -oE '[0-9]+$' | sort -n | tail -1 || true)"
if [ -n "$declared" ]; then
  [ "$declared" = "$max_r" ] || fail "文档声明到 R${declared}，判据表实际到 R${max_r}"
fi

# ④ R12 阈值：文档「失败 >3」与脚本分级字面量必须同步，断链分支不许被删。
r12_row="$(grep -E '^\| \*\*R12\*\*' "$DOC" | head -1)"
[ -n "$r12_row" ] || fail "§3 找不到 R12 行"
printf '%s' "$r12_row" | grep -q '失败 >3' || fail "§3 R12 的 FAIL 线不是 失败 >3"
grep -q 'A_DISTILL_FAIL" -gt 3' "$SCRIPT" || fail "脚本 R12 的 FAIL 线不是 >3"
grep -q 'A_DISTILL" -eq 0' "$SCRIPT"     || fail "脚本 R12 缺断链判级分支"

# ⑤ R13 注入体量：文档两档字面量与脚本分级必须同步，无样本分支不许被删。
# 这条判据守的是「查到了但送不进去」——R2 命中率看不出宿主把 stdout 换成占位。
r13_row="$(grep -E '^\| \*\*R13\*\*' "$DOC" | head -1)"
[ -n "$r13_row" ] || fail "§3 找不到 R13 行"
printf '%s' "$r13_row" | grep -q '≤3600' || fail "§3 R13 的 PASS 线不是 ≤3600"
printf '%s' "$r13_row" | grep -q '>8000' || fail "§3 R13 的 FAIL 线不是 >8000"
grep -q 'A_INJ_MAX" -le 3600' "$SCRIPT" || fail "脚本 R13 的 PASS 线不是 ≤3600"
grep -q 'A_INJ_MAX" -gt 8000' "$SCRIPT" || fail "脚本 R13 的 FAIL 线不是 >8000"
grep -q 'A_INJ_N" -eq 0'      "$SCRIPT" || fail "脚本 R13 缺无样本分支"

# ⑥ §5 处置表必须给 R3 一行。实证：2026-11-17 R3 首次判 FAIL 时，§5 只有 R1/R2/R7/R6/R8/R9/
# R10/R11/R12/R13/R14 与 I 组，R3 无对应动作可引用。只守 R3——R4/R5 本就无需处置行，不能一刀切。
if ! sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -qE '^\| R3 '; then
  fail "§5 缺 R3 处置行（R3 FAIL 时会无动作可引用）"
fi

# ⑦ R15 阈值与只读边界：TTL 14 天两侧同步；巡检**只报不处置**。
# 曾经的边界退化是「巡检替人执行 gc --candidates（内含删 90 天 accepted 行的 DELETE）」，
# 所以这里既守正向文案，也用负向断言禁止再把写路径装回巡检。
r15_row="$(grep -E '^\| \*\*R15\*\*' "$DOC" | head -1)"
[ -n "$r15_row" ] || fail "§3 找不到 R15 行"
printf '%s' "$r15_row" | grep -q '14 天' || fail "§3 R15 的 TTL 不是 14 天"
grep -q 'TTL_DAYS=14' "$SCRIPT" || fail "脚本 R15 的 TTL_DAYS 不是 14"
printf '%s' "$r15_row" | grep -q '只报不处置' || fail "§3 R15 没写明「只报不处置」"
# 负向：巡检不许再有写开关 / 写子命令 / 替人判断连带删除。
grep -q 'GC_DISABLED' "$SCRIPT" && fail "脚本仍保留 --no-gc 开关（GC_DISABLED）"
grep -q -- '"$MEM_BIN" gc' "$SCRIPT" && fail "脚本仍会调用 mem gc（巡检必须只读）"
grep -q 'ACC_OLD' "$SCRIPT" && fail "脚本仍查询待删的已采纳行（ACC_OLD）"
# 正向：§5 必须给手工命令——处置归维护/人工。
r15_action="$(sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -E '^\| R15 ' || true)"
printf '%s' "$r15_action" | grep -q 'mem gc --candidates' \
  || fail "§5 R15 处置未给手工命令"

# ⑦b S13 终态出口（2026-11-26）：物理删除与状态迁移是两条路，脚本 detail 与 §5
# 处置都要提 mem gc --retention（rejected 90 天 / 完成 Bundle 30 天 / 孤立决策账，
# 默认关）。只提命令、不执行——巡检只读边界不变，TTL_DAYS=14 定值不漂移。
printf '%s' "$r15_action" | grep -q 'mem gc --retention' \
  || fail "§5 R15 处置缺 S13 终态物理删除出口 mem gc --retention"
printf '%s' "$(grep 'add R15 WARN' "$SCRIPT" | grep '候选超期' || true)" | grep -q 'mem gc --retention' \
  || fail "脚本 R15 WARN detail 缺 S13 终态出口 mem gc --retention"
grep -q 'TTL_DAYS=14' "$SCRIPT" || fail "S13 之后脚本 R15 的 TTL_DAYS 仍必须是 14"

# ⑧ R3 口径：脚本 detail 与文档依据段都必须写明「端到端」。
# 实证：文档长期写「实测量级 9–15ms」，而 R3 量的是端到端（进程启动 + 冷缓存 + 注入渲染），
# 2026-11-19 复核端到端按日中位数已到 45.5ms、长尾 167ms，纯检索仍 10ms。
# 口径不分，每个窗口都会把「进程启动 + 并发」读成「检索退化」，并去删库/改阈值。
printf '%s' "$(grep 'add R3 WARN' "$SCRIPT" || true)" | grep -q '端到端' \
  || fail "脚本 R3 WARN 文案未区分端到端口径"
r3_basis="$(sed -n '/\*\*R3 的 50ms\*\*/,/\*\*R2 的 30%\*\*/p' "$DOC")"
[ -n "$r3_basis" ] || fail "§3 找不到 R3 阈值依据段"
printf '%s' "$r3_basis" | grep -q '端到端' \
  || fail "§3 R3 依据段未区分端到端口径"
printf '%s' "$r3_basis" | grep -q '纯检索' \
  || fail "§3 R3 依据段未保留纯检索对照值"
# 反向断言：两处都不许再出现「9ms 是典型值 / 实测量级 9–15ms」的旧口径。
if grep -q '典型值 ~9ms' "$SCRIPT"; then
  fail "脚本 R3 WARN 退回「典型值 ~9ms」旧口径"
fi
if printf '%s' "$r3_basis" | grep -q '实测量级 9–15ms'; then
  fail "§3 R3 依据段退回「实测量级 9–15ms」旧口径"
fi
# ⑨ R3 副本对照法必须两处都在：这是 2026-11-20 唯一能把「运行态慢」与「代码慢」分开的
# 手段（同二进制对库副本 6–15ms / 活库 43–205ms）。删了就只剩拍脑袋分因。
printf '%s' "$(grep 'add R3 WARN' "$SCRIPT" || true)" | grep -q '副本对照' \
  || fail "脚本 R3 WARN 文案缺「副本对照」分流手段"
printf '%s' "$r3_basis" | grep -q '库副本对照' \
  || fail "§3 R3 依据段缺库副本对照法与实测值"

# ⑨b R3 的归因必须是「尚未定论」，不许把已否证的两个原因写回判据文案。
# 实证：2026-11-20 对照实验——整目录副本挂一个常驻 `mem mcp` 6 次 11–13ms（中位 11.5ms）；
# 带 4.2MB `-wal` 的副本 11.7ms，与只含主库文件的副本同值。故「常驻连接」与「WAL 常驻」
# 单独都解释不了活库 150–205ms，写回文案会让下一轮照着错原因排障。
r3_warn="$(grep 'add R3 WARN' "$SCRIPT" || true)"
if printf '%s' "$r3_warn" | grep -q '常驻连接'; then
  fail "脚本 R3 WARN 把「常驻连接」当已定原因（2026-11-20 已否证）"
fi
if printf '%s' "$r3_warn" | grep -q 'WAL 常驻'; then
  fail "脚本 R3 WARN 把「WAL 常驻」当已定原因（2026-11-20 已否证）"
fi
# 正向断言：否证结论与 spool 旁证必须留在文案里，否则下一轮会重做同一实验。
printf '%s' "$r3_warn" | grep -q '否证' \
  || fail "脚本 R3 WARN 缺「mcp 数量 / WAL 大小已否证」的结论"
printf '%s' "$r3_warn" | grep -q 'spool' \
  || fail "脚本 R3 WARN 缺「本会话 spool 可作写失败旁证」的提示"
printf '%s' "$r3_basis" | grep -q '已否证' \
  || fail "§3 R3 依据段缺否证结论（会重复归因到 mcp/WAL）"

# ⑩ R11 语义：帧数量的是 -wal **文件高水位**，不等于数据滞留。
# 实证：2026-11-20 仅取主库文件（不含 -wal）的快照仍含 5249/5264 条观测，checkpoint 一直在推进，
# 而 -wal 稳定 4.0MB；此前 detail 断言「被动 checkpoint 未跟上」，据此外推「WAL 拖慢读侧」缺依据。
grep -q '被动 checkpoint 未跟上' "$SCRIPT" \
  && fail "脚本 R11 detail 退回「被动 checkpoint 未跟上」的错误归因"
printf '%s' "$(grep 'add R11 WARN' "$SCRIPT" || true)" | grep -q '不代表数据滞留' \
  || fail "脚本 R11 WARN 未说明「帧数≠滞留」"
printf '%s' "$(grep 'add R11 WARN' "$SCRIPT" || true)" | grep -q '高水位' \
  || fail "脚本 R11 WARN 未用「高水位」口径"
r11_row="$(grep -E '^\| \*\*R11\*\*' "$DOC" | head -1)"
[ -n "$r11_row" ] || fail "§3 找不到 R11 行"
printf '%s' "$r11_row" | grep -q '高水位' \
  || fail "§3 R11 名称未改成「WAL 高水位」"
printf '%s' "$r11_row" | grep -q 'WAL 堆积' \
  && fail "§3 R11 名称退回「WAL 堆积」"
r11_basis="$(sed -n '/\*\*R11 的 1000 \/ 2000 帧\*\*/,/\*\*R12 的 0 \/ 3/p' "$DOC")"
[ -n "$r11_basis" ] || fail "§3 找不到 R11 阈值依据段"
printf '%s' "$r11_basis" | grep -q '文件高水位' \
  || fail "§3 R11 依据段未说明帧数是文件高水位"
printf '%s' "$r11_basis" | grep -q '5249/5264' \
  || fail "§3 R11 依据段缺主库独立快照的实证数据"
r11_action="$(sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -E '^\| R11 ' || true)"
printf '%s' "$r11_action" | grep -q 'log == checkpointed' \
  || fail "§5 R11 处置未给「log == checkpointed 即无滞留」的判据"

# ⑩ R7 计量对象：2026-11-22 前脚本只数 `*.jsonl`，漏掉 drain 认领后残留的
# `*.jsonl.processing`——那种段明明还在盘上（重启后 DrainSpool 仍会读它），
# 判据却数 0 报 PASS。文档与脚本必须同时覆盖两种后缀，且 detail 要能区分
# 「下次会补写」与「重试已跑但没消化」。
r7_row="$(grep -E '^\| \*\*R7\*\*' "$DOC" | head -1)"
[ -n "$r7_row" ] || fail "§3 找不到 R7 行"
printf '%s' "$r7_row" | grep -q 'jsonl.processing' \
  || fail "§3 R7 未把 *.jsonl.processing 计入（口径与 isSpoolSegment 脱钩）"
grep -q "SPOOL_STUCK=\$(find" "$SCRIPT" \
  || fail "脚本 R7 未统计 *.jsonl.processing"
grep -q 'SPOOL_TOTAL=\$(( ${SPOOL_FILES:-0} + ${SPOOL_STUCK:-0} ))' "$SCRIPT" \
  || fail "脚本 R7 判级未按 jsonl + processing 合计"
grep -q 'mode // "") == "spool-drain"' "$SCRIPT" \
  || fail "脚本 R7 未读 spool-drain 审计（无法区分「会补写」与「补写在失败」）"
# 文案在 R7_TAIL 里组装（add 行只拼接变量），所以按全文断言，不按某一行。
grep -q '重试已跑但没消化' "$SCRIPT" \
  || fail "脚本 R7 detail 缺「重试已跑但没消化」分支"
r7_action="$(sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -E '^\| R7 ' || true)"
printf '%s' "$r7_action" | grep -q '重试本身在失败' \
  || fail "§5 R7 处置未说明「同一文件跨会话存活即重试本身在失败」"

# ⑫ R16 维护可见性：24h 阈值两侧同步；巡检只报，处置命令只出现在文案里。
# 实证动机：S4 已有 backlog 读数，但没有任何判据回答「积压是没人看还是维护没跑」——
# 24h = 12h 巡检窗口 ×2，连续两个窗口没成功维护才算无人值守断了。
r16_row="$(grep -E '^\| \*\*R16\*\*' "$DOC" | head -1 || true)"
[ -n "$r16_row" ] || fail "§3 找不到 R16 行"
printf '%s' "$r16_row" | grep -q '24h' || fail "§3 R16 的维护滞后阈值不是 24h"
printf '%s' "$r16_row" | grep -q 'open_bundles' || fail "§3 R16 未写明读 backlog.open_bundles"
printf '%s' "$r16_row" | grep -q 'bundles_isolated' || fail "§3 R16 未写明读 backlog.bundles_isolated"
grep -q 'R16_MAINTAIN_MAX_AGE_H=24' "$SCRIPT" || fail "脚本 R16 的阈值常量不是 24（小时）"
grep -q 'mem maintain --catch-up' "$SCRIPT" || fail "脚本 R16 未给处置命令 mem maintain --catch-up"
# 负向：巡检本身仍然只读，不得调用 maintain 本体。
grep -q -- '"$MEM_BIN" maintain' "$SCRIPT" && fail "脚本调用了 mem maintain（巡检必须只读）"
r16_action="$(sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -E '^\| R16 ' || true)"
printf '%s' "$r16_action" | grep -q 'mem maintain --catch-up' \
  || fail "§5 R16 处置未给 mem maintain --catch-up"
# §4 不降级清单必须点名 R16：维护滞后与库里有多少条记忆无关。
sed -n '/^## 4\./,/^## 5\./p' "$DOC" | grep -q 'R16' \
  || fail "§4 冷启动例外未把 R16 列入「不降级」"

# ⑬ R17 证据准入 SLA：15 天（1296000 秒）阈值两侧一致；数据源只用同一次 stats --json；
# 不设 FAIL；§5 给人工命令但巡检仍只读。设计依据：design-data-lifecycle.md §5 休眠期豁免。
r17_row="$(grep -E '^\| \*\*R17\*\*' "$DOC" | head -1 || true)"
[ -n "$r17_row" ] || fail "§3 找不到 R17 行"
printf '%s' "$r17_row" | grep -Fq '15 天'     || fail "§3 R17 的 SLA 不是 15 天"
printf '%s' "$r17_row" | grep -Fq '1296000'   || fail "§3 R17 的秒数不是 1296000"
grep -Fq 'R17_MAX_AGE_S=$(( 15 * 86400 ))' "$SCRIPT" \
  || fail "脚本 R17 的 SLA 常量不是 15*86400（1296000 秒）"
grep -Fq '1296000 秒' "$SCRIPT" || fail "脚本 R17 detail 未报 1296000 秒"
# 数据源两侧同名，且脚本只读同一份 STATS_JSON 的 backlog，不许另起 sqlite / CLI。
for field in evidence_backlog_sources oldest_unconsumed_evidence_age_seconds; do
  printf '%s' "$r17_row" | grep -Fq "$field" || fail "§3 R17 未写明读 backlog.$field"
  grep -Fq ".backlog.$field" "$SCRIPT" || fail "脚本 R17 未从 backlog.$field 取数"
done
stats_calls="$(grep -Fc '"$MEM_BIN" stats --json' "$SCRIPT")"
[ "$stats_calls" -eq 1 ] || fail "mem stats --json 应只调用一次（R17 必须复用），实际 ${stats_calls} 次"
if grep -q 'sqlite3' "$SCRIPT"; then
  fail "脚本引入 sqlite3 调用（R17 必须只用 stats --json）"
fi
# 无 FAIL 档：文档 FAIL 列是「—...」，脚本也无 add R17 FAIL。
r17_fail_col="$(printf '%s' "$r17_row" | awk -F'|' '{gsub(/[ \t]/,"",$7); print $7}')"
case "$r17_fail_col" in
  —*) ;;
  *) fail "§3 R17 的 FAIL 档应为空（—），实际: ${r17_fail_col}" ;;
esac
if grep -q 'add R17 FAIL' "$SCRIPT"; then
  fail "脚本 R17 设了 FAIL 档（休眠期豁免，参照 R16 不设 FAIL）"
fi
r17_action="$(sed -n '/^## 5\./,/^## 6\./p' "$DOC" | grep -E '^\| R17 ' || true)"
[ -n "$r17_action" ] || fail "§5 缺 R17 处置行"
printf '%s' "$r17_action" | grep -Fq 'mem maintain --catch-up' \
  || fail "§5 R17 处置未给 mem maintain --catch-up"
sed -n '/^## 4\./,/^## 5\./p' "$DOC" | grep -Fq 'R17' \
  || fail "§4 冷启动例外未把 R17 列入「不降级」"
sed -n '/^## 6\./,/^## 7\./p' "$DOC" | grep -Fq 'R17' \
  || fail "§6 只读说明未覆盖 R17"
grep -Fq '未消费证据见 R17' "$SCRIPT" || fail "脚本 R16 detail 未指向 R17 读数"
# 只读边界：处置命令只出现在文案里，脚本不得自己调用。
if grep -q -- '"$MEM_BIN" maintain' "$SCRIPT"; then
  fail "脚本调用了 mem maintain（R17 巡检必须只读）"
fi
if grep -q -- '"$MEM_BIN" gc' "$SCRIPT"; then
  fail "脚本调用了 mem gc（R17 巡检必须只读）"
fi

# ⑭ R12 的 by_actor 自动化率只作 detail：新字段有分布与「默认关闭」提示，
# 旧字段只写一行说明；两侧文档/脚本都要有，且不得新增阈值或改级别。
grep -Fq 'by_actor' "$DOC" || fail "§3 R12 依据段未说明 by_actor 自动化率只作 detail"
grep -Fq '旧二进制无 by_actor' "$SCRIPT" || fail "脚本 R12 缺「旧二进制无 by_actor」分支"
grep -Fq 'model-auto=0 可能是默认关闭而非故障' "$SCRIPT" \
  || fail "脚本 R12 缺「model-auto=0 可能是默认关闭」提示"
if grep -q 'add R12 .*by_actor.*FAIL' "$SCRIPT"; then
  fail "脚本 R12 把 by_actor 用于判级（只能作 detail）"
fi

echo "ok  watch-sync: $(printf '%s\n' "$doc_ids" | wc -l | tr -d ' ') 个判据 ID 双向一致；R8 阈值 5/15 与「只数可裁决」口径同步；R12 阈值 >3 与断链分支同步、by_actor 只作 detail；R13 阈值 3600/8000 与无样本分支同步；§5 含 R3 处置行；R15 TTL 14 天与「只报不处置」的只读边界同步；R15 S13 终态出口（mem gc --retention）两处同步且 TTL_DAYS 定值不漂移；R16 维护可见性 24h 阈值与处置命令同步；R17 证据准入 SLA 15 天/1296000 秒两侧同步、无 FAIL 档、只读单一 stats 来源；R3 端到端/纯检索口径与副本对照法两处同步；R11 高水位口径（帧数≠滞留）两处同步；枚举到 R${max_r}"
