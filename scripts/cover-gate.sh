#!/usr/bin/env bash
#
# v2mem 单元测试覆盖率门禁
#
# 阈值不在本文件里 —— 读 coverage-policy.txt（单一真理源）。
# 退出码：0 = 达标；1 = 有包低于下限或测试失败；2 = 用法/环境错误。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
POLICY="$ROOT/coverage-policy.txt"

UPDATE=0
FORCE=0
for arg in "$@"; do
  case "$arg" in
    --update) UPDATE=1 ;;
    --force)  FORCE=1 ;;
    -h|--help)
      cat <<'USAGE'
用法：scripts/cover-gate.sh [--update [--force]]

  不带参数      跑测试，按 coverage-policy.txt 逐包校验下限；低于下限退出 1
  --update      用当前实测值刷新下限（棘轮：只允许上调）
  --force       配合 --update 使用，允许下调下限（须在提交信息里说明原因）
USAGE
      exit 0 ;;
    *) echo "未知参数：$arg（用 --help 查看用法）" >&2; exit 2 ;;
  esac
done

if [ ! -f "$POLICY" ]; then
  echo "错误：找不到策略文件 $POLICY" >&2
  echo "覆盖率必须有一份显式的门槛清单，不允许在没有策略的情况下通过。" >&2
  exit 2
fi

if [ "$FORCE" = 1 ] && [ "$UPDATE" = 0 ]; then
  echo "错误：--force 只能与 --update 搭配使用" >&2
  exit 2
fi

PROFILE="$(mktemp -t v2mem-cover)"
TESTLOG="$(mktemp -t v2mem-cover-log)"
NEWPOLICY="$(mktemp -t v2mem-policy)"
trap 'rm -f "$PROFILE" "$TESTLOG" "$NEWPOLICY"' EXIT

cd "$ROOT"

echo "→ 采集覆盖率（CGO_ENABLED=0 go test ./... -coverprofile）"
if ! CGO_ENABLED=0 go test ./... -coverprofile="$PROFILE" >"$TESTLOG" 2>&1; then
  echo "✗ 测试未通过 —— 失败的测试集谈不上覆盖率，请先修测试：" >&2
  cat "$TESTLOG" >&2
  exit 1
fi

# 按包聚合 profile：输出「包 <TAB> 覆盖率% <TAB> 语句数」。
# profile 行格式：import/path/file.go:起行.列,止行.列 语句数 命中次数
ACTUAL="$(
  awk '
    NR == 1 { next }                                     # mode: set
    {
      pkg = $1
      sub(/:[0-9]+\.[0-9]+,[0-9]+\.[0-9]+$/, "", pkg)    # 剥掉 :行.列,行.列
      sub(/\/[^\/]+$/, "", pkg)                          # 剥掉文件名，剩包路径
      st  = $2 + 0
      cnt = $3 + 0
      total[pkg] += st
      if (cnt > 0) covered[pkg] += st
      gtotal += st
      if (cnt > 0) gcovered += st
    }
    END {
      for (p in total) if (total[p] > 0) printf "%s\t%.1f\t%d\n", p, 100 * covered[p] / total[p], total[p]
      if (gtotal > 0) printf "TOTAL\t%.1f\t%d\n", 100 * gcovered / gtotal, gtotal
    }
  ' "$PROFILE" | LC_ALL=C sort
)"

if [ -z "$ACTUAL" ]; then
  echo "错误：覆盖率为空 —— 一个测试都没跑起来？" >&2
  exit 1
fi

GATE_STATUS=0
awk -v update="$UPDATE" -v force="$FORCE" -v outfile="$NEWPOLICY" '
  # ---------- 第一遍：策略文件 ----------
  FNR == NR {
    raw[++rn] = $0
    if ($0 ~ /^#/ || $0 ~ /^[[:space:]]*$/) next
    split($0, f, "\t")
    if (f[1] == "" || f[2] == "") {
      printf "✗ 策略行格式不对（应形如「包路径<TAB>下限<TAB>目标」）：%s\n", $0 > "/dev/stderr"
      bad = 1
      next
    }
    lower[f[1]]  = f[2] + 0
    target[f[1]] = f[3] + 0
    order[++n]   = f[1]
    known[f[1]]  = 1
    next
  }

  # ---------- 第二遍：实测 ----------
  { actual[$1] = $2 + 0 }

  END {
    if (bad) exit 2

    fail = 0

    if (!update) {
      printf "%-20s %8s %7s %8s %9s  %s\n", "PACKAGE", "ACTUAL", "MIN", "TARGET", "MARGIN", "VERDICT"
      printf "%s\n", "-------------------------------------------------------------------------------"
    }

    for (i = 1; i <= n; i++) {
      p = order[i]
      short = p; sub(/^github\.com\/wanghui\/v2mem\//, "", short)

      if (!(p in actual)) {
        if (!update) printf "%-20s %8s %7s %8s %9s  未采集到\n", short, "-", lower[p] "%", target[p] "%", "-"
        printf "✗ %s 未采集到覆盖率：包可能已删除/改名，或该包下没有任何测试文件\n", short > "/dev/stderr"
        fail = 1
        continue
      }

      margin = actual[p] - lower[p]
      # 校验模式：低于下限即失败。
      # 刷新模式：低于下限不算失败 —— 棘轮会保持原下限（即门槛只升不降）。
      if (margin < 0 && !update) fail = 1

      if (!update) {
        verdict = (margin >= 0) ? "ok" : sprintf("FAIL  低于下限 %.1fpt", -margin)
        printf "%-20s %7.1f%% %6.0f%% %7.0f%% %+8.1f  %s\n", short, actual[p], lower[p], target[p], margin, verdict
      }

      if (update) {
        newlo = int(actual[p])                 # 向下取整 = 历史最好的整数下界
        if (newlo < lower[p] && !force) {
          printf "保持 %s 下限为 %d（本次实测 %.1f%% 更低；确需降标请加 --force）\n", short, lower[p], actual[p] > "/dev/stderr"
        } else if (newlo > lower[p]) {
          printf "上调 %s 下限：%d → %d（实测 %.1f%%）\n", short, lower[p], newlo, actual[p] > "/dev/stderr"
        } else if (newlo < lower[p]) {
          printf "下调 %s 下限：%d → %d（实测 %.1f%%）\n", short, lower[p], newlo, actual[p] > "/dev/stderr"
        }
      }
    }

    # ---------- 未登记的新包 ----------
    for (p in actual) {
      if (p == "TOTAL") continue
      if (!(p in known)) {
        printf "✗ 新增包未在策略中登记：%s（实测 %.1f%%）\n", p, actual[p] > "/dev/stderr"
        printf "  请在 coverage-policy.txt 写入其下限 —— 不允许存在无门槛的包。\n" > "/dev/stderr"
        fail = 1
        unregistered = 1
      }
    }

    # ---------- 棘轮：生成新策略 ----------
    if (update) {
      for (i = 1; i <= rn; i++) {
        line = raw[i]
        if (line ~ /^#/ || line ~ /^[[:space:]]*$/) { print line > outfile; continue }
        split(line, f, "\t")
        p = f[1]; goal = f[3]
        if (p in actual) {
          newlo = int(actual[p])
          if (newlo < lower[p] && !force) newlo = lower[p]
        } else {
          newlo = lower[p]
        }
        printf "%s\t%d\t%s\n", p, newlo, goal > outfile
      }
    }

    if (!update) {
      printf "\n"
      if (fail) {
        if (unregistered) {
          printf "✗ 覆盖率门禁未通过：存在未登记的新包（见上）。\n" > "/dev/stderr"
        } else {
          printf "✗ 覆盖率门禁未通过：补测试，或确认下降合理后用 `make cover-update` 刷新下限。\n" > "/dev/stderr"
        }
        exit 1
      }
      printf "✓ 覆盖率门禁通过（下限见 coverage-policy.txt）。\n"
    }
    exit(fail ? 1 : 0)
  }
' "$POLICY" - <<< "$ACTUAL" || GATE_STATUS=$?

if [ "$UPDATE" = 1 ]; then
  if [ "$GATE_STATUS" != 0 ]; then
    echo "✗ 校验失败，未刷新策略文件。" >&2
    exit 1
  fi
  if diff -q "$POLICY" "$NEWPOLICY" >/dev/null 2>&1; then
    echo "→ 策略无需变更（实测值与现有下限一致）。"
  else
    cp "$NEWPOLICY" "$POLICY"
    echo "→ 已更新 $(basename "$POLICY")，当前内容："
    sed -e 's/^/    /' "$POLICY"
  fi
fi

exit "$GATE_STATUS"
