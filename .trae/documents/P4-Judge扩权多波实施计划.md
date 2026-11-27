# P4 Judge 扩权多波实施计划

## Summary

用户已确认本轮做**完整 P4**：补齐前置 + Judge 自动 reject + Judge-gated accept；默认启用；由 Judge 接手自动裁决，`maintain` 退回只发窗。采用多波 subagent：波 1 并行补上下文/触发接管/标定统计，波 2 并行落裁决与回滚，波 3 集成灰度与验收，波 4 合入安装观察。

**基线**：main `739eb75`；工作区有一条未提交的 `AGENTS.md` 自动记忆规则，先作为 Wave 0 提交。

## Current State Analysis

- `cmd/mem/judge.go` 仍是影子：`applyJudgments` 只调 `MarkJudgeVerdictCAS` 写 `judge_verdict/judge_model/judge_reason`，不改 status（`cmd/mem/judge.go:220-258`）。
- P4 前置已完成 3 项：CAS（`internal/store/judge.go:31-56`）、一输入一输出（`validateJudgeVerdicts`）、上下文 revision/task_state/证据摘要；prompt 已升 `judge-v3`。
- 缺失：已有记忆上下文、按 judge_model 的 reject 标定、Judge reject/accept 落库、Judge reject 的按版本回滚。
- 自动触发 `maybeSpawnJudge` 只在 `Stop/SessionEnd`，默认要求 `MEM_JUDGE_SPAWN=1` 或 `model.json auto_spawn=true`（`cmd/mem/judge_hook.go:48-121`）；`judgeArgs` 不含 apply 参数。
- 当前钩子 `distillProcArgs` 是 `maintain --model-auto --max-bundles 20`（`cmd/mem/distill_hook.go:17-23`），与“Judge 接手”冲突，需要改为 proposal-only。
- `review stats` 只按 `commit_decisions.model_tag` 统计已创建记忆（`internal/store/review.go:900-940`）；`RollbackModelTag` 只处理 `created_memory=1` 的记忆（`internal/store/review.go:787-804`），不覆盖 Judge reject 的候选。

## Decisions

1. **Judge 接手**：SessionStart 的自动写侧改为 `maintain --stage=proposal --max-bundles 20`；`Stop/SessionEnd` 的 Judge 负责 accept/reject。
2. **默认启用**：`model.json.auto_spawn` 改为三态（缺省=开，显式 false=关）；`MEM_JUDGE_SPAWN` 仍优先覆盖；`MEM_NO_AUTOMATION=1` 总闸仍最优先。
3. **手动 Judge 仍影子**：`mem judge` 只有显式 `--apply-reject/--apply-accept` 才改状态；自动子进程默认带这两个 flag。
4. **reject 落库**：Judge reject 走 `CommitReject`，actor=`judge`，理由码 `judge_reject`；冲突类（`review_class=conflict`）跳过自动 reject。
5. **accept 落库**：新增 `JudgeAutoCommit`，复用 `commit.Matrix{AllowModelAuto:true}`；只有矩阵判 `auto_commit` 才写记忆，Defer/Conflict 只记裁决，绝不直接 `CommitAccept`。
6. **节流不变**：自动 Judge 仍受默认 20 条 pending / 24h 冷却约束；“默认启用”指无需额外授权，不是每次 Stop 都跑。
7. **回滚**：扩展 `review rollback --model-tag`，同时回滚该 tag 的 Judge reject（候选回 pending），人工后续动作过的跳过；dry-run 零写。
8. **标定**：`review stats` 增加 Judge 维度（verdicts/reject/reopen），与现有记忆后审核统计并列；阈值只 WARN。
9. **不升 schema**：使用现有 `judge_verdict/judge_model/judge_reason/commit_decisions.model_tag`；prompt 版本升 `judge-v4`。

## Wave Plan

| 波 | 任务 | 子代理 | 依赖 | 主要文件 |
|:--|:--|:--|:--|:--|
| 0 | 提交 AGENTS 规则、基线门禁 | 主控 | — | `AGENTS.md` |
| 1 | A 已有记忆上下文 | 1 | — | `cmd/mem/judge.go`、judge tests |
| 1 | B 触发接管 + 默认启用 | 1 | — | `cmd/mem/judge_hook.go`、`distill_hook.go`、`internal/distill/modelconfig.go` |
| 1 | C Judge 标定统计 | 1 | — | `internal/store/review.go`、`cmd/mem/review.go` |
| 2 | D Judge reject/accept 落库 | 1 | A | `cmd/mem/judge.go`、`internal/store/judge.go`、`commit.go` |
| 2 | E Judge reject 按版本回滚 | 1 | C | `internal/store/review.go`、`cmd/mem/review.go` |
| 3 | F 集成/灰度/验收 | 主控 | A–E | 临时 HOME/DB、`docs/acceptance-automation.md` |
| 4 | G 合入、安装、观察 | 主控 | F | main + `~/.local/bin/mem` |

## Detailed Tasks

### Wave 0：基线（主控）

- 提交已在工作区的 `AGENTS.md` 自动记忆规则。
- 记录 `make test` / `make cover-gate` 基线；确认 `~/.v2mem` 生产库不动。
- 每个代码任务从最新 main 建独立 `.worktree/<task>` / `dev/<task>`，只提交不 merge。

### Wave 1 / A：已有记忆上下文

**Branch**：`dev/p4-context`。

- `judgeCandidatesJSON` 增加 `neighbor_memories`：对每条候选，用 `st.Search(store.SearchQuery{Query: 截断正文 80 rune, Project: cand.Project, Limit: 3})` 取最多 3 条已有记忆正文摘要；project 为空时 `Scope:"all"`。
- prompt 增加一句：`neighbor_memories` 是已有记忆，用于判断重复/更新，不要回显；版本 `judge-v3 → judge-v4`。
- 测试：种子记忆出现在 JSON；跨工程不串；空上下文输出省略字段；正文仍截断 600 rune。
- 门禁：`make test` + `go test ./cmd/mem -run TestJudge`。

### Wave 1 / B：触发接管 + 默认启用

**Branch**：`dev/p4-trigger`。

- `ModelConfig.AutoSpawn` 改为 `*bool`（或等价三态）；`judgeAutoSpawnEnabled`：
  - env `MEM_JUDGE_SPAWN` 1/0 优先；
  - `Mode=="off"` → false；
  - 显式 `auto_spawn:false` → false；
  - 缺省且有模型配置 → true。
- `judgeArgs(limit)` 改为 `judge --limit N --apply-reject --apply-accept`。
- `distillProcArgs` 改为 `maintain --stage=proposal --max-bundles 20`（Judge 接手，不再 `--model-auto`）。
- 更新 `judge_hook_test.go`、`proposal_hook_test.go` 与 modelconfig 测试；读路径零 I/O 断言保留。
- 门禁：`make test` + `make test-watch`。

### Wave 1 / C：Judge 标定统计

**Branch**：`dev/p4-calibration`。

- `ReviewStats` 保持现有记忆后审核行；新增按 `candidates.judge_model` 的 Judge 行：
  - `verdicts`（judge_verdict 非空）、`reject`（judge_verdict=reject）、`reopened`（后续 reopen 决策）、`applied`（对应 commit_decisions actor=judge）。
- `mem review stats` 文本增加第二张表/列；JSON 增加字段，旧字段不变。
- 测试：种子 verdict/reject/reopen/applied；空样本仍输出“暂无”。
- 门禁：`make test` + `cover-gate`（store 82% 下限）。

### Wave 2 / D：Judge reject/accept 落库

**Branch**：`dev/p4-apply`（基于 A 合入后的 main）。

- `cmdJudge` 增加 `--apply-reject` / `--apply-accept`（默认 false）。
- `applyJudgments` 在 `MarkJudgeVerdictCAS` 成功后：
  - `reject` + applyReject：`CommitReject(id, revision, "judge", "judge:"+reason)`；`ReviewClass=="conflict"` 跳过；stale/closed 计入 skipped。
  - `accept` + applyAccept：`st.JudgeAutoCommit(id, revision, modelTag)`；矩阵 `AllowModelAuto:true`，只有 `Committed` 才落记忆；Defer/Conflict 计数。
  - `unsure` 不改状态。
- `Candidate` 投影补 `ReviewClass`（candidateSelect/scanCandidate/candidateByID）。
- store 新增 `JudgeAutoCommit`（镜像 `ModelAutoCommit`，actor=`judge`）。
- `CommitReject` 理由码按 actor：`judge_reject` / `model_reject` / `human_reject`。
- 输出新增 `applied_accept` / `applied_reject` / `skipped`。
- 测试：accept 过矩阵；Defer 不写记忆；Conflict reject 跳过；CAS stale 跳过；幂等重跑不重复。

### Wave 2 / E：Judge reject 按版本回滚

**Branch**：`dev/p4-rollback`（基于 C 合入后的 main）。

- 扩展 `RollbackModelTag`：除现有 `created_memory=1` 记忆外，命中 `commit_decisions`：
  - `action=reject`、`actor=judge`、`model_tag=?`、候选仍 `rejected`；
  - 若之后有 `actor=human` 的 reopen/accept/reject，跳过并报告；
  - 调 `RollbackCandidate(..., deleteMemory=false)` 回 pending，记 reopen；dry-run 只计数。
- `RollbackReport` 增加 `JudgeReopened` / `JudgeSkipped`；CLI 输出与 JSON 更新。
- 测试：judge reject 回滚回 pending；人工已处理跳过；dry-run 零写；重跑幂等。

### Wave 3 / F：集成、灰度与验收（主控）

- 临时 HOME + 假 CLI 编排：
  1. `maintain --stage=proposal` 造 fact/decision/conflict 候选；
  2. `judge --apply-reject --apply-accept` 用假 verdict（accept fact、reject 过程叙述、conflict、unsure）；
  3. 断言：fact 落未审核记忆；过程叙述 rejected；conflict 仍 pending/仅裁决；unsure 不变；
  4. `review stats` 看到 judge 标签；`review rollback --model-tag judge...` 把 reject 回 pending。
- 生产只读：`mem review stats`、`mem stats`；不跑生产 Judge apply。
- 更新 `docs/acceptance-automation.md`：P4 前置/扩权结果、默认启用开关、回滚方式。
- 全量门禁：`make test`、`cover-gate`、`test-faults`、`test-e2e`、`test-watch`。

### Wave 4 / G：合入与观察（主控）

- 按 A→B→C→D→E 顺序 `git merge --no-ff`，正文写 `Base/Gate/Closes` + 测试尾部。
- main 复跑门禁，清理 worktree/分支。
- `make install` 更新 `~/.local/bin/mem`；确认 `MEM_JUDGE_SPAWN=0` / `auto_spawn:false` / `MEM_NO_AUTOMATION=1` 三条回退路径。
- 观察下一批真实会话的 `review stats`、`review ls`、reject/accept 比例；异常先关自动 Judge。

## Verification Matrix

| 目标 | 证明 |
|:--|:--|
| 已有记忆上下文 | Judge JSON 含 neighbor_memories；跨工程不串 |
| 默认启用 | 无 env、有模型配置、auto_spawn 缺省 → Judge 拉起；显式 false/Mode off → 不拉起 |
| Judge 接手 | SessionStart 只跑 maintain proposal；Stop 拉 judge apply |
| reject 落库 | 过程叙述候选 status=rejected，decision actor=judge/reason=judge_reject |
| accept 过矩阵 | fact 落未审核记忆；Defer/Conflict 不写记忆；不会直接 CommitAccept |
| 回滚 | judge reject 候选回 pending；人工处理过的跳过；dry-run 零写 |
| 标定 | review stats 按 judge_model 显示 verdict/reject/reopen |
| 门禁 | main 上四道门禁 + watch 全绿 |

## Risks & Mitigations

- **默认自动改状态**：auto_spawn 默认开；回退 `MEM_JUDGE_SPAWN=0`、`auto_spawn:false`、`MEM_NO_AUTOMATION=1`。
- **Judge 误 reject**：`review rollback --model-tag` 可批量回 pending；冲突类不自动 reject。
- **accept 绕过矩阵**：只走 `JudgeAutoCommit` 的 Matrix，绝不直接 `CommitAccept`。
- **成本/延迟**：仍受 20 条/24h 冷却；Judge 每批最多 20 条，邻居检索最多 3 条。
- **标定样本不足**：`review stats` 只提示 WARN，不自动调阈值；首轮观察后再判断。
- **工作区规则未提交**：Wave 0 先提交 AGENTS.md，避免混入代码分支。

## Assumptions

- 用户确认：完整 P4（前置 + reject + accept）、默认开启、Judge 接手。
- 自动 Judge 仍保留 20 条/24h 节流；如需更快，由 `MEM_JUDGE_MIN_PENDING` / `MEM_JUDGE_COOLDOWN_H` 调整。
- 不新增 schema；Judge 回滚不删记忆，只回候选。
- 生产存量积压与 P4 独立；本轮不扩大 drain 范围。
