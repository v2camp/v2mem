# 设计：用 v2mem 接替 harness 记忆（workbuddy / traeWork / TRAE 原生）

- 日期：2026-09-17
- 状态：草案待复核
- 关联：docs/design-harness.md、docs/design-workbuddy.md、docs/design-cross-device.md、docs/design-write-governance.md

## 1. 背景与目标

用户近期常用的三种记忆表面（TRAE 原生 `~/.trae-cn/memory/`、WorkBuddy `MEMORY.md`、TraeWork 项目记忆文件）各自为政：
跨工具/设备不共享、每轮全量注入逼近容量上限需人工压缩、写权与容量都由 harness 掌控且不可迁移。

目标：将这些记忆表面的**读与存**统一收拢到 v2mem（单文件 SQLite 记忆层）作为共享层，满足：

- **跨工具/设备统一**：同一份记忆可被 workbuddy、traeWork、终端多处取用；换工具不丢记忆。
- **解耦工具锁定**：存储/检索面不锁死任一 harness；未来记忆沉淀为 skill/cli/mcp 等产品资产，内部封装基础算法或小型 agent 有稳定接缝。

核心可行性门槛已由用户点名：**现有检索是纯句法检索（无语义向量），语义等价不同词会召回有损**。本设计以"先测后切 + 发布门禁"应对，不凭直觉担保。

## 2. 决策记录（已与用户逐项确认）

| 维度 | 决策 |
|---|---|
| 接管范围 | A(原生记忆)+B(WorkBuddy MEMORY)+C(TraeWork 项目记忆) 全接管，**不选只读叠加视角** |
| 写侧 | **双路径**：harness 任务收尾总结兜底（内容生成源）+ v2mem 统一收录 |
| 读侧 | **原生=极小硬规则小字条**（始终注入、不依赖检索）+ v2mem 细节按需检索 |
| 跨设备 | **git 仓库同步＋合并**（自托管、零长驻服务、避开云盘 SQLite 锁坑） |
| 检索门禁 | **先测后切**：真实 gold 集量 Recall@5，设 `recall5_min` 门槛，不达标不切 |
| 版本 | **CI 自动按 conventional commits 迭代并打 tag**；eval 正式报告与 tag 同步、可复现 |

## 3. 架构分量与组件

四个独立工作流，可在隔离 worktree 中**并行开发**，验收各自独立；依赖仅存在于最终集成点。

### WS1 读侧接入与瘦身
- `mem notes`：从现有 project_memory/workbuddy 抽取一行一规则的"硬规则小字条"，幂等生成；写入 harness 原生记忆槽。此后 harness 原生注入只含小字条。
- v2mem 注入分工：session-start 注入小字条；prompt-submit 用 query 检索 top-k 细节注入，**默认排除 rule 项**（rule 已在小字条，避免预算重复）。
- 接入开关：读侧 feature-flag；门禁达标前灰度"v2mem 注入+原生小字条"，达标后降原生大记忆；保留一键回退到"原有 namespace 原生记忆"。
- 护栏：hook 只读（扩展现有 `TestCmdHookReadOnly`），断言注入去掉重复 rule、不产生写。

### WS2 写侧统一收录（双路径）
- 统一入口 `mem ingest --source harness-summary`（或 MCP 工具）：把任务收尾的 summary 灌库，走 source 治理 + content_hash 去重 + merge 覆盖；task-finish 生成的 report 作为兜底内容源。
- 未来沉淀为 skill/cli/mcp 资产时，写入口单一、可复现。

### WS3 跨设备 git 同步＋合并
- `mem sync`：git pull → 三方合并 → push。
- 合并以 `content_hash` 为 key：同名事实取 `updated_at` 新者胜；同 hash 无法自动决断（时间戳接近/内容冲突）时两版**并存**并打来源标记，交由 `mem merge` 人工收敛——不静默吞掉任一设备上的记忆。
- 入仓对象：仅 `mem.db` 与 `audit.jsonl`，避免把钩子/配置等杂项带上远端。
- 磁盘级：杜绝多设备同时打开同一 db 并发写（会损坏）；git 只在拉/推的瞬间接触 db 副本，本地始终只写本机那份。
- 回退：冲突合并失败则不推送、停在 pull 后状态，`--abort` 复原；写库前 `VACUUM INTO` 备份（沿用既有机制）。

### WS4 版本自动迭代 + eval 报告同步 + 发布门禁
- 迭代推导：自上次 tag 之后合入 main 的 commits，按 conventional 取最高档：`!`/`BREAKING`→major；`feat:`→minor；其余→patch。
- CI：main 合入触发 → `make cover-gate` 通过 → 跑正式版 eval（真实 gold 集，`eval-policy.txt` 记 `recall5_min`，默认 0.60）→ 达标才 `git tag` + push + 生成 `.mem/reports/v<tag>.md`；不达标 CI 红，不发版。
- 报告头部：`tag`、`commit sha`、`coverage%`、`Recall@5`、**gold 集 hash**、日期、gold 行数；同 tag 可复现。
- gold 集入仓 `gold/`，版本化。
- 阈值单一真理源：新增 `eval-policy.txt` 与 `coverage-policy.txt` 并列；CI 与本地 `make release` 读同一份。
- override：仅本地 `make release OVERRIDE=major|minor|patch`；CI 无旁路放行。

## 4. 错误处理与回退

- 任何注入/检索/同步异常 → 回退到原 harness 原生记忆，观测一轮，**不静默降级**；审计日志记录失败事件带 source 与原因。
- sync 冲突合并失败 → 不推送、停在 pull 后状态，`--abort` 复原；db 操作前 `VACUUM INTO` 备份（沿用既有机制）。
- 发版评估不达标 → 中止，不覆盖已有 tag；报告保留该次失败快照。

## 5. 范围划线（本轮**不做**）

- 语义向量检索（嵌入）——保持检索器接口可替换（`SearchQuery`/`Hit`），后续作为第二路插接，不动 hooks。与用户"未来封装基础算法"接缝一致。
- 两层自动处理机制、记忆→skill/cli/mcp 资产化、小型 agent 本体。
- 自建中心同步服务（git 同步已满足，避免违背零服务定位）。

## 6. 测试与验收

- 沿用包内覆盖率视角，`make cover-gate` 门禁不降。
- WS1：hook 只读护栏扩展；`notes` 幂等；flag 切换/回退。
- WS2：ingest 去重/覆盖/source 治理；task-finish 流水线。
- WS3：三方合并单测、git 冲突注入、db 备份。
- WS4：迭代推导单测（major/minor/patch 分支）、门禁达标/不达标路径、报告可复现、gold 集 hash。

## 7. 并行提交与集成

- 每 WS 走仓库 worktree 流程（一任务一分支一 worktree 于 `.worktree/`），各自过 `make cover-gate`。
- 集成顺序：WS4(门禁/版本地基先行) → WS1/WS2 并行落地读/写侧 → WS3 最后接通跨设备；最终一次合并进 main 经 `make cover-gate` + 正式版 eval 达标后由 CI 自动打 tag。