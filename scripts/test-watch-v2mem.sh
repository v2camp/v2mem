#!/usr/bin/env bash
set -euo pipefail
# 回归：R8 候选积压 / R9 载荷残留 / R10 写侧延迟 / R11 WAL 堆积 /
# R12 写侧自动决策链路 / R13 注入体量 的分级边界，逐格对应 docs/ops-watch.md §3 判据表。
# 用临时 V2MEM_DIR + mem stub，不触碰生产库；只断言 --json 输出的级别。
# 重点守住四条口径：R8 按工程分组取最大值（跨工程不相加）、R10 只取 stop
# （不与 obs-append 双计）、无样本记 INFO 而非 PASS、R12 判级只依赖 audit
# （eval activity 读不到时仍须独立判级）。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WATCH="$ROOT/scripts/watch-v2mem.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NOW="$(date +%s)"
N=0

# cand_json <project> <n> [content]：造 n 条 pending 候选。
# content 可塞 transcript 载荷用于 R9；默认是可读的原子事实。
cand_json() {
  jq -nc --arg p "$1" --argjson n "$2" --arg c "${3:-ok}" \
    '[range(0;$n) | {id: ("c" + $p + (.|tostring)), project:$p,
                     status:"pending", content:$c, kind:"fact"}]'
}

merge_json() { jq -nc --argjson a "$1" --argjson b "$2" '$a + $b'; }

# run_case <名称> <stop_ms|none> <wal_frames> <obs_append> <candidates_json> [audit_extra] [activity_json] [audit_lines]
run_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4" cands="$5"
  local extra_audit="${6:-}" activity="${7:-}" audit_lines="${8:-}"
  local d="$TMP/$name"
  mkdir -p "$d/v2mem" "$d/wb" "$d/home"

  cat > "$d/mem-stub" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  stats) echo "总条数 10" ;;
  candidates) printf '%s\n' "${STUB_CAND_JSON:-[]}" ;;
  eval) printf '%s\n' "${STUB_ACTIVITY_JSON:-}" ;;
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
  if [ -n "$audit_lines" ]; then
    # 给了完整审计体就整段替换：R13 要区分「无读侧记录」与「只有旧格式记录」，
    # 默认夹具恒有两条读侧记录，做不到这两种场景。
    printf '%s' "$audit_lines" > "$d/v2mem/audit.jsonl"
  fi
  if [ -n "$extra_audit" ]; then
    printf '%s' "$extra_audit" >> "$d/v2mem/audit.jsonl"
  fi

  if [ "$frames" -gt 0 ]; then
    dd if=/dev/zero of="$d/v2mem/mem.db-wal" bs=4096 count="$frames" 2>/dev/null
  fi

  V2MEM_DIR="$d/v2mem" \
  WORKBUDDY_CONFIG_DIR="$d/wb" \
  HOME="$d/home" \
  MEM_BIN="$d/mem-stub" \
  STUB_CAND_JSON="$cands" \
  STUB_ACTIVITY_JSON="$activity" \
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
  N=$((N+1))
  printf 'ok  %-26s R10=%-4s R11=%-4s R8=%-4s R9=%-4s\n' \
    "$name" "$want_r10" "$want_r11" "${want_r8:--}" "${want_r9:--}"
}

EMPTY='[]'

# R12 夹具：造 distill 审计行与 mem eval activity --json 的返回。
# 判级只依赖前者；后者只补诊断文案，所以要单独测「读不到也不影响判级」。
dline() {
  printf '{"ts":%s,"event":"distill","source":"%s","mode":"%s","failed":%s}\n' "$NOW" "$1" "$2" "$3"
}
decline() { printf '{"ts":%s,"event":"%s"}\n' "$NOW" "$1"; }
activity_json() {
  jq -nc --arg v "$1" '{exists:true, verdict:$v, summary:{observations:1,distill_ok:1,distill_fail:0,accepts:0,rejects:0}}'
}

# assert_r12 <名称> <obs yes/no> <audit_extra> <activity_json> <want_level> [detail 子串]
assert_r12() {
  local name="$1" with_obs="$2" extra="$3" activity="$4" want="$5" needle="${6:-}"
  local out got detail
  out="$(run_case "$name" 100 0 "$with_obs" "$EMPTY" "$extra" "$activity")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R12") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R12") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: R12 want=$want got=$got" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL $name: R12 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R12=%-4s\n' "$name" "$want"
}

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

# R12 边界：无样本 INFO / 有蒸馏且 0 失败 PASS / 断链 WARN / 失败 1–3 WARN / >3 FAIL。
assert_r12 r12_no_sample        no  '' '' INFO
assert_r12 r12_pass_rules       yes "$(dline distill ok false)$(dline distill empty false)" "$(activity_json '本次任务有沉淀')" PASS '蒸馏 2'
assert_r12 r12_broken_chain     yes '' '' WARN 'MEM_DISTILL_SPAWN'
assert_r12 r12_fail_1           yes "$(dline distill fail true)" '' WARN
assert_r12 r12_fail_3           yes "$(dline distill fail true)$(dline distill fail true)$(dline distill fail true)" '' WARN
assert_r12 r12_fail_4           yes "$(dline distill fail true)$(dline distill fail true)$(dline distill fail true)$(dline distill fail true)" '' FAIL
assert_r12 r12_proposal_split   yes "$(dline proposal ok false)$(dline distill empty false)" '' PASS 'proposal 1'
assert_r12 r12_empty_not_fail   yes "$(dline distill empty false)$(dline distill noop false)" '' PASS '合法空 2'
assert_r12 r12_invalid_mode     yes "$(dline proposal invalid false)" '' WARN
assert_r12 r12_activity_badjson yes "$(dline distill ok false)" 'not-json' PASS
assert_r12 r12_activity_verdict yes "$(dline distill ok false)" "$(activity_json '有观测无采纳')" PASS '有观测无采纳'
assert_r12 r12_decisions_shown  yes "$(dline distill ok false)$(decline accept)$(decline reject)" '' PASS 'accept 1 reject 1'

# R13 夹具：iline 造带 inject_bytes 的读侧记录；oldline 造升级前无该字段的旧记录。
iline()   { printf '{"ts":%s,"event":"%s","inject_bytes":%s}\n' "$NOW" "${2:-session-start}" "$1"; }
oldline() { printf '{"ts":%s,"event":"%s","ms":9,"empty":false}\n' "$NOW" "${1:-prompt-submit}"; }

# assert_r13 <名称> <audit_lines> <want_level> [detail 子串]
assert_r13() {
  local name="$1" lines="$2" want="$3" needle="${4:-}"
  local out got detail
  out="$(run_case "$name" 100 0 no "$EMPTY" '' '' "$lines")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R13") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R13") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: R13 want=$want got=$got" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL $name: R13 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R13=%-4s\n' "$name" "$want"
}

# R13 边界：≤3600 PASS（含边界）/ 3601–8000 WARN（含 8000）/ >8000 FAIL。
assert_r13 r13_pass_3599       "$(iline 3599)"                            PASS
assert_r13 r13_pass_3600       "$(iline 3600)"                            PASS
assert_r13 r13_warn_3601       "$(iline 3601)"                            WARN
assert_r13 r13_warn_8000       "$(iline 8000)"                            WARN
assert_r13 r13_fail_8001       "$(iline 8001)"                            FAIL '8001'
# 只有旧记录（无 inject_bytes）：不得当 0 判 PASS，应 INFO 并点明是升级前的记录。
assert_r13 r13_old_records     "$(oldline prompt-submit)"                 INFO '旧记录'
# 读侧事件有三个：只有 pre-compact 的旧记录同样算「有读侧记录」，不得错报「无读侧记录」。
assert_r13 r13_old_precompact  "$(oldline pre-compact)"                  INFO '旧记录'
# pre-compact 带字段时也必须计入最大值（不只 session-start / prompt-submit）。
assert_r13 r13_precompact_over "$(iline 9000 pre-compact)"                FAIL '9000'
# 窗口内无读侧记录（只有写侧观测）：同为 INFO，但说明必须不同。
assert_r13 r13_no_read_side    "$(printf '{"ts":%s,"event":"obs-append","ms":9}\n' "$NOW")" INFO '无读侧记录'
# 混合：旧记录 + 8001 必须 FAIL，证明缺失字段不会被当 0 拉低最大值。
assert_r13 r13_mixed_old_over  "$(oldline prompt-submit)$(iline 8001)"    FAIL '8001'

echo "watch-v2mem 判据回归 ${N}/${N} PASS"
