#!/usr/bin/env bash
# fmt-check 自身的回归：干净目录放行、坏格式目录拦截、缺 gofmt 时区分退出码。
#
# 守的是「门禁真的会失败」。一个只会返回 0 的检查脚本能让 16 个未格式化文件
# 再次漂进 main，这正是本脚本存在的原因。
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
script="$here/fmt-check.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fail=0
check() { # name expected_rc actual_rc detail
	if [ "$2" != "$3" ]; then
		echo "FAIL $1: 期望退出码 $2，实际 $3（$4）" >&2
		fail=1
	else
		echo "ok   $1"
	fi
}

# 1) 干净目录：放行
mkdir -p "$tmp/clean"
printf 'package demo\n\nfunc F() {}\n' >"$tmp/clean/a.go"
bash "$script" "$tmp/clean" >/dev/null 2>&1
check "干净目录放行" 0 "$?"

# 2) 尾换行缺失：必须拦截（本次漂移的真实形态）
mkdir -p "$tmp/noeol"
printf 'package demo\n\nfunc F() {}' >"$tmp/noeol/bad.go"
bash "$script" "$tmp/noeol" >/dev/null 2>&1
check "缺文件尾换行被拦截" 1 "$?"

# 3) 缩进错乱：必须拦截
mkdir -p "$tmp/badindent"
printf 'package demo\n\nfunc G() {\n      return\n}\n' >"$tmp/badindent/c.go"
bash "$script" "$tmp/badindent" >/dev/null 2>&1
check "缩进错乱被拦截" 1 "$?"

# 4) 报错信息要能点出文件路径，否则修不动
err="$(bash "$script" "$tmp/noeol" 2>&1 >/dev/null || true)"
case "$err" in
*bad.go*) echo "ok   报错含文件名" ;;
*) echo "FAIL 报错未含文件名：$err" >&2; fail=1 ;;
esac

# 5) gofmt 缺失：退出码 2，与「有未格式化文件」(1) 区分开，
#    否则 CI 里工具链缺失会被误读成代码格式问题。
#    用 env -i 把 PATH 指向不存在的目录，command -v gofmt 必然失败。
env -i PATH=/nonexistent /bin/bash "$script" "$tmp/clean" >/dev/null 2>&1
check "gofmt 缺失返回 2 而非 1" 2 "$?"

if [ "$fail" -ne 0 ]; then
	echo "fmt-check 回归失败" >&2
	exit 1
fi
echo "全部通过"
