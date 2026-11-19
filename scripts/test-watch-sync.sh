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

# ⑦ R15 阈值与自主边界：TTL 14 天两侧同步；「只迁移不删除」的保护不许被删。
# 这条判据是唯一会写库的（gc --candidates），边界退化比阈值漂移更危险：
# 少了 ACC_OLD 检查，脚本就会替人执行 GCCandidates 里那条 DELETE。
r15_row="$(grep -E '^\| \*\*R15\*\*' "$DOC" | head -1)"
[ -n "$r15_row" ] || fail "§3 找不到 R15 行"
printf '%s' "$r15_row" | grep -q '14 天' || fail "§3 R15 的 TTL 不是 14 天"
grep -q 'TTL_DAYS=14'  "$SCRIPT" || fail "脚本 R15 的 TTL_DAYS 不是 14"
grep -q 'GC_DISABLED'  "$SCRIPT" || fail "脚本 R15 缺 --no-gc 逃生开关"
grep -q 'ACC_OLD'      "$SCRIPT" || fail "脚本 R15 缺「待删已采纳行则转人工」保护"

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

echo "ok  watch-sync: $(printf '%s\n' "$doc_ids" | wc -l | tr -d ' ') 个判据 ID 双向一致；R8 阈值 5/15 同步；R12 阈值 >3 与断链分支同步；R13 阈值 3600/8000 与无样本分支同步；§5 含 R3 处置行；R15 TTL 14 天与转人工保护同步；R3 端到端/纯检索口径两处同步；枚举到 R${max_r}"
