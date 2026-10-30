# design-write-governance：写侧治理与短/跨会话记忆分配

> 关联设计索引：见 [`docs/DESIGN.md`](./DESIGN.md)。
> 触发：两次用量视图「读 20~21 / 写 7~208」，208 条新增全部一次性灌入，暴露「写没有门槛、没有来源标记」的治理缺口。

## 1. 背景与触发

- 两次 `mem eval activity`（旧版 `mem report`）显示 **读 20~21 / 写 7~208**。
- 208 条「新增」全部来自 `project=perf`、同一两秒内、`event=manual-add` —— **无门槛的批量写入**，污染了永久记忆库，也把用量统计的"写侧"虚高到无从解读。
- 两个空命中查询（`日志怎么搬进记忆库` / `AGENTS.md 每轮注入`）说明记忆库还没存够高频要查的内容——**读侧低与"该入库的还没入库"互为因果**。

## 2. 事实排查（现状）

| 项 | 现状 |
|:---|:---|
| 写库路径 | 仅 3 条：CLI `mem add`（`cmd/mem/main.go:300`）、MCP `add` 工具（`cmd/mem/mcp.go:278`）、`mem ingest`（`cmd/mem/level1.go:94`） |
| 钩子写永久库吗 | **否**。钩子仍不 `store.Add`。观测缓冲写入同库另表 `observations`（`source=hook-observe`）。默认 `mem search` 不看它；`layer=observation` 或 `both` 才查痕迹 |
| 去重键 | `(content_hash, project)`，跨工程同句各自独立 —— 设计文档明示的既定语义，**不是 bug**（见 `design-dedup-merge` e2e） |
| 那 208 条是谁 | 脚本紧凑循环调 `mem add` 灌入（对应 ~200 条检索基准）。**既非 hooks 自动灌，也非 LLM 逐条决策**——是第三类：无来源标记的批量写入 |

## 3. 原则（方案 A：写必由 LLM 决策）

1. **永久记忆只经 LLM 明确决策写入**（CLI/MCP `add`、任务收尾总结）；**hooks 绝不 `store.Add` 进 Level 2**。观测只进 `observations`。默认检索不看它；`layer=observation` 或 `both` 才查痕迹。
2. **短时记忆 = 会话注入文件（Level 1）**：由 LLM 在会话内显式维护，容量由 `mem budget` 守卫，机械化迁移交给 `mem ingest`。
3. **长时记忆 = Level 2 事实库**：以 Harness 任务收尾总结为主写入。
4. **批量 / bench / test / 搬运写入打 `source` 标记**：写侧审计带来源，用量统计默认排除非生产写入。

## 4. 短时 vs 跨会话记忆分配

| 逻辑层 | 物理 | 谁写 | 生命周期 |
|:---|:---|:---|:---|
| 会话级（短时） | Level 1 注入文件（AGENTS.md / MEMORY.md） | LLM 在会话内显式更新 | 随会话 |
| 项目 / 跨会话（长时） | Level 2 store，`project` 标记 | LLM `add` / 收尾总结 / `ingest`（机械化搬运） | 持久，`TTL` 可选 |

> v2mem 的会话内"热记忆"仍在 Harness 上下文（DESIGN §1：Harness 注入策略不可控）；
> v2mem 承载的是**用户可见的记忆与长时库**，工作文件的内容由 LLM 显式维护而非 hooks 自动写。

## 5. 写治理落地

- `store.AddInput` 增加 `Source string`；取值建议：`harness-summary`（收尾总结）/ `human`（人工 CLI）/ `llm`（LLM 经 MCP）/ `ingest`（机械化搬运）/ `bench` / `test`。
- 写侧审计记录（`manual-add`）携带 `source`；空值回填 `human` 以兼容旧数据。
- `mem eval activity`（及收尾结论）**默认排除 `source∈{bench,test}`**，用 `--include-bench` 放开。
- **hooks 只读护栏**：钩子事件永不落入写侧计数（`manual-add`）；为防回归，测试里断言钩子不产生 write，且 `memories` 条数不变。
- **观测缓冲**（2026-10-30）：`PostToolUse` / `Stop` / `SessionEnd` 只追加 `observations`。审计里触发事件与 `obs-append` 都留痕，但观测次数**只计成功写入的 `obs-append`**，触发事件不进读侧、写侧或该次数。写库失败落到库旁 `spool/*.jsonl`（默认即 `~/.v2mem/spool`），下次 `SessionStart` 有界 drain。默认无常驻进程。

### 观测 / 蒸馏 / 采纳

三态同库不同表。hooks **仍然不写** `memories`。

| 态 | 谁写 | 落点 | 进 `mem search`？ |
|:---|:---|:---|:---|
| 观测 | hooks（失败则 spool） | `observations` | 默认否；`--layer observation` 或 `both` 可搜 |
| 蒸馏 | `mem distill`（规则或可选本地模型） | `candidates`，`status=pending`，`source=distill` | 否 |
| 采纳 | `mem accept` | `memories`，`source=distill-accept`，并写 `edges.rel=derived_from`（dst 是候选 id） | 是 |

- `mem distill` / `mem reject` / `import-transcript` **禁止** `INSERT memories`。LLM 超时或 JSON 不合规：写 `distill_runs` 失败行，不写候选，默认退出码 0；`--strict` 才非零。
- 同一会话的同一批观测只蒸馏一次（`distill_runs.window_key`）。Stop 每轮都触发时不会放大噪声。
- 无 `transcript_path` 的 Stop 不拉起 distill，观测仍保留。
- 可选：`MEM_DISTILL_SPAWN=1` 时 Stop / SessionEnd 末尾 `Start` 一次 `mem distill --provider none`（非常驻）。默认不拉起。
- `SessionStart` 在 spool drain 之后机会性跑 `distill --pending`，**只用规则抽取**，约 200ms 上限，失败不阻断注入。`MEM_NO_DISTILL=1` 关掉这一步。
- `mem eval activity` 默认排除 `source∈{bench,test,distill-pending}`。观测侧另计 `obs / distill_ok / distill_fail / accept / reject`。有观测且无人采纳时结论为「有观测无采纳」。accept 计入写侧，`source=distill-accept`。
- PreCompact 仍只重注入，不写观测、不蒸馏。见 [`design-harness.md`](./design-harness.md)。为何不照搬常驻 consolidate，见 [`design-anti-agentmemory.md`](./design-anti-agentmemory.md)。

## 6. 读低估：已知边界（接受，非缺陷）

读侧只统计**通过 mem 检索的事件**（CLI `search` / MCP / hook 每次提交一次的注入）。AI 经上下文**间接消费**记忆（例如直接读注入内容、依赖 AGENTS.md 不检索）不计数 —— 低估是设计使然，**文档化，不作缺陷修复**。

## 7. 迁移与回滚

- 纯新增字段（`source` 空值=旧数据），无破坏；现有库不动。
- bench 既有数据**不删除**，仅统计层面默认排除。
- 回滚：撤掉 source 分支的统计过滤即可；`source` 列本身可留。
- 观测三表（`observations` / `candidates` / `distill_runs`）由 `CREATE TABLE IF NOT EXISTS` 在 Open 时补上，不改 `memories` 的列。

## 8. 测试面

- `Add` 的 `Source` 默认值（兼容旧调用）。
- 审计记录带 `source`；`eval activity` 对 `bench/test` 默认过滤、`--include-bench` 放开。
- 钩子只读护栏：钩子会话产生的审计只有读事件，无写事件。

## 9. 关键字段必填 与 审计日志边界

**关键字段必填**（skill / MCP schema / hintBlock 中均已注明，缺失即拒绝并报错）：

| 操作 | 必填字段 | 说明 |
|:---|:---|:---|
| `add` / MCP `add` | `content`（原子事实正文） | 一条只写一个原子事实；重复内容自动覆盖，不新增 |
| `touch` / MCP `touch` | `id` | 记忆 id 或前缀，缺失时无法定位 |
| `search` / MCP `search` | `query` | 空查询没有检索意义，直接报错 |

MCP `add` 的 schema 声明 `required:["content"]`；写侧一律带 `source`（CLI/MCP 写入口
在描述中注明来源语义），缺省不落空值 —— `AddInput.Source` 空回填 `human`。

**审计日志边界**（交叉验证的事实记录，见 `internal/audit`）：

- **体量上限**：超过 5MB 自动轮转归档为 `.1`，保留至 `.3`，更旧删除，防止 `~/.v2mem` 被撑爆。
- **异步写入**：长驻的 MCP server 用 `AsyncAppender` 后台写，工具调用不摊磁盘 IO；队列满时
  退化同步写（宁可慢一次也不丢观测）。CLI/hook 等短命进程保留同步写 —— 进程退出前必须落盘，
  异步在此场景只会增加丢记录风险。
- **写失败静默**：审计只是观测，任何写失败都不得影响注入或宿主会话。