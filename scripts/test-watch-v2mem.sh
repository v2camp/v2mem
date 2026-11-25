#!/usr/bin/env bash
set -euo pipefail
# 回归：R3 越线文案与分段变量边界 / R8 候选积压 / R9 载荷残留 / R10 写侧延迟 /
# R11 WAL 堆积 / R12 写侧自动决策链路 / R13 注入体量 / R14 归档体量 / R15 候选 TTL
# 的分级边界，逐格对应 docs/ops-watch.md §3 判据表。
# 用临时 V2MEM_DIR + mem stub，不触碰生产库；只断言 --json 输出的级别。
# 重点守住四条口径：R8 按工程分组取最大值（跨工程不相加）、R10 只取 stop
# （不与 obs-append 双计）、无样本记 INFO 而非 PASS、R12 判级只依赖 audit
# （eval activity 读不到时仍须独立判级）。R14 的口径是**落盘量**：样本必须实写
# 到 evidence 目录（APFS 上 truncate 的稀疏文件 du 记 0），且只判字节不判条数。
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WATCH="$ROOT/scripts/watch-v2mem.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
NOW="$(date +%s)"
N=0

# R15 只读断言靠 stub 落盘的完整 argv（空写集 / 无 --status accepted 额外查询）。
ARGV_LOG="$TMP/argv.log"
: > "$ARGV_LOG"

# cand_json <project> <n> [content] [created_at]：造 n 条 pending 候选。
# content 可塞 transcript 载荷用于 R9；默认是可读的原子事实。
# created_at 默认取 NOW（窗口内），传更早的时间戳即「存量」。
cand_json() {
  jq -nc --arg p "$1" --argjson n "$2" --arg c "${3:-ok}" --argjson ts "${4:-$NOW}" \
    '[range(0;$n) | {id: ("c" + $p + (.|tostring)), project:$p,
                     status:"pending", content:$c, kind:"fact", created_at:$ts}]'
}

merge_json() { jq -nc --argjson a "$1" --argjson b "$2" '$a + $b'; }

# build_fixture <dir> <stop_ms|none> <wal_frames> <obs yes/no> [audit_extra] [audit_lines] [prompts]
# 造 mem stub + 四事件 settings + 审计日志 + 按需的 -wal。R14 复用同一套夹具，
# 只额外往 evidence 目录写文件，所以抽出来而不是在 run_case 里内联。
# prompts 覆盖默认的单条 prompt-submit（R2 按工程口径要造多条提问）；
# audit_lines 非空时整段替换审计体（R13 要区分「无读侧记录」与「只有旧格式记录」）。
build_fixture() {
  local d="$1" stop_ms="$2" frames="$3" with_obs="$4"
  local extra_audit="${5:-}" audit_lines="${6:-}" prompts="${7:-}"
  mkdir -p "$d/v2mem" "$d/wb" "$d/home"

  cat > "$d/mem-stub" <<'STUB'
#!/usr/bin/env bash
# 记录完整 argv：R15 的「只读」断言就靠它——写子命令一个都不许出现。
printf '%s\n' "$*" >> "${STUB_ARGV_LOG:-/dev/null}"
case "${1:-}" in
  stats)
    echo "总条数 ${STUB_TOTAL:-10}"
    if [ -n "${STUB_PROJ_STATS:-}" ]; then printf '按工程   %s\n' "$STUB_PROJ_STATS"; fi
    ;;
  candidates) printf '%s\n' "${STUB_CAND_JSON:-[]}" ;;
  eval) printf '%s\n' "${STUB_ACTIVITY_JSON:-}" ;;
  # 任何写子命令都会落一个标记文件：⑥ 的「除 watch-state.json 外无写入」才有牙。
  gc|maintain|accept|reject|consolidate|import|ingest)
    : > "${STUB_WRITE_MARK:-/dev/null}" ;;
esac
exit 0
STUB
  chmod +x "$d/mem-stub"

  # 四个事件齐全，避免 I2=FAIL 让脚本以非零退出；本测试只断言 R8–R15。
  cat > "$d/wb/settings.json" <<'SETTINGS'
{"hooks":{
  "SessionStart":     [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "UserPromptSubmit": [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "Stop":             [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}],
  "SessionEnd":       [{"hooks":[{"type":"command","command":"mem hook # v2mem"}]}]
}}
SETTINGS

  {
    # 读侧恒带 search_ms：R3 判这一段，不带就是旧格式记录（按无样本 INFO）。
    printf '{"ts":%s,"event":"session-start","ms":10,"empty":false,"search_ms":3,"open_ms":5}\n' "$NOW"
    if [ -n "$prompts" ]; then
      printf '%s' "$prompts"
    else
      printf '{"ts":%s,"event":"prompt-submit","ms":12,"empty":false,"search_ms":4,"open_ms":5}\n' "$NOW"
    fi
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
}

# run_watch <dir> [candidates_json] [activity_json] [proj_stats] [total]
run_watch() {
  local d="$1" cands="${2:-[]}" activity="${3:-}" proj_stats="${4:-}" total="${5:-}"
  V2MEM_DIR="$d/v2mem" \
  WORKBUDDY_CONFIG_DIR="$d/wb" \
  HOME="$d/home" \
  MEM_BIN="$d/mem-stub" \
  STUB_CAND_JSON="$cands" \
  STUB_ACTIVITY_JSON="$activity" \
  STUB_PROJ_STATS="$proj_stats" \
  STUB_TOTAL="$total" \
  STUB_ARGV_LOG="$ARGV_LOG" \
  STUB_WRITE_MARK="$d/v2mem/write-mark.db" \
    bash "$WATCH" --json 2>/dev/null || true
}

# run_case <名称> <stop_ms|none> <wal_frames> <obs_append> <candidates_json>
#          [audit_extra] [activity_json] [audit_lines] [prompts_json] [proj_stats] [total]
run_case() {
  local name="$1" stop_ms="$2" frames="$3" with_obs="$4" cands="$5"
  local extra_audit="${6:-}" activity="${7:-}" audit_lines="${8:-}"
  local prompts="${9:-}" proj_stats="${10:-}" total="${11:-}"
  local d="$TMP/$name"
  build_fixture "$d" "$stop_ms" "$frames" "$with_obs" "$extra_audit" "$audit_lines" "$prompts"
  run_watch "$d" "$cands" "$activity" "$proj_stats" "$total"
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
  printf '{"ts":%s,"event":"distill","source":"%s","provider":"%s","mode":"%s","failed":%s}\n' "$NOW" "$1" "$2" "$3" "$4"
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

# R7 spool 积压：此前这一判据完全没有用例（判据表登记了，回归脚本里没有），
# 于是「只数 *.jsonl」的漏计活到今天。两个口径都要守住：
#   ① 计数含 *.jsonl.processing——drain 认领（rename）后崩溃/恢复失败留下的段，
#      旧实现数 0 → 明明有积压却判 PASS；
#   ② detail 要区分「下次会补写」与「重试已跑但没消化」，后者才需要人介入。
# assert_r7 <名称> <jsonl 数> <processing 数> <额外审计体> <want_level> [detail 子串]
assert_r7() {
  local name="$1" n_jsonl="$2" n_proc="$3" extra="${4:-}" want="$5" needle="${6:-}"
  local d="$TMP/$name" i out got detail
  build_fixture "$d" 100 0 no "$extra"
  for ((i = 0; i < n_jsonl; i++)); do
    mkdir -p "$d/v2mem/spool/sess-$i"
    : > "$d/v2mem/spool/sess-$i/seg-$i.jsonl"
  done
  for ((i = 0; i < n_proc; i++)); do
    mkdir -p "$d/v2mem/spool/stuck-$i"
    : > "$d/v2mem/spool/stuck-$i/seg-$i.jsonl.processing"
  done
  out="$(run_watch "$d")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R7") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R7") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: R7 want=$want got=$got" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL $name: R7 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R7=%-4s\n' "$name" "$want"
}

# 边界：0 PASS / 1–3 WARN / >3 FAIL。
assert_r7 r7_pass               0 0 '' PASS '写库链路正常'
assert_r7 r7_warn_one           1 0 '' WARN '下次 SessionStart 会补写'
assert_r7 r7_warn_three         3 0 '' WARN ''
assert_r7 r7_fail_four          4 0 '' FAIL '写库持续失败'
# 核心回归：只有已认领未完成的段时，旧实现数 *.jsonl 得 0 判 PASS。
assert_r7 r7_processing_only    0 1 '' WARN '已认领未完成'
assert_r7 r7_processing_counts  3 3 '' FAIL ''
# 回放尝试过且失败：文案不得再承诺「下次会补写」。
R7_FAILED_AUDIT="$(printf '{"ts":%s,"event":"obs-append","mode":"spool-drain","count":0,"failed":1}\n' "$NOW")"
assert_r7 r7_retry_failed       1 0 "$R7_FAILED_AUDIT" WARN '重试已跑但没消化'
# 同一条审计但 failed=0（补写成功过）→ 仍是「会补写」，不误报成失败。
R7_OK_AUDIT="$(printf '{"ts":%s,"event":"obs-append","mode":"spool-drain","count":3,"failed":0}\n' "$NOW")"
assert_r7 r7_retry_ok           1 0 "$R7_OK_AUDIT" WARN '下次 SessionStart 会补写'
# 窗口外的失败记录不算（否则一次历史失败会永久改写文案）。
R7_OLD_AUDIT="$(printf '{"ts":%s,"event":"obs-append","mode":"spool-drain","count":0,"failed":2}\n' "$((NOW - 172800))")"
assert_r7 r7_retry_out_window   1 0 "$R7_OLD_AUDIT" WARN '下次 SessionStart 会补写'

# R8 边界：单工程 ≤5 PASS / 6–15 WARN / >15 FAIL（阈值未改，只是单位变成「单个工程」）。
assert_case r8_pass_5          100 0 no "$(cand_json A 5)"  PASS PASS PASS PASS
assert_case r8_warn_6          100 0 no "$(cand_json A 6)"  PASS PASS WARN PASS
assert_case r8_warn_15         100 0 no "$(cand_json A 15)" PASS PASS WARN PASS
assert_case r8_fail_16         100 0 no "$(cand_json A 16)" PASS PASS FAIL PASS
# 核心回归：两个工程各 4 条（全局 8）不得按总数误判 WARN；窗口取最坏工程。
assert_case r8_multi_no_sum    100 0 no "$(merge_json "$(cand_json A 4)" "$(cand_json B 4)")" PASS PASS PASS PASS
assert_case r8_multi_worst     100 0 no "$(merge_json "$(cand_json A 16)" "$(cand_json B 2)")" PASS PASS FAIL PASS

# R8 口径（2026-11-20 起）：只数 reviewable != false 的 pending，不可裁决的单独报数。
# cand_json_triage <project> <可裁决数> <不可裁决数>
cand_json_triage() {
  jq -nc --arg p "$1" --argjson r "$2" --argjson e "$3" --argjson ts "$NOW" '
    [range(0;$r) | {id:("r"+$p+(.|tostring)), project:$p, status:"pending",
                    content:"根因：建表前必须补幂等迁移", kind:"pitfall",
                    created_at:$ts, reviewable:true}]
  + [range(0;$e) | {id:("e"+$p+(.|tostring)), project:$p, status:"pending",
                    content:"已完成 52/65，进程存活", kind:"pitfall",
                    created_at:$ts, reviewable:false}]'
}

# assert_r8_triage <名称> <cands> <want_level> [detail 子串]
assert_r8_triage() {
  local name="$1" cands="$2" want="$3" needle="${4:-}"
  local out got detail
  out="$(run_case "$name" 100 0 no "$cands")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R8") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R8") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: R8 want=$want got=${got}" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL $name: R8 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R8=%-4s\n' "$name" "$want"
}

# 18 条过程叙述 + 2 条可裁决：判级只看 2（PASS），但 18 必须仍在 detail 里报出来，
# 否则「口径修正」就变成了静默丢弃。
assert_r8_triage r8_exhaust_excluded  "$(cand_json_triage A 2 18)"  PASS '另有 18 条'
# 不可裁决不得掩盖真积压：16 条可裁决仍然 FAIL，不能因为总量里混了废气就降级。
assert_r8_triage r8_exhaust_not_masking "$(cand_json_triage A 16 20)" FAIL '16'
# 缺 reviewable 键（旧二进制 / 老夹具）按可裁决处理，行为与改口径前一致。
assert_case r8_no_triage_key   100 0 no "$(cand_json A 6)" PASS PASS WARN PASS

# R9 等价性：content 含 transcript 原始 JSON 载荷 → WARN（与旧版 grep 口径一致）。
assert_case r9_noise_warn      100 0 no "$(cand_json A 1 '{"id":1,"type":"x"}')" PASS PASS PASS WARN
# R9 第二种形态：工具调用载荷的文本形态（工具名 + 对象字面量）。旧检测式只认
# {"id"…}/{"type"…}，把 `Shell {"command"…` / `Grep {"pattern"…` 判成干净候选——
# 2026-11-15 本机 2 条 11-02 存量候选就是这样漏掉的。
assert_case r9_shell_payload   100 0 no "$(cand_json A 1 'Shell {"command":"git log --oneline"}')" PASS PASS PASS WARN
assert_case r9_grep_payload    100 0 no "$(cand_json A 1 'Grep {"pattern":"Exit Code|stderr"}')" PASS PASS PASS WARN
# 前导空白与换行后紧跟工具名同样算残留。
assert_case r9_indented_payload 100 0 no "$(cand_json A 1 '  Read {"file_path":"/tmp/a"}')" PASS PASS PASS WARN
# 反向保护：正文里提到工具名但不是载荷开头（没有对象字面量紧随）不算残留。
assert_case r9_mention_ok      100 0 no "$(cand_json A 1 '排查时先用 Grep 过滤 stderr，再看 Exit Code')" PASS PASS PASS PASS
assert_case r9_json_in_prose_ok 100 0 no "$(cand_json A 1 '边界样例 {"command": "ls"} 不是载荷开头')" PASS PASS PASS PASS

# R9 新鲜度口径：判级统计全部 pending，但 detail 必须区分「窗口内新增」与「存量」。
# 旧文案恒写「单窗口内产生」，把 11-02 的存量残留说成过滤刚失效（2026-11-16 实测误报）。
OLD=$(( NOW - 14*86400 ))
assert_r9_detail() { # <名称> <cands> <detail 子串>
  local name="$1" cands="$2" needle="$3" out detail got
  out="$(run_case "$name" 100 0 no "$cands")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R9") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R9") | .detail')"
  if [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL $name: R9 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R9=%-4s detail 含 %s\n' "$name" "$got" "$needle"
}
# 窗口内新增：才允许断言过滤失效。
assert_r9_detail r9_fresh_says_fresh "$(cand_json A 1 'Shell {"command":"ls"}' "$NOW")" "窗口内新增 1 条"
# 存量残留：不得再写「单窗口内产生」，要给出年龄。
assert_r9_detail r9_legacy_says_legacy "$(cand_json A 1 'Shell {"command":"ls"}' "$OLD")" "全为存量（最早 14 天前）"
# 混合：新鲜数只算窗口内的，存量单独计数。
assert_r9_detail r9_mixed_counts_fresh \
  "$(merge_json "$(cand_json A 1 'Grep {"pattern":"x"}' "$NOW")" "$(cand_json A 2 'Read {"file_path":"/tmp/a"}' "$OLD")")" \
  "窗口内新增 1 条（另有存量 2 条"
# >3 且全为存量：判 FAIL，但结论落在「先清理、再复查」，不诬指准入逻辑。
assert_r9_detail r9_legacy_fail "$(cand_json A 4 'Shell {"command":"ls"}' "$OLD")" "全为存量（最早 14 天前），非本窗口新增"

# ---------- R15 候选 TTL：只报不处置（巡检只读） ----------
# 口径：pending 超 14 天 → WARN，detail 说明「只报不处置」并给手工命令与静态复核提示。
# 断言的核心不是文案而是**行为**：stub 记完整 argv，写子命令（gc / maintain / accept /
# reject / consolidate / import / ingest）一个都不许出现；除 watch-state.json 外无文件被写。
STALE=$(( NOW - 15*86400 ))     # 越过 14 天阈值
OLD_ACC=$(( NOW - 100*86400 ))  # 90 天前的已采纳行：只作静态提示，不查、不处置

R15_FAILS=0
r15_fail() { echo "FAIL $1: $2" >&2; R15_FAILS=$((R15_FAILS+1)); }
r15_level()  { printf '%s' "$R15_OUT" | jq -r '.items[] | select(.id=="R15") | .level' 2>/dev/null || true; }
r15_detail() { printf '%s' "$R15_OUT" | jq -r '.items[] | select(.id=="R15") | .detail' 2>/dev/null || true; }
r15_ok() { N=$((N+1)); printf 'ok  %-26s R15=%s\n' "$1" "$(r15_level)"; }

# r15_run <名称> <pending_json>：fixture + 清 argv 日志 + 前后文件哈希基线
r15_run() {
  local name="$1" cands="$2"
  R15_DIR="$TMP/$name"
  build_fixture "$R15_DIR" 100 0 no
  : > "$ARGV_LOG"
  R15_BEFORE="$(find "$R15_DIR" -type f ! -name watch-state.json -exec shasum {} + | sort)"
  R15_OUT="$(run_watch "$R15_DIR" "$cands")"
  R15_AFTER="$(find "$R15_DIR" -type f ! -name watch-state.json -exec shasum {} + | sort)"
  R15_ARGV="$(cat "$ARGV_LOG")"
}

# ① 有超期：WARN + 「只报不处置」。
r15_run r15_stale_warn_only "$(cand_json A 2 'ok' "$STALE")"
if [ "$(r15_level)" != WARN ]; then
  r15_fail r15_stale_warn_only "want=WARN got=$(r15_level)（$(r15_detail)）"
elif [[ "$(r15_detail)" != *"只报不处置"* ]]; then
  r15_fail r15_stale_warn_only "detail 缺「只报不处置」，got: $(r15_detail)"
else
  r15_ok r15_stale_warn_only
fi

# ② 无超期：PASS，且 detail 复述与 GCCandidates 同源的 14 天。
r15_run r15_no_stale "$(cand_json A 2)"
if [ "$(r15_level)" != PASS ]; then
  r15_fail r15_no_stale "want=PASS got=$(r15_level)（$(r15_detail)）"
elif [[ "$(r15_detail)" != *"无超过 14 天的 pending"* ]]; then
  r15_fail r15_no_stale "detail 缺「无超过 14 天的 pending」，got: $(r15_detail)"
else
  r15_ok r15_no_stale
fi

# ③ 写子命令全集为空：巡检不许调 gc / maintain / accept / reject / consolidate / import / ingest。
r15_run r15_argv_no_write "$(cand_json A 2 'ok' "$STALE")"
WRITE_HITS="$(printf '%s\n' "$R15_ARGV" | awk 'NF {print $1}' \
  | grep -Ex 'gc|maintain|accept|reject|consolidate|import|ingest' || true)"
if [ -z "$R15_ARGV" ]; then
  r15_fail r15_argv_no_write "stub 没记录到任何 argv，断言无法成立"
elif [ -n "$WRITE_HITS" ]; then
  r15_fail r15_argv_no_write "巡检调了写子命令: $(printf '%s' "$WRITE_HITS" | tr '\n' ' ')"
else
  r15_ok r15_argv_no_write
fi

# ④ 有 90 天前已采纳行：只在 detail 里静态提示「先复核」，不再多做一次 accepted 查询。
r15_run r15_accepted_static \
  "$(merge_json "$(cand_json A 2 'ok' "$STALE")" "$(cand_json B 1 'ok' "$OLD_ACC")")"
if [[ "$(r15_detail)" != *"先复核 90 天前"* ]]; then
  r15_fail r15_accepted_static "detail 缺「先复核 90 天前」静态提示，got: $(r15_detail)"
elif printf '%s\n' "$R15_ARGV" | grep -q -- '--status accepted'; then
  r15_fail r15_accepted_static "巡检仍额外查询 accepted（巡检不该替人判断连带删除）"
else
  r15_ok r15_accepted_static
fi

# ⑤ --no-gc 已删除：参数解析必须报未知参数（退出码以 main 实际行为为准，只要求非 0）。
NOGC_ERR="$TMP/nogc.err"
if bash "$WATCH" --no-gc >/dev/null 2>"$NOGC_ERR"; then
  r15_fail r15_no_gc_flag "已删除的 --no-gc 仍被接受（退出码 0）"
elif ! grep -q '未知参数' "$NOGC_ERR"; then
  r15_fail r15_no_gc_flag "--no-gc 应以「未知参数」失败，stderr: $(tr '\n' ' ' < "$NOGC_ERR")"
else
  N=$((N+1)); printf 'ok  %-26s --no-gc 被拒\n' r15_no_gc_flag
fi

# ⑥ 唯一写入是 watch-state.json：其余文件哈希前后必须完全一致。
r15_run r15_only_state_file "$(cand_json A 2 'ok' "$STALE")"
if [ "$R15_BEFORE" != "$R15_AFTER" ]; then
  r15_fail r15_only_state_file "除 watch-state.json 外还有文件被写"
else
  r15_ok r15_only_state_file
fi

if [ "$R15_FAILS" -ne 0 ]; then
  echo "R15 组失败 ${R15_FAILS} 例（巡检必须全程只读）" >&2
  exit 1
fi

# R2 按工程口径：只有「该工程库内 ≥20 条」的样本才进命中率分母。
# 旧实现用全局 empty/A_PROMPT 算，窗口内提问全落在空库工程上时会稳定误判 FAIL。
plines() { # plines <project> <n> <empty>；project 传空串表示该行不带 project 字段
  local p="$1" n="$2" e="$3" i emp
  for (( i = 0; i < n; i++ )); do
    emp=false
    if [ "$i" -lt "$e" ]; then emp=true; fi
    if [ -z "$p" ]; then
      printf '{"ts":%s,"event":"prompt-submit","ms":12,"empty":%s}\n' "$NOW" "$emp"
    else
      printf '{"ts":%s,"event":"prompt-submit","ms":12,"empty":%s,"project":"%s"}\n' "$NOW" "$emp" "$p"
    fi
  done
}

assert_r2() { # <名称> <prompts> <proj_stats> <total> <want_level> [detail 子串...]
  local name="$1" prompts="$2" pstats="$3" total="$4" want="$5"
  shift 5
  local out got detail needle
  out="$(run_case "$name" 100 0 no "$EMPTY" '' '' '' "$prompts" "$pstats" "$total")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R2") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R2") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: R2 want=$want got=${got}（${detail}）" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  for needle in "$@"; do
    if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
      echo "FAIL $name: R2 detail 缺 '${needle}'，got: ${detail}" >&2
      exit 1
    fi
  done
  N=$((N+1))
  printf 'ok  %-26s R2=%-4s\n' "$name" "$want"
}

# 核心回归：库全局 200 条、但窗口内 6 次提问全来自库内 0 条的工程 →
# 旧版按全局算得 0% 判 FAIL，新版应剔除冷启动样本、判 INFO 并点明原因。
assert_r2 r2_cold_project     "$(plines cold 6 6)"                   ''        200 INFO '全落在库内 <20 条' '空注入属设计预期'
# 冷启动剔除 ≠ 注入为空：规则段与观测注入不看 memories 条数，4 次里 3 次有注入时
# INFO 必须报出注入次数，否则「确实注入了」会被读成「没有可注入内容」。
assert_r2 r2_cold_some_inject "$(plines cold 4 1)"                   ''        200 INFO '全落在库内 <20 条' '其中 3 次仍注入'
# 冷启动样本被剔除，不稀释有料工程的命中率：hot 10 次空 2 次 → 80% PASS。
assert_r2 r2_cold_excluded    "$(plines hot 10 2)$(plines cold 5 5)"  'hot=50'  200 PASS '已剔除冷启动工程 5 次'
# 反向保护：有料工程全空仍须判 FAIL，剔除逻辑不能把真故障一起吞掉。
assert_r2 r2_hot_all_empty    "$(plines hot 10 10)"                  'hot=50'  200 FAIL
assert_r2 r2_hot_warn         "$(plines hot 10 8)"                   'hot=50'  200 WARN
# 旧审计无 project 字段且「按工程」缺失 → 退回全局口径，不把未知工程当冷启动。
assert_r2 r2_legacy_no_proj   "$(plines '' 4 4)"                     ''         10 INFO

# 采集失败保护：不可解析的 JSON 不得静默判 PASS。
assert_case r8_read_fail       100 0 no 'not-json' PASS PASS WARN WARN

# R12 边界：无样本 INFO / 有蒸馏且 0 失败 PASS / 断链 WARN / 失败 1–3 WARN / >3 FAIL。
# 无样本也要复述四段计数（观测/蒸馏分流/合法空/失败/裁决）：INFO 不比别的级别少信息，
# 2026-11-15 起连续四个窗口的 INFO 分支都丢了 D12，靠人工发现。
assert_r12 r12_no_sample        no  '' '' INFO '观测 0'
assert_r12 r12_pass_rules       yes "$(dline proposal rules ok false)$(dline proposal rules empty false)" "$(activity_json '本次任务有沉淀')" PASS 'rules 2'
assert_r12 r12_broken_chain     yes '' '' WARN 'MEM_DISTILL_SPAWN'
assert_r12 r12_fail_1           yes "$(dline proposal rules fail true)" '' WARN
assert_r12 r12_fail_3           yes "$(dline proposal rules fail true)$(dline proposal rules fail true)$(dline proposal rules fail true)" '' WARN
assert_r12 r12_fail_4           yes "$(dline proposal rules fail true)$(dline proposal rules fail true)$(dline proposal rules fail true)$(dline proposal rules fail true)" '' FAIL
assert_r12 r12_provider_split   yes "$(dline proposal rules ok false)$(dline proposal none empty false)" '' PASS 'rules 1 · 其他 1'
assert_r12 r12_empty_not_fail   yes "$(dline proposal rules empty false)$(dline proposal rules noop false)" '' PASS '合法空 2'
assert_r12 r12_invalid_mode     yes "$(dline proposal rules invalid false)" '' WARN
assert_r12 r12_activity_badjson yes "$(dline proposal rules ok false)" 'not-json' PASS
assert_r12 r12_activity_verdict yes "$(dline proposal rules ok false)" "$(activity_json '有观测无采纳')" PASS '有观测无采纳'
assert_r12 r12_decisions_shown  yes "$(dline proposal rules ok false)$(decline accept)$(decline reject)" '' PASS 'accept 1 reject 1'

# R13 夹具：iline 造带 inject_bytes 的读侧记录；oldline 造升级前无该字段的旧记录。
iline()   { printf '{"ts":%s,"event":"%s","inject_bytes":%s}\n' "$NOW" "${2:-session-start}" "$1"; }
oldline() { printf '{"ts":%s,"event":"%s","ms":9,"empty":false}\n' "$NOW" "${1:-prompt-submit}"; }

# assert_r15 <名称> <audit_lines> <want_level> [detail 子串]
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

# R14 夹具：evidence 的落盘量。必须**实写**——APFS 上 truncate 出来的稀疏文件 `du` 记 0，
# 拿它当样本会永远 PASS。同一目录增量追加，避免为两个边界值各写一遍。
# 不断言 detail 里的具体 MiB 数：`du` 统计分配块，实测物理占用比逻辑大小高 1–2%
# （写 500MiB 报 507–519MiB 不等），贴着阈值构造会偶发误判。故两侧各留 ~100MiB 余量，
# 只断言判级的**语义**（「未超」「超过 512MiB」），阈值本身的改动由 sync 检查守住。
R14_DIR="$TMP/r14"
R14_NODIR="$TMP/r14-nodir"
build_fixture "$R14_DIR" 100 0 no
build_fixture "$R14_NODIR" 100 0 no
mkdir -p "$R14_DIR/v2mem/evidence"
ev_write() { dd if=/dev/zero of="$R14_DIR/v2mem/evidence/$1" bs=1m count="$2" 2>/dev/null; }

# assert_r14 <名称> <dir> <want_level> [detail 子串]
assert_r14() {
  local name="$1" d="$2" want="$3" needle="${4:-}"
  local out got detail
  out="$(run_watch "$d")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R14") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R14") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL ${name}: R14 want=${want} got=${got}" >&2
    printf '%s\n' "$out" >&2
    exit 1
  fi
  if [ -n "$needle" ] && [[ "$detail" != *"$needle"* ]]; then
    echo "FAIL ${name}: R14 detail 缺 '${needle}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R14=%-4s\n' "$name" "$want"
}

# 无归档目录 → INFO（无样本），不得判 PASS 掩盖「未启用归档」。
assert_r14 r14_no_dir        "$R14_NODIR" INFO '不存在'
# 目录存在但空 → PASS（尚无观测落盘），与「无目录」的措辞必须不同。
assert_r14 r14_empty_dir     "$R14_DIR"   PASS '无归档文件'
# 400MiB → PASS（距 512MiB 水位有余量，吸收 du 的分配抖动）。
ev_write a 400
assert_r14 r14_pass_under    "$R14_DIR"   PASS '未超 512MiB'
# 再 +200MiB → 约 600MiB → WARN；只判字节，文件数仍是 2 也要按体积判。
ev_write b 200
assert_r14 r14_warn_over     "$R14_DIR"   WARN '超过 512MiB'

# R3 分段分支：默认夹具没有分段字段，走不到 R3 的展开分支。macOS bash 3.2 在
# set -u 下会把 "$R3_NOTE；" 的全角分号首字节并进变量名，脚本在吐出 JSON 前就终止。
# 这里用带 claim_ms 的窗口强制走该分支，断言 R3 完整产出且带分段文案。
R3_SEG_DIR="$TMP/r3_seg"
build_fixture "$R3_SEG_DIR" none 0 no "" \
  "$(printf '{"ts":%s,"event":"prompt-submit","ms":12,"empty":false,"search_ms":4,"claim_ms":3}\n' "$NOW")"
r3_out="$(run_watch "$R3_SEG_DIR")"
r3_detail="$(printf '%s' "$r3_out" | jq -r '.items[] | select(.id=="R3") | .detail' 2>/dev/null || true)"
if [[ "$r3_detail" != *"分段均值"* ]]; then
  echo "FAIL r3_seg_note: R3 detail 缺分段文案（脚本可能中途退出）: ${r3_detail}" >&2
  printf '%s\n' "$r3_out" >&2
  exit 1
fi
N=$((N+1))
printf 'ok  %-26s R3=分段文案完整\n' "r3_seg_note"

# R3 判级字段是 search_ms，不是总 ms：总耗时 900ms 而 search_ms 5 必须 PASS
# （旧口径会判 FAIL，正是本机误报的形态）。旧记录没有 search_ms → INFO，不当 0 判 PASS。
# 文案侧同样要钉住过期结论（2026-11-22 更正）：旧说「同一 2266 字符 prompt 逐次耗时 40→205ms
# 单调上行、属真回归」，而进程内检索同 11 次为 18→55ms、端到端 ms 在 11-21/11-22 回落
# 54/54、191/49ms——非单调，漂移在进程启动与审计写入。故 detail 不得再出现「单调上行」。
# assert_r3 <名称> <审计体> <want_level> [必须不含]
assert_r3() {
  local name="$1" lines="$2" want="$3" banned="${4:-}" d out got detail
  d="$TMP/$name"
  build_fixture "$d" none 0 no "" "$lines"
  out="$(run_watch "$d")"
  got="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R3") | .level')"
  detail="$(printf '%s' "$out" | jq -r '.items[] | select(.id=="R3") | .detail')"
  if [ "$got" != "$want" ]; then
    echo "FAIL ${name}: R3 want=$want got=$got" >&2
    exit 1
  fi
  if [ -n "$banned" ] && [[ "$detail" == *"$banned"* ]]; then
    echo "FAIL ${name}: R3 detail 仍含过期结论 '${banned}'，got: ${detail}" >&2
    exit 1
  fi
  N=$((N+1))
  printf 'ok  %-26s R3=%-4s\n' "$name" "$want"
}
assert_r3 r3_search_pass "$(printf '{"ts":%s,"event":"prompt-submit","ms":900,"empty":false,"search_ms":5,"open_ms":40,"evidence_ms":800}\n' "$NOW")" PASS '单调上行'
assert_r3 r3_search_warn "$(printf '{"ts":%s,"event":"prompt-submit","ms":120,"empty":false,"search_ms":60}\n' "$NOW")" WARN '单调上行'
assert_r3 r3_search_fail "$(printf '{"ts":%s,"event":"prompt-submit","ms":300,"empty":false,"search_ms":250}\n' "$NOW")" FAIL
assert_r3 r3_no_seg_info "$(printf '{"ts":%s,"event":"prompt-submit","ms":300,"empty":false}\n' "$NOW")" INFO

# ---- R6/TOTAL：stats「总条数」带中文注解（2026-11-23 6c167a3 起）----
# 旧解析 `awk '{print $2}'` 会把「20527（全库各数据表之和）」整段当数字，后果是三重的：
# `[ "$TOTAL" -lt 20 ]` 报 "integer expression expected"、json 头写出 `"total":20527（…）`
# 不合法、watch-state.json 也写成坏 JSON（下一轮 R6 拿不到基线，静默退回「首次基线」）。
# 这里断言：stderr 无 integer 报错、标题只留数字、state 文件可被 jq 解析。
TOT_DIR="$TMP/tot_annotated"
build_fixture "$TOT_DIR" none 0 no
TOT_ERR="$TMP/tot.err"
tot_out="$(V2MEM_DIR="$TOT_DIR/v2mem" WORKBUDDY_CONFIG_DIR="$TOT_DIR/wb" HOME="$TOT_DIR/home" \
  MEM_BIN="$TOT_DIR/mem-stub" STUB_CAND_JSON="$EMPTY" \
  STUB_TOTAL='20527（全库各数据表之和）' \
  bash "$WATCH" --json 2>"$TOT_ERR" || true)"
if grep -qi 'integer expression' "$TOT_ERR"; then
  echo "FAIL tot_annotated: 仍把 stats 注解当数字: $(tr '\n' ' ' < "$TOT_ERR")" >&2
  exit 1
fi
i1_title="$(printf '%s' "$tot_out" | jq -r '.items[] | select(.id=="I1") | .title' 2>/dev/null || true)"
i1_detail="$(printf '%s' "$tot_out" | jq -r '.items[] | select(.id=="I1") | .detail' 2>/dev/null || true)"
if [[ "$i1_detail" == *"（"* ]]; then
  echo "FAIL tot_annotated: I1 detail 仍带注解: ${i1_detail}" >&2
  exit 1
fi
state_total="$(jq -r '.total' "$TOT_DIR/v2mem/watch-state.json" 2>/dev/null || echo BAD)"
if [ "$state_total" != "20527" ]; then
  echo "FAIL tot_annotated: watch-state.json 不可用/基线不是数字: ${state_total}" >&2
  cat "$TOT_DIR/v2mem/watch-state.json" >&2 2>/dev/null || true
  exit 1
fi
N=$((N+1))
printf 'ok  %-26s I1=%s state=%s\n' "tot_annotated" "$i1_title" "$state_total"

# ---- R4：只发生 accept 也必须算「有写入活动」----
# accept 是裁决闭环的落库出口（6 条 accept 就是 6 条新记忆）。旧口径只数
# manual-add/write，会在「55 reject + 6 accept」的窗口里报「窗口内无写入」。
ACC_DIR="$TMP/r4_accept_only"
build_fixture "$ACC_DIR" none 0 no "$(printf '{"ts":%s,"event":"accept","source":"distill-accept"}\n' "$NOW")"
acc_out="$(run_watch "$ACC_DIR")"
acc_lvl="$(printf '%s' "$acc_out" | jq -r '.items[] | select(.id=="R4") | .level')"
acc_detail="$(printf '%s' "$acc_out" | jq -r '.items[] | select(.id=="R4") | .detail')"
if [ "$acc_lvl" != "PASS" ] || [[ "$acc_detail" != *"accept"* ]]; then
  echo "FAIL r4_accept_only: want=PASS(mention accept) got=${acc_lvl} ${acc_detail}" >&2
  exit 1
fi
N=$((N+1))
printf 'ok  %-26s R4=%-4s\n' "r4_accept_only" "$acc_lvl"

# ---- R8：标题要同时报总数与判级依据 ----
# 5 条 pending 散在 3 个工程（2/2/1）时，判级取最大单工程 2（PASS），
# 但标题只写 2 会与 detail 列出的 5 条自相矛盾，读起来像「只有 2 条待办」。
MIX_CANDS="$(merge_json "$(cand_json pa 2)" "$(merge_json "$(cand_json pb 2)" "$(cand_json pc 1)")")"
MIX_DIR="$TMP/r8_title_mix"
build_fixture "$MIX_DIR" none 0 no
mix_out="$(run_watch "$MIX_DIR" "$MIX_CANDS")"
mix_lvl="$(printf '%s' "$mix_out" | jq -r '.items[] | select(.id=="R8") | .level')"
mix_title="$(printf '%s' "$mix_out" | jq -r '.items[] | select(.id=="R8") | .title')"
if [ "$mix_lvl" != "PASS" ] || [[ "$mix_title" != *"5 条"* ]] || [[ "$mix_title" != *"最大单工程 2"* ]]; then
  echo "FAIL r8_title_mix: want=PASS 且标题含总数 5 与「最大单工程 2」，got=${mix_lvl} ${mix_title}" >&2
  exit 1
fi
N=$((N+1))
printf 'ok  %-26s R8=%-4s %s\n' "r8_title_mix" "$mix_lvl" "$mix_title"

echo "watch-v2mem 判据回归 ${N}/${N} PASS"