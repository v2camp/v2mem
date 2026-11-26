# 数据动线与统一维护（design-data-lifecycle）

> **定位**：回答「每一份存储从准入到准出的动线是什么、如何自动闭环、SLA 怎么定义」。
> **来源**：2026-11-24 全量数据审计（21 类表 + 持久化文件逐项矩阵）与 2026-11-25 产品边界裁决。
> **读者**：实现者。本文件是 P0–P4 的实施依据，另一台机器照此开发。
> **相关**：[`VALUE.md`](./VALUE.md)（红线）、[`design-write-governance.md`](./design-write-governance.md)
> （三态链路）、[`design-admission.md`](./design-admission.md)（判定器现状）、
> [`ops-watch.md`](./ops-watch.md)（巡检判据）、[`contracts/storage-layering.md`](../contracts/storage-layering.md)（外置口径）。

---

## 0. 裁决摘要与取代声明

**已由所有者裁定的产品边界（2026-11-25，不再讨论）**：

1. **人工门槛取消**：候选由模型基于**可配置提示词**自动处理，产出的记忆由人工**后审核**
   （确认 / 修正 / 撤回），人工根据错误样例迭代提示词。不是事前授权，是事后治理。
2. **休眠 / 关机期间不承诺处理时限**，不为此保留外部调度。
3. **持续故障**由人工修复后 `mem maintain --catch-up` 一键批量追账。
4. **15 天是活动数据的处理 SLA**（活跃期持续处理、唤醒即追赶），不是物理删除期限。
5. **LLM Judge 只做语义建议器**：调度、幂等、重试、GC、引用保护、提交与回滚由确定性维护系统负责。

**取代声明**（下列条款与本文件冲突处以本文件为准；P0 落地第一步按 §10 同步修订原文）：

| 被取代条款 | 去向 |
|:---|:---|
| `VALUE.md` §4 例外 (b) 影子期 ≥95%/样本 ≥200 才放行、(c) `--apply` 与回滚同批 | §6 后审核治理（回滚保留，形态变为按 `model@promptver` 撤回） |
| `design-admission.md` §2「模型不能比矩阵更宽」、§3 影子期转正门槛 | §6 / §7 |
| `design-write-governance.md` §12.2 修订注「不自动**授予**」 | §6（模型自动授予，后审核兜底） |
| `internal/commit/policy.go` 的 `ActionHumanAccept` 语义（`manualKinds`、global/cross_project 一律人工） | §6.3 矩阵改造 |

**不变**：`VALUE.md` §4 例外 (a) 按版本隔离（升级为提示词版本归因，见 §6.2）；
全部红线保留（钩子绝不阻断宿主、不做云服务/常驻进程、向量不进检索关键路径、L1 不接收细节、
二级索引只指向不判定、归档字节口径）。

---

## 1. 目标动线（唯一一条证据流）

```text
捕获/补写 → observations + 原始归档 → 连续 Bundle → executor（规则或模型）
  → candidates → 提交矩阵（确定性）× 模型自动裁决（提示词）
  → memories(未审核) / rejected / failed→quarantined
  → 人工后审核：确认 / 修正 / 撤回 → 最终记忆生命周期（检索、GC、同步）
```

**单轨规则**：

1. **Proposal 独占证据消费进度**。旧规则蒸馏改造成 executor 之一；
   `distill_runs.finished_at` / `window_key` 退出消费游标职责（可留作运行账）。
   schema v4 起 `distill_runs` 不再有 `UNIQUE(session_id, started_at)`：
   同 session 同一秒多条运行账是 `maintain` 连续追账的合法形态；窗口幂等由
   `ux_distill_window` 部分索引守护（见 [design-db-stamp.md](./design-db-stamp.md)）。
2. **0 条有效提案也必须形成 noop 完成水位**。禁止「过滤后为空就不记进度」的空转。
3. 发窗水位 = 已发放，Bundle 完成状态 = 已处理，两者语义分离但不再存在第二条旧规则消费链。
4. 每源有界重试、退避、最大 attempt；超限进入**可回捞隔离区**。
   单个坏 Bundle、坏 spool 段或序号洞不得永久阻塞健康源。
5. 15 天内必须**得到处置**（自动提交 / 自动拒绝 / 转 quarantine），不是必须采纳。
   超期 pending 进可回捞待人工/隔离，**禁止把改状态伪装成已消费**（`expired` 不得充当消费水位）。

---

## 2. 状态机与终态

### 2.1 候选与记忆

```text
candidates: pending
  ├─ 模型 accept → CommitAccept → memories（未审核，可检索可注入）
  │     └─ 人工：确认 → confirmed ｜ 修正 → corrected（记 actor，保留模型原提案）
  │              ｜ 撤回 → withdrawn（退出主检索，恢复 rejected 或其他明确终态）
  ├─ 模型 reject → rejected（保留证据，可回捞）
  └─ 调用/输出校验失败 → failed（等待重试；不能当成 reject）
        └─ 重试超限 → quarantined（人工确认修复后显式重放）
```

- 未审核记忆**立即成为最终记忆**并进检索——积压不能从 pending 搬进另一个看不见的队列。
- 证据缺失、冲突、输出校验失败**不得为了消灭 pending 强行写入**：矩阵的
  `ActionDefer` / `ActionConflict` 与 failed 路径保留。
- 模型自动拒绝保留证据；判错可原路退回（候选仍在）。

### 2.2 终态保留（P3 落地，GC 只清终态、只清过保留期）

| 终态 | 保留策略 | 出口 |
|:---|:---|:---|
| `rejected` | 带理由码与证据引用保留（拟 90 天），到期随候选 GC | 可回捞查询 |
| `quarantined` | 不自动重放；人工确认修复后显式重新加入 | 人工 |
| `failed` | 重试队列内有限退避；超限转 quarantined | 自动→人工 |
| `withdrawn` | 保留模型原提案与撤回原因，供提示词改进统计 | 按版本统计 |
| 完成 Bundle（ok/noop） | 有保留期后回收（现状 ok/noop 无删除——补出口） | 完成账 |
| `commit_decisions` | 保留最近窗口 + 必要幂等键，分级回收（现状无 GC——补出口） | 幂等 |
| `plans/*.jsonl` | 回滚凭证，与候选/决策账一致的回滚保留期 | 回滚 |
| 最终记忆及其有效证据 | 按记忆生命周期保留；豁免 15 天 | 记忆 GC |

---

## 3. `mem maintain`：统一维护入口

短命、幂等、单实例。一条命令承接全部写侧维护；Hook 与巡检只负责唤醒与判级。

**一次唤醒后的内部顺序**（固定，可从中断点恢复）：

1. 获取单实例租约（防多 Hook 并发制造维护风暴）；
2. 恢复 spool `.processing`、过期租约、transcript 中断水位与上次检查点；
3. 补齐 transcript，连续生成 Bundle（含 noop 完成水位）；
4. **最老优先**执行就绪 Bundle（规则或模型只是 executor）；
5. 对候选走**确定性提交矩阵 + 模型自动裁决**（§6），自动提交合格项；
6. 其余项进入明确终态：`rejected` / `failed`（重试）/ `quarantined`；
7. 在完整引用保护（§8）下回收观测、归档与终态账本；
8. 保存检查点；队列空则退出，未空但有预算则继续 drain。

**两个模式**：

| 模式 | 语义 |
|:---|:---|
| `mem maintain --catch-up` | 故障恢复：恢复过期租约/中断水位 → 最老优先重试可恢复 failed/backlog → 连续完成到队列清空或遇仍需人工的错误 → 输出处理/提交/拒绝/隔离/剩余数量。必须幂等，不重复提交 |
| 巡检（watch 脚本） | **只读**判级：脚本正文不得出现任何写子命令（S3 已删 `--no-gc`，写侧维护单独授权）；状态迁移与物理删除分属 maintain，巡检只报 |

**边界**：

- 瞬时故障（网络抖动）自动有限重试，不必都麻烦人工；持续故障达上限停止空转，等人工修复。
- `quarantine` 默认不盲目重放；数据库物理损坏先恢复备份，`--catch-up` 不能代替灾备。
- **统一暴露**：最老 pending 年龄、最老未消费 evidence、最老 Bundle/spool、隔离量、
  最后成功维护时间。没有这些指标，「命令存在」会被误认为 SLA 已满足。

### 3.1 统一触达面（问题找人，零认知负荷的核心）

所有「等人工」出口——quarantine、failed 超限、未审核积压、模型不可用、备份/轮转失败、
处理超期——**只经一行触达用户**，不得要求人主动巡检或记得跑命令：

- 形态：SessionStart 注入行 `待人工 N 项（隔离 x ｜ 未审核最老 y 天 ｜ 模型不可用）`，
  仅 N>0 时出现；沿用 `design-write-governance.md` §12.2 点 1 的固定预算让路逻辑，
  与 pending 计数合并展示，不新增第二条注入。
- 指标（上一条）面向机器与告警，注入行面向人；**命令行输出不算触达**。
- 没有这一行，删掉定时巡检后「出问题到被发现」之间无界，自动化只完成了一半。

> **S11 已落地**：SessionStart 单行注入（`待人工 N 项（隔离 x ｜ 未审核最老 y 天 ｜
> 模型不可用 ｜ 维护失败）`，仅 N>0 时出现，与 `[待裁决]` 计数合并为同一条、沿用固定
> 预算让路）+ 唯一总闸 `MEM_NO_AUTOMATION=1`（只拦 proposal spawn / 冷启动 / judge spawn，
> 不拦 spool drain 等数据通路与任何手动命令）。

### 3.2 积压指标口径（S4）

`mem stats` 的文本三行（积压 / 隔离 / 维护）与 `--json` 顶层 `backlog` 对象暴露下列原始值。
**不判级、不设阈值**——判级仍由巡检的 R7/R8/R15 承担；这里只回答「现在积压多少、多久了」。

| 指标 | 口径 | 数据源 | 空值 |
|:---|:---|:---|:---|
| pending | 全部 `status='pending'`，不套 R8 的 reviewable 过滤 | `candidates` | 0 |
| 最老 pending 年龄 | `now - MIN(created_at)`，负值夹 0 | `candidates` | 无 pending → `null` |
| 未消费证据源数 | `last_seq > cursor_seq` 的源数 | `evidence_sources` | 0 |
| 最老未消费证据年龄 | 代际匹配（`s.generation = a.generation`）且 `a.to_seq > s.cursor_seq` 的 `MIN(a.created_at)` | `evidence_archives` ⋈ `evidence_sources` | 无匹配 → `null` |
| open Bundle | `status IN ('pending','leased','fail','partial','backlog')`（复用 `proposalOpenStatuses`，含 partial） | `evidence_bundles` | 0 |
| 最老 open Bundle 年龄 | 同上集合的 `MIN(created_at)` | `evidence_bundles` | 无 open → `null` |
| spool 段 | `<db-dir>/spool/<session>/seg-*.jsonl[.processing]` 的文件数；`.tmp` 不计 | 文件系统 | `0` 段 |
| 最老 spool 段年龄 | 上述文件的 `MIN(mtime)` | 文件系统 | `null` |
| 隔离量 | candidates `('quarantined','failed')` + bundles `'quarantined'` 两个计数 | `candidates` / `evidence_bundles` | 0 |
| 最后成功维护 | `db_meta['maintain:last_ok']`（unix 秒），`cmdGC` / `cmdConsolidate` 成功返回后写 | `db_meta` | 从未维护 → `null` |

两条口径说明：

- **Bundle 隔离由重试超限自动写入（S6a）；候选隔离待 S10**：`fail` / `partial` 累计到最大 attempt 后由
  runner 自动写 `quarantined`，可用 `mem distill --quarantined` 查看、`--requeue` 回捞
  （重置 attempt，不动 cursor 与 Bundle 条目）。候选侧 `quarantined` / `failed` 的写入者仍未
  落地（S10 引入）；先暴露计数是为了让后续切片有稳定读数面，而不是让「没有指标」被误读成
  「没有隔离」。
- **源数与年龄是两个口径，并列不合并**：源数按 `last_seq > cursor_seq`（源还有未发放的序号），
  年龄按归档的 `to_seq > cursor_seq`（确实有未消费的字节）。二者回答的问题不同，合成的单一
  「积压量」会掩盖「源在增长但归档未落」或反之的异常。

---

## 4. Harness 触发拓扑（唯一主触发面）

**原则：Hook 只负责捕获和唤醒；一次唤醒后由 maintain 连续处理到底，不逐阶段等下一次 Hook。**

| 事件 | 职责 |
|:---|:---|
| `SessionStart` | 恢复崩溃租约、处理 spool、检查上次未完成水位；必要时**异步**启动 maintain。保持轻量 |
| `UserPromptSubmit` | 写用户证据、检索注入；若维护未运行且已有积压，可异步唤醒。不阻塞提问 |
| `PostToolUse` | 只捕获证据。不承担维护，不作为正确性依赖 |
| `Stop` | 补写 transcript 与 stop 观测，**主要 maintain/drain 触发点** |
| `SessionEnd` | 最后补写、排空 audit、再次 drain；尾部证据处理完后才清理会话游标 |
| `PreCompact` | 读侧重注入；可选唤醒维护，正确性不依赖它 |

- 无 `SessionEnd` 的 Harness（TraeCode 现状）用 `Stop` 作降级入口，能力矩阵里注明语义较弱。
- **无任何原生钩子的 Harness**（如 freebuff：仅 MCP + knowledge.md/AGENTS.md 规则）：
  Hook 触发面整体缺失，不能按确定性 SLA 承诺。两条兜底：(a) **MCP 机会唤醒**——
  工具调用本身是触点，累计 N 次后顺手唤醒 maintain；(b) L1 文本钩子写明
  「无原生钩子时由 agent 任务收尾代跑 `mem maintain --wake`」。能力矩阵如实标注上限。
- **五小时巡检退出主数据面**：删除，或降级为只读 watchdog；SLA 不再靠它兜底
  （休眠期本就豁免，见 §5）。
- **前置验证项（采用 Harness-only 前必测）**：detach 子进程在宿主退出、机器睡眠、
  强制结束时的存活与恢复行为。结论进 `design-harness.md`。

---

## 5. SLA 口径（正式定义）

> **系统活跃且依赖可用时持续自动处理；休眠期（项目无会话、宿主退出、关机）不计入
> 自动处理承诺，唤醒后自动从持久化水位恢复并追赶。**

- 活跃期：证据从准入到准出持续闭环，非最终记忆数据不积压超过 15 天（处理 SLA）。
- 唤醒追赶：下次 Harness 唤醒时立即发现超期并 catch-up；**可以显示「已超期」**，
  但不承诺在无人运行时消灭超期。休眠第 30 天打开项目，第 15 天的墙钟目标已错过——
  如实展示，不宣称未超期。
- 最终记忆（`memories` 及有效证据）豁免 15 天，按记忆生命周期保留。

---

## 6. 模型自动提交与人工后审核

### 6.1 与旧准入的关系

正式替代 `design-admission.md` 的两条：decision/preference/rule、global/cross_project
**不再事前人工**；「影子期 ≥95%、样本 ≥200 才放行自动」不再作为放行门槛。
新底线四条：**版本可归因、未审核可识别、错误可撤回、人工修正不被批量回滚覆盖**。

### 6.2 提示词治理

- **`mem setup` 自带默认提示词**（版本化）：开箱即自动化，提示词是可选定制项，
  不是自动化的前置条件——「配好提示词才有自动化」等于把自动化建在人肉前置上。
- **提示词文件化、版本化**（Harness 不继承交互 shell 环境）：`~/.v2mem/prompts/` 或
  `model.json` 内嵌字段，`mem setup` 管理。每次裁决记录 `judge_model=provider:model@promptver`
  与内容哈希；改提示词只影响后续裁决，不改写历史归因。
- **模型只输出结构化 accept/reject + 闭集理由码**，由现有提交代码完成校验、幂等写入
  与证据关联——模型不直接写库。
- **提示词迭代阈值触发，不是例行工作台**：撤回率/修正率/误拒率越线才自动生成
  错误摘要包，人坐下一次改完；平时不打扰，按版本比较新旧指标。模型不得自行修改提示词。

### 6.3 提交矩阵改造（`internal/commit/policy.go`）

| 现状 | 改造 |
|:---|:---|
| `manualKinds`（decision/preference/rule）→ `ActionHumanAccept` | 模型可自动 accept（提示词归因），落 `memories` 未审核态 |
| global / cross_project / project 缺失 → `ActionHumanAccept` | 同上；全局类错误面大，**撤回与回滚优先级最高** |
| `ActionDefer`（证据缺失/等级不足/pitfall 缺 error 或 resolution/task 状态非法） | **保留**：不得为消灭 pending 强行写入 |
| `ActionConflict` | **保留**：冲突优先于一切 |
| actor 集合 `auto` / `human` / `human-reviewed-llm` / `judge` | 新增 `model-auto`（模型自动采纳）；后审核动作记 `human` + `review_action` |

- 自动 accept 仍走 `CommitAccept`：revision CAS、幂等键、`edges.derived_from`、
  `memory_evidence` 绑定全部保留。
- 幂等键纳入提示词版本（或独立 `prompt_hash` 维度），避免换提示词后误判重放。

> **S10b 已落地（提交矩阵改造）**：`commit.Matrix` 增加可注入开关 `AllowModelAuto`
> （默认 false，零值行为与改造前逐字一致）；打开后 manualKinds / global /
> cross_project / project 缺失分支可模型自动 accept，**Conflict / Defer 仍提前返回
> 优先保留**。幂等键纳入 `model_tag` 维度（`IdempotencyKeyWithModel`），换提示词后
> 同候选同 revision 的新裁决不被旧键挡成重放；不带版本的路径键值逐字节不变。
> 生产开关挂 `mem accept --model-auto --model-tag <model@promptver>`（默认关），
> 裁决 actor=`model-auto`、`model_tag` 落 `commit_decisions`，记忆留未审核态供后审核。

### 6.4 人工后审核（例外式保险，不是例行工序）

**默认信任自动入库**——后审核是出错时的保险，不是每条必经的工序。例行审核队列
= 把「没人点按钮」的病换个位置复发。只有三类进复核摘要（阈值触发，见 §6.2）：

1. 低置信（模型 self-score 低 / 理由码含糊）；
2. 高影响抽样：`global` / `cross_project` / `decision` / `preference` / `rule`
   按约 10% 比例抽样（落地按实际撤回率调）；
3. 冲突标记（`ActionConflict` 同源信号）。

| 动作 | 语义 |
|:---|:---|
| 确认 | 标记人工已审核（`reviewed_at` / `reviewed_by`） |
| 修正 | 保存人工修订内容并记 actor，保留模型原提案供比较 |
| 撤回 | 立即退出主检索，恢复 `rejected` 或其他明确终态 |

- **负反馈一步可达**：检索命中后 agent 应能一句话问出「依据哪条记忆」（读侧溯源），
  错了 `mem review withdraw <id>` 一步撤回。撤回率同时是提示词迭代的输入（§6.2）。
- **未审核只作观测不作义务**：注入行显示计数与最老年龄（§3.1），不要求人定期清空。
- **按版本回滚**：`mem review --rollback <model@promptver>` 撤回一批自动记忆；
  **已被人工确认或修正的条目跳过并报告**，不得盲目删除。
- 直写治理口径一并明确：显式 `mem add`、MCP `add`、`ingest`/`import` 仍可直接写最终记忆
  （`source` 已区分）；直写默认视为人工已审核，不进复核摘要。

> **S10a 已落地（schema v3 复核数据面）**：`memories.review_state` / `reviewed_at` /
> `reviewed_by` 与候选的 `status_reason` / `judge_reason` / `attempt`、裁决账的
> `model_tag` / `created_memory` 已由 `store.Open` 幂等补列迁移；`mem add` 等直写
> 写入即标 `confirmed`，`SetMemoryReviewState` 收敛状态取值，`withdrawn` 经
> `visibilityWhere` 退出 Search/List（行与证据保留，可 restore），`mem review ls`
> 提供一屏四态只读视图（pending / summary / quarantined / failed）。
> **`review_state` 回填只认可证明的直写来源**（白名单 `human|llm|ingest|harness-summary|bench|test`），
> `distill-accept` 与空 `source` 的 legacy 行一律留未审核，不得把全库一律标 `confirmed`；
> 回填只在补列那一次跑，不随每次 `Open` 重来。
> 动作面（`review withdraw` / `restore` / 按 `model@promptver` 回滚）与 `model-auto`
> 生产者留 S10b。

> **S10b 已落地（复核动作面）**：
> - **四动作** `mem review confirm|correct|withdraw|restore <memoryID>`（全部
>   `--json`、错误如实退出码 1，二级路由在 `cmd/mem/review.go` 内 switch）。
>   撤回恢复候选 `rejected` + `status_reason` 明确终态；每次动作记一条
>   `commit_decisions`（action=`review_action`，actor=`human`），correct 的
>   before/after（模型原提案与人工修订）进 `reason_codes_json`，不新增 schema 列。
> - **按版本回滚** `mem review rollback --model-tag <model@promptver> [--dry-run]`：
>   只命中 `created_memory=1` 且该 tag 的决策；人工已确认/修正的跳过并报告，
>   已撤回计 already，其余撤回 + 候选回 rejected + 记一条 `reopen` 决策（保 G9
>   裁决闭环）；**只撤回不删行**；dry-run 零写入；重跑幂等。
> - **标定统计** `mem review stats`：按 `model_tag` 分组给样本数与
>   confirm/correct/withdraw 率、误拒回捞（restore）数；阈值越线只提示 WARN，
>   不自动改提示词、不自动动作（严格只读）。
> - **复核摘要真实口径**：count 与 list 从同一查询取（修 S10a 的 JOIN 口径
>   不一致，候选被 GC 后不再出现「计数 1、列表 0」）；摘要只含三类——低置信
>   理由码、冲突标记、高影响确定性抽样（`hash(memory_id)%10==0`，
>   global/cross_project/decision/preference/rule），其余 model-auto 产物默认信任。
> - **eval flow G9/G12 在回滚后仍全绿**（`test/flow` R8 断言），不靠放宽 G 断言迁就实现。

### 6.5 shadow Judge 自动 reject、数据外发、跨设备同步

三者**分别授权，不捆绑开启**：shadow Judge 自动 reject（可回捞，风险低）可单独开；
候选正文外发到远端模型已在 `VALUE.md` §4 例外边界内；**候选与裁决账的跨设备同步默认关**
（先定冲突与归并语义再开）。

---

## 7. Judge 定位与扩权前置

**定位**：Proposal/候选的语义建议器。`maintain` 与提交矩阵是确定性控制者。
Judge 可以给 verdict 与理由建议，**不承担**调度、幂等、重试、GC、引用保护、提交权限。

现状差距（扩权前必须补齐，来自审计勘误）：

1. 按 `judge_model`（含 prompt 版本）分组的标定统计——现有聚合无法按模型版本独立验证；
2. `status`/`revision` CAS，防并发裁决覆盖；
3. 「一输入必一输出」完整性校验与未知 ID 错误处理；
4. 裁决上下文补齐：原始证据、已有记忆、task_state、revision；
5. **独立的 Judge 理由字段**，不覆盖提案 `rationale`；
6. 按 `judge_model` 的 rollback（§6.4 已列，Judge 视角同一能力）；
7. 自动 accept 必须再过提交矩阵，不得把 Judge accept 直接映射为 `CommitAccept`。

**模型不可用是常态（key 过期、额度耗尽、CLI 掉登录），不是例外**：自动降级到
**规则 executor 继续跑**——fact/task 照常自动提交，语义裁决待模型恢复后补判，
failed 不堆积；只有降级超上限才进 §3.1 触达面。人只剩「换 key」一件事。

默认 20 条阈值与 24 小时冷却是**触发节流**，不是处理 SLA 的替代。

---

## 8. 引用保护、GC 与终态出口

### 8.1 顺序铁律（P0）

**生产回读证明 → 全量引用保护 → 破坏性 GC 前备份 → 才允许扩大自动物理 GC。**

1. **生产回读证明**：归档现状是「写入 + 删除能力存在，生产回读不完整」——
   `ReadArchiveRecord`/`RefHealth` 只有测试在用。先补**生产级**按 `raw_ref` 读回 + 校验
   `archive_hash` 的入口，证明能读回之前不扩大自动 GC。
2. **全量引用保护**（GC 前必须检查的集合）：pending 候选、`candidate_evidence`、
   未消费 Bundle、未 Bundle 化活跃序号范围、最终记忆、`memory_evidence.archive_hash`、
   `plans/` 回滚凭证、（现状缺口）`edges` 对端指向 candidate 的孤边。
3. **破坏性 GC 前备份**：`backup/` 统一保留代数/年龄，删除前确认新备份有效。

### 8.2 各类出口（P3）

| 对象 | 改造 |
|:---|:---|
| 观测 / 归档 | 单证据流后按引用图 GC；补活跃范围与 memory 引用保护 |
| 完成 Bundle（ok/noop） | 有界保留后回收；`quarantined` 补生产入口；坏头有界重试 + 隔离 |
| `commit_decisions` | 保留最近窗口 + 幂等键，分级回收 |
| `read_traces` / items | 自动维护调用现有 GC，修正「活跃会话」判断；随父表统一保留 |
| `spool/*.jsonl` | 按段隔离、坏行 dead-letter、完成状态持久化、公平调度（S6c 已落地：坏行/确定性坏段进 `spool/dead-letter`，`.drain-cursor` 轮转会话） |
| 序号洞（`evidence_gaps`） | 遇缺失区间记洞并把 cursor 越过，同轮继续出窗；`mem distill --gaps` 只读出口，不可回捞（S6c 已落地） |
| `spool/seg-*.tmp` | 安全恢复或年龄回收（现状不被 drain 匹配，无出口） |
| `evidence/transcripts/*.cursor` | 会话终态回收；余量由维护入口续传 |
| 审计 `audit.jsonl` | 现有 5MiB 轮盘保留；**补 `.1–.3` 合并查询**（现状只查当前文件，历史断点）；轮转失败可观测 |
| `spawn.log` | 复用有界日志策略（现状无轮转） |
| `edges` | 接入 provenance/export 消费，或明确标记内部审计数据并设保留期 |
| 同步 `mem.jsonl`/`audit.jsonl` | 声明为**最终记忆投影**（memories/tags/memory_evidence），不是备份；同步审计补有界合并/轮转 |
| `.mem/reports`、`manifest.json` 等 | 开发工件保留策略，不混运行时记忆 |
| Harness 安装配置（hooks.json、.bak、AGENTS 注入、MCP/sync approval 配置） | 配置型，不设 15 天 SLA；`.bak` 限代数、安装幂等、破坏性维护前有可恢复备份 |

---

## 9. 改造清单（按数据集，源自逐项审计）

| 组 | 对象 | 现状问题 | 改造 |
|:---|:---|:---|:---|
| 证据链 | `observations` | 双消费者、双水位；GC 引用保护不足 | 单证据流；先保护再自动 GC |
| 证据链 | `evidence_sources`（55 源全部积压） | 无自动持续 drain | Proposal 独占消费进度；按源公平调度 |
| 证据链 | `evidence_archives` | 生产回读不完整；活跃范围可能被 GC | §8.1 回读证明 + 引用保护 |
| 证据链 | `evidence_bundles` | ok/noop 无删除；quarantined 无入口；坏头阻塞 | 有界重试、隔离、完成账回收、健康源继续 |
| 证据链 | `evidence_bundle_items` | 出口被 Bundle 阻断 | 随完成 Bundle 保留期回收 |
| 候选链 | `candidates` | 无自动 accept；rejected 永不删 | §6 自动裁决；终态保留 |
| 候选链 | `candidate_evidence` | rejected 候选使其永存；观测 GC 不检查它 | 并入统一引用图 |
| 候选链 | `commit_decisions` | 无 GC、无查询出口 | §8.2 |
| 候选链 | `plans/*.jsonl` | 无保留期 | 与回滚保留期一致 |
| 记忆链 | `memories` / `tags` / `fts_mem` / `mem_sigs` | 派生闭环完好 | 无需独立产线；显式直写口径见 §6.4 |
| 记忆链 | `edges` | dst 可指向 candidate，删除留孤边；弱消费 | 对端回收 + §8.2 出口 |
| 记忆链 | `memory_evidence` | 随记忆闭环 | 归档 GC 必查 `archive_hash` |
| 账本 | `distill_runs` | 旧水位不可靠 | 退出消费游标；完成 Bundle 不永久钉住 |
| 账本 | `migration_marks` | 静态标记 | 有界、随实体生命周期 |
| 账本 | `hook_dedup` / `db_meta` | 前者有自动出口；后者固定配置 | 纳入统一维护 / 不按年龄清 |
| 轨迹 | `read_traces` / `read_trace_items` | 无自动年龄出口 | 自动维护调用 GC |
| 索引 | `fts_obs` | 派生索引完好 | 随观测统一处理 |
| 文件 | `mem.db`/-wal/-shm | 主真源 | 维护前备份；回收受引用保护 |
| 文件 | spool / transcripts / spawn.log / backup / .bak | 各自缺口 | 见 §8.2 |

---

## 10. 实施顺序（P0–P4）与落地同步修改

**P0 — 防数据损失 + 权威同步**（没有这步，后面的自动化都在裸奔）：

1. 生产级归档回读与校验入口（§8.1）；
2. 全量引用保护（含 edges 对端、candidate_evidence、未消费范围）；
3. 破坏性 GC 前备份；拆分 R15 的状态迁移与物理删除（归 maintain）；
4. 巡检 / 维护分离：巡检正文零写子命令（S3 已删 `--no-gc`），写侧维护单独授权；
5. 最老积压指标（§3 统一暴露）；
6. **同步修订权威文档**（见下表），避免代码行为与设计权威矛盾。

**P1 — 消除双轨**：规则 executor 化、noop 推进、最老优先 + 按源公平调度、
有界重试、quarantine/dead-letter、序号洞处置。

> 按源公平调度的实现口径：每个源只看最老头，头在退避/租约中则整源让位，
> 绝不越头新建窗口（否则失败窗被同源新窗越过，连续水位即被打穿）。

**P2 — 真正无人值守**：统一 `mem maintain` 由 Harness 事件持续触发（§4），
五小时巡检退出主数据面；模型自动裁决 + 提交矩阵自动提交（§6）；
后审核 CLI 与未审核计量上线；**命令面收编（§12）随本阶段同批**。
detach 存活性验证在此期间完成。

**P3 — 终态出口**：§8.2 全表落地（rejected/Bundle/commit history/plans/日志/备份/
transcript cursor/同步审计/配置 .bak）；同步范围声明；轮转审计合并查询。

**P4 — Judge 扩权**（不作前置条件）：§7 前置项齐 + 标定与回滚验证后逐级开放。

**落地同步修改清单**（P0.6）。文档侧三项**已在本设计提交内完成**（VALUE.md 例外、
design-admission.md 取代声明、design-write-governance.md 再修订），代码与判据侧随开发落地：

| 文件 | 修改 | 状态 |
|:---|:---|:---|
| `docs/VALUE.md` §4 例外 | (b)(c) 重写为后审核治理；(a) 补提示词版本归因 | ✅ 本提交 |
| `docs/design-admission.md` 头部 | 取代声明；§2/§3 降级为标定工具 | ✅ 本提交 |
| `docs/design-write-governance.md` §12.2 修订注 | 「不自动授予」→「自动授予 + 后审核」 | ✅ 本提交 |
| `internal/commit/policy.go` | §6.3 矩阵改造（含 actor 与幂等键） | 开发时 |
| `cmd/mem` / MCP 工具面 | §12 命令面收编（随 P2）；`review` 系列替换 accept/reject/batch/candidates | 开发时 |
| 评测门禁 | §13 动线级 e2e（`eval flow` + U1–U8 + 逐跳 case，随各 P 阶段交付） | 开发时 |
| `docs/ops-watch.md` + `scripts/watch-v2mem.sh` | 两处同改：R15 拆分只读化；R8 增未审核/最老年龄口径 | 开发时 |
| `docs/design-harness.md` | detach 验证结论、无 SessionEnd 降级矩阵 | 开发时 |

---

## 11. 验收判据

1. **回滚不误删**：`--rollback <model@promptver>` 跳过人工确认/修正过的记忆并报告。
2. **追账幂等**：`mem maintain --catch-up` 连跑两次，第二次零重复提交（幂等键生效）。
3. **归档读得回**：生产入口按 `raw_ref` 读回并校验 `archive_hash`；读回失败则 GC 拒绝扩大。
4. **无空转**：0 有效提案的窗口也推进 noop 水位；`distill_runs` 不再被反复重判同一批。
5. **指标可见**：最老 pending 年龄 / 最老未消费 evidence / 最老 Bundle、spool / 隔离量 /
   最后成功维护时间可查。
6. **坏数据不阻塞**：注入一个坏 Bundle / 坏 spool 段 / 序号洞，健康源继续被处理，
   坏件进 quarantine。
7. **护栏不回退**：hooks 只读护栏、`make cover-gate`（TOTAL ≥83%）、
   `make test-watch` 判据表↔脚本同步检查全绿。
8. **SLA 语义**：模拟休眠 30 天后唤醒，如实显示超期并完成追赶；不出现「把改状态
   伪装成已消费」的水位推进。
9. **人工介入率**：审计 `actor` 统计「人工动作次数 / 自动处置次数 < 1%」；
   验收场景：连续 30 天无人工干预，准入→准出全链路自动走完，需人工事件全部经
   §3.1 触达面可见、无一靠主动巡检才发现。

---

## 12. 命令面简化（随 P2 收编）

> 判据：**每个命令都要答「自动化环里谁调它」**——答案是「用户例行去调」的即设计味，
> 收编或退役。现状 33 个 CLI 子命令 + 8 个 MCP 工具 → 目标约 20 + 7。
> 用户与 agent 的常规操作只剩「读写 + 出错撤回」，其余要么自动、要么一次性、
> 要么在 §3.1 触达面里找你。

### 12.1 移除

| 命令 | 理由 | 去向 |
|:---|:---|:---|
| `batch`（--plan/--dry-run/--rollback/--reject-judged） | 「AI 出方案、人给授权」的批量清积压，是「没人点按钮」的补丁 | 归档+回滚机制内迁 `review --rollback`；`--reject-judged` 随影子判定退役 |
| `accept` / `reject`（CLI + MCP） | 单条裁决按钮被模型裁决取代；人工唯一剩余否定动作是事后撤回 | `review withdraw`；误拒例外口 `review restore <id>`（rejected 回捞重推） |
| `judge` 执行面 | 判跑内联 maintain，不再是「记得去跑」的命令 | 标定统计并入 `review stats`；`--apply` 已撤、永不恢复 |
| `migrate` | 静态标记随旧库迁移完成退役 | — |

> **S12 已落地（旧轨蒸馏单轨）**：`mem distill --session/--pending` 与 `import-transcript
> --into candidates|both` 删除，`distillOne` / `SessionsNeedingDistill` / `ObservationsForDistill`
> 一并退役——自动写侧只剩 proposal 单轨（SessionStart spawn + 一次性冷启动），手动入口
> 只剩 `distill --proposal/--bundle` 与三个运维出口。`judge --stats` 退役，标定统计由
> `mem review stats` 承接（judge 命令本体保留，判跑内联 maintain 属 §12.2 随 P2）。
> CLI `batch` / `accept` / `reject` / `migrate` 的下线随后续切片执行（MCP 面已先行收编，见 §12.3）。

### 12.2 收编进 `mem maintain`

`distill` / `gc`（含 R15 的 `gc --candidates` 手动路径）/ `consolidate` / 判跑——
都是维护阶段，降为内部步骤；保留 `maintain --stage=…`、`--no-gc` 调试口。

### 12.3 合并成一个待处置视图

`candidates` 系列（ls/triage/policy）+ `review` = 一屏四态 `mem review ls`：
pending（模型没跑）+ 复核摘要（例外式）+ quarantined + failed。
MCP：`candidates_ls`/`accept`/`reject`（3）→ `review_ls`/`review_withdraw`（2）
+ `maintain_wake`（§4 无钩子 Harness 兜底）。

> **S12 已落地（MCP 面）**：`candidates_ls` / `accept` / `reject` 三件已删除；
> `review_ls`（复用 `store.ListReviewQueue` 一屏四态）、`review_withdraw`
> （复用 `store.ApplyMemoryReview` 单事务撤回）、`maintain_wake`（短命 `mem maintain`
> 子进程，幂等）已上线，工具面维持 8 件（读写核心 5 + 复核 3）。

### 12.4 开关收敛

`MEM_DISTILL_SPAWN`、`MEM_NO_DISTILL`、`MEM_NO_COLDSTART` 退役——
双轨时代各链路各自的闸。收敛为 maintain 统一预算/冷却（落 `model.json`）
+ **唯一总闸** `MEM_NO_AUTOMATION=1`。开关越多，「自动化在不在跑」越难一句话回答。

> **S12 已落地**：三个散闸的代码、测试、脚本、文档出现点已一次清干净；`MEM_JUDGE_SPAWN`
> / `MEM_JUDGE_MIN_PENDING` / `MEM_JUDGE_COOLDOWN_H` 按 S12 切片口径**保留为配置**而非闸
> （判定器的开/关/阈值/冷却，属 `model.json` 授权语义，见 design-admission §2.5）。
> 退役后裸跑（无任何 env）行为与退役前逐项一致，由 automation_gate_test 锁定；
> 退役开关的残留值一律被无视。

### 12.5 保留不动

读写核心 `add`/`search`/`ls`/`touch`/`show`/`forget`/`feedback` · 一次性配置
`init`/`setup`/`uninstall`/`harness`/`hook` · 诊断门禁 `audit`/`stats`/`eval`/
`lexicon`/`asset`/`notes`/`ingest`/`budget` · 灾备同步 `export`/`import`/
`import-transcript`/`sync` · `mcp`。

### 12.6 已知代价

模型整体失效期间无单条人工放行口（修模型后 `--catch-up`）。若确需逃生口，
`accept` 降级为 debug override（actor=human 记账）比保留全套按钮便宜——
默认不做，需要时单独放行。收编随 P2 落地，§11.7 护栏断言同步改命令名。

---

## 13. 动线级端到端评测（TDD 视角）

> 完整方案（指标口径、U1–U8 用户动线矩阵、逐跳内容流转 case、账本对账形态、
> 随 P 阶段交付节奏）见 [`design-eval-flows.md`](./design-eval-flows.md)。
> 一句话：单元覆盖率守包内逻辑，动线 e2e 守**准出率 100%**（豁免关机等客观场景）；
> **先写 case 后写实现**，case 不绿不算完成。
