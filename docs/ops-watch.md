# v2mem 运行观测：动线与判据

> **定位**：回答「装好之后，它有没有按设计在跑」。安装是否可用只在这里校验一次；
> 之后关注的是**数据流是否通畅**，不是进程是否活着。
> **真理源**：本文的判据表。`scripts/watch-v2mem.sh` 是它的可执行形态，**改判据必须同步改两处**。
> 关联：[`VALUE.md`](./VALUE.md) 索引链、[`install.md`](./install.md) 安装与接入、[`design-eval.md`](./design-eval.md) 评测口径。

---

## 0. 三个阶段，别混在一起

| 阶段 | 何时 | 判据 | 谁执行 |
|:---|:---|:---|:---|
| **A 安装校验** | 首次 / 换机 / 升级 WorkBuddy 或 mem 之后 | 钩子写入、MCP 批准、库可读写 | 人工一次，或 `watch-v2mem.sh` 的 I 组 |
| **B 稳态运行** | 每次会话，自动 | 不需要判据——设计了就不该干预 | 宿主事件触发，代码强制 |
| **C 定期观测** | 每 12h | 第 3 节的 R 组判据 | 定时任务跑脚本 |

**B 阶段"自动运行"的含义**：用户与 Agent 都不需要记得「该记了 / 该查了」。
`SessionStart` 注入类码表，`UserPromptSubmit` 按 query 检索并顺带落一条 `user-prompt` 证据，
`Stop`/`SessionEnd` 落观测。说话人归属的完整设计见 [`design-speaker-attribution.md`](./design-speaker-attribution.md)。
这些是 `native` 钩子，**代码强制**，不是靠模型自觉。唯一概率性的一环是模型是否照着类码表去检索——
这一点只能靠命中率衡量，不能靠约定保证。

---

## 1. 动线：一次会话里发生什么

| 事件 | 动作 | 产物 | 期望 |
|:---|:---|:---|:---|
| `SessionStart` | 注入类码表 P/D/X/T + `kind=rule` 硬规则小字条；有界 drain `spool/`；机会性规则抽取 | stdout 注入块 | 恒定极小，不随库增长 |
| `UserPromptSubmit` | 用提问原文检索 `memories`，命中则注入；**另落一条 `user-prompt` 证据**（不新增审计行） | stdout 注入块 + `observations` 一行 | 命中注入 ≤ 预算；空命中只留类码表 |
| `PostToolUse` / `Stop` / `SessionEnd` | 有界摘要写 `observations`（**不**调 `mem add`） | `obs-append` 审计 | 不进检索面 |
| 写库失败 | 追加 `~/.v2mem/spool/<session>.jsonl`（0600） | spool 文件 | 下次 SessionStart 补写并清空 |
| 手动 `mem distill` / 规则抽取 | 观测 → 候选（带 `attributed_to`） | `candidates`（隔离区） | **不进检索**，须 `mem accept` |

> `UserPromptSubmit` 的证据写入**不额外记 `obs-append` 审计**：R1 / R5 / R12 的计数口径
> 保持原样，R5 的「观测落库」仍只统计写事件产生的 `obs-append`。

三条不变量，违反即视为动线异常：

1. **观测不进检索面**。`mem search` 默认 `layer=memory`，只看已定记忆。
2. **蒸馏产物不直接进主库**。必须经 `candidates` → `mem accept`（[`design-write-governance.md`](./design-write-governance.md) 三态）。
3. **钩子绝不阻断宿主**。任何异常静默放过，退出码恒 0。

---

## 2. 观测面为什么不是"进程健康"

参考实现 `agent-memory` 的 `src/health/thresholds.ts` 用一组资源阈值判健康：
`eventLoopLag` 100/500ms、CPU 80/90%、内存 RSS 512MB floor，输出 `healthy|degraded|critical`。
**这套指标对 v2mem 不适用**——v2mem 零常驻进程，`ps` 里没有 `mem`，没有监听端口
（[`design-anti-agentmemory.md`](./design-anti-agentmemory.md)）。

同样，「陈旧 pid 文件」（对方的 `stale-pidfile` 诊断项）在这里没有对应物：
没有 pid 文件就没有陈旧问题。这是"不搬常驻栈"换来的运维收益，不是缺失。

v2mem 该观测的是**数据流四段**：注入 → 检索 → 写入 → 回收。
每一段都有可量化信号，全部已落在 `~/.v2mem/audit.jsonl`：

```bash
mem audit --stats      # 全量：激活次数、空注入率、读/写比、按事件
mem eval activity --since 12h   # 窗口用量视图 + 空命中查询清单
```

**一个容易踩的判据错误**：`session-start` 的 `empty` 恒为 `false`（类码表必注入），
把它算进"命中率"会得到虚高的数字。**只有 `prompt-submit` 的 `empty` 反映检索质量。**

---

## 3. 判据表

`I` = 安装态，`R` = 运行态。级别：`PASS` / `WARN` / `FAIL` / `INFO`。
`INFO` 不是合格，是**当前不构成判据**（冷启动期、无样本）。脚本对 `INFO` 不计入结论。

| ID | 检查 | 采集 | PASS | WARN | FAIL |
|:---|:---|:---|:---|:---|:---|
| **I1** | 二进制可执行 | `mem stats` | 返回 0 | — | 非 0 |
| **I2** | 钩子四事件齐全 | `settings.json` 中 `mem hook` 覆盖的事件数 | 4 | 1–3 | 0 |
| **I3** | MCP 已批准 | `mcp-approvals.json` 有 `*::v2mem` | 有 | 无 | — |
| **I4** | MCP 进程 | `pgrep -f "mem mcp"` | ≥1 | 0 | — |
| **I5** | Trae 钩子执行环境 | `agent-hooks.log` 中 traecode 块的 `ExecEnv` | `Host` | `Sandbox`（读得到、写不进） | 未匹配到 traecode 块时 `INFO` |
| **R1** | 钩子被真实触发 | 窗口内审计事件数 | ≥1 | — | 0 |
| **R2** | 检索命中率 | **按 `project` 分组**：仅统计「该工程库内 ≥20 条」的 `prompt-submit`，`1 - empty 占比`；库内 <20 条的工程样本单列为冷启动剔除，并在 `INFO` 里报出这批样本的实际注入次数 | ≥30% | 10–30% | <10% |
| **R3** | 检索延迟 | 窗口内 `prompt-submit` / `session-start` 的 **`search_ms`** 均值（检索+渲染；总 `ms` 只作 detail，见下） | <50ms | 50–200ms | >200ms |
| **R4** | 写侧活动 | 窗口内**记忆层写入**数：`manual-add` + `write` + `accept`（`accept` 是裁决闭环的落库出口，`mem maintain --model-auto` 自动裁决也记 `accept`；观测写入由 R5 单独判，不在这里双计） | ≥1 | 0（库≥20 条时） | — |
| **R5** | 观测落库 | 窗口内 `obs-append` 数 | ≥1 | — | — |
| **R6** | 库规模 | `mem stats` 总条数（全库各数据表之和，见「按表」行）vs 上次快照 | 未下降 | 下降 | — |
| **R7** | spool 积压 | `spool/**/seg-*.jsonl` 数 **加上** `*.jsonl.processing` 数（后者是 drain 已认领、未读完的段，与 `evidence.isSpoolSegment` 同口径；detail 另读窗口内 `mode=spool-drain` 的 `failed` 区分「下次会补写」与「重试已跑但没消化」）。坏行/坏段进 `spool/dead-letter`，不随正常段计数 | 0 | 1–3 | >3 |
| **R8** | 候选积压 | `mem review ls --state all --json` 的 `states.pending` 按 `project` 分组后的**单工程** `reviewable != false` 条数；**判级取各工程最大值**，标题另报可裁决议总数（两个数并列，避免「5 条散在 3 个工程」被读成 2 条）。不可裁决的（过程叙述/助手偏好）单独报数，不计入 | ≤5 | 6–15 | >15 |
| **R9** | 候选内容质量 | `review ls --state all --json` 的 pending 项 `content` 中载荷残留条数，两种形态都算：transcript 原始 JSON（`{"id"…}`/`{"type"…}`）与工具载荷文本（`Grep {`/`Shell {` 等工具名 + 对象字面量开头） | 0 | 1–3 | >3 |
| **R10** | 写侧延迟 | 窗口内 `stop` 的 `ms` 均值（无样本记 `INFO`） | <1000ms | 1000–3000ms | >3000ms |
| **R11** | WAL 高水位 | `mem.db-wal` 字节数 / 4096（无 -wal 记 0）；量的是**文件高水位**，不是积压量 | ≤1000 帧 | 1001–2000 | >2000 |
| **R12** | 写侧自动决策链路 | 窗口内 `obs-append` → `event=distill`（按 `provider` 分流 rules / 其他）→ 失败状态数 → `accept` / `reject` 计数；只读 `audit.jsonl` + `mem eval activity --json` | 有蒸馏且失败 0 | 断链或失败 1–3 | 失败 >3 |
| **R13** | 注入体量 | 窗口内读侧（`session-start` / `prompt-submit` / `pre-compact`）审计记录 `inject_bytes` 的**最大值**（无样本记 `INFO`） | ≤3600 | 3601–8000 | >8000 |
| **R14** | 归档体量 | `~/.v2mem/evidence` 下文件数与总字节（`find` / `du`，只读）；detail 附 `mem stats --json` 的 `size_bytes` 作对照 | ≤512MiB | >512MiB | —（不设 FAIL，见依据） |
| **R15** | 候选 TTL | `mem review ls --state all --json` 的 pending 项 `created_at` 超过 **14 天**的条数；**只报不处置**：超期报 WARN 并给手工命令与复核提示，状态迁移/删除归维护入口或人工（终态物理删除走 `mem gc --retention`，S13） | 0 条超期 | 有超期（只报不处置） | — |
| **R16** | 维护可见性 | `mem stats --json` 顶层 `backlog` 的 `open_bundles` / `last_maintain_ok_age_seconds` / `bundles_isolated`（复用同一次 stats 调用，不额外读写）；只判级不处置 | 无积压，或有积压但 24h 内维护成功 | 有积压且（从未维护或 `last_maintain_ok_age_seconds` > **24h**），或 `bundles_isolated` > 0；detail 给 `mem maintain --catch-up` | —（不设 FAIL，维护滞后不是数据损失） |
| **R17** | 证据准入 SLA | `mem stats --json` 顶层 `backlog` 的 `evidence_backlog_sources` / `oldest_unconsumed_evidence_age_seconds`（复用同一次 stats 调用，不新增 sqlite 调用或 CLI）；只判级不处置 | 无未消费源，或最老未消费证据 ≤ **15 天** | > 15 天（1296000 秒）；detail 给 `mem maintain --catch-up` | —（不设 FAIL，休眠期豁免） |

阈值依据（不凭空定）：

- **R3 的 50ms**：本判据量的是**进程内检索**（`search_ms`，热态健康值 10–20ms）；**端到端**（进程启动 + 开库 + 去重事务 + 证据 fsync + 维护 + 注入渲染）实测 40–200ms，只作 detail，不参与判级。
  50ms 是纯检索的 5 倍冗余，越过说明库膨胀或检索路径退化；端到端越线先看分段，**不要先动阈值**。
  **采集口径**：只取 `prompt-submit` / `session-start`（读侧注入），**且判级用 `search_ms`**。
  `stop` 与 `obs-append` 共用同一个 Stop hook 计时器，其 `ms` 是整个捕获/归档流程耗时，不是检索
  （早期脚本对所有带 `ms` 的事件取均值，把 500ms 级写侧耗时混进来，产生稳定误报），写侧现归 R10。
  **判级字段切换（2026-11-23）**：此前判总 `ms`——它是端到端，含进程启动、开库、去重事务、
  证据归档 fsync、spool 排空与内联蒸馏（200ms 上限），单次就能顶到 FAIL 线。切换前在库内 0 条、
  注入体量恒定、宿主 load 34/12 核时判出 202ms FAIL，而同一时刻 `mem search -no-audit` 与
  `mem stats` 空启动同为 0.04–0.05s——测的是宿主排队而非检索。脚本在 detail 展示总 `ms` 与各分段
  均值供观察，**只有 `search_ms` 参与判级**；旧记录无 `search_ms` 判 `INFO`（无样本），**不当 0 判 PASS**。
  **端到端为什么不能当判据（2026-11-19/20 复核，452 个读侧样本）**：按日中位 11-10→11-18 为
  11→17→24→37→45.5ms，11-20 十四样本均值 94.7／中位 102；同期纯检索仍 ~10ms、单测 p95=2.72ms。
  库副本对照（`cp mem.db{,-wal,-shm}` 到 /tmp 后用同一二进制打）副本 6–15ms、活库 43–205ms，指向运行态，
  但「常驻 `mem mcp` 数」与「`-wal` 大小」**已否证**（副本挂常驻 mcp 11.5ms、带 4.2MB `-wal` 11.7ms），
  R11 的 `-wal` 帧数同理只作参考（量的是文件高水位），不能拿来解释 R3；
  同 2266 字符 prompt 端到端 40→205→54/54→191/49ms **非单调**、进程内检索 18→55ms，故成因在
  **进程启动 + 审计写入**，不是检索回归。另抓到写侧偶发失败：11-20 14:22 的 `prompt-submit` 证据未入库
  而落 `spool/<session>/seg-*.jsonl`（`seq=0`/`generation=0`，`openEvidenceSession` 失败分支）。
  **均值对并发敏感（2026-11-17 实证）**：某窗口 8 个读侧样本分两批同秒触发（3 个 hook 并发 1737–1873ms、
  4 个并发 158–368ms），旧口径（总 `ms`）均值 809ms 判 FAIL 而同期纯读 10–20ms——越线先看样本分布与宿主 load，分因见 §5 R3。
  **`open_ms` / `search_ms` 分段（2026-11-19）**：读侧审计除总 `ms` 外，另记
  `open_ms`（`store.Open`）与 `search_ms`（检索+渲染）。只记总耗时无法区分
  「检索变慢」与「开库/进程启动变慢」，2026-11-18 那轮（hook 210ms、纯读 20ms、
  load 1.60）就卡在这个盲区：三项读数一致地指向「固定开销」，但分不出具体在哪一段。
  **补齐三段（2026-11-20）**：再加 `claim_ms`（hook 幂等去重事务，另开库连接）、
  `evidence_ms`（prompt-submit 落用户原话证据：`openEvidenceSession` + 归档 fsync）、
  `maint_ms`（session-start 的 spool 排空与挂起蒸馏）。脚本 R3 detail 在字段存在时展示
  分段均值与「其余」（＝`ms` 减去五段，含 stdout 写与审计追加）。有了这五段，R3 越线时
  能直接指出是哪一段，而不是只得出「不在检索上」。判级仍取窗口均值，字段见上（`search_ms`）。
- **R2 的 30%**：`docs/design-eval.md` 的口径是「能取到的细节无限深」，命中率随库内容增长而上升；
  初期低是必然。30% 是"库有料但仍常空手"的告警线。
- **R2 的分组口径（2026-11-15）**：与 R8 同源——库是全局的、`project` 是列，一个工程库里有料
  不代表另一个工程也有料。旧实现用全局 `empty / prompt-submit` 算，只要窗口内提问集中在
  「库内 0 条」的工程上（本机实测 14/15 次落在空库工程），就会得到稳定但无意义的 FAIL。
  现按 `project` 分组，用 `mem stats` 的「按工程」行取各工程条数，把 <20 条的工程样本单列为
  冷启动剔除，只在有料工程上判级；**全部样本都被剔除时判 `INFO` 并列出工程名**。
  「按工程」行缺失（旧版 `mem stats` / 测试 stub）或审计无 `project` 字段时退回全局口径，
  不把未知工程当冷启动，避免静默判 `PASS`。
- **库 20 条 / 评测 20 样本**：`mem audit` 自己提示「可评测样本 ≥20 再下结论」，同一口径沿用。
- **R8 的 15**：候选是隔离区，长期不处理会让下次蒸馏覆盖判断；经验值，可随实测调整。
  **分组口径（2026-11-09）**：阈值按**单个 `project`** 计，跨工程候选不相加，窗口值取各工程计数的
  **最大值**（任一工程越线即按该级判）。理由：库本就全局、`project` 是列，别的项目候选是各自会话的
  合法蒸馏产物；旧口径把它们全局累加，会诱导「在 A 工程会话里把 B 工程候选 reject 掉」的误诊
  （[`design-write-governance.md`](./design-write-governance.md) §12）。`mem review ls --state pending` 与 MCP
  `review_ls` 提供就近查看（投影带 `project` / `content`）。R9 的采集同步改读
  `review ls --state all --json` 的 `content`，检测式与旧版
  `grep '\{"(id|parentId|callId|type)"'` 等价，**判定语义与阈值均未变**。
  `review ls --state all --json` 不可解析时 R8/R9 按 `WARN` 观察，不静默判 `PASS`。
  **只数可裁决（2026-11-20）**：阈值 5/15 未变，**计量对象**改为「现行准入判据下仍可裁决的 pending」。
  起因：助手准入判据 2026-11-17 才落到写侧，此前入库的行按定义都没过这道门——本机 50 条 pending 里
  17 条是同一台 cron 的进度汇报、8 条是助手过程叙述、6 条是巡检报告本身。把它们当「待办」，
  R8 会长期 `FAIL` 且 §5 的处置不可执行：它不是没人裁决，是**不可裁决**。
  判据只能约束新行、不能追认旧行，故在读侧重算：`reviewable` 由 `internal/distill.Reviewable`
  对每条打分（CLI 在 `review ls` 投影里补算），与写侧共用同一套规则（`assistantDurable` / 助手不产 preference），脚本里不另写正则。
  认定规则：`attributed_to=user` 一律可裁决；`assistant` 须过准入判据且非 `preference`；
  来源为空（判据落地前的旧行）按可裁决处理，不替人下结论。
  旧行**不删不改**，`mem review ls --state pending` 照常可见，并按 R15 的 TTL 自然过期；
  detail 固定报出「另有 N 条不可裁决」，口径修正不等于静默丢弃。
  **脚本与二进制需同批发布**：本版 `review ls` 投影带 `project` / `content` / `created_at` /
  `reviewable`；旧二进制的 pending 项缺这些键时 R8/R9/R15 会降级，重建 `mem` 后恢复。
  **detail 附「可先裁决」（2026-11-19）**：越线时按最积压工程取前 3 条，写
  `id:正文前 40 字`。裁决本身仍是人做的，这一步只省掉裁决前的一次 `review ls`——
  判定语义、阈值与级别均未变。取数集与积压计数同源（可裁决集合），不可裁决的行不列进
  「可先裁决」，否则会把人引到本来就无法 reject 的条目上。
  **检测式补第二种形态（2026-11-15）**：上面那个 grep 只认 transcript JSON。工具调用载荷的
  文本形态（`Shell {"command"…`、`Grep {"pattern"…`，即工具名 + 对象字面量开头）不含这些键，
  会被判成干净候选——本机 2 条 11-02 存量候选就是这样漏到 pending 里的，R9 一直报 `PASS`。
  现同时匹配 `^\s*(Grep|Shell|Read|Bash|Edit|Write|Glob|WebFetch|TodoWrite|MultiEdit)\s*\{`；
  只锚定**开头**，正文里提到工具名不算残留。阈值不变（0 / 1–3 / >3）。
- **R9 的 0 / 3**：候选须显式 `mem accept` 才升格（[`design-write-governance.md`](./design-write-governance.md)
  三态），所以结构化载荷残留**不是事实而是污染**，出现即说明蒸馏输入未按
  `proposalEligible` 的准入过滤。判 0 为 PASS；单次 `distill` 实测最多产出 6 条候选
  （`distill_runs` 观测），故 1–3 视为单巡检窗口内可人工清理的余量，>3 判为过滤失效。
  检测为启发式：候选内容含 `{"id"` / `{"parentId"` / `{"callId"` / `{"type"` 等
  transcript 字段即计数。**R9 不随冷启动降级**——污染与库规模无关。
  **detail 必须区分「窗口内新增」与「存量」（2026-11-16）**：判级仍统计**全部 pending**
  （存量不清就一直计数，这是有意的），但候选不随窗口淘汰，只报条数会把 11-02 的残留说成
  「单窗口内产生」，把历史遗留误诊成「过滤刚失效」，诱导去改本来正常的准入逻辑。
  故 detail 拆两段：命中集里 `created_at > 窗口下界` 的记为**窗口内新增**，其余记为存量并给出
  最早年龄（`最早 N 天前` / `最早为今日` / `创建时间不可读`）。  **只有窗口内新增 >0 才允许
  写「过滤未生效」**；全为存量时结论落在「用 mem reject 清理」，不提过滤。阈值与级别不变。
  **产出端收窄（2026-11-17）**：`internal/distill` 加了助手准入判据与否定语境判断
  （[`design-speaker-attribution.md`](./design-speaker-attribution.md) §5），实测同一批会话
  重放候选 42 → 9 条。**候选数下降、R8 随之转 PASS 属预期**，不要当成「蒸馏断链」——
  断链看 R12（`event=distill` 是否还在写、失败数是否为 0）。
- **R10 的 1000 / 3000ms**：宿主 `Stop` hook 是同步调用，`~/.workbuddy/settings.json`
  给的 `timeout` 为 20s，这是会话收尾的硬上限。1000ms 占预算 5%、3000ms 占 15%，
  越线即应介入，但距中断会话仍有余量。实测对照（`~/.v2mem/audit.jsonl`，
  `obs-append` 全量 n=77）：健康期 11-01~11-05 日均 3–20ms，全量
  p50=81ms / p90=1154ms / p95=2847ms / max=7554ms——两档与 p90/p95 同量级，
  只让最慢的约 10% / 5% 告警，避免长会话的正常批量补写被误判。
  **采集必须只取 `stop`**：`obs-append` 与其共用同一个计时器，两者都取会双计均值。
  R3 已把写侧排除在「检索延迟」之外，R10 是给这部分耗时一个独立判级出口，
  不是把 R3 的口径改回去。
  **`rows` / `batch` 字段（2026-11-19）**：`stop` 事件现在带 `rows`（本次实际写入的
  观测行数，含 stop 标记本身）与 `batch`（本次补写是否走了 `MEM_EVIDENCE_BATCH` 的批量
  归档路径）。此前 `obs-append` 的 `count` 恒为 0，一个窗口 3257 行观测只对应 3 个事件，
  体量完全不可见；`batch` 则把「灰度开关有没有真的传到 hook 进程」从 `ps eww` 考古
  变成事件自带的事实——实测 `launchctl setenv` 对**已运行**的宿主进程无效，
  同一台机器会因「图形实例启动」与「终端启动」出现两种行为。
- **R11 的 1000 / 2000 帧**：`internal/store/store.go:70` 只设了 `journal_mode=WAL`、
  `busy_timeout=5000`、`synchronous=NORMAL`，未改 `wal_autocheckpoint`，即 SQLite
  默认 1000 页；超过 1000 帧只说明历史上写到过这个位置，**不等于 checkpoint 跟不上**。
  2000 帧约为阈值的 2 倍（按 `page_size=4096` ≈ 8MB）。脚本按 4096 字节/页整除折算，
  是巡检的近似口径（未扣 WAL 头与每帧 24 字节头，误差约 0.6%，不影响分级），
  detail 同时给出原始字节数供复核。实测 11-08 04:19 为 1118 帧 / 4.5MB，
  手动 `PRAGMA wal_checkpoint(TRUNCATE)` 返回 `0|0|0` 并归零，证明可回收。
  **判据命名的更正（2026-11-20）**：帧数由 `-wal` 文件大小折算，量的是**文件高水位**。
  SQLite 的被动 checkpoint 只把页回写主库、**不缩文件**，只有 `TRUNCATE` 或最后一个
  连接关闭才收缩——所以「帧数 > 1000」与「有数据滞留」不是同一件事。实证：仅取主库
  文件（不含 `-wal`）的快照仍含 5249/5264 条观测（99.7%，最新到 11-19 20:29），
  说明 checkpoint 一直在推进，而同期 `-wal` 稳定在 4.0MB / 约 1018 帧。此前 R11 的
  detail 断言「被动 checkpoint 未跟上」，据此外推「WAL 拖慢读侧」的两轮结论都缺依据；
  判滞留改看 §5 的 `(busy,log,checkpointed)` 三元组（`log == checkpointed` 即无滞留）。
  **局限**：这是单点采样，刚 checkpoint 完会读到 0；它只反映 WAL 文件状态，
  **不用于推断写侧延迟**——11-08 黑盒实测中 hook 进程退出后 WAL 帧恒为 0，
  而 Stop 耗时仍随 transcript 行数线性上升，两者无因果关系（见
  [`design-write-batch.md`](./design-write-batch.md) §0）。R10 与 R11 各自独立判级。
- **R12 的 0 / 3 与断链口径（2026-11-09 新增）**：这里的「自动决策」指 hook 驱动的
  `observations → distill_runs → candidates` 链，**不含** `accept` / `reject` —— 后者是显式裁决，
  积压由 R8 判级、动作在 §5 R8。R12 只回答「自动段有没有推进」：
  - `obs-append = 0` → `INFO`。这是**无样本**，与 R3 / R10 同类，不是 §4 的冷启动降级。
  - `event=distill = 0` 而有观测 → `WARN`（断链）。成因按 §5 R12 的唯一总闸
    `MEM_NO_AUTOMATION`（S12 已退役 `MEM_DISTILL_SPAWN` / `MEM_NO_DISTILL` 散闸，
    自动写侧唯一常规路径是 SessionStart 的 proposal spawn）与 `startDistillProc` 失败
    （只打 stderr、不写审计，故库里看不出来）逐项排查。
    **不判 `FAIL`**：`MEM_NO_AUTOMATION=1` 完全停掉自动化是合法配置，判 FAIL 会制造恒久误报。
  - 失败状态沿用 R7 / R9 的「单窗口可人工处理余量」分档 `0 / 1–3 / >3`。失败集合取
    `failed=true` 或 `mode ∈ {fail, partial, invalid, backlog}`：Proposal 会写 `noop` /
    `invalid` / `backlog`，而 `audit.Summarize` 只认 `mode=ok` / `mode=fail`，
    `mem eval activity` 的 `distill_fail` 因此有盲区，R12 直接读审计补齐。
    `empty` / `noop` / `ok` 是合法结果，不计失败。
    `quarantined` 是人工出口，不计失败率；用 `mem distill --quarantined` 查看、`--requeue` 回捞。
  - `provider=rules` 为 0 **不判级**：单轨后规则由 proposal 链尾 executor 兜底，
    远端 executor 成功时 provider 本来就不是 rules。detail 展示 rules / 其他分流，
    用于确认规则兜底在跑、以及有没有误接 LLM，不作为判级依据。
  - 裁决活性（`accept` / `reject` 计数）与 `mem eval activity --json` 的 `verdict`
    （含「有观测无采纳」）只作 detail，不判级：零裁决已由 R8 的积压判级承接，避免重复告警。
    `eval activity` 读不到或不可解析时 R12 判级不受影响。
  - **`by_actor` 自动化率只作 detail（2026-11-27）**：`mem stats --json` 顶层 `by_actor`（actor → 数量）存在时，
    detail 末尾追加分布（如 `model-auto N / human N / review_action N`）；`model-auto=0` 可能是自动化默认关闭
    而非故障，与上面「不判 FAIL」同源。字段缺失（旧二进制）只写「旧二进制无 by_actor」，
    **不改变 R12 级别、不新增阈值**。
  - **采集只读**：`audit.jsonl` + `mem eval activity --json`（`cmdEvalActivity` 不开库），
    不执行 `mem distill` / `accept` / `reject` / `gc`。`distill_runs` 的 `status` / `error` /
    `attempt` 与 `evidence_bundles` 积压留作 §5 的深挖命令，不进脚本采集，避免引入 sqlite 依赖。
  - **`distill_runs` 有保留期（2026-11-23 起）**：`mem gc --audit`（默认 720h）会删超期运行账，
    但**每个会话必留最新一条 `ok`/`empty`**——那是 `SessionsNeedingDistill` 的游标，删了会话会被
    无限重蒸。所以 §5 R12 的 `SELECT ... FROM distill_runs ORDER BY started_at DESC LIMIT 20`
    查的是「近期运行」，不是全史；追查 30 天以上的断链只剩 `audit.jsonl`（轮转 3 份）。
    `read_traces` 同样有保留期，且**只删「所属会话已无新观测」的轨迹**——它是
    `SessionInjectedRuleIDs` 的注入去重依据（按 session 无时间窗读）。
- **R13 的 3600 / 8000 字节（2026-11-10 新增）**：单位是**字节**不是 rune。宿主按 stdout 的字节
  （字符）数设硬上限，本仓库的预算按 rune 收，CJK 下两者最坏差 3 倍（UTF-8 一个汉字 3 字节），
  所以只能拿字节对齐宿主的尺子。
  - **3600 = 默认预算的最坏值**：`cmd/mem/hook.go` 的 `defaultHookBudget=1200` rune × 3 字节/rune。
    它表示「没人用环境变量抬高预算」时的注入上限；`--max-chars` 或 `MEM_PRECOMPACT_BUDGET`
    被主动调大后越线是预期行为，按 §5 R13 处置，而不是据此认定故障。
  - **8000 = 宿主上限的 80% 安全线**：10,000 是 Claude Code 钩子 stdout 的实测上限
    （来源：本仓库对 claude-mem 的对标研究 `~/code/oss-research/claude-mem/RESEARCH.md` §3.2）。
    **其他 harness 的实际阈值未知，v2mem 无法探测**，所以取 80% 作保守线，而不是贴着 10000 定。
  - **越线后果与其他判据不同**：宿主不报错，而是把整块 stdout **替换成一个约 2KB 的占位字符串，
    仍然返回成功**——用户侧只看到「记忆没出现」。因此 R2 管「查不到」、R13 管「查到了但送不进去」，
    两者不可互相替代：R2 PASS 只说明检索命中了，不代表宿主收下了。
  - **口径**：记的是 `out.Text` 的字节数，即**未包 JSON 信封**的正文；Cursor 系外层还有
    `{"additional_context":"..."}`，包壳与转义后实际 stdout 更大，故阈值留有余量。
  - **字段缺失 = 升级前的旧记录**：脚本按「无样本」处理，不当 0 算——把旧记录当 0 会拉低最大值、
    掩盖真实越线。`A_INJ_READ`（读侧记录总数，含 `pre-compact`）用来区分「窗口内真无读侧记录」
    与「读侧记录全是旧格式」，`A_INJ_N` 是其中带 `inject_bytes` 字段的样本数。
- **R14 的 512MiB（2026-11-13 新增，只有 PASS / WARN 两档）**：量的是观测归档
  `~/.v2mem/evidence` 的**磁盘水位**。它是 [`VALUE.md`](./VALUE.md) §4.1 定义的
  「只写的事实源」，一条观测一份 `.jsonl.gz`。**准出路径自 2026-11-23 起存在，但有前提**：
  `mem gc --evidence` 只删「已经没有任何观测引用」的段（含磁盘文件），因此必须先跑
  `mem gc --obs` 回收观测行，归档才随之成为孤儿；单跑 `--evidence` 在观测未回收时
  释放 0 字节（本机实证：3474 段全部被观测引用）。**仍不能手工删 `~/.v2mem/evidence`**：
  那会破坏 `memory_evidence` 的溯源目标——记忆会显示「有证据」却打不开原始记录。
  因此这条判据回答的是「水位要不要裁决」，不是「是否故障」。
  - **512MiB ≈ 10 万份归档**：实测 2026-11-12 为 8215 份 / 41MiB（均值 5.1KB/条）。
    `GCObservations` 的 `maxKeep` 默认 50000 条是产品自己声明的观测保留上限，
    512MiB ≈ 其 2 倍。越过说明归档量已超出「自己认为需要保留」的量级，应显式裁决保留策略。
  - **为什么不设 FAIL 档**：归档的准出依赖观测先被回收（观测默认保留 720h），水位既不随
    清理即时下降、也不会降到零，任何 FAIL 线都会被「归档量本就长期偏高」永久触发，
    与 R12 断链不判 FAIL 同理——恒久误报会让判据失去意义。越过 512MiB 后按 §5 R14 处置；
    确认接受现状就上调阈值并留记录（口径同 §5 R13）。
  - **口径是磁盘占用，不是条数**：`du -sk` 的块数（APFS 稀疏文件不占块，故样本必须实写）
    加文件数。**不做「折算成多少条观测」的推算**：`gc` 之后行数会降而文件数不降，两者本就会脱钩，
    判据只量落盘量，不量语义。主库 `size_bytes` 仅作对照——主库与归档通常同源增长（观测行），
    但主库会因 freelist 复用而平台化，归档不会。
  - **不随冷启动降级**：它是累计量，与库里有几条记忆无关。（R15 同样不写库，只报不处置，见下条。）
- **R15 的 14 天与「只报不处置」（2026-11-17 新增，2026-11-25 S3 收口；2026-11-26 S13 补终态出口口径）**：TTL 阈值不是新定的，
  是照抄 `GCCandidates` 的默认值（`internal/store/candidates.go:380`，`cmd/mem/main.go`
  传入 `14*24h` / `90*24h`）。加这条判据的原因是**机制存在但没人执行**：
  2026-11-17 实测本机 2 条 11-02 候选已超期 14.8 天仍是 `pending`，`crontab` 与
  `LaunchAgents` 均无调度，而 `mem gc --candidates` 只有手工入口——TTL 形同虚设，
  且失效是**静默的**（不报错，只是永不生效），表现为 pending 只增不减、R8 长期 FAIL。
  - **巡检只读，处置归维护**：`design-data-lifecycle.md` §3 把状态迁移与物理删除划给
    `mem maintain`（或人工），巡检只判级。`GCCandidates` 还含一条「删 90 天以上 accepted 行」
    的 `DELETE`，不可逆，属 §12.5 要人判的那一类——让一个自称只读的定时脚本执行它，
    等于把不可逆决策藏进 cron。故 R15 超期时只报 `WARN`，detail 给手工命令与
    「先复核 90 天前的已采纳行」的静态提示，**不查 accepted、不调 gc**。
  - **S13 终态出口与 R15 的分工（2026-11-26）**：`mem gc --retention`（默认关）是
    rejected 90 天 / 完成 Bundle 30 天 / 孤立决策账的**物理删除**出口，与
    `gc --candidates` 的 pending→expired **状态迁移**是两条路——先迁移、复核后再物理删。
    R15 的 TTL_DAYS=14 定值不漂移：它只量「过期机制有没有被跑」，detail 里只**提示**
    这条命令，巡检本身仍不执行任何写操作。注意它对 R14 零影响：`--retention` 只清库内
    完成账行（evidence_bundles），不删归档文件，归档落盘量照旧只增不减。
  - 为什么不在 hook 里自动处置：`design-write-governance.md` §12.5 已否决「挂 `SessionStart`
    自动过期」——撞 hooks 只读护栏、污染 R3 读侧延迟口径、且用户看不见。
  - 判级无 `FAIL` 档：`WARN` 的含义是「有超期候选待人处置」，需要人知道一次；
    处置后下一窗口回 `PASS`。
  - 巡检没有写开关：只读是默认且唯一的行为。

- **R16 的 24h 与「不设 FAIL」（2026-11-25 S8 新增）**：阈值不是新定的——S4 已经把
  `open_bundles` / `last_maintain_ok_age_seconds` / `bundles_isolated` 暴露在
  `mem stats --json` 的 `backlog` 里（口径见 `design-data-lifecycle.md` §3.2），
  缺的只是一条把三者合成判级的判据。**24h = 巡检窗口 12h × 2**：一个窗口没跑完
  （宿主休眠、维护被预算截断）属正常，连续两个窗口（24h）仍没有成功维护才是
  「无人值守断了」。12h 是 `scripts/watch-v2mem.sh` 的默认调度间隔（§6），
  与它对齐而不是另立一个数字。
  - **有积压但 24h 内维护成功判 PASS**：说明维护在跟，积压只是本轮没跑完；
    把它报成 WARN 会让「预算 20 条、队列 25 条」这种正常状态每个窗口都告警。
  - **隔离 > 0 单独 WARN**：`quarantined` 不会自动重放（§3.1「等人工」出口），
    积压再少也需要人修复后显式 `mem maintain --catch-up` 追账；
    detail 必须带这条命令，否则「有问题」但「不知道做什么」。
  - **不设 FAIL**：维护滞后是可恢复的调度事实，不是数据损失；与 R12 断链、R14
    体量同一取舍——把「还没跑」判成 FAIL 会让告警失去区分度。真正的数据风险
    由 §8 的引用保护与备份承接，不由巡检承接。
  - **不随冷启动降级**：它量的是调度水位，与库里有多少条记忆无关（见 §4）。
  - **detail 附 R17 未消费证据读数**：backlog 可读时带一句源数与最老年龄，只作交叉观察，不改变 R16 级别。

- **R17 的 15 天与「不设 FAIL」（2026-11-27 新增）**：阈值不是新定的——照抄
  [`design-data-lifecycle.md`](./design-data-lifecycle.md) §5 的处理 SLA：活跃期证据从准入到准出不积压超过
  **15 天（1296000 秒）**。同一节明确**休眠期（无会话/宿主退出/关机）不计入承诺**，唤醒后追赶并可如实显示超期，
  故与 R16 同一取舍：只设 PASS / WARN，**不设 FAIL**（数据风险由 §8 与备份承接，不由巡检承接）。
  - **单一只读来源**：`evidence_backlog_sources` / `oldest_unconsumed_evidence_age_seconds` 都取自同一次
    `mem stats --json` 顶层 `backlog`，**不新增 sqlite 调用或 CLI**，不写库、不改配置。
  - **无未消费源判 PASS**：即使年龄字段残留旧值也以源数为准；有源但年龄字段读不到时按 WARN 未核查，
    绝不静默判 PASS（同 R16）。
  - **detail 给人工命令**：报未消费源数、最老年龄（天/小时）与 `mem maintain --catch-up`；
    巡检只报不执行，超期时按 §5 R17 由人工追账。
  - **不随冷启动降级**：它量的是证据流水位，与库里有多少条记忆无关（见 §4）。

> 积压原始读数见 `mem stats --json` 顶层 `backlog`，口径见 `design-data-lifecycle.md` §3.2；巡检只判级。

---

## 4. 冷启动期例外（重要）

**库条数 < 20 时，R2 与 R4 降级为 `INFO`。** 理由：此时空注入率高、写侧稀疏都是**设计预期**，
不是故障。用成熟期的阈值去判初期状态，会得到稳定但无意义的 FAIL，最终导致告警疲劳。

R2 的「库条数」是**按工程**读的：某个工程库内 <20 条，落在它上面的提问即视为冷启动样本，
不参与命中率；只有库内 ≥20 条的工程才按 30% / 10% 判级。这样同一台机器上「A 工程有料、
B 工程刚起步」不会被混算成一个 FAIL。

**冷启动剔除 ≠ 注入为空（2026-11-21 起）**：`memories` 条数只决定「不按 30% 判级」，不决定
「一定没有注入」——规则段与观测注入不看 `memories` 条数，冷启动工程的注入率实测可达一半
（11-21 02:30 窗口：31 次提问全落在库内 <20 条的工程，其中 16 次仍有注入）。因此 `INFO` 会把
这批样本的注入次数与空次数一并报出；`空注入属设计预期` 的文案只在真的全部为空时出现。

判据必须阶段化，否则「符合预期」这四个字没有定义。跨过 20 条后自动启用完整阈值。

**例外只覆盖 R2 / R4。** R9、R10、R11、R12、R13、R14、R15、R16、R17 不降级：候选载荷残留是输入过滤失效、写侧延迟
是同步 hook 的耗时、WAL 高水位是文件量级、写侧决策断链是自动链路没推进、注入体量是
单次注入的字节数、归档体量是累计落盘量、候选 TTL 是「到期未过期」的机制失效、维护可见性是调度水位与最后成功时间的差、
证据准入 SLA 是休眠期豁免后的墙钟水位，都与库里有多少条记忆无关，冷启动期同样应报。R12 的 `INFO` 是「窗口内无 `obs-append` 样本」，
与 R3 / R10 / R13 的「无样本」同类，**不是**本节的降级。

---

## 5. 异常处置

| 现象 | 首要怀疑 | 动作 |
|:---|:---|:---|
| R1 = 0（钩子零事件） | 配置写错文件 | 查 `WORKBUDDY_CONFIG_DIR`，确认钩子在 `~/.workbuddy/settings.json` 而**非** `~/.codebuddy/settings.json` |
| R2 低但 R1 正常 | 库稀疏 / 同义词未覆盖 / **找到了但供给弃答** | 先分因，再对症：① `mem eval activity --since 12h` 列出空命中 query；② 手工复跑 `mem search "<query>"`，**也空** → 检索侧（补记忆或 `mem lexicon init`）；**有结果** → 供给侧弃答，直接查结局分布：`sqlite3 -readonly ~/.v2mem/mem.db "SELECT outcome, COUNT(*) FROM read_traces WHERE created_at > strftime('%s','now')-43200 GROUP BY outcome;"`。`abstained_no_hit` 是检索侧没找到，`abstained_low_quality` 是**找到了但被判无支持**（典型：只有 broad 单路支持），`skipped_gate` 是客套跳过（正常）。第三种（低质量弃答）的机制证据与复现见 [`design-eval.md`](./design-eval.md) §17.1 与 `make eval-hook-ab`——**不要**靠调低质量门阈值来让 R2 变好看 |
| R3 检索延迟越线 | 库膨胀 / WAL 写锁竞争拖慢检索（总 `ms` 高而 `search_ms` 正常则属启动或写侧，已不判 R3） | 先分因，**不要先动阈值**：① 看 detail 里 `search_ms` 与总 `ms` 的差——差得远说明慢在启动/证据/维护，不是检索；② 复测纯读路径 `mem search -no-audit -scope current "<query>"` 跑 3–5 次取中位数，健康值 10–20ms（含进程启动约 40ms）；③ `search_ms` 仍高再查库膨胀（`mem stats` 条数与体积）；④ **库副本对照**：`cp mem.db{,-wal,-shm} /tmp/p/` 后用同一二进制打 `mem hook -db /tmp/p/mem.db`（2026-11-20 实测副本 6–15ms / 活库 43–205ms）——副本正常而活库慢指向运行态，但**不要**归因到「常驻 `mem mcp` 数」或「`-wal` 未回收」（两者已否证），改为查同秒并发 hook 与宿主 IO；⑤ 查本会话有无 `spool/<session>/seg-*.jsonl`：有则说明同一时刻写路径也失败过，先修争用再谈延迟；⑥ 高负载时不用单窗口均值下结论，等窗口滑过再复评。判 `INFO`「无分段样本」说明二进制早于分段埋点，先 `make build` 再评。**不要**为让读数回落而删观测行或改阈值 |
| R7 持续增长 | 写库失败 | 查 `~/.v2mem` 权限与磁盘；spool 在下次 SessionStart 重试，**同一文件跨会话存活即重试本身在失败**（看 audit `mode=spool-drain` 的 `failed`），别只等下一次；先用库副本跑同一段确认能否补上 |
| R6 下降 | 非预期删除 | 口径是全库各表之和（`mem stats`「按表」行），下降先对位是**哪张表**少了；再查 `mem gc` / `mem consolidate` 是否自动跑过。`candidates` 的 pending→expired 是状态迁移、行数不变，不该触发本项；库被外部删除后重建也走这条 |
| I2 掉到 1–3 | 宿主升级覆盖配置 | 重跑 `mem init --harness workbuddy`（幂等、带备份） |
| I5 = WARN（`ExecEnv: Sandbox`） | Trae 钩子跑在沙箱，`~/.v2mem` 禁写 | Hooks 面板运行方式改「本地自动运行」；或在沙箱配置放行 `~/.v2mem` |
| I3 缺失 | 连接器开关被关 | 连接器管理页重新开启；**不影响 R 组**，hook 与 MCP 相互独立 |
| R8 候选积压 | 隔离区长期未裁决 | 先看 detail 里的**按工程分组**计数，在**对应工程的会话里**就近裁决：`mem review ls --state pending --json` 先分清哪些可裁决（`reviewable=false` 的是过程叙述/助手偏好，别逐条 reject，交给 R15 的 TTL），需要单条动作时再 `mem accept` / `mem reject`（兼容保留命令）。不要跨工程批量 reject |
| R9 载荷残留 | 蒸馏输入未按准入过滤 | 先修 `internal/proposal` 的 `Eligible` 判据（`distillOne` 旧轨已随 S12 退役，输入过滤全在 proposal 链路），再逐条 `mem reject` 清存量；只清不修会在下个窗口复现 |
| R10 写侧延迟越线 | 单次 `Stop` 补写 transcript 过多，每条证据各做一次归档 fsync | 先看窗口内 `stop` 条数与 transcript 增量；实测瓶颈是 `writeArchive` 的逐条 fsync（占 88%），DB 事务仅占 12%，故**批量事务不是治本手段**。治本方案见 [`design-write-batch.md`](./design-write-batch.md)（批量归档，P2 已实现）：在启动宿主的环境导出 `MEM_EVIDENCE_BATCH=1` 灰度开启，默认关；实测 N=93 由 925ms 降到 58ms。**2026-11-19 复核：批量开启后生产仍约 15ms/行（748 行 9 秒），设计验收的 0.6ms/行只在小样本上成立**，故 Stop 侧另有 8s 时间预算（`transcriptStopBudget`）：超预算停在块边界、游标只推进到该处、下一次 Stop 续传，这是 20s 硬超时下的兜底，开关只算优化。`PRAGMA wal_checkpoint(TRUNCATE)` 只回收 WAL、与延迟无因果；**不要**删除归档或观测行来降耗时。注意 `Stop` hook `timeout=20s`，接近上限会阻断宿主会话 |
| R11 WAL 高水位越线 | 文件不缩是常态（passive checkpoint 不收缩 `-wal`），先判是否真有滞留 | 先取三元组：`sqlite3 ~/.v2mem/mem.db "PRAGMA wal_checkpoint(PASSIVE);"`（或 `PRAGMA wal_checkpoint;`）并记下 `(busy, log, checkpointed)`——`log == checkpointed` 即无滞留，帧数只是历史高水位，**不必**清。确有滞留（`busy=1` 说明有长读事务）再查各 harness 的 `mem mcp` 长连，确认后 `PRAGMA wal_checkpoint(TRUNCATE);` 回收文件。**不要**手工删除 `mem.db-wal`——里面有尚未落盘的已提交事务；也**不要**把 R11 的帧数当作 R3 延迟的证据 |
| R12 写侧决策断链 / 失败 | 自动蒸馏没推进，或蒸馏落了失败状态 | **只读诊断，不要重跑 `mem distill` 掩盖问题**。断链（`obs-append` 有量但 `event=distill=0`）：核对 `MEM_NO_AUTOMATION` 总闸是否被导出（S12 后它是唯一自动化开关，SessionStart 的 proposal spawn 是唯一常规写侧路径），查宿主 stderr 有无 `startDistillProc` 失败；再跑 `sqlite3 -readonly ~/.v2mem/mem.db "SELECT status,error,attempt,started_at FROM distill_runs ORDER BY started_at DESC LIMIT 20;"` 看是否真的没落 run。失败态：查 `distill_runs` 的 `error` / `attempt` 与 `evidence_bundles` 是否 `backlog` / `quarantined` 挡住后续窗口。detail 里 `rules=0` 但其他 provider 有量属正常（远端成功），不要据此回滚；detail 里零裁决 → 按 §5 R8 在对应工程会话就近裁决。quarantined 是人工出口，不计失败率；用 mem distill --quarantined 查看、--requeue 回捞 |
| R13 注入体量越线 | 预算被调高，或 `session-start` 类码表膨胀 | 先查是否有人把 `MEM_PRECOMPACT_BUDGET` 或其他预算变量调高（`pre-compact` 单独读这个环境变量，见 `cmd/mem/hook.go:156-161` 的 `positiveEnvInt` 分支）；再查 `session-start` 的类码表是否膨胀——它恒定注入、不随库增长，是体积主项。确认是有意为之就调阈值并留记录，**不要**靠删记忆来降体积 |
| R14 归档体量越线 | 归档增长快于观测保留策略 | 先分因：① **量增长**（文件数与字节同步涨）→ 按下面两步回收，再裁决保留策略；② **单条体积异常**（字节涨、文件数不涨）→ 查 `observeRawMaxRunes`（`cmd/mem/observe.go:34`，32000 rune 上限）是否被抬高，或单条 transcript 本身变大。**回收只有一条路**：先 `mem gc --obs`（删观测行）再 `mem gc --evidence`（删已无观测引用的段与磁盘文件），两条合跑即同一轮生效；`--evidence` 有 1h 写入保护期。单跑 `--evidence` 在观测未回收时释放 0 字节——**不要**据此认为功能失效。**不要**直接删 `~/.v2mem/evidence`：里面是 `memory_evidence` 的溯源目标，删了记忆的证据链就指向空处。确认接受现状就上调阈值并在提交信息里写明原因 |
| R15 候选超期 | TTL 从未被执行 | 巡检**只报不处置**。先复核 90 天前的已采纳行：`sqlite3 -readonly ~/.v2mem/mem.db "SELECT id,created_at,project,substr(content,1,60) FROM candidates WHERE status='accepted' AND created_at < strftime('%s','now','-90 days');"`——`gc --candidates` 会连带删除它们、不可逆；确认后再手工执行 `mem gc --candidates -db ~/.v2mem/mem.db`（pending → expired，行仍在库）。终态**物理删除**是另一条路：`mem gc --retention`（S13，默认关；rejected 90 天 / 完成 Bundle 30 天 / 孤立决策账 / 超龄 plans，删前自动备份）。**不要**为了消掉 WARN 去删 `candidates` 行——`expired` 是可查状态，删了不可逆 |
| R16 维护滞后 / 有积压且有隔离 | `mem maintain` 没被触发，或持续故障后没人追账 | 巡检**只报不处置**。先看 backlog 的 `open_bundles` 与 `bundles_isolated`：有隔离则按 §5 R12 查 `evidence_bundles` 的失败原因、修复后 `mem distill --quarantined` 查看；随后执行 `mem maintain --catch-up` 连续追账（幂等，重复跑不会重复提交），再 `mem stats --json` 确认 `last_maintain_ok_age_seconds` 回落。**不要**为了消掉 WARN 去删 Bundle 行或改 `maintain:last_ok`——前者会跳过证据，后者会把「没维护」伪装成「维护过」 |
| R17 证据准入超 SLA | 活跃期证据未在 15 天内消费；或休眠唤醒后尚未追赶（超期可如实显示，不承诺无人运行时消灭） | 巡检**只报不处置**。先看 backlog 的 `evidence_backlog_sources` 与 `oldest_unconsumed_evidence_age_seconds`：确认项目近期是否有会话（休眠期豁免，见 §3 R17）；活跃期超期则执行 `mem maintain --catch-up` 追账（幂等），再 `mem stats --json` 确认最老年龄回落。**不要**为消掉 WARN 手工删证据归档或改水位；不设 FAIL 的依据见 §3 R17 |

处置完成后重跑一次脚本确认级别回落，**不要**在未确认前反复改配置。

---

## 6. 调度与基线

- 定时任务：每 12 小时跑 `scripts/watch-v2mem.sh --window 12h`。
- 基线快照：`~/.v2mem/watch-state.json`（`{ts,total,window}`），供 R6 对比；首次跑记为 `INFO`。
- 脚本**全程只读**：不写库、不改配置、不执行任何写子命令（`gc` / `maintain` / `accept` /
  `reject` / `consolidate` / `import` / `ingest`）。唯一写入是上述快照 `watch-state.json`。
  `--quiet` 只输出非 PASS 项，`--json` 供机器消费。
  R12 额外调用的 `mem eval activity --json` 只读 `audit.jsonl`（`cmdEvalActivity` 不打开数据库），
  不写审计、不写库；该命令不可用时 R12 仍按审计独立判级。
- R15 超期只报 `WARN` 并给手工命令：状态迁移与物理删除归 `mem maintain` 或人工，
  巡检不替人做不可逆决策；只读是默认且唯一的行为，没有需要关掉的写路径。
- R16 同样只报：detail 里的 `mem maintain --catch-up` 是给人执行的命令，巡检不调用它；
  积压与最后维护时间都取自同一次 `mem stats --json`，不额外开库、不写状态。
- R17 同样只报：detail 里的 `mem maintain --catch-up` 是给人执行的命令，巡检不调用它；
  未消费源数与最老年龄取自同一次 `mem stats --json` 的 `backlog`，不新增 sqlite 调用或 CLI、不写状态。
- 告警口径：只有 `FAIL` 需要人介入；`WARN` 观察，`INFO` 忽略。

---

## 7. 参考实现的取舍

> **本节只写与「运行观测」直接相关的部分。**完整架构对照（分层抽象、写入流水线、检索融合、
> 后端矩阵、可借鉴清单）见 [`design-ref-landscape.md`](./design-ref-landscape.md)——
> 不要把本文当成对这两个项目的整体评价。

参考仓库：`~/code/oss-research/mem0`、`~/code/oss-research/agentmemory`。

| 借鉴 | 来源 | 落在哪 |
|:---|:---|:---|
| 阈值集中在单一对象、可覆盖 | `agentmemory/src/health/thresholds.ts` 的 `DEFAULTS` | 本文第三节 + 脚本常量 |
| 三态 `healthy/degraded/critical` | 同上 `evaluateHealth` | PASS / WARN / FAIL |
| 诊断项固定枚举，便于长期统计同类问题频次 | `doctor-diagnostics.ts` 的 `DIAGNOSTIC_IDS` | 判据 ID（I1…I5 / R1…R17） |
| 机器可读 alert token 与人类可读 note 分离 | 同上 `alerts[]` / `notes[]` | `--json` 的 `items[]` |

| 不借鉴 | 理由 |
|:---|:---|
| 进程资源阈值（event loop lag / CPU / RSS） | v2mem 无常驻进程，无对象可测 |
| `stale-pidfile` 类诊断 | 没有 pid 文件，问题不存在 |
| mem0 的 `ADD` 事件遥测 | mem0 走 LLM 抽取 + 向量库，写侧决策模型与 v2mem 的 `candidates → accept` 不同；搬来会对齐错误的写治理语义。**这并不等于不观测写侧**：R12 观测的是自动链路是否推进，不是内容决策由谁做（§3 R12） |

> mem0 的 `ADDITIVE_EXTRACTION_PROMPT`（写侧抽取）与 agentmemory 的 4-tier 衰减
> 对**检索质量**有意义，但对**运行观测**没有——观测只看链路是否通、量是否合理，不看内容好不好。
> 内容质量归 `docs/design-eval.md`（recall / write 评测），两者不要混在一个指标里。

---

## 8. 单轨与历史 SOP

单轨已生效（S5b）：SessionStart 无条件拉起 proposal，冷启动同步切换，`MEM_PROPOSAL_HOOK` 已退役。
历史灰度 SOP（原 §8.1–8.7）参见 git 历史。
