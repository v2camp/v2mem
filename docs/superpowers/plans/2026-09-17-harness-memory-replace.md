# v2mem 接替 harness 记忆 — 实施计划（4 工作流并行）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 workbuddy/traeWork/TRAE 原生三处 harness 记忆的读与存收拢到 v2mem，配 git 跨设备同步与 CI 版本门禁，一次并行落地。

**Architecture:** 四个解耦工作流各走独立 worktree（AGENTS.md 流程）：
WS4 先立版本/门禁地基 → WS1(读瘦身)/WS2(写收录) 并行 → WS3(git 同步) 最后接通。各 WS 独立过 `make cover-gate`，最终一次合入 main。

**Tech Stack:** Go（SQLite/FTS5/mini hash 已内建）、GitHub Actions、conventional commits。

**来源 spec：** `docs/design-harness-memory-replace.md`。

---

## Sequence 总览

| WS | 内容 | 依赖 |
|---|---|---|
| WS4 | eval-policy + 版本推导 + `make release` + CI GitHub Actions + gold 集 | 无（先行） |
| WS1 | `mem notes` 硬规则小字条 + 注入分工 + 读侧 flag/回退 | 无 |
| WS2 | `mem ingest --source harness-summary` 统一收录（双路径） | 无 |
| WS3 | `mem sync` git 三方合并 | WS1/WS2 的数据格式稳定后 |

4 个 WS 互不写同一文件，可并行；最终合入 main 后跑 cover-gate + 正式版 eval，达标由 CI 打 tag。

---

# WS4 — 版本自动迭代 + eval 门禁 + CI

**Files:**
- Create: `eval-policy.txt`
- Create: `scripts/derive-version.sh`
- Create: `scripts/release-eval.sh`
- Create: `scripts/eval-report.sh`
- Create: `Makefile`（新增目标：`release` `release-dry` `eval-gate`）
- Create: `.github/workflows/release.yml`
- Create: `gold/README.md`、`gold/<seed>.gold.jsonl`
- Modify: `cmd/mem/main.go`（`mem eval` 增加 `--gold-dir` / `--policy` 绑定）

## Task WS4.1: eval-policy.txt（门槛单一真理源）

- [ ] **Step 1: 写测试**

`scripts/` 尚无 Go 测试；本任务用 shell 断言文件存在且含合法键。创建 `scripts/test-eval-policy.sh`：

```bash
#!/usr/bin/env bash
set -euo pipefail
[ -f eval-policy.txt ] || { echo "missing eval-policy.txt"; exit 1; }
grep -q '^recall5_min=0.60$' eval-policy.txt || { echo "bad recall5_min"; exit 1; }
echo "eval-policy ok"
```

- [ ] **Step 2: 跑测试确认失败**

`bash scripts/test-eval-policy.sh`；预期：`missing eval-policy.txt`（文件不存在，非零退出）。

- [ ] **Step 3: 创建 eval-policy.txt**

```
# 发布门禁阈值（单一真理源）。CI 与 make release 同读。
recall5_min=0.60
```

- [ ] **Step 4: 重跑测试确认通过**

`bash scripts/test-eval-policy.sh` 预期：`eval-policy ok`。

- [ ] **Step 5: Commit**

```bash
git add eval-policy.txt scripts/test-eval-policy.sh
git commit -m "build: 新增 eval-policy 门槛文件与自检脚本"
```

## Task WS4.2: 版本推导脚本（conventional commits）

- [ ] **Step 1: 写失败测试**

创建 `scripts/test-derive-version.sh`：

```bash
#!/usr/bin/env bash
set -euo pipefail
# 用一个临时仓库模拟「自上次 tag 后」的不同 commit 类型
tmp=$(mktemp -d); cd "$tmp"
out=$(bash /abs/path/v2mem/scripts/derive-version.sh --repo "$tmp" --last v0.1.0)
echo "$out"
```

- [ ] **Step 2: 实现 derive-version.sh**

```bash
#!/usr/bin/env bash
set -euo pipefail
# derive-version.sh --last <tag> [--override major|minor|patch]
# 输出下一版号 vX.Y.Z；无 override 时按 conventional commits 最高档推导。
LAST="$2"; shift 2
OVERRIDE=""
[ "${1:-}" = "--override" ] && { OVERRIDE="$2"; }
MAJOR_UP=0; MINOR_UP=0; PATCH_UP=0
while IFS= read -r line; do
  case "$line" in
    *'!'*|BREAKING*|feat!*|fix!*) MAJOR_UP=1 ;;
    feat:*) MINOR_UP=1 ;;
  esac
done < <(git log --pretty=%s "$LAST"..HEAD 2>/dev/null || echo "")
# 其余提交（fix/docs/chore/refactor）记 patch 增长
if grep -Eq '^\[(fix|docs|chore|refactor)\]' <(git log --pretty=%s "$LAST"..HEAD 2>/dev/null || echo ""); then
  PATCH_UP=1
fi
[ "$PATCH_UP" = 1 ] && [ "$MINOR_UP" = 1 ] && PATCH_UP=0
[ "$MINOR_UP" = 1 ] && [ "$MAJOR_UP" = 1 ] && MINOR_UP=0
major=$(( ${LAST%%.*} )); rest=${LAST#*.}; minor=$(( ${rest%%.*} )); patch=$(( ${rest##*.} ))
bump=$( [ -n "$OVERRIDE" ] && echo "$OVERRIDE" || { [ $MAJOR_UP = 1 ] && echo major || { [ $MINOR_UP = 1 ] && echo minor || echo patch; }; })
case "$bump" in
  major) major=$((major+1)); minor=0; patch=0 ;;
  minor) minor=$((minor+1)); patch=0 ;;
  patch) patch=$((patch+1)) ;;
esac
echo "v$major.$minor.$patch"
```

- [ ] **Step 3: 跑测试确认通过**

预期打印下一版号。覆盖三态：只 `feat:` → minor；含 `!` → major；只 fix/docs → patch。

## Task WS4.3: coverage-policy.txt 之外，eval 报告生成

- [ ] **Step 1: 实现 eval-report.sh**（输出 `.mem/reports/v<tag>.md`，头含 tag/sha/coverage/recall5/gold hash）

```bash
#!/usr/bin/env bash
set -euo pipefail
# 用法: eval-report.sh <tag> <sha> <coverage> <recall5> <goldhash>
TAG="$1"; SHA="$2"; COV="$3"; R5="$4"; GH="$5"
mkdir -p .mem/reports
cat > ".mem/reports/$TAG.md" <<EOF
# v2mem eval 报告 $TAG
- tag: $TAG
- commit: $SHA
- coverage: $COV
- recall5: $R5
- gold_hash: $GH
- date: $(date -u +%Y-%m-%d)
EOF
git_hash() { sha1sum "$1" | awk '{print $1}'; }
```

## Task WS4.4: Makefile release 目标（本地可预演）

- [ ] **Step 1: 在 Makefile 追加目标**（读 eval-policy 前先 cover-gate）

```make
eval-policy = $$(grep -oP 'recall5_min=.*' eval-policy.txt | cut -d= -f2)
release-dry:
	@./scripts/derive-version.sh --repo . --last $$(git describe --tags --abbrev=0 2>/dev/null || echo v0.1.0)
release:
	@make cover-gate
	@./scripts/release-eval.sh $(if $(OVERRIDE),--override $(OVERRIDE),)
```

## Task WS4.5: gold 起步集 + eval 绑定 policy

- [ ] **Step 1: 建 gold/README.md** 说明格式（JSONL：`{"query":..,"gold":["<hash前缀>.."]}`）、来源（真实 audit query + 人工标注）、版本化管理。
- [ ] **Step 2: 手动标注起步集**：从 `~/.v2mem/audit.jsonl` / 历史会话挑 ≥10 条真实 query，标注对应记忆 hash 前缀，写入 `gold/v0.1-seed.gold.jsonl`。
- [ ] **Step 3: cmd/mem 的 `eval recall` 支持 `--gold-dir`**（不传则回退现有 `--gold`）。新测试确保 `--gold-dir` 读取同目录所有 `*.gold.jsonl` 并合并为金标准。

## Task WS4.6: 真实样本门禁触发位（eval-gate）

- [ ] **Step 1: `make eval-gate`**：跑正式版 eval（gold 起步集）→ 解析 Recall@5 → 与 `eval-policy.txt` 的 `recall5_min` 比 → 不达标 `echo RED` 非零退出。写 `scripts/release-eval.sh` 承载该判据（`--json` 解析 recall5）。

## Task WS4.7: GitHub Actions 强制自动打 tag

- [ ] **Step 1: 建 `.github/workflows/release.yml`**

```yaml
name: release
on:
  push:
    branches: [ main ]
permissions:
  contents: write
jobs:
  gate-and-tag:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with: { fetch-depth: 0 }
      - uses: actions/setup-go@v5
        with: { go-version: '1.22' }
      - run: make cover-gate
      - run: make eval-gate   # 不达标 → 本步失败 → 跳过 tag
      - name: derive tag
        id: ver
        run: echo "tag=$(bash scripts/derive-version.sh --repo . --last $(git describe --tags --abbrev=0 2>/dev/null || echo v0.1.0))" >> $GITHUB_OUTPUT
      - name: tag & push & report
        run: |
          git config user.email "ci@v2mem" && git config user.name "v2mem-ci"
          git tag "${{ steps.ver.outputs.tag }}"
          git push origin "${{ steps.ver.outputs.tag }}"
```

- [ ] **Step 2: 本地 `bash -n` 校验 yaml 外语法**（无 Go 依赖部分）。CI 效果在推到远端后观察。

---

# WS1 — 读侧瘦身：`mem notes` + 注入分工

**Files:**
- Create: `cmd/mem/notes.go`
- Modify: `cmd/mem/main.go`（注册 `mem notes`）
- Create: `cmd/mem/notes_test.go`
- Modify: `cmd/mem/hook.go`（prompt-submit 减少 rule 注入的重复）

## Task WS1.1: `mem notes` 生成硬规则小字条（幂等）

- [ ] **Step 1: 写失败测试** `notes_test.go`：

```go
func TestNotesIdempotent(t *testing.T) {
    db := testDB(t)
    if err := cmdAddDebug(db, "全局硬规则：commit 前必跑 make cover-gate", "global"); err != nil { t.Fatal(err) }
    out1, _ := runNotes(db)
    out2, _ := runNotes(db)
    if out1 != out2 { t.Fatalf("notes 应幂等：\n%q\n%q", out1, out2) }
}
```
> 注：`cmdAddDebug` 为测试辅助（后续引入或复用现有 cmdAdd 全局分支）。

- [ ] **Step 2: 实现 notes.go**：查询 `kind='rule'`（或 project 空、salience 高）的记忆，一行一规则输出纯文本；多次运行输出稳定（按 kind/updated_at 定序）。
- [ ] **Step 3: 跑测试确认通过。**

## Task WS1.2: 注入分工 + 读侧 flag 与回退

- [ ] **Step 1: 写失败测试**：断言 hook 注入时，若细节条与 notes 小字条重复则去掉重复（预算不重复）。
- [ ] **Step 2: 实现**：hook 注入前对规则类记忆做一次过滤，命中 notes 小字条内容的不再重复注入；新增 `MEM_NO_NOTES` 环境开关作 flag，置位则读侧回到"仅 v2mem 原始注入"。
- [ ] **Step 3: 跑测试确认通过。**

---

# WS2 — 写侧统一收录（`mem ingest --source`）

**Files:**
- Modify: `cmd/mem/ingest.go`
- Create: `cmd/mem/ingest_test.go`

## Task WS2.1: 补 `--source` 锚定双路径写

- [ ] **Step 1: 写失败测试**：`ingest --source harness-summary` 写入的记忆 `source='harness-summary'`；未显式给 source 时默认不冒充 harness-summary（沿用默认 ingest）。
- [ ] **Step 2: 实现**：复用已加的 `AddInput.Source/Tool`；仅 whitelist 允许 `harness-summary`，其余回落默认。
- [ ] **Step 3: 跑测试确认通过。**

## Task WS2.2: 审计记录写侧 source

- [ ] **Step 1: 写失败测试**：ingest 写下后审计记录带 `source`。
- [ ] **Step 2: 实现**：已在 `audit.Record.Source` 打通，ingest 收尾补传。
- [ ] **Step 3: 跑测试确认通过。**

---

# WS3 — 跨设备 git 同步（`mem sync`）

**Files:**
- Create: `cmd/mem/sync.go`
- Create: `cmd/mem/sync_test.go`

## Task WS3.1: 三路合并（content_hash + updated_at）

- [ ] **Step 1: 写失败测试**：构造 A/B 两副本，同 hash 不同时间 → 最近者胜；同 hash 同近 → 并存待人工 merge。
- [ ] **Step 2: 实现**：`mem sync <remoteRepos...>` 读取远端 db 导出（复用 `ExportRecord` 含 Source）、本地导入，按 hash+updated_at 判并（逻辑详见 spec §WS3）。
- [ ] **Step 3: 跑测试确认通过。**

## Task WS3.2: git 拉推 + 备份 + abort

- [ ] **Step 1: 写失败测试**：构建一 bare 远端 → push db 到临时分支 → 本地改 → pull 合并 → 断言无丢失、冲突时 `--abort` 复原且 db 有 `VACUUM INTO` 备份。
- [ ] **Step 2: 实现 sync.go 的 git 编排（`git pull`→merge→`git push`），写前备份。
- [ ] **Step 3: 跑测试确认通过。**

---

# WS 自检对照（写作时逐条打勾）
- [ ] spec §3 WS1 → WS1 任务；WS2 → WS2；WS3 → WS3；WS4 → WS4
- [ ] spec §6 测试项 → 每个 WS 至少 3 个 TDD 测试；cover-gate 不降
- [ ] spec §4 回退 → WS1.2 回退开关 / WS3.2 abort+备份
- [ ] spec §5 范围（不做向量/装备/ server）→ 无越界任务

## Execution Handoff
计划已定。执行方式二选一（见交付对话）：推荐**子代理驱动**，每任务新起子代理、任务间两段式审查，适合并行 4 worktree。