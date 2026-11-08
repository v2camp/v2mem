#!/usr/bin/env bash
set -euo pipefail
# 回归：写侧延迟 R10 / WAL 堆积 R11 的分级边界，逐格对应 docs/ops-watch.md §3 判据表。
# 用临时 V2MEM_DIR + mem stub，不触碰生产库；只断言 --json 输出的 R10/R11 级别。
# 重点守住两条口径：R10 只取 stop（不与 obs-append 双计）、无样本记 INFO 而非 PASS。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WATCH="$ROOT/scripts/watch-v2mem.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NOW="$(date +%s)"

# run_case <名称> <stop_ms|none> <wal_frames> <obs_append=yes|no>
run_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4"
  local d="$TMP/$name"
  mkdir -p "$d/v2mem" "$d/wb" "$d/home"

  cat > "$d/mem-stub" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  stats) echo "总条数 10" ;;
  candidates) echo "没有候选" ;;
esac
exit 0
STUB
  chmod +x "$d/mem-stub"

  # 四个事件齐全，避免 I2=FAIL 让脚本以非零退出；本测试只断言 R10/R11。
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
    bash "$WATCH" --json 2>/dev/null || true
}

# assert_case <名称> <stop_ms|none> <wal_frames> <obs> <R10期望> <R11期望>
assert_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4" want_r10="$5" want_r11="$6"
  local out got
  out="$(run_case "$name" "$stop_ms" "$frames" "$with_obs")"
  for pair in "R10:$want_r10" "R11:$want_r11"; do
    local id="${pair%%:*}" want="${pair##*:}"
    got="$(printf '%s' "$out" | jq -r --arg id "$id" '.items[] | select(.id==$id) | .level')"
    if [ "$got" != "$want" ]; then
      echo "FAIL $name: $id want=$want got=$got" >&2
      printf '%s\n' "$out" >&2
      exit 1
    fi
  done
  printf 'ok  %-24s R10=%-4s R11=%-4s\n' "$name" "$want_r10" "$want_r11"
}

# R10 边界：<1000 PASS / 1000–3000 WARN / >3000 FAIL / 无样本 INFO
assert_case r10_pass            100 0 no  PASS PASS
assert_case r10_warn_lower     1000 0 no  WARN PASS
assert_case r10_warn_upper     3000 0 no  WARN PASS
assert_case r10_fail           3001 0 no  FAIL PASS
assert_case r10_no_sample      none 0 no  INFO PASS
# stop=1000 与 obs-append=9000 共存：只取 stop，不得被拉成 FAIL
assert_case r10_no_double_count 1000 0 yes WARN PASS

# R11 边界：≤1000 PASS / 1001–2000 WARN / >2000 FAIL
assert_case r11_pass_boundary  100 1000 no PASS PASS
assert_case r11_warn           100 1500 no PASS WARN
assert_case r11_fail           100 2500 no PASS FAIL

echo "watch-v2mem 判据回归 9/9 PASS"
