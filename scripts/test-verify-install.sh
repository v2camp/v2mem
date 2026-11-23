#!/usr/bin/env bash
# verify-install.sh 的回归：临时 HOME + mem stub + fixture，不触碰真实配置与库。
# 覆盖 docs/install.md §4 的 A–D 判据、只读性、ID 集合同步与退出码。
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SCRIPT="$ROOT/scripts/verify-install.sh"
DOC="$ROOT/docs/install.md"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/v2mem-verify.XXXXXX")"
trap 'chmod -R u+w "$TMP" 2>/dev/null; rm -rf "$TMP"' EXIT

WORK="$TMP/work"
mkdir -p "$WORK"
PASS_N=0
FAIL_N=0
FAILED=""

ok() {
	PASS_N=$((PASS_N + 1))
	printf 'PASS %s\n' "$1"
}

no() {
	FAIL_N=$((FAIL_N + 1))
	FAILED="${FAILED}${FAILED:+
}$1"
	printf 'FAIL %s\n' "$1"
}

expect_has() { # desc pattern output
	if printf '%s' "$3" | grep -qE "$2"; then ok "$1"; else
		no "$1（未匹配 /$2/）"
		printf '%s\n' "$3" | sed 's/^/    /' | head -20
	fi
}

expect_no() { # desc pattern output
	if printf '%s' "$3" | grep -qE "$2"; then
		no "$1（不应匹配 /$2/）"
		printf '%s\n' "$3" | sed 's/^/    /' | head -20
	else ok "$1"; fi
}

expect_exit() { # desc expected actual
	if [ "$3" -eq "$2" ]; then ok "$1"; else no "$1（exit=$3，期望 $2）"; fi
}

BASE_PATH="$(dirname "$(command -v jq)"):/usr/bin:/bin:/usr/sbin:/sbin"
CASE_PATH=""
CASE_MEM_BIN=""
CASE_DB=""

new_home() {
	H="$TMP/home-$1"
	rm -rf "$H"
	mkdir -p "$H/bin" "$H/.local/bin" "$H/go/bin" "$H/.workbuddy" "$H/.trae-cn" "$H/.v2mem" "$H/Applications"
	cat >"$H/bin/mem" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
  help) echo "usage"; exit 0 ;;
  stats) echo "总条数 0"; exit 0 ;;
  harness) [ -f "$HOME/.harness.json" ] && cat "$HOME/.harness.json" || echo '[]'; exit 0 ;;
esac
exit 1
STUB
	chmod +x "$H/bin/mem"
	ln -sfn "$H/bin/mem" "$H/.local/bin/mem"
	cat >"$H/.harness.json" <<'JSON'
[
  {"name":"workbuddy","label":"WorkBuddy","tier":"native","global_config":["~/.workbuddy/settings.json"],"verify":"新开 WorkBuddy 会话"},
  {"name":"traecode","label":"TraeCode","tier":"native","global_config":["~/.trae-cn/hooks.json"],"verify":"新开 TraeCode 会话"},
  {"name":"codex","label":"Codex","tier":"native","global_config":["~/.codex/hooks.json"],"verify":"codex /hooks"}
]
JSON
	CASE_PATH="$H/bin:$H/.local/bin:$BASE_PATH"
	CASE_MEM_BIN="$H/bin/mem"
	CASE_DB=""
}

run() {
	(
		cd "$WORK" || exit 1
		HOME="$H" \
			PATH="${CASE_PATH:-$H/bin:$BASE_PATH}" \
			MEM_BIN="${CASE_MEM_BIN-$H/bin/mem}" \
			V2MEM_DIR="${CASE_DB:-$H/.v2mem}" \
			WORKBUDDY_CONFIG_DIR="$H/.workbuddy" \
			VERIFY_APP_DIR="$H/Applications" \
			GOPATH="$H/go" \
			bash "$SCRIPT" "$@"
	)
}

hooks_file() { printf '%s/.workbuddy/settings.json' "$H"; }

write_hooks() { # n events
	local n="$1" f
	f="$(hooks_file)"
	jq -n --argjson n "$n" --arg cmd "$H/bin/mem hook --harness workbuddy  # v2mem" '
		{hooks: (reduce range(0; $n) as $i ({}; .["Ev\($i)"] = [{hooks: [{type: "command", command: $cmd}]}]))}
	' >"$f"
}

write_trae_hooks() {
	local f="$H/.trae-cn/hooks.json"
	jq -n --arg cmd "$H/bin/mem hook --harness traecode  # v2mem" '
		{hooks: {
			SessionStart: [{hooks: [{type: "command", command: $cmd}]}],
			UserPromptSubmit: [{hooks: [{type: "command", command: $cmd}]}],
			Stop: [{hooks: [{type: "command", command: $cmd}]}]
		}}
	' >"$f"
}

# ---------- T1 正常态 ----------
new_home good
write_hooks 4
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {type: "stdio", command: $c, args: ["mcp"]}}}' >"$H/.workbuddy/mcp.json"
printf '{"abc::v2mem":{}}' >"$H/.workbuddy/mcp-approvals.json"
OUT="$(run --harness workbuddy)"
RC=$?
expect_exit "T1 正常态退出 0" 0 "$RC"
expect_has "T1 A1 PASS" 'PASS A1' "$OUT"
expect_has "T1 C1 PASS" 'PASS C1' "$OUT"
expect_has "T1 C2 PASS" 'PASS C2' "$OUT"
expect_has "T1 C3 PASS" 'PASS C3' "$OUT"
expect_has "T1 C4 PASS" 'PASS C4' "$OUT"
expect_no "T1 无 FAIL" '^FAIL ' "$OUT"

# ---------- T2 无 mem ----------
new_home nomem
CASE_PATH="$BASE_PATH"
CASE_MEM_BIN=""
OUT="$(run --harness workbuddy)"
RC=$?
expect_exit "T2 无 mem 退出 1" 1 "$RC"
expect_has "T2 A1 FAIL" 'FAIL A1' "$OUT"

# ---------- T3 软链悬空 ----------
new_home dangling
ln -sfn "$H/does-not-exist" "$H/.local/bin/mem"
CASE_MEM_BIN="$H/.local/bin/mem"
OUT="$(run --harness workbuddy)"
expect_has "T3 A2 悬空 FAIL" 'FAIL A2' "$OUT"

# ---------- T4 软链指向 worktree ----------
new_home worktree
mkdir -p "$H/repo/.worktree/t/bin"
cp "$H/bin/mem" "$H/repo/.worktree/t/bin/mem"
ln -sfn "$H/repo/.worktree/t/bin/mem" "$H/.local/bin/mem"
CASE_MEM_BIN="$H/.local/bin/mem"
OUT="$(run --harness workbuddy)"
expect_has "T4 A2 worktree WARN" 'WARN A2' "$OUT"

# ---------- T5 多来源 ----------
new_home multi
printf '#!/usr/bin/env bash\nexit 0\n' >"$H/go/bin/mem"
chmod +x "$H/go/bin/mem"
OUT="$(run --harness workbuddy)"
expect_has "T5 A3 多来源 WARN" 'WARN A3' "$OUT"

# ---------- T6 DB 同步盘 ----------
new_home syncdb
CASE_DB="$H/Library/Mobile Documents/com~apple~CloudDocs/v2mem"
mkdir -p "$CASE_DB"
OUT="$(run --harness workbuddy)"
expect_has "T6 A4 同步盘 FAIL" 'FAIL A4' "$OUT"

# ---------- T7 hooks 指向另一份 mem ----------
new_home hookpath
write_hooks 4
mkdir -p "$H/other"
cp "$H/bin/mem" "$H/other/mem"
jq --arg c "$H/other/mem hook --harness workbuddy  # v2mem" \
	'.hooks.Ev0[0].hooks[0].command = $c' "$(hooks_file)" >"$H/tmp.json" && mv "$H/tmp.json" "$(hooks_file)"
OUT="$(run --harness workbuddy)"
expect_has "T7 A5 不同源 WARN" 'WARN A5' "$OUT"

# ---------- T8 宿主已有非 v2mem hooks ----------
new_home b1
write_hooks 4
jq '.hooks.Other = [{hooks: [{type: "command", command: "echo other"}]}]' "$(hooks_file)" >"$H/tmp.json" && mv "$H/tmp.json" "$(hooks_file)"
OUT="$(run --harness workbuddy)"
expect_has "T8 B1 INFO" 'INFO B1' "$OUT"

# ---------- T9 disableAllHooks ----------
new_home b2
write_hooks 4
jq '.disableAllHooks = true' "$(hooks_file)" >"$H/tmp.json" && mv "$H/tmp.json" "$(hooks_file)"
OUT="$(run --harness workbuddy)"
expect_has "T9 B2 WARN" 'WARN B2' "$OUT"

# ---------- T10 用户规则禁令 ----------
new_home b5
write_hooks 4
printf '# 规则\n禁止使用 hooks 自动注入。\n' >"$WORK/AGENTS.md"
OUT="$(run --harness workbuddy)"
expect_has "T10 B5 INFO 命中" 'INFO B5.*人工裁决' "$OUT"
rm -f "$WORK/AGENTS.md"

# ---------- T11 同名 MCP 错 command ----------
new_home b3
write_hooks 4
jq -n '{mcpServers: {v2mem: {command: "other-mem", args: ["mcp"]}}}' >"$H/.workbuddy/mcp.json"
OUT="$(run --harness workbuddy)"
expect_has "T11 B3 WARN" 'WARN B3' "$OUT"

# ---------- T12 同一事件重复挂载 ----------
new_home b4
write_hooks 1
jq '.hooks.Ev0 += [{hooks: [{type: "command", command: "/x/mem hook --harness workbuddy  # v2mem"}]}]' "$(hooks_file)" >"$H/tmp.json" && mv "$H/tmp.json" "$(hooks_file)"
OUT="$(run --harness workbuddy)"
expect_has "T12 B4 WARN" 'WARN B4' "$OUT"

# ---------- T13 未接入：默认 FAIL，preflight 降 INFO ----------
new_home c1
printf '{"hooks":{}}' >"$(hooks_file)"
OUT="$(run --harness workbuddy)"
RC=$?
expect_exit "T13 默认退出 1" 1 "$RC"
expect_has "T13 C1 默认 FAIL" 'FAIL C1' "$OUT"
OUT="$(run --preflight --harness workbuddy)"
RC=$?
expect_exit "T13 preflight 退出 0" 0 "$RC"
expect_has "T13 preflight C1 INFO" 'INFO C1' "$OUT"
expect_has "T13 preflight C2 INFO" 'INFO C2' "$OUT"

# ---------- T14 仅 1 个事件 ----------
new_home c2
write_hooks 1
OUT="$(run --harness workbuddy)"
expect_has "T14 C2 WARN" 'WARN C2' "$OUT"

# ---------- T15 MCP 未声明 ----------
new_home c3
write_hooks 4
OUT="$(run --harness workbuddy)"
expect_has "T15 C3 WARN" 'WARN C3' "$OUT"

# ---------- T16 MCP 已声明未批准 ----------
new_home c4
write_hooks 4
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {type: "stdio", command: $c, args: ["mcp"]}}}' >"$H/.workbuddy/mcp.json"
OUT="$(run --harness workbuddy)"
expect_has "T16 C4 WARN" 'WARN C4' "$OUT"

# ---------- T17 JSON 与 ID 集合同步 ----------
new_home json
write_hooks 4
OUT="$(run --harness workbuddy --json)"
if printf '%s' "$OUT" | jq -e 'type == "array" and length > 0' >/dev/null 2>&1; then
	ok "T17 JSON 可解析"
else
	no "T17 JSON 可解析"
fi
DOC_IDS="$(grep -oE '\| \*\*[A-D][0-9]\*\*' "$DOC" | tr -d '|* ' | sort)"
OUT_IDS="$(printf '%s' "$OUT" | jq -r '.[].id' 2>/dev/null | sort)"
if [ -n "$DOC_IDS" ] && [ "$DOC_IDS" = "$OUT_IDS" ]; then
	ok "T17 判据 ID 与 docs/install.md 一致"
else
	no "T17 判据 ID 与 docs/install.md 不一致"
	printf '    doc: %s\n    out: %s\n' "$(printf '%s' "$DOC_IDS" | tr '\n' ' ')" "$(printf '%s' "$OUT_IDS" | tr '\n' ' ')"
fi

# ---------- T18 --quiet 隐藏 PASS ----------
OUT="$(run --harness workbuddy --quiet)"
expect_no "T18 quiet 隐藏 PASS" '^PASS ' "$OUT"

# ---------- T19 未知参数 ----------
"$SCRIPT" --nope >/dev/null 2>&1
expect_exit "T19 未知参数退出 2" 2 "$?"

# ---------- T20 只读性 ----------
new_home readonly
write_hooks 4
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {command: $c, args: ["mcp"]}}}' >"$H/.workbuddy/mcp.json"
BEFORE="$(find "$H" -type f ! -name '*.json.tmp' -exec cksum {} + 2>/dev/null | sort)"
run --harness workbuddy >/dev/null 2>&1
AFTER="$(find "$H" -type f ! -name '*.json.tmp' -exec cksum {} + 2>/dev/null | sort)"
if [ "$BEFORE" = "$AFTER" ]; then ok "T20 运行前后文件未变"; else no "T20 运行前后文件被修改"; fi

# ---------- T22 WorkBuddy 缺 type: stdio ----------
new_home wbtype
write_hooks 4
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {command: $c, args: ["mcp"]}}}' >"$H/.workbuddy/mcp.json"
OUT="$(run --harness workbuddy)"
expect_has "T22 C3 缺 type WARN" 'WARN C3.*type' "$OUT"

# ---------- T24 WorkBuddy 连接器关闭（disabled: true）----------
new_home wbdisabled
write_hooks 4
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {type: "stdio", command: $c, args: ["mcp"], disabled: true}}}' >"$H/.workbuddy/mcp.json"
printf '{"abc::v2mem":{}}' >"$H/.workbuddy/mcp-approvals.json"
OUT="$(run --harness workbuddy)"
expect_has "T24 C3 禁用 WARN" 'WARN C3.*禁用' "$OUT"
OUT="$(run --harness workbuddy --preflight)"
expect_has "T24 preflight 也报禁用" 'WARN C3.*禁用' "$OUT"

# ---------- T23 Trae 的 User/mcp.json 可被读取 ----------
new_home traemcp
write_trae_hooks
mkdir -p "$H/Library/Application Support/Trae CN/User"
jq -n --arg c "$H/bin/mem" '{mcpServers: {v2mem: {command: $c, args: ["mcp"]}}}' >"$H/Library/Application Support/Trae CN/User/mcp.json"
OUT="$(run --harness traecode)"
expect_has "T23 C3 读取 Trae MCP" 'PASS C3.*Trae CN' "$OUT"

# ---------- T21 --runtime 转述 watch ----------
new_home d1
write_hooks 4
mkdir -p "$H/scripts"
cp "$SCRIPT" "$H/scripts/verify-install.sh"
cat >"$H/scripts/watch-v2mem.sh" <<'W'
#!/usr/bin/env bash
exit 0
W
OUT="$(HOME="$H" PATH="$H/bin:$BASE_PATH" MEM_BIN="$H/bin/mem" V2MEM_DIR="$H/.v2mem" \
	WORKBUDDY_CONFIG_DIR="$H/.workbuddy" GOPATH="$H/go" bash "$H/scripts/verify-install.sh" --harness workbuddy --runtime 2>&1)"
expect_has "T21 runtime PASS" 'PASS D1' "$OUT"
cat >"$H/scripts/watch-v2mem.sh" <<'W'
#!/usr/bin/env bash
echo "WARN R2 命中率低"
exit 0
W
OUT="$(HOME="$H" PATH="$H/bin:$BASE_PATH" MEM_BIN="$H/bin/mem" V2MEM_DIR="$H/.v2mem" \
	WORKBUDDY_CONFIG_DIR="$H/.workbuddy" GOPATH="$H/go" bash "$H/scripts/verify-install.sh" --harness workbuddy --runtime 2>&1)"
expect_has "T21 runtime WARN" 'WARN D1' "$OUT"
cat >"$H/scripts/watch-v2mem.sh" <<'W'
#!/usr/bin/env bash
echo "FAIL R1 无事件"
exit 1
W
OUT="$(HOME="$H" PATH="$H/bin:$BASE_PATH" MEM_BIN="$H/bin/mem" V2MEM_DIR="$H/.v2mem" \
	WORKBUDDY_CONFIG_DIR="$H/.workbuddy" GOPATH="$H/go" bash "$H/scripts/verify-install.sh" --harness workbuddy --runtime 2>&1)"
expect_has "T21 runtime FAIL" 'FAIL D1' "$OUT"

# ---------- 汇总 ----------
printf '\n总计: %d PASS / %d FAIL\n' "$PASS_N" "$FAIL_N"
if [ "$FAIL_N" -gt 0 ]; then
	printf '失败项:\n%s\n' "$FAILED"
	exit 1
fi
exit 0
