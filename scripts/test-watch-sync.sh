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

echo "ok  watch-sync: $(printf '%s\n' "$doc_ids" | wc -l | tr -d ' ') 个判据 ID 双向一致；R8 阈值 5/15 同步；枚举到 R${max_r}"
