#!/usr/bin/env bash
set -euo pipefail
# test-eval-prod-gate.sh —— eval-prod-gate.sh 自测：全绿退出 0；G8 红退出非 0。
#
# 夹具：临时库 + sqlite3 合成最小动线账（G1–G6 有样本、G7–G13 prod 无样本降级），
# 与 test/flow 夹具同口径，不碰生产库；sqlite3 只用于造夹具，门禁主脚本只依赖 jq。
# 全部写操作都在 mktemp -d 的临时库上，退出即清理。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GATE="$ROOT/scripts/eval-prod-gate.sh"
TMPD="$(mktemp -d "${TMPDIR:-/tmp}/v2mem-prod-gate-test.XXXXXX")"
trap 'rm -rf "$TMPD"' EXIT

for tool in jq sqlite3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "RED: 自测需要 $tool" >&2; exit 2; }
done

MEM_BIN="${MEM_BIN:-$ROOT/bin/mem}"
if [ ! -x "$MEM_BIN" ]; then
  ( cd "$ROOT" && CGO_ENABLED=0 go build -o "$TMPD/mem" ./cmd/mem ) || {
    echo "RED: 构建 mem 失败" >&2; exit 2; }
  MEM_BIN="$TMPD/mem"
fi
export MEM_BIN

# ---------- 夹具：先让 mem 建表，再灌最小合法动线账 ----------
SEED="$TMPD/seed.db"
NOW="$(date +%s)"
"$MEM_BIN" eval flow --db "$SEED" --profile=prod --json >"$TMPD/seed.json" 2>"$TMPD/seed.err" || true

sqlite3 "$SEED" <<SQL
INSERT INTO observations(id, session_id, event, content_hash, created_at,
    source_id, source_generation, source_seq, event_uid, archive_hash)
  VALUES('gate-obs-1','gate-session','stop','gate-obs-ch',$NOW,
    'gate-src',1,1,'gate-ev-1','gate-ah-1');
INSERT INTO evidence_sources(id, generation, last_seq, cursor_seq, created_at, updated_at)
  VALUES('gate-src',1,1,1,$NOW,$NOW);
INSERT INTO evidence_archives(archive_hash, source_id, generation, from_seq, to_seq,
    raw_count, byte_size, path, created_at)
  VALUES('gate-ah-1','gate-src',1,1,1,1,16,'/tmp/gate-ah-1.gz',$NOW);
INSERT INTO evidence_bundles(id, source_id, generation, from_seq_exclusive, to_seq_inclusive,
    evidence_hash, status, attempt, last_run_id, created_at, updated_at)
  VALUES('gate-bundle-noop','gate-src',1,0,1,'gate-bh','noop',1,'gate-run-1',$NOW,$NOW);
INSERT INTO distill_runs(id, session_id, provider, status, started_at, finished_at,
    bundle_id, generated_count, duplicate_count, attempt)
  VALUES('gate-run-1','gate-session','fake','noop',$NOW,$NOW,'gate-bundle-noop',0,0,1);
INSERT INTO candidates(id, session_id, kind, content, content_hash, status, source,
    created_at, bundle_id, revision)
  VALUES('gate-cand-1','gate-session','fact','准出候选','gate-ch-1','rejected','distill',
    $NOW,'gate-bundle-1',0);
INSERT INTO commit_decisions(id, candidate_id, revision, action, actor, idempotency_key, created_at)
  VALUES('gate-dec-1','gate-cand-1',0,'reject','model-auto','gate-key-1',$NOW);
SQL

# ---------- Case 1：全绿 → 退出 0 ----------
set +e
OUT1="$(bash "$GATE" --db "$SEED" 2>"$TMPD/c1.err")"
RC1=$?
set -e
printf '%s\n' "$OUT1"
if [ "$RC1" -ne 0 ]; then
  echo "FAIL case1: 全绿夹具应退出 0，实际 $RC1" >&2
  cat "$TMPD/c1.err" >&2 || true
  exit 1
fi
if ! printf '%s\n' "$OUT1" | grep -q '\[prod-gate\] PASS'; then
  echo "FAIL case1: 未打印 PASS 摘要" >&2; exit 1
fi
if ! printf '%s\n' "$OUT1" | grep -q 'G7 已知假红来源已登记'; then
  echo "FAIL case1: 缺 G7 假红来源标注" >&2; exit 1
fi
echo "PASS case1: 全绿退出 0（G8-G12=5/5，准出率 100%，manual_rate 0% 仅诊断）"

# ---------- Case 2：制造 G8 红（超 15 天 SLA 仍 pending）→ 退出非 0 ----------
BAD="$TMPD/bad.db"
cp "$SEED" "$BAD"
for s in -wal -shm; do
  if [ -f "$SEED$s" ]; then cp "$SEED$s" "$BAD$s"; fi
done
STALE=$(( NOW - 20 * 86400 ))
sqlite3 "$BAD" <<SQL
INSERT INTO candidates(id, session_id, kind, content, content_hash, status, source,
    created_at, bundle_id, revision)
  VALUES('gate-stale-1','gate-session','fact','超期悬空候选','gate-ch-stale','pending','distill',
    $STALE,'gate-bad-window',0);
SQL
set +e
OUT2="$(bash "$GATE" --db "$BAD" --json 2>"$TMPD/c2.err")"
RC2=$?
set -e
if [ "$RC2" -eq 0 ]; then
  echo "FAIL case2: G8 红应退出非 0，实际 0" >&2; exit 1
fi
G8_PASS="$(printf '%s' "$OUT2" | jq -r '.gate_checks[] | select(.id == "G8") | .pass')"
if [ "$G8_PASS" != "false" ]; then
  echo "FAIL case2: G8 应为 FAIL，实际 $G8_PASS" >&2
  printf '%s\n' "$OUT2" >&2; exit 1
fi
printf '%s\n' "$OUT2" | jq -c '{result, eval_exit_code, gate_passed, manual_rate, g8: (.gate_checks[] | select(.id == "G8"))}'
echo "PASS case2: G8 红退出=${RC2}；G8=FAIL，1 条 pending 超 20 天"

# ---------- Case 3：--json 的 PASS 分支 + 参数边界 ----------
set +e
OUT3="$(bash "$GATE" --db "$SEED" --json 2>"$TMPD/c3.err")"
RC3=$?
set -e
if [ "$RC3" -ne 0 ] || [ "$(printf '%s' "$OUT3" | jq -r '.result')" != "PASS" ]; then
  echo "FAIL case3: --json 全绿应输出 result=PASS 且退出 0" >&2; exit 1
fi
bash "$GATE" --help >/dev/null 2>&1
set +e
bash "$GATE" --bogus >/dev/null 2>&1
RC4=$?
set -e
if [ "$RC4" -ne 2 ]; then
  echo "FAIL case3: 未知参数应退出 2，实际 $RC4" >&2; exit 1
fi
echo "PASS case3: --json PASS 分支 / --help / 未知参数退出码 2 均正确"

echo "ALL PASS（test-eval-prod-gate.sh）"
