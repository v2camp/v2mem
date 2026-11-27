# 自动化闭环多波验收记录（2026-11-26）

> **定位**：建议 1–5 的多波落地证据。代码在 main，真实生产库只做只读诊断；
> 所有写操作均在临时 HOME / 临时库 / 生产库副本完成。
> **基线**：`43ed16c`；本轮结束 HEAD 见 §6。
> **相关**：[`design-data-lifecycle.md`](./design-data-lifecycle.md)、
> [`design-eval-flows.md`](./design-eval-flows.md)、[`acceptance-p4.md`](./acceptance-p4.md)。

## 1. 结论摘要

- 波 1–3 全部合入：maintain 自动裁决、`memories.scope` 写入、eval prod 剖面、
  maintain 全阶段、detach 验证、cursor 回收、CLI 命令面低风险收编。
- `mem maintain --model-auto --stage=all` 在临时库一次跑通
  proposal→adjudicate→consolidate→gc：1 条候选自动采纳为未审核记忆，
  `commit_decisions.actor=model-auto`，`manual_rate=0`。
- 生产库副本：`eval flow --profile=ci` 8/13（与既有基线一致）；
  `--profile=prod` 13/13，`legacy_exempt=343`（69 候选 + 274 运行账）。
- 生产真实短板仍是**吞吐与启用**：180/180 源未消费、14,269 序号待处理、
  最老 13.4 天；单轨仅 16 个 Bundle / 298 items（全 noop）。
  自动裁决机制已就绪，但生产尚未开启，`commit_decisions` 仍 0 条 model-auto。
- 本轮不包含 P4（Judge 扩权）；`accept`/`reject` 保留为兼容入口，下一轮灰度签字后删除。

## 2. 波 1：生产解阻

### 2.1 A：maintain 自动裁决（`252e1ed` → merge `e9ba3ac`）

- 新增 `--model-auto` / `--model-tag` / `--adjudicate-scope=this-run|all` /
  `--max-adjudications`；默认关，无 flag 时行为不变。
- `this-run` 只裁决本轮 Bundle 的 pending；`all` 才触碰历史 pending。
- `--dry-run` 只计数 `would_adjudicate`，零 decision/零写；单条失败聚合返回。
- 临时库灰度输出（节选）：

```json
{"processed":1,"ok":1,"committed":1,"adjudicate_scope":"this-run",
 "would_adjudicate":1,"adjudicated":1,"adjudication_errors":0}
```

- 库内结果：`candidates accepted=1`，`commit_decisions` = `model-auto/auto_commit`，
  记忆 `review_state=''`（待后审核）。
- 测试：默认 maintain 不落记忆；model-auto 落记忆；this-run/all 范围；
  dry-run 零写；缺 tag 报错；临时库 hook→maintain→eval flow 13/13、`manual_rate=0`。
- 门禁：`make test`、`make cover-gate`（store 82.0%，cmd/mem 82.3%）全绿。

### 2.2 B：`memories.scope` 写入（`b9fc470` → merge `11ea620`）

- `upsertAcceptedMemory` INSERT 写 scope；UPDATE 只补空、不覆盖非空。
- `normalizeMemoryScope` 只允许 `global|cross_project|project`，未知值置空。
- 不升 schema、不回填历史；旧行 scope 为空属于已知历史。
- 测试覆盖 global 写入、空 scope 不覆盖、未知值归一、summary 确定性抽样；
  store 覆盖率 82.1%（下限 82%）。

### 2.3 C：eval legacy / prod 剖面（`8a4dd21` → merge `521a86c`）

- 新增 `FlowOptions{Profile,AllowEmpty}` 与 `CheckFlowWithOptions`；
  legacy 判定只用 `bundle_id=''`，不引用日期。
- 默认 `ci` 严格口径不变；`prod` 过滤 legacy 并降级“无样本”分支，
  当前行真实违例仍 FAIL。G9 的无候选分支也纳入 `AllowEmpty`
  （过滤后 0 候选否则会误红），已在设计文档注明。
- 生产库副本（只读）：

| 剖面 | 结果 | 说明 |
|:---|:---|:---|
| `ci` | 8/13 | G7/G9/G11/G12/G13 红，与基线一致 |
| `prod` | 13/13 | `legacy_exempt=343`（69 候选 + 274 运行账） |

- 临时库：`ci` 12/13（仅 G7 无失败样本），`prod` 13/13。

### 2.4 D：积压/SLA 只读诊断（无代码提交）

| 桶 | 源数 | 结论 |
|:---|:---|:---|
| 可出窗但未出窗（有未读字节、无预算） | 179 | `MAX(to_seq)>cursor_seq`，cursor 多为 0 |
| 出窗但全 noop | 1 | `063320fc` 已 16 窗 298 条，均为 unparsed/pointer/ineligible；余 229 行 |
| 无归档/无未读字节 | 0 | 不存在“假积压” |

- 16 个 Bundle 全来自同一 transcript 源；298 items 100% `unparsed`、
  `proposal_eligible=0`、`fidelity=pointer`，`countEligible()==0` 直接 noop，不调模型。
- 覆盖仅 2% 的直接原因：钩子预算固定 `--max-bundles 1`
  （`cmd/mem/distill_hook.go:21`），今日约 16 次触发=16 窗；64KB 上限使每窗约 20–27 条。
- 估算约 679 窗才能清空；真正可提案的约 1,510 条（≈60 窗），
  95.7% 积压是高噪声 transcript。
- 代码/配置问题（本轮未修，转入下一轮）：
  1. 钩子固定 1 窗，追不上 14k 积压；
  2. `internal/store/evidence.go:533` 严格 `created_at` 排序让 13k transcript 长期占队头，
     堵住 135 个高价值 hook 源；
  3. `maintain` 遇 `SQLITE_BUSY` 会整轮早停（`spawn.log` 已见）；
  4. 88 条 legacy observations（`source_id=''`）永不入 Bundle，属历史不参与积压。

## 3. 波 2：维护全阶段与终态

### 3.1 E：`maintain --stage` 全阶段（`a9c8203` → merge `42c585a`）

- 新增 `--stage=proposal|adjudicate|consolidate|gc|all`，默认 `proposal`；
  逗号多选按固定顺序执行，`all` 显式 opt-in。
- 抽取 `runConsolidate` / `runRetention` 复用核心，CLI 输出与退出码保持兼容。
- 临时库 `--stage=all` 实测：

```text
proposal ok(processed=1) → adjudicate ok(committed=1) → consolidate ok(0) → gc ok(0)
```

- `--dry-run` 全阶段零写：candidates/decisions/memories 计数前后不变；
  `consolidate` 无预览接口，dry-run 标 `skipped`（本轮已知限制）。
- 门禁：`make test`、`make cover-gate`、`make test-faults`（F7–F12 无回归）。

### 3.2 G：detach 存活验证（`9cf6c1a` → merge `141d0e7`）

- 报告写入 [`design-harness.md`](./design-harness.md) §12.15，6 个场景：
  正常退出、父 SIGKILL、子进程 STOP→租约到期 catch-up、并行重复触发、
  子进程 SIGKILL、进程组信号。
- 结论：父正常退出/单 PID SIGKILL 下子进程存活并按租约恢复；重复触发只消费一次。
- 已知限制：未设 `Setpgid`，`killpg`/Ctrl-C 会带走子进程；真实整机睡眠未测；
  子进程 stdout 丢弃。
- **附带发现（已修复）**：`candidates.run_id` 与 `evidence_bundles.last_run_id` 曾恒空。
  `internal/proposal/runner.go` 先给 `out.RunID` 赋值，随后 `out.Result = res` 又把它覆盖成空串；
  修复后 RunID 在 `runClaimed` 入口生成、执行结果保留同一值，并作为 `distill_runs.id` 写入，
  让候选/Bundle 的 run_id 指向真实运行账；回归用例
  `TestRunnerRunIDLinksCandidateLedgerAndBundle` / `TestRunnerNoopRunIDLinksLedger`。

### 3.3 H：transcript cursor 终态回收（`2d00fb2` → merge `daf7bf9`）

- 超龄 30 天且（transcript 不存在或 `Size <= Offset`）才删除；未读余量、
  坏 JSON、不可读文件全部保留；dry-run 只计数。
- 报告字段 `cursors_deleted` + `cursors_listed`；原恒 0 的 `cursors` 字段被替换
  （仓内无其他消费者，外部脚本需同步）。
- 测试 5 类组合；`make test` / `make test-watch`（90/90）/ `cover-gate` 全绿。
- 合入后 `maintain --stage=gc` 的 counts 键由 `cursors` 适配为 `cursors_deleted`
  （`b2cc2b9` 集成修复）。

## 4. 波 3：CLI 命令面收编（`91e6763` → merge `31ff791`）

- 删除 `migrate` / `batch` / `candidates`：调用退出码 2，并提示替代入口：
  - `batch` → `mem review rollback`；
  - `candidates` → `mem review ls --state pending|all`；
  - `migrate` → 已无必要。
- `mem review ls` 承接候选视图：补 `project/content/created_at/reviewable`；
  巡检 R8/R9/R15 改读 `review ls --state all --json`，只读边界与 R15 处置命令未动。
- `accept`/`reject`/`judge`/`distill`/`gc`/`consolidate` 保留为兼容/运维兜底，
  usage 标记下一轮随 P4/灰度删除。
- 实际命令面 **37→34**（计划中的 33→30 是旧口径）；`knownSubcommands` 由测试锁定 34。
- 门禁：`make test`、`make test-watch`（90/90 + 21 判据同步）、`cover-gate`
  （cmd/mem 82.4%，TOTAL 84.8%）。

## 5. 生产只读指标（2026-11-26）

| 指标 | 实测 |
|:---|:---|
| evidence sources | 180（全 hook） |
| 未消费源 / 待处理序号 | 180 / 14,269 |
| 最老未消费证据 | 约 13.4 天（15 天 SLA） |
| observations / archives | 14,655 / 14,567 |
| evidence_bundles / items | 16 / 298（全 noop） |
| candidates | accepted 6 / expired 2 / pending 6 / rejected 55 |
| commit_decisions | human accept 6 / reject 55 / review_action 2；**model-auto 0** |
| memories | reviewed 11 + 未审核 7 |
| eval flow（库副本） | ci 8/13；prod 13/13（legacy_exempt 343） |

口径：`ci` 只用于干净夹具/tag 判据；生产健康检查用 `prod`，不得用生产库的
ci 红项判 tag，也不得用 prod 绿灯掩盖当前行违例。

解释：机制侧已能自动裁决与清理；生产侧尚未启用 `--model-auto`，
积压与 13.4 天 SLA 是当前最大运行风险。

## 6. 门禁与合入

- main HEAD：`31ff791`（波 3 合并）；本轮文档提交在其后。
- `make test`：watch 90/90 + `test/flow` + 全包 ok。
- `make cover-gate`：store 82.1%（≥82）、cmd/mem 82.4%（≥81）、TOTAL 84.8%（≥83）。
- `make test-faults`：`ok test/faults 13.502s`。
- `make test-e2e`：01–07 全 PASS，`ok test/e2e 5.855s`。
- `make perf`：`ok test/perf 11.804s`。
- `make test-watch`：90/90；`test-watch-sync` 21 判据一致。

## 7. 运行缺口补强（2026-11-27）

- 钩子自动写侧 argv 改为 `maintain --model-auto --max-bundles 20`：一次会话
  完成发窗 + model-auto 裁决，不再停在候选；预算从 1 窗提到 20 窗。
  真机 hook 集成：SessionStart 后落 1 条 `model-auto/auto_commit` 未审核记忆，
  `spawn.log` 无错误。
- 发窗改为短积压优先（`evidenceSourcesWithBacklog`），避免大 transcript 源
  长期霸占队头；新增长短积压源调度用例。
- `maintainRun` 对 SQLITE_BUSY 做 3 次小步重试，避免一遇锁就整轮早停。
- eval 两面口径写入 `design-eval-flows`：ci 用于夹具/tag，prod 用于生产健康。
- 仍然存在的是**积压存量**：代码已能追，生产仍有 181 源 / 14,261 序号 /
  13.5 天，需要后续会话或一次有界 catch-up 实际消化；这不属于代码缺口。

## 8. 未决 / 下一轮

> 已关闭：run_id 联动（见 §3.2）——`candidates.run_id` /
> `evidence_bundles.last_run_id` 现在指向真实 `distill_runs` 行。

1. **积压存量**（D 发现）：短积压优先 + 20 窗/次已落地；仍需观察实际 drain
   曲线，必要时做一次有界 catch-up。
2. **生产启用观察**：钩子已接线 model-auto；下一轮看真实 auto_commit /
   withdraw / correct 与 manual_rate，再决定是否删除 accept/reject。
3. **accept/reject 最终删除**：等生产灰度签字。
4. **P4 Judge 扩权**：前置项尚未齐（CAS、输入输出完整性、上下文）；本轮开始补。
5. **已知限制**：真实整机睡眠未测；进程组信号会带走子进程；consolidate dry-run 无预览；
   历史 memories.scope 未回填；`cursors` JSON 字段改名需外部脚本同步。
