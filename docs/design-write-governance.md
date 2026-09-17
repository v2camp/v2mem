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
| 钩子是写吗 | **否**。钩子（`prompt-submit`/`session-start`/`manual-search`）只做读侧注入，从不写库 |
| 去重键 | `(content_hash, project)`，跨工程同句各自独立 —— 设计文档明示的既定语义，**不是 bug**（见 `design-dedup-merge` e2e） |
| 那 208 条是谁 | 脚本紧凑循环调 `mem add` 灌入（对应 ~200 条检索基准）。**既非 hooks 自动灌，也非 LLM 逐条决策**——是第三类：无来源标记的批量写入 |

## 3. 原则（方案 A：写必由 LLM 决策）

1. **永久记忆只经 LLM 明确决策写入**（CLI/MCP `add`、任务收尾总结）；**hooks 永远只读注入，绝不自动写**。
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
- **hooks 只读护栏**：新增断言/文档，确保钩子事件永不落入写侧计数；为防回归，测试里断言「钩子触发的 read 事件不产生 write」。

## 6. 读低估：已知边界（接受，非缺陷）

读侧只统计**通过 mem 检索的事件**（CLI `search` / MCP / hook 每次提交一次的注入）。AI 经上下文**间接消费**记忆（例如直接读注入内容、依赖 AGENTS.md 不检索）不计数 —— 低估是设计使然，**文档化，不作缺陷修复**。

## 7. 迁移与回滚

- 纯新增字段（`source` 空值=旧数据），无破坏；现有库不动。
- bench 既有数据**不删除**，仅统计层面默认排除。
- 回滚：撤掉 source 分支的统计过滤即可，DB 无 schema 变更。

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