#!/usr/bin/env bash
# v2mem 安装一致性只读校验
#
# 判据的真理源是 docs/install.md §4；本文件是它的可执行形态，两者必须同步改。
# 只读：不写库、不改宿主配置；唯一执行的 mem 子命令是 help / harness / stats，
# 且 stats 只在库已存在时执行（库缺失时 stats 会建库，破坏只读）。
#
# 用法:
#   scripts/verify-install.sh                         # A–C 组（自动找已接入 v2mem 的宿主）
#   scripts/verify-install.sh --preflight             # 安装前：A–B 组，C 组缺项降 INFO
#   scripts/verify-install.sh --harness workbuddy     # 指定宿主，逗号分隔可多选
#   scripts/verify-install.sh --runtime --window 12h  # 追加 D 组：转述 watch-v2mem.sh 的 I/R
#   scripts/verify-install.sh --json | --quiet
#
# 退出码: 0=无 FAIL  1=有 FAIL  2=用法错
set -uo pipefail

MEM_BIN="${MEM_BIN:-$(command -v mem 2>/dev/null || true)}"
V2MEM_DIR="${V2MEM_DIR:-$HOME/.v2mem}"
WB_DIR="${WORKBUDDY_CONFIG_DIR:-$HOME/.workbuddy}"
WINDOW="12h"
PREFLIGHT=0
RUNTIME=0
JSON_OUT=0
QUIET=0
HARNESS_ARG=""

usage() {
	sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
	case "$1" in
	--preflight) PREFLIGHT=1; shift ;;
	--runtime) RUNTIME=1; shift ;;
	--window) WINDOW="${2:-12h}"; shift 2 ;;
	--harness) HARNESS_ARG="${2:-}"; shift 2 ;;
	--json) JSON_OUT=1; shift ;;
	--quiet) QUIET=1; shift ;;
	-h | --help) usage; exit 0 ;;
	*)
		printf '未知参数: %s\n' "$1" >&2
		usage >&2
		exit 2
		;;
	esac
done

expand_home() {
	case "$1" in
	'~') printf '%s' "$HOME" ;;
	'~/'*) printf '%s/%s' "$HOME" "${1#\~/}" ;;
	*) printf '%s' "$1" ;;
	esac
}

# 解析软链，输出最终路径；不依赖 GNU readlink -f（macOS 兼容）。
resolve_path() {
	local p="$1" n=0 t d b
	while [ -L "$p" ]; do
		n=$((n + 1))
		[ "$n" -gt 40 ] && break
		t="$(readlink "$p" 2>/dev/null)" || break
		case "$t" in
		/*) p="$t" ;;
		*) p="$(dirname "$p")/$t" ;;
		esac
	done
	d="$(dirname "$p")"
	b="$(basename "$p")"
	if [ -d "$d" ]; then
		(cd "$d" 2>/dev/null && printf '%s/%s' "$(pwd -P)" "$b") || printf '%s' "$p"
	else
		printf '%s' "$p"
	fi
}

json_escape() {
	awk 'BEGIN { ORS = "" }
		{
			gsub(/\\/, "\\\\"); gsub(/"/, "\\\""); gsub(/\t/, "\\t"); gsub(/\r/, "")
			if (NR > 1) printf "\\n"
			printf "%s", $0
		}'
}

RESULTS=() # ID<TAB>LEVEL<TAB>TITLE<TAB>DETAIL

add() {
	local id="$1" level="$2" title="$3" detail="${4:-}"
	detail="$(printf '%s' "$detail" | tr '\n\t' '  ' | cut -c1-240)"
	RESULTS+=("${id}"$'\t'"${level}"$'\t'"${title}"$'\t'"${detail}")
}

print_results() {
	local row id level title detail first=1
	if [ "$JSON_OUT" -eq 1 ]; then
		printf '['
		for row in "${RESULTS[@]}"; do
			IFS=$'\t' read -r id level title detail <<<"$row"
			if [ "$QUIET" -eq 1 ] && [ "$level" = "PASS" ]; then continue; fi
			[ "$first" -eq 0 ] && printf ','
			first=0
			printf '{"id":"%s","level":"%s","title":"%s","detail":"%s"}' \
				"$(printf '%s' "$id" | json_escape)" \
				"$(printf '%s' "$level" | json_escape)" \
				"$(printf '%s' "$title" | json_escape)" \
				"$(printf '%s' "$detail" | json_escape)"
		done
		printf ']\n'
		return
	fi
	for row in "${RESULTS[@]}"; do
		IFS=$'\t' read -r id level title detail <<<"$row"
		if [ "$QUIET" -eq 1 ] && [ "$level" = "PASS" ]; then continue; fi
		if [ -n "$detail" ]; then
			printf '%-4s %-2s %s — %s\n' "$level" "$id" "$title" "$detail"
		else
			printf '%-4s %-2s %s\n' "$level" "$id" "$title"
		fi
	done
}

finish() {
	print_results
	local row level fails=0
	for row in "${RESULTS[@]}"; do
		level="${row#*$'\t'}"
		level="${level%%$'\t'*}"
		[ "$level" = "FAIL" ] && fails=$((fails + 1))
	done
	[ "$fails" -gt 0 ] && exit 1
	exit 0
}

# ---------- 宿主信息 ----------
HARNESS_JSON=""
HAS_JQ=0
if command -v jq >/dev/null 2>&1; then
	HAS_JQ=1
	if [ -n "$MEM_BIN" ]; then
		HARNESS_JSON="$("$MEM_BIN" harness --json 2>/dev/null || true)"
	fi
fi

if [ "$HAS_JQ" -eq 1 ] && printf '%s' "$HARNESS_JSON" | jq -e 'type == "array"' >/dev/null 2>&1; then
	:
else
	HARNESS_JSON=""
fi

h_field() { # name field
	printf '%s' "$HARNESS_JSON" | jq -r --arg n "$1" --arg f "$2" \
		'.[] | select(.name == $n) | (.[$f] // empty)' 2>/dev/null
}

h_resolve_name() {
	local n="$1"
	[ -z "$HARNESS_JSON" ] && {
		printf '%s' "$n"
		return
	}
	printf '%s' "$HARNESS_JSON" | jq -r --arg n "$n" \
		'.[] | select(.name == $n or ((.aliases // []) | index($n))) | .name' 2>/dev/null | head -1
}

h_global_configs() {
	printf '%s' "$HARNESS_JSON" | jq -r --arg n "$1" \
		'.[] | select(.name == $n) | (.global_config // [])[]' 2>/dev/null
}

h_instruction_paths() {
	printf '%s' "$HARNESS_JSON" | jq -r --arg n "$1" \
		'.[] | select(.name == $n) | (.instruction // [])[]' 2>/dev/null
}

pick_config() { # name -> first global config whose parent dir exists, else first
	local name="$1" c p first=""
	while IFS= read -r c; do
		[ -z "$c" ] && continue
		p="$(expand_home "$c")"
		[ -z "$first" ] && first="$p"
		if [ -d "$(dirname "$p")" ]; then
			printf '%s' "$p"
			return
		fi
	done <<<"$(h_global_configs "$name")"
	printf '%s' "$first"
}

config_has_marker() {
	local cfg="$1"
	[ -f "$cfg" ] || return 1
	grep -qE '# v2mem|mem hook' "$cfg" 2>/dev/null
}

hook_events() { # cfg -> 含 mem hook 的 top-level hook 事件数
	local cfg="$1"
	[ -f "$cfg" ] || {
		printf '0'
		return
	}
	jq -r '[.hooks // {} | to_entries[] | select((.value | tostring) | contains("mem hook"))] | length' \
		"$cfg" 2>/dev/null || printf '0'
}

hook_commands() { # cfg -> all command strings
	local cfg="$1"
	[ -f "$cfg" ] || return
	jq -r '.. | objects | .command? // empty' "$cfg" 2>/dev/null
}

# ---------- 选定宿主 ----------
SELECTED=""     # name<TAB>tier<TAB>cfg
CFG_FILES=""    # 去重后的配置文件
ALL_DETECTED=""

add_cfg_file() {
	case "
$CFG_FILES" in
	*"
$1"*) ;;
	*) CFG_FILES="${CFG_FILES}${CFG_FILES:+
}$1" ;;
	esac
}

select_one() {
	local raw="$1" name tier cfg inst
	name="$(h_resolve_name "$raw")"
	[ -z "$name" ] && name="$raw"
	tier="$(h_field "$name" tier)"
	[ -z "$tier" ] && tier="native"
	if [ "$tier" = "instruction" ]; then
		inst=""
		while IFS= read -r c; do
			[ -z "$c" ] && continue
			inst="$(expand_home "$c")"
			break
		done <<<"$(h_instruction_paths "$name")"
		cfg="$inst"
	else
		cfg="$(pick_config "$name")"
	fi
	SELECTED="${SELECTED}${SELECTED:+
}${name}"$'\t'"${tier}"$'\t'"${cfg}"
	[ -n "$cfg" ] && add_cfg_file "$cfg"
}

if [ -n "$HARNESS_ARG" ]; then
	IFS=',' read -r -a _names <<<"$HARNESS_ARG"
	for _n in "${_names[@]}"; do
		[ -n "$_n" ] && select_one "$_n"
	done
elif [ -n "$HARNESS_JSON" ]; then
	# 默认找「已写入 v2mem 标记」的宿主；没有则提示用 --harness 指定
	while IFS= read -r _n; do
		[ -z "$_n" ] && continue
		_tier="$(h_field "$_n" tier)"
		_cfg="$(pick_config "$_n")"
		if [ "$_tier" = "instruction" ]; then
			_i=""
			while IFS= read -r _c; do
				[ -z "$_c" ] && continue
				_i="$(expand_home "$_c")"
				break
			done <<<"$(h_instruction_paths "$_n")"
			_cfg="$_i"
		fi
		if [ -n "$_cfg" ] && config_has_marker "$_cfg"; then
			SELECTED="${SELECTED}${SELECTED:+
}${_n}"$'\t'"${_tier}"$'\t'"${_cfg}"
			add_cfg_file "$_cfg"
		elif [ -n "$_cfg" ] && [ -d "$(dirname "$_cfg")" ]; then
			ALL_DETECTED="${ALL_DETECTED}${ALL_DETECTED:+, }$_n"
		fi
	done <<<"$(printf '%s' "$HARNESS_JSON" | jq -r '.[].name' 2>/dev/null)"
fi

# ---------- A. 来源 ----------
if [ "$HAS_JQ" -eq 1 ]; then
	add A0 PASS "jq 可用" "$(command -v jq)"
else
	add A0 WARN "jq 缺失" "结构化检查降级为 INFO；安装 jq 后重跑"
fi

BIN_PATH=""
if [ -n "$MEM_BIN" ]; then
	BIN_PATH="$(command -v "$MEM_BIN" 2>/dev/null || printf '%s' "$MEM_BIN")"
fi

if [ -z "$BIN_PATH" ]; then
	if [ "$PREFLIGHT" -eq 1 ]; then
		add A1 WARN "未找到 mem" "按 docs/install.md S0–S2 安装"
	else
		add A1 FAIL "未找到 mem" "command -v mem 为空；先完成安装"
	fi
elif ! "$BIN_PATH" help >/dev/null 2>&1; then
	add A1 FAIL "mem 不可执行" "$BIN_PATH help 返回非 0"
elif [ -f "$V2MEM_DIR/mem.db" ]; then
	if _err="$("$BIN_PATH" stats 2>&1 >/dev/null)"; then
		add A1 PASS "mem 可执行、库可读" "$BIN_PATH"
	else
		add A1 FAIL "mem stats 失败" "$(printf '%s' "$_err" | head -1)；若提示库版本高于二进制，升级 mem"
	fi
else
	add A1 PASS "mem 可执行；库尚未创建" "$BIN_PATH"
fi

if [ -z "$BIN_PATH" ]; then
	add A2 INFO "无 mem 落点" "跳过软链检查"
elif [ -L "$BIN_PATH" ]; then
	_target="$(resolve_path "$BIN_PATH")"
	if [ ! -e "$_target" ]; then
		add A2 FAIL "软链悬空" "$BIN_PATH → $_target"
	elif printf '%s' "$_target" | grep -q '/\.worktree/'; then
		add A2 WARN "软链指向 worktree" "$BIN_PATH → ${_target}；合入后需重指主仓库"
	else
		add A2 PASS "软链有效" "$BIN_PATH → $_target"
	fi
else
	add A2 PASS "PATH 上是真实文件" "$BIN_PATH"
fi

# A3 多来源：type -a + 常见安装目录
_src=""
_add_src() {
	[ -z "$1" ] && return
	[ -e "$1" ] || [ -L "$1" ] || return
	_r="$(resolve_path "$1")"
	case "
$_src" in
	*"
$_r"*) ;;
	*) _src="${_src}${_src:+
}$_r" ;;
	esac
}
while IFS= read -r _line; do
	case "$_line" in
	*" is "*) _add_src "${_line##* is }" ;;
	esac
done <<<"$(type -a mem 2>/dev/null || true)"
_add_src "$HOME/.local/bin/mem"
_gopath="${GOPATH:-}"
if [ -z "$_gopath" ] && command -v go >/dev/null 2>&1; then
	_gopath="$(go env GOPATH 2>/dev/null || true)"
fi
[ -n "$_gopath" ] && _add_src "$_gopath/bin/mem"
_src_count="$(printf '%s\n' "$_src" | grep -c . || true)"
case "$_src_count" in
0) add A3 INFO "未发现 mem 文件" "PATH 与常见安装目录都没有" ;;
1) add A3 PASS "安装来源唯一" "$_src" ;;
*) add A3 WARN "存在多份 mem" "$(printf '%s' "$_src" | tr '\n' ' ')" ;;
esac

# A4 DB 目录
if [ ! -d "$V2MEM_DIR" ]; then
	add A4 INFO "库目录尚未创建" "$V2MEM_DIR"
else
	_real_db="$(resolve_path "$V2MEM_DIR")"
	_low="$(printf '%s' "$_real_db" | tr '[:upper:]' '[:lower:]')"
	if printf '%s' "$_low" | grep -qE 'icloud|dropbox|onedrive|google drive|googledrive|nutstore|坚果云|baidu(netdisk|sync)|百度网盘|mobile documents|com~apple~clouddocs'; then
		add A4 FAIL "库在同步盘" "${_real_db}；活库必须放本地目录"
	elif [ ! -w "$V2MEM_DIR" ]; then
		add A4 FAIL "库目录不可写" "$_real_db"
	elif [ -L "$V2MEM_DIR/mem.db" ]; then
		add A4 WARN "mem.db 是软链" "$V2MEM_DIR/mem.db → $(resolve_path "$V2MEM_DIR/mem.db")"
	else
		add A4 PASS "DB 目录可写且不在同步盘" "$_real_db"
	fi
fi

# A5 hooks 里的 mem 路径与当前 mem 同源
_a5_bad=""
_a5_seen=0
if [ "$HAS_JQ" -eq 1 ]; then
	while IFS= read -r _cfg; do
		[ -z "$_cfg" ] && continue
		[ ! -f "$_cfg" ] && continue
		while IFS= read -r _cmd; do
			case "$_cmd" in
			*"mem hook"*) ;;
			*) continue ;;
			esac
			_a5_seen=1
			_cmd="${_cmd#"${_cmd%%[![:space:]]*}"}"
			case "$_cmd" in
			\'*)
				_p="${_cmd#\'}"
				_p="${_p%%\'*}"
				;;
			*) _p="${_cmd%%[[:space:]]*}" ;;
			esac
			if [ ! -x "$_p" ]; then
				_a5_bad="${_a5_bad}${_a5_bad:+, }$_p(不存在)"
			elif [ -n "$BIN_PATH" ] && [ "$(resolve_path "$_p")" != "$(resolve_path "$BIN_PATH")" ]; then
				_a5_bad="${_a5_bad}${_a5_bad:+, }$_p"
			fi
		done <<<"$(hook_commands "$_cfg")"
	done <<<"$CFG_FILES"
fi
if [ "$HAS_JQ" -eq 0 ]; then
	add A5 INFO "缺 jq，跳过 hooks 路径检查" ""
elif [ "$_a5_seen" -eq 0 ]; then
	add A5 PASS "无 v2mem hooks 路径可查" "尚未接入或未检测到宿主"
elif [ -n "$_a5_bad" ]; then
	add A5 WARN "hooks 指向另一份 mem" "$_a5_bad"
else
	add A5 PASS "hooks 与当前 mem 同源" "$BIN_PATH"
fi

# ---------- B. 冲突 ----------
if [ "$HAS_JQ" -eq 0 ]; then
	add B1 INFO "缺 jq，跳过 hooks 冲突检查" "安装 jq 后重跑"
	add B2 INFO "缺 jq，跳过 disableAllHooks" ""
	add B4 INFO "缺 jq，跳过重复挂载检查" ""
else
	_b1_n=0
	_b1_where=""
	_b2_where=""
	_b4_where=""
	while IFS= read -r _cfg; do
		[ -z "$_cfg" ] && continue
		[ ! -f "$_cfg" ] && continue
		_n="$(jq -r '[.. | objects | .command? // empty | select(contains("mem hook") | not)] | length' "$_cfg" 2>/dev/null || printf '0')"
		case "$_n" in ''|*[!0-9]*) _n=0 ;; esac
		if [ "$_n" -gt 0 ]; then
			_b1_n=$((_b1_n + _n))
			_b1_where="${_b1_where}${_b1_where:+, }$_cfg"
		fi
		if [ "$(jq -r 'if .disableAllHooks == true then "yes" else empty end' "$_cfg" 2>/dev/null)" = "yes" ]; then
			_b2_where="${_b2_where}${_b2_where:+, }$_cfg"
		fi
		_dup="$(jq -r '.hooks // {} | to_entries[] | {k: .key, n: ([.value | .. | objects | .command? // empty | select(contains("mem hook"))] | length)} | select(.n > 1) | "\(.k)=\(.n)"' "$_cfg" 2>/dev/null || true)"
		[ -n "$_dup" ] && _b4_where="${_b4_where}${_b4_where:+, }${_cfg}(${_dup//$'\n'/,})"
	done <<<"$CFG_FILES"
	if [ "$_b1_n" -gt 0 ]; then
		add B1 INFO "宿主已有非 v2mem hooks" "${_b1_n} 条：${_b1_where}；同事件注意注入重复"
	else
		add B1 PASS "无其他 hooks 竞争" "仅 v2mem 或未配置"
	fi
	if [ -n "$_b2_where" ]; then
		add B2 WARN "disableAllHooks=true" "${_b2_where}；所有钩子都不执行"
	else
		add B2 PASS "未禁用全部钩子" ""
	fi
fi

# B3 / C3 共用 MCP 扫描
_mcp_found=""
_mcp_bad=""
_scan_json_mcp() {
	local f="$1" cmd
	[ -f "$f" ] || return
	cmd="$(jq -r '.mcpServers.v2mem.command? // empty' "$f" 2>/dev/null || true)"
	[ -z "$cmd" ] && return
	_mcp_found="${_mcp_found}${_mcp_found:+, }$f"
	case "$(basename "$cmd")" in
	mem) ;;
	*) _mcp_bad="${_mcp_bad}${_mcp_bad:+, }$f(command=$cmd)" ;;
	esac
}
_scan_toml_mcp() {
	local f="$1" cmd
	[ -f "$f" ] || return
	cmd="$(awk '
		/^\[mcp_servers\.v2mem\]/ { f = 1; next }
		/^\[/ { f = 0 }
		f && /^[[:space:]]*command[[:space:]]*=/ {
			sub(/^[^=]*=[[:space:]]*/, ""); gsub(/"/, ""); print; exit
		}' "$f" 2>/dev/null)"
	[ -z "$cmd" ] && return
	_mcp_found="${_mcp_found}${_mcp_found:+, }$f"
	case "$(basename "$cmd")" in
	mem) ;;
	*) _mcp_bad="${_mcp_bad}${_mcp_bad:+, }$f(command=$cmd)" ;;
	esac
}
if [ "$HAS_JQ" -eq 1 ]; then
	_scan_json_mcp "$WB_DIR/mcp.json"
	_scan_json_mcp "$HOME/.cursor/mcp.json"
	_scan_json_mcp "$HOME/.claude.json"
	_scan_json_mcp "$PWD/.mcp.json"
	_scan_toml_mcp "$HOME/.codex/config.toml"
fi

if [ "$HAS_JQ" -eq 1 ]; then
	if [ -n "$_mcp_bad" ]; then
		add B3 WARN "同名 MCP 指向别的命令" "$_mcp_bad"
	else
		add B3 PASS "无同名 MCP 冲突" ""
	fi
else
	add B3 INFO "缺 jq，跳过 MCP 冲突检查" ""
fi

if [ "$HAS_JQ" -eq 1 ]; then
	if [ -n "$_b4_where" ]; then
		add B4 WARN "同一事件重复挂载 v2mem" "$_b4_where"
	else
		add B4 PASS "无重复挂载" ""
	fi
fi

# B5 用户规则禁令（启发式，只报 INFO）
_rules_hits=""
_rule_files=""
for _rf in "$PWD/AGENTS.md" "$HOME/.claude/CLAUDE.md" "$HOME/.codex/AGENTS.md"; do
	[ -f "$_rf" ] && _rule_files="${_rule_files}${_rule_files:+
}$_rf"
done
[ -e "$HOME/.trae-cn/user_rules" ] && _rule_files="${_rule_files}${_rule_files:+
}$HOME/.trae-cn/user_rules"
_pat='(禁止|不要|禁用|不得|不允许|别).*(hook|hooks|MCP|mcp|记忆|mem)|(hook|hooks|MCP|mcp|记忆|mem).*(禁止|不要|禁用|不得|不允许)'
while IFS= read -r _rf; do
	[ -z "$_rf" ] && continue
	if [ -d "$_rf" ]; then
		_h="$(grep -rnEi "$_pat" "$_rf" 2>/dev/null | head -3 || true)"
	else
		_h="$(grep -nEi "$_pat" "$_rf" 2>/dev/null | head -3 || true)"
	fi
	[ -n "$_h" ] && _rules_hits="${_rules_hits}${_rules_hits:+
}${_rf}: ${_h}"
done <<<"$_rule_files"
if [ -n "$_rules_hits" ]; then
	add B5 INFO "用户规则有相关禁令，需人工裁决" "$(printf '%s' "$_rules_hits" | tr '\n' ' ')"
else
	add B5 INFO "未发现明确禁令" "B5 仅为启发式；仍建议人工读规则文件"
fi

# ---------- C. 接入 ----------
_c1_bad=""
_c1_ok=""
_c2_min=99
_c2_detail=""
_c5=""
if [ "$HAS_JQ" -eq 0 ]; then
	add C1 INFO "缺 jq，跳过接入检查" ""
	add C2 INFO "缺 jq，跳过事件统计" ""
	add C3 INFO "缺 jq，跳过 MCP 检查" ""
	add C4 INFO "缺 jq，跳过批准记录" ""
	add C5 INFO "缺 jq，无面板清单" ""
elif [ -z "$SELECTED" ]; then
	add C1 INFO "未选定宿主" "${ALL_DETECTED:+已检测到：${ALL_DETECTED}；}用 --harness 指定"
	add C2 INFO "未选定宿主" ""
else
	while IFS=$'\t' read -r _name _tier _cfg; do
		[ -z "$_name" ] && continue
		if [ "$_tier" = "instruction" ]; then
			if [ -n "$_cfg" ] && [ -f "$_cfg" ] && grep -q 'v2mem' "$_cfg" 2>/dev/null; then
				_c1_ok="${_c1_ok}${_c1_ok:+, }$_name"
				[ "$_c2_min" -gt 2 ] && _c2_min=2
			else
				_c1_bad="${_c1_bad}${_c1_bad:+, }$_name(instruction)"
			fi
		elif config_has_marker "$_cfg"; then
			_c1_ok="${_c1_ok}${_c1_ok:+, }$_name"
			_ev="$(hook_events "$_cfg")"
			case "$_ev" in ''|*[!0-9]*) _ev=0 ;; esac
			[ "$_ev" -lt "$_c2_min" ] && _c2_min="$_ev"
			_c2_detail="${_c2_detail}${_c2_detail:+, }${_name}=${_ev}"
		else
			_c1_bad="${_c1_bad}${_c1_bad:+, }$_name"
			_c2_min=0
			_c2_detail="${_c2_detail}${_c2_detail:+, }${_name}=0"
		fi
		_v="$(h_field "$_name" verify)"
		[ -n "$_v" ] && _c5="${_c5}${_c5:+
}${_name}: ${_v}"
	done <<<"$SELECTED"

	if [ -n "$_c1_bad" ]; then
		if [ "$PREFLIGHT" -eq 1 ]; then
			add C1 INFO "尚未接入（preflight 不判失败）" "$_c1_bad"
		else
			add C1 FAIL "缺少 v2mem 条目" "$_c1_bad"
		fi
	else
		add C1 PASS "所选宿主已接入" "$_c1_ok"
	fi
	case "$_c2_min" in
	0)
		if [ "$PREFLIGHT" -eq 1 ]; then
			add C2 INFO "事件不完整（preflight 不判失败）" "$_c2_detail"
		else
			add C2 FAIL "hooks 事件为 0" "$_c2_detail"
		fi
		;;
	1) add C2 WARN "仅 1 个事件" "${_c2_detail}；重跑 mem init 补齐" ;;
	*) add C2 PASS "事件完整度达标" "$_c2_detail" ;;
	esac
fi

if [ "$HAS_JQ" -eq 1 ]; then
if [ -n "$_mcp_found" ] && [ -z "$_mcp_bad" ]; then
	add C3 PASS "MCP 已声明" "$_mcp_found"
elif [ -n "$_mcp_bad" ]; then
	add C3 WARN "MCP 声明存在错配" "$_mcp_bad"
else
	_gui=""
	_file_host=""
	while IFS=$'\t' read -r _name _tier _cfg; do
		case "$_name" in
		traecode|traework|qoder|qoder-cn|qoderwork|qoderwork-cn) _gui="${_gui}${_gui:+, }$_name" ;;
		workbuddy|codex|cursor|claude|dsh) _file_host="${_file_host}${_file_host:+, }$_name" ;;
		esac
	done <<<"$SELECTED"
	if [ -n "$_file_host" ]; then
		if [ "$PREFLIGHT" -eq 1 ]; then
			add C3 INFO "MCP 尚未声明（preflight 不判缺项）" "文件型宿主：${_file_host}"
		else
			add C3 WARN "未找到 MCP 声明" "文件型宿主：${_file_host}；按 docs/install.md S4 注册"
		fi
	elif [ -n "$_gui" ]; then
		add C3 INFO "MCP 由宿主面板管理" "${_gui}；脚本无法只读确认"
	else
		add C3 INFO "未选定宿主，跳过 MCP 声明" ""
	fi
fi

_wb_selected=0
while IFS=$'\t' read -r _name _tier _cfg; do
	[ "$_name" = "workbuddy" ] && _wb_selected=1
done <<<"$SELECTED"
if [ "$_wb_selected" -eq 0 ]; then
	add C4 INFO "未选 WorkBuddy" "跳过批准记录检查"
elif jq -e 'to_entries[] | select(.key | endswith("::v2mem"))' "$WB_DIR/mcp-approvals.json" >/dev/null 2>&1; then
	add C4 PASS "WorkBuddy MCP 已批准" "mcp-approvals.json 有 v2mem"
elif jq -e '.mcpServers.v2mem' "$WB_DIR/mcp.json" >/dev/null 2>&1; then
	add C4 WARN "MCP 已声明未批准" "连接器页打开 v2mem 开关"
else
	add C4 INFO "WorkBuddy 未声明 MCP" "如需 MCP：先写 mcp.json，再开连接器开关"
fi

if [ -n "$_c5" ]; then
	add C5 INFO "宿主面板确认清单" "$(printf '%s' "$_c5" | tr '\n' ' ')"
else
	add C5 INFO "无面板确认项" "未选定宿主或该宿主无需面板"
fi
fi

# ---------- D. 运行态 ----------
if [ "$RUNTIME" -eq 0 ]; then
	add D1 INFO "未启用 --runtime" "需要稳态判据时加 --runtime"
else
	_watch="$(dirname "$0")/watch-v2mem.sh"
	if [ ! -f "$_watch" ]; then
		add D1 INFO "找不到 watch-v2mem.sh" "$_watch"
	else
		_out="$(bash "$_watch" --window "$WINDOW" --quiet 2>&1)"
		_rc=$?
		if [ "$_rc" -eq 0 ] && [ -z "$_out" ]; then
			add D1 PASS "运行态无 FAIL/WARN" "窗口 ${WINDOW}"
		elif [ "$_rc" -eq 0 ]; then
			add D1 WARN "运行态有 WARN" "$(printf '%s' "$_out" | head -3 | tr '\n' ' ')"
		else
			add D1 FAIL "运行态有 FAIL" "$(printf '%s' "$_out" | head -3 | tr '\n' ' ')"
		fi
	fi
fi

finish
