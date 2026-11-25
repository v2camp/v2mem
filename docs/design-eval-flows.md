# 动线级端到端评测（design-eval-flows）

> **定位**：回答「数据动线的自动化用什么 case 证明」。单元覆盖率（`cover-gate`）
> 守包内逻辑；本文件守**用户动线 + 内容流转**全链路，核心是准入→准出的
> **准出率 100%**（不考虑关机等客观条件）与应用处理的**严密度**。
> **来源**：[`design-data-lifecycle.md`](./design-data-lifecycle.md) §13（2026-11-25 拆出）。
> **读者**：实现者。原则：**先写 case 后写实现**——每个 P 阶段的动线 case 与该阶段
> 同批交付，case 不绿不算完成。评测并入 `make eval-gate` / `make test-e2e` 门禁。

## 1. 核心度量：准出率与严密度

| 指标 | 定义 | 目标 |
|:---|:---|:---|
| **准出率** | 准入数据到达明确终态的比例（accepted→memory / rejected / failed→quarantine 经重试）；分母=准入事件，分子=终态事件 | **100%**（豁免场景外无一悬空） |
| 严密度 | 证据缺失/冲突/输出非法必须落 Defer/Conflict/failed，不得为消灭 pending 强行写入 | 违例 0 |
| 重复提交 | 同一输入两次处理的重复落库数（幂等键） | 0 |
| 悬空数据 | 超处理 SLA 仍未到终态且未进 quarantine 的行 | 0 |
| 人工介入率 | 见 design-data-lifecycle §11.9 | <1% |

**豁免场景**（准出率分母剔除，靠 fake clock + 进程生命周期模拟显式标记，不是口头声明）：
宿主退出、机器睡眠、关机、显式 `MEM_NO_AUTOMATION`、数据库物理损坏恢复期。

## 2. Case 矩阵：用户动线

| # | 动线 | 断言 |
|:---|:---|:---|
| U1 | 装完即用（默认提示词、零人工配置） | 一次会话结束即候选→自动裁决→记忆，全程无人工 |
| U2 | 日常多会话 | 连续 N 会话准出率 100%，未审核计数经触达面可见 |
| U3 | 错误记忆撤回 | 一步 `withdraw` 退出检索；按 `model@promptver` 回滚跳过人工修正条目 |
| U4 | 模型 key 失效 | 降级规则 executor 不积压 failed；触达面提示；换 key 后补判 |
| U5 | 故障后追账 | `--catch-up` 幂等：连跑两次第二次零重复提交 |
| U6 | 无钩子 Harness | MCP 机会唤醒/agent 代跑路径有产出（能力矩阵标注上限） |
| U7 | 休眠唤醒 | 超期如实显示 + 追赶完成，水位不伪装已消费 |
| U8 | 高影响抽样复核 | global/rule 类约 10% 进复核摘要，其余不打扰 |

## 3. Case 矩阵：内容流转（逐跳，每跳三类）

每跳至少：**正常、边界（空/超长/重复/乱序）、故障（写失败/坏数据）**。

| 跳 | 代表 case |
|:---|:---|
| 捕获 → observations | 幂等（event_id/content_hash）；spool 落盘后 drain 重放不丢不重 |
| observations → 归档 | 32000 rune 截断口径、archive_hash 稳定、scrub 生效 |
| 归档 → Bundle | 连续发窗、0 提案 noop 水位、序号洞不阻塞健康源 |
| Bundle → executor | 规则/模型两 executor 同契约；失败→failed 不算 noop |
| 候选 → 裁决 | 矩阵全分支（Defer/Conflict/AutoCommit）；模型输出非法→failed 不入库 |
| 裁决 → 记忆 | 幂等键、revision CAS、edges/memory_evidence 绑定完整 |
| 记忆 → 终态 | 撤回/修正/按版本回滚（人工修正条目跳过并报告） |
| → GC | 引用保护全集不误删；读回校验失败拒绝扩大 GC |

### 3.1 G 类逐跳自动检查（`mem eval flow`）

下表由 §3 的逐跳断言拆成**可执行**检查：每条只从库表/审计账判定，
空库或该跳无样本一律显式 FAIL（「没验证」不等于「没问题」）。
ID 与 `mem eval flow` 输出一一对应；改口径必须同步 `internal/eval/flow_check.go`
与 `test/flow`。运行：`mem eval flow [--db] [--json] [--window 360h]`，
`make test` 已包含该套件（`test-flow`）。

| ID | 跳 | 检查（口径） | 数据来源 | 自动检查 |
|:---|:---|:---|:---|:---|
| G1 | 捕获 → observations | 同一 (source, generation, seq) 只允许一行（幂等键） | observations | `eval flow` 的 G1 |
| G2 | observations → 归档 | 非 legacy 观测必须有 archive_hash 且归档账存在 | observations、evidence_archives、migration_marks | `eval flow` 的 G2 |
| G3 | observations → 归档 | 每个归档 raw_count = 实际观测行数 | evidence_archives、observations | `eval flow` 的 G3 |
| G4 | observations → 归档 | 同源同代归档 from/to 首尾相接，且与观测序号边界一致 | evidence_archives、observations | `eval flow` 的 G4 |
| G5 | 归档 → Bundle | Bundle 从 0 开始、窗口首尾相接、末窗覆盖 cursor | evidence_bundles、evidence_sources | `eval flow` 的 G5 |
| G6 | 归档 → Bundle | noop 窗口必须有 noop/empty 运行账、attempt≥1、零候选 | evidence_bundles、distill_runs、candidates | `eval flow` 的 G6 |
| G7 | Bundle → executor | fail/partial/backlog 必须重试（attempt 递增）或按 5 次上限隔离，不得冒充 noop | distill_runs、evidence_bundles | `eval flow` 的 G7 |
| G8 | 候选 → 裁决 | 超过 15 天 SLA 仍 pending 的候选 = 悬空，必须为 0 | candidates（--window 可调） | `eval flow` 的 G8 |
| G9 | 候选 → 裁决 | accepted/rejected 必须有匹配裁决；expired 必须有显式裁决（不能顶替消费） | candidates、commit_decisions | `eval flow` 的 G9 |
| G10 | 候选 → 裁决 | (candidate, revision, action) 与幂等键唯一，决策 revision 不超前 | commit_decisions、candidates | `eval flow` 的 G10 |
| G11 | 裁决 → 记忆 | 采纳必须落到 memories；裁决 memory_id 一致；候选证据在 memory_evidence 全绑定 | candidates、commit_decisions、candidate_evidence、memory_evidence、memories | `eval flow` 的 G11 |
| G12 | 候选 → 裁决 | 逐 Bundle 窗口「终态数 = 准入数」，准出率 100%（复用账本对账） | candidates | `eval flow` 的 G12 |
| G13 | 候选 → 裁决 | 同一会话同一内容不重复落库；采纳不得无证据强写 | candidates、candidate_evidence | `eval flow` 的 G13 |

**无法从库表/审计自动判定的口径（需 U 类人工或独立注入 case，不在 G1–G13 内）**：

- **spool 落盘后 drain 重放不丢不重**：需要文件系统故障注入与二次进程启动，
  G1 只证明「重放后库内无重复」，不证明 spool 本身不丢。
- **32000 rune 截断与 scrub 生效**：需要读归档原文比对，属内容级断言；
  库表只存摘要/引用。
- **规则 / 模型两 executor 同契约**：需要同一窗口分别跑两条链路再对比输出，
  是运行时等价性，不是存量数据可判。
- **revision CAS 的并发竞态**：G10 只能看最终行是否单调，无法证明并发写者被 CAS 挡住。
- **撤回 / 修正 / 按版本回滚**：P1 尚无 withdraw 落库入口，留待对应能力落地。
- **→ GC 引用保护**：P3 交付，本波不预判。
- **关机 / 休眠 / 总闸等豁免**：需要 fake clock + 进程生命周期模拟显式标记，
  G12 未把 `Exempt` 纳入自动账本。


## 4. 形态与门禁

- 临时库 + fake clock + fake executor（确定性输出）+ 故障注入（写失败/坏段/序号洞）；
  不触碰 `~/.v2mem`，沿用 `make test-e2e` 的隔离纪律。
- 准出率断言做成**账本对账**：`终态数 = 准入数` 逐窗口核对（复用 commit_decisions 与
  Bundle 完成账），全量对账而非抽查。
- 并入 `make test`（`test-flow` 套件）：新增 `eval flow`
  （准出率/重复率/悬空/严密度违例）；`make test-watch` 判据回归不变。
  P1 起每次合入都跑逐跳验收，任一 G 项失败 `make test` 即非零。
  黑盒 U1–U8 脚本与 `eval-gate` 的接入沿用后续阶段交付。
- case 随 P 阶段交付：P1 交付 §3 证据链 case，P2 交付 U1/U2/U4/U5 与裁决→记忆 case，
  P3 交付 GC/终态 case，U3/U6/U7/U8 在对应能力落地时同批。
