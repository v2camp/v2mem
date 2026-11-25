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

## 4. 形态与门禁

- 临时库 + fake clock + fake executor（确定性输出）+ 故障注入（写失败/坏段/序号洞）；
  不触碰 `~/.v2mem`，沿用 `make test-e2e` 的隔离纪律。
- 准出率断言做成**账本对账**：`终态数 = 准入数` 逐窗口核对（复用 commit_decisions 与
  Bundle 完成账），全量对账而非抽查。
- 并入 `make eval-gate`：新增 `eval flow`（准出率/重复率/悬空/严密度违例）与
  黑盒 U1–U8 脚本；`make test-watch` 判据回归不变。
- case 随 P 阶段交付：P1 交付 §3 证据链 case，P2 交付 U1/U2/U4/U5 与裁决→记忆 case，
  P3 交付 GC/终态 case，U3/U6/U7/U8 在对应能力落地时同批。
