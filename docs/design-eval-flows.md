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

> 走查步骤、命令级操作、证据留痕与判定人见
> [`acceptance-p4.md`](./acceptance-p4.md) §4（U1–U8）。

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
与 `test/flow`。运行：`mem eval flow [--db] [--json] [--window 360h] [--profile ci|prod]`，
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
| G9 | 候选 → 裁决 | accepted/rejected/expired 必须有匹配裁决；failed/quarantined/withdrawn 不要求裁决；pending 只看最近一次 reopen 之后的终态裁决 | candidates、commit_decisions | `eval flow` 的 G9 |
| G10 | 候选 → 裁决 | (candidate, revision, action) 与幂等键唯一，决策 revision 不超前 | commit_decisions、candidates | `eval flow` 的 G10 |
| G11 | 裁决 → 记忆 | 采纳必须落到 memories；裁决 memory_id 一致；候选证据在 memory_evidence 全绑定 | candidates、commit_decisions、candidate_evidence、memory_evidence、memories | `eval flow` 的 G11 |
| G12 | 候选 → 裁决 | 逐 Bundle 窗口「终态数 = 准入数」，准出率 100%（终态 = accepted/rejected/expired/quarantined/withdrawn；failed 非终态，复用账本对账） | candidates | `eval flow` 的 G12 |
| G13 | 候选 → 裁决 | 同一会话同一内容不重复落库；采纳不得无证据强写 | candidates、candidate_evidence | `eval flow` 的 G13 |

> **prod 剖面（legacy 豁免）**：`--profile=ci`（默认）保持上表严格口径。`--profile=prod` 先按
> `bundle_id` 为空剔除 legacy 候选（G8/G9/G11/G12/G13 输入）与 legacy 运行账（G7 输入），
> 报告以 `legacy_exempt` 给出豁免总数；`--allow-empty`（prod 下默认开）只把过滤后的
> 「无样本」分支降级为 PASS-with-note。带 bundle_id 的当前行真实违例仍 FAIL。
> 实现见 `internal/eval/legacy.go` 与 `CheckFlowWithOptions`。

> **S10a 口径变更（G9 / G12）**：
> - **G9**：`failed` / `quarantined` / `withdrawn` 不是裁决驱动的状态（`failed` 在重试队列、
>   `quarantined` / `withdrawn` 是明确终态），要求它们也配 accept/reject 裁决只会逼实现
>   为过闸伪造一条裁决；`pending` 的「已有终态裁决」只看**最近一次 `reopen` 之后**——
>   `mem batch --rollback` 退回 pending 时历史裁决合法留账，旧口径会把每个回滚过的候选判红。
> - **G12**：终态集合补 `quarantined` / `withdrawn`；`failed` **不**计终态（还在重试队列里，
>   算成终态会让准出率虚高，与 §3「失败→failed 不算 noop」是同一句话）。
> - 实现见 `internal/eval/flow_check.go`；用例 `TestCheckFlowG9AdmitsFailedQuarantinedWithdrawn`、
>   `TestCheckFlowG9PendingIgnoresDecisionsBeforeReopen`、`TestCheckFlowG12TerminalSet`。

> **验收状态（波 5，基线 `079c98d`）**：G1–G13 已被两套互证覆盖——`test/flow`
> 门禁（E2 全绿 / E3 单点破坏 / E4 只读 / R8 回滚回归）与 CLI 真实动线基线
> （`mem eval flow` 全 PASS）。逐条结果与动线构造见
> [`acceptance-p4.md`](./acceptance-p4.md) §3；S12/S14 合入后由主控复跑基线。

### 3.2 故障矩阵（S9a/S9b：hook / gc / distill / maintain）

> 判据不是「命令返回 0」，而是**不丢数据、不静默、重启幂等、退出码如实**。
> 单元格写的是注入手法与期望终态；`—` 表示该入口无此故障面（如 gc 不读外部输入）。
> 自动注入：`test/faults`（`make test-faults`）；**F1–F12 覆盖 hook/gc/distill/maintain**。
> 前三列（S9a）见下表；maintain 入口的 F7–F12 见 §3.2.1。

| ID | 故障 | hook | gc | distill |
|:---|:---|:---|:---|:---|
| F1 | 部分写（表写入失败） | `FailWrites(observations)` → 退出 0，事件落 spool；撤销后 drain 补回，重放不重复 | `FailWritesOn(candidates, DELETE)` → 非 0；UPDATE 标过期与删行同事务回滚，无半提交 | 第二条候选 INSERT 失败 → `partial` 不消费窗口；重试补齐，按内容去重 |
| F2 | kill -9 中途 | Stop 处理 transcript 时 SIGKILL → 重启补写，`(source_id,source_seq)` 无重复；`*.jsonl.processing` 可重放 | — | 同 F1：partial 后重跑即崩溃恢复口径（进程内事务已回滚） |
| F3 | 只读/磁盘不可写 | 归档目录 0555 → 退出 0（钩子不阻断宿主），事件转 spool；恢复后补回 | 库目录 0555 → 非 0，stderr 可诊断，库内状态不变 | 库目录 0555 + `--strict` → 非 0，不 panic |
| F4 | 库被占用（SQLITE_BUSY） | fail-open：写失败进 spool（同 F1 路径） | 外部连接持 `BEGIN IMMEDIATE` → 拿不到写锁即如实非 0（`busy_timeout` 内不重试成功），状态不变 | 同 gc（共用 `store.Open` 的 `busy_timeout=5000`） |
| F5 | 坏输入 | spool 坏 JSON → 退出 0，原文进 `spool/dead-letter`，正常段继续 | — | executor 输出非法 → `invalid`，0 候选，窗口不消费；`--strict` 非 0 |
| F6 | 超大负载 | prompt > 32000 rune → 归档按上限截断（现行实现保留前 32000 + 省略号），不丢整条 | — | 1MiB ByteCap（按归档段大小）切窗，不丢证据；单条超限至少出 1 条 |

**dry-run / force 列**：`distill --dry-run --bundle` 在候选写入被全面注入失败时仍须
退出 0、0 写入、不消费窗口；`gc --no-backup` 是破坏性路径的 force 开关，故障用例
一律显式带上，避免备份目录干扰注入点。hook 无 dry-run/force 面。

#### 3.2.1 maintain 入口（S9b，F7–F12）

| ID | 故障 | 注入与期望终态 |
|:---|:---|:---|
| F7 | 部分写（第 2 条候选 INSERT 失败） | `FailWritesWhere(candidates, INSERT)` 命中候选二 → JSON 报 `partial=1`、逐窗 `error` 与 stderr 带注入原因；成功那条与 `candidate_evidence` 同事务落库，无半提交；撤销后 `--force` 重跑按内容去重，恰好 2 条、0 重复 |
| F8 | kill -9 中途（Executor 执行中） | 假 CLI 写同步 marker 后阻塞 → SIGKILL；Bundle 停在 `leased` 且租约有效、0 候选；租约到期后重启接管，`attempt` 只 +1、候选补齐且无重复、租约释放（不永久 leased） |
| F9 | 只读/不可写（库目录 0555） | `maintain` 非 0，stderr 可诊断、不 panic；Bundle 仍 pending、attempt=0、无候选、不写 `maintain:last_ok` |
| F10 | SQLITE_BUSY（另连接持 `BEGIN IMMEDIATE`） | 非 0，stderr 含 busy/locked，不 panic；Bundle 状态/attempt/租约不变、0 候选；解锁后重跑完成 |
| F11 | `--dry-run` × 全表写故障 | `candidates` 与 `evidence_bundles` 装写触发器 → 仍必须退出 0；`would_generate=1`、remaining 不变、0 候选、0 运行账、Bundle 未认领、不写 `last_ok`/`last_fail_at` |
| F12 | `--force` + 退避中的 failed 头 + 候选写故障 | 清退避后立即重试，`fail=1` 且 error 逐窗可见；`attempt` 仅由 claim 加 1（2→3），失败后重新进入退避；再次不 `--force` 只让位，attempt/status 不变、0 候选 |

> F9 的 `maintain:last_fail_at` 尚未落地（基线 S8 只写 `maintain:last_ok`）：
> 本片不改产品，只守「失败可见 + 状态一致」，该键留给 S11/后续补齐。

**无法从库表/审计自动判定的口径（需 U 类人工或独立注入 case，不在 G1–G13 内）**：

- **spool 落盘后 drain 重放不丢不重**：需要文件系统故障注入与二次进程启动，
  G1 只证明「重放后库内无重复」，不证明 spool 本身不丢（已由 §3.2 F2 覆盖）。
- **32000 rune 截断与 scrub 生效**：需要读归档原文比对，属内容级断言；
  库表只存摘要/引用（截断边界已由 §3.2 F6 覆盖；scrub 仍属内容级）。
- **规则 / 模型两 executor 同契约**：需要同一窗口分别跑两条链路再对比输出，
  是运行时等价性，不是存量数据可判。
- **revision CAS 的并发竞态**：G10 只能看最终行是否单调，无法证明并发写者被 CAS 挡住。
- **撤回 / 修正 / 按版本回滚**：P1 尚无 withdraw 落库入口，留待对应能力落地。
- **→ GC 引用保护**：P3 交付，本波不预判。
- **关机 / 休眠 / 总闸等豁免**：需要 fake clock + 进程生命周期模拟显式标记，
  G12 未把 `Exempt` 纳入自动账本。


### 3.3 B 类逐跳检查矩阵（S10a 数据面 / S10b 动作面）

> 口径来源 `design-data-lifecycle.md` §2.1 / §6.4 与 §12.3 一屏四态。
> B1–B5 是 S10a 已落地的数据面与只读视图；B6–B9 的动作面 S10b 已落地。

| ID | 断言 | 状态 |
|:---|:---|:---|
| B1 | 一屏四态视图：`mem review ls` 的 pending / summary / quarantined / failed 共用同一投影，空态输出 `[]` 而非 `null`，计数不受 `--state` / `--limit` 截断 | S10a 已落地 |
| B2 | 提交矩阵全分支：Defer / Conflict / AutoCommit 各落对应状态，不得为消灭 pending 强行写入 | S10a 已落地 |
| B3 | 模型输出非法 → `failed`（不入库、不顶替消费），`status_reason` 留最后一次原因 | S10a 已落地 / S10b 接通真实链路（`mem judge` 解析失败即留痕重试） |
| B4 | `failed` 重试超上限 → `quarantined`：`attempt` 递增、`status_reason` 记原因，quarantined 为终态（不可再隔离 / 提交 / 拒绝） | S10a 已落地 / S10b 接通真实链路（上限 `JudgeInvalidRetryLimit=3`） |
| B5 | 幂等与 CAS：同一 (candidate, revision, action) 幂等键唯一；重放不新增裁决行，revision CAS 不超前 | S10a 已落地 |
| B6 | `withdraw`：一步撤回退出主检索，行与证据保留（可追溯） | S10b 已落地 |
| B7 | `restore`：撤回 / 误拒可原路恢复，重新进入检索 | S10b 已落地 |
| B8 | 按 `model@promptver` 回滚：只撤该版本自动记忆，跳过人工确认 / 修正条目并报告 | S10b 已落地 |
| B9 | `model-auto` 默认关：模型自动采纳作为可配置档，裁决按 `model@promptver` 归因 | S10b 已落地 |

## 4. 形态与门禁

- 临时库 + fake clock + fake executor（确定性输出）+ 故障注入（写失败/坏段/序号洞）；
  不触碰 `~/.v2mem`，沿用 `make test-e2e` 的隔离纪律。
- 故障注入单独门禁 `make test-faults`（`test/faults`，build tag `faults`）：
  要 SIGKILL、等 SQLITE_BUSY、改目录权限，故意不进默认 `make test`。
- 准出率断言做成**账本对账**：`终态数 = 准入数` 逐窗口核对（复用 commit_decisions 与
  Bundle 完成账），全量对账而非抽查。
- 并入 `make test`（`test-flow` 套件）：新增 `eval flow`
  （准出率/重复率/悬空/严密度违例）；`make test-watch` 判据回归不变。
  P1 起每次合入都跑逐跳验收，任一 G 项失败 `make test` 即非零。
  黑盒 U1–U8 脚本与 `eval-gate` 的接入沿用后续阶段交付。
- **两面口径**：`make test-flow` / tag 判据在干净夹具上跑 `--profile=ci`（严格）；
  生产健康检查用 `--profile=prod`（legacy 豁免 + 无样本降级）。不得用生产库的
  ci 红项判 tag，也不得用 prod 绿灯掩盖当前行违例。
- case 随 P 阶段交付：P1 交付 §3 证据链 case，P2 交付 U1/U2/U4/U5 与裁决→记忆 case，
  P3 交付 GC/终态 case，U3/U6/U7/U8 在对应能力落地时同批。
