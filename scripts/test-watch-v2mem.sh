#!/usr/bin/env bash
set -euo pipefail
# 回归：R8 候选积压 / R9 载荷残留 / R10 写侧延迟 / R11 WAL 堆积 的分级边界，
# 逐格对应 docs/ops-watch.md §3 判据表。
# 用临时 V2MEM_DIR + mem stub，不触碰生产库；只断言 --json 输出的级别。
# 重点守住三条口径：R8 按工程分组取最大值（跨工程不相加）、R10 只取 stop
# （不与 obs-append 双计）、无样本记 INFO 而非 PASS。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WATCH="$ROOT/scripts/watch-v2mem.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NOW="$(date +%s)"

# cand_json <project> <n> [content]：造 n 条 pending 候选。
# content 可塞 transcript 载荷用于 R9；默认是可读的原子事实。
cand_json() {
  jq -nc --arg p "$1" --argjson n "$2" --arg c "${3:-ok}" \
    '[range(0;$n) | {id: ("c" + $p + (.|tostring)), project:$p,
                     status:"pending", content:$c, kind:"fact"}]'
}

merge_json() { jq -nc --argjson a "$1" --argjson b "$2" '$a + $b'; }

# run_case <名称> <stop_ms|none> <wal_frames> <obs_append> <candidates_json>
run_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4" cands="$5"
  local d="$TMP/$name"
  mkdir -p "$d/v2mem" "$d/wb" "$d/home"

  cat > "$d/mem-stub" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  stats) echo "总条数 10" ;;
  candidates) printf '%s\n' "${STUB_CAND_JSON:-[]}" ;;
esac
exit 0
STUB
  chmod +x "$d/mem-stub"

  # 四个事件齐全，避免 I2=FAIL 让脚本以非零退出；本测试只断言 R8/R9/R10/R11。
  cat > "$d/wb/settings.json" <<'SETTINGS'
{"hooks":{
  "SessionStart":     [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "UserPromptSubmit": [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "Stop":             [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "SessionEnd":       [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}]
}}
SETTINGS

  {
    printf '{"ts":%s,"event":"session-start","ms":10,"empty":false}\n' "$NOW"
    printf '{"ts":%s,"event":"prompt-submit","ms":12,"empty":false}\n' "$NOW"
    if [ "$stop_ms" != "none" ]; then
      printf '{"ts":%s,"event":"stop","ms":%s}\n' "$NOW" "$stop_ms"
    fi
    if [ "$with_obs" = "yes" ]; then
      printf '{"ts":%s,"event":"obs-append","ms":9000}\n' "$NOW"
    fi
  } > "$d/v2mem/audit.jsonl"

  if [ "$frames" -gt 0 ]; then
    dd if=/dev/zero of="$d/v2mem/mem.db-wal" bs=4096 count="$frames" 2>/dev/null
  fi

  V2MEM_DIR="$d/v2mem" \
  WORKBUDDY_CONFIG_DIR="$d/wb" \
  HOME="$d/home" \
  MEM_BIN="$d/mem-stub" \
  STUB_CAND_JSON="$cands" \
    bash "$WATCH" --json 2>/dev/null || true
}

# assert_case <名称> <stop_ms|none> <wal_frames> <obs> <cands> <R10> <R11> [R8] [R9]
assert_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4" cands="$5" want_r10="$6" want_r11="$7"
  local want_r8="${8:-}" want_r9="${9:-}"
  local out pairs="R10:$want_r10 R11:$want_r11" got
  [ -n "$want_r8" ] && pairs="$pairs R8:$want_r8"
  [ -n "$want_r9" ] && pairs="$pairs R9:$want_r9"
  out="$(run_case "$name" "$stop_ms" "$frames" "$with_obs" "$cands")"
  for pair in $pairs; do
    local id="${pair%%:*}" want="${pair##*:}"
    got="$(printf '%s' "$out" | jq -r --arg id "$id" '.items[] | select(.id==$id) | .level')"
    if [ "$got" != "$want" ]; then
      echo "FAIL $name: $id want=$want got=$got" >&2
      printf '%s\n' "$out" >&2
      exit 1
    fi
  done
  printf 'ok  %-26s R10=%-4s R11=%-4s R8=%-4s R9=%-4s\n' \
    "$name" "$want_r10" "$want_r11" "${want_r8:--}" "${want_r9:--}"
}

EMPTY='[]'

# R10 边界：<1000 PASS / 1000–3000 WARN / >3000 FAIL / 无样本 INFO；顺带断言 R8/R9 空库 PASS。
assert_case r10_pass            100 0 no "$EMPTY" PASS PASS PASS PASS
assert_case r10_warn_lower     1000 0 no "$EMPTY" WARN PASS PASS PASS
assert_case r10_warn_upper     3000 0 no "$EMPTY" WARN PASS PASS PASS
assert_case r10_fail           3001 0 no "$EMPTY" FAIL PASS PASS PASS
assert_case r10_no_sample      none 0 no "$EMPTY" INFO PASS PASS PASS
# stop=1000 与 obs-append=9000 共存：只取 stop，不得被拉成 FAIL。
assert_case r10_no_double_count 1000 0 yes "$EMPTY" WARN PASS PASS PASS

# R11 边界：≤1000 PASS / 1001–2000 WARN / >2000 FAIL。
assert_case r11_pass_boundary  100 1000 no "$EMPTY" PASS PASS PASS PASS
assert_case r11_warn           100 1500 no "$EMPTY" PASS WARN PASS PASS
assert_case r11_fail           100 2500 no "$EMPTY" PASS FAIL PASS PASS

# R8 边界：单工程 ≤5 PASS / 6–15 WARN / >15 FAIL（阈值未改，只是单位变成「单个工程」）。
assert_case r8_pass_5          100 0 no "$(cand_json A 5)"  PASS PASS PASS PASS
assert_case r8_warn_6          100 0 no "$(cand_json A 6)"  PASS PASS WARN PASS
assert_case r8_warn_15         100 0 no "$(cand_json A 15)" PASS PASS WARN PASS
assert_case r8_fail_16         100 0 no "$(cand_json A 16)" PASS PASS FAIL PASS
# 核心回归：两个工程各 4 条（全局 8）不得按总数误判 WARN；窗口取最坏工程。
assert_case r8_multi_no_sum    100 0 no "$(merge_json "$(cand_json A 4)" "$(cand_json B 4)")" PASS PASS PASS PASS
assert_case r8_multi_worst     100 0 no "$(merge_json "$(cand_json A 16)" "$(cand_json B 2)")" PASS PASS FAIL PASS

# R9 等价性：content 含 transcript 原始 JSON 载荷 → WARN（与旧版 grep 口径一致）。
assert_case r9_noise_warn      100 0 no "$(cand_json A 1 '{"id":1,"type":"x"}')" PASS PASS PASS WARN

# 采集失败保护：不可解析的 JSON 不得静默判 PASS。
assert_case r8_read_fail       100 0 no 'not-json' PASS PASS WARN WARN

echo "watch-v2mem 判据回归 17/17 PASS"
