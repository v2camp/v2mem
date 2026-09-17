#!/usr/bin/env bash
set -euo pipefail
# 自检 derive-version.sh 的三态推导 + --override + 无 tag 回退。
# 用一个临时 git 仓库自造提交来模拟「自上次 tag 后」的不同 conventional commit。
SCRIPT="$(cd "$(dirname "$0")" && pwd)/derive-version.sh"
fail=0

run_in_repo() { # run_in_repo <repo> <--last tag> <arg...>
  repo="$1"; shift
  bash "$SCRIPT" --repo "$repo" "$@"
}

# 构造一个空仓库，HEAD 指向某个起始提交
new_repo() {
  d="$(mktemp -d)"
  git -C "$d" init -q
  git -C "$d" config user.email t@t && git -C "$d" config user.name t
  echo base > "$d/base.txt"; git -C "$d" add .; git -C "$d" commit -qm "chore: base"
  echo "$d"
}

# 追加一个「有文件改动」的提交（否则 git commit 因无暂存改动会非零退出触发 set -e）
commit_file() { # commit_file <repo> <msg>
  d="$1"; m="$2"
  echo "x" >> "$d/count.txt"
  git -C "$d" add . && git -C "$d" commit -qm "$m"
}

# 空仓库（--last 之后的提交都是 patch 或没有）：若无 fancy 提交则回退 patch
{
  d="$(new_repo)"
  git -C "$d" tag v0.1.0
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0)"
  [ "$out" = "v0.1.1" ] || { echo "FAIL patch: got $out"; fail=1; }
  rm -rf "$d"
}

# feat: → minor
{
  d="$(new_repo)"; git -C "$d" tag v0.1.0
  commit_file "$d" "feat: 新增 --gold-dir"
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0)"
  [ "$out" = "v0.2.0" ] || { echo "FAIL minor: got $out"; fail=1; }
  rm -rf "$d"
}

# 含 ! → major（破坏性变更）
{
  d="$(new_repo)"; git -C "$d" tag v0.1.0
  commit_file "$d" "feat!: 重写 eval 口径"
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0)"
  [ "$out" = "v1.0.0" ] || { echo "FAIL major: got $out"; fail=1; }
  rm -rf "$d"
}

# fix 与 feat 同时存在 → 取最高档 minor（patch 不叠加）
{
  d="$(new_repo)"; git -C "$d" tag v0.1.0
  commit_file "$d" "fix: 修复分片"
  commit_file "$d" "feat: 加检索"
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0)"
  [ "$out" = "v0.2.0" ] || { echo "FAIL fix+feat 应 minor: got $out"; fail=1; }
  rm -rf "$d"
}

# --override 显式升档，忽略提交历史
{
  d="$(new_repo)"; git -C "$d" tag v0.1.0
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0 --override major)"
  [ "$out" = "v1.0.0" ] || { echo "FAIL override major: got $out"; fail=1; }
  out="$(bash "$SCRIPT" --repo "$d" --last v0.1.0 --override minor)"
  [ "$out" = "v0.2.0" ] || { echo "FAIL override minor: got $out"; fail=1; }
  rm -rf "$d"
}

# 无 v 前缀的 --last（容忍）+ 无 tag 回退
{
  d="$(new_repo)"
  out="$(bash "$SCRIPT" --repo "$d" --last 0.1.0)"   # no commits after → patch
  [ "$out" = "v0.1.1" ] || { echo "FAIL no-v base: got $out"; fail=1; }
  rm -rf "$d"
}

if [ "$fail" = 0 ]; then
  echo "derive-version ok"
else
  echo "derive-version FAIL" >&2
  exit 1
fi