# v2mem 运行观测：动线与判据

> **定位**：回答「装好之后，它有没有按设计在跑」。安装是否可用只在这里校验一次；
> 之后关注的是**数据流是否通畅**，不是进程是否活着。
> **真理源**：本文的判据表。`scripts/watch-v2mem.sh` 是它的可执行形态，**改判据必须同步改两处**。
> 关联：[`VALUE.md`](./VALUE.md) 索引链、[`setup.md`](./setup.md) 接入规范、[`design-eval.md`](./design-eval.md) 评测口径。

---

## 0. 三个阶段，别混在一起

| 阶段 | 何时 | 判据 | 谁执行 |
|:---|:---|:---|:---|
| **A 安装校验** | 首次 / 换机 / 升级 WorkBuddy 或 mem 之后 | 钩子写入、MCP 批准、库可读写 | 人工一次，或 `watch-v2mem.sh` 的 I 组 |
| **B 稳态运行** | 每次会话，自动 | 不需要判据——设计了就不该干预 | 宿主事件触发，代码强制 |
| **C 定期观测** | 每 12h | 第 3 节的 R 组判据 | 定时任务跑脚本 |

**B 阶段"自动运行"的含义**：用户与 Agent 都不需要记得「该记了 / 该查了」。
`SessionStart` 注入类码表，`UserPromptSubmit` 按 query 检索，`Stop`/`SessionEnd` 落观测。
这些是 `native` 钩子，**代码强制**，不是靠模型自觉。唯一概率性的一环是模型是否照着类码表去检索——
这一点只能靠命中率衡量，不能靠约定保证。

---

## 1. 动线：一次会话里发生什么

| 事件 | 动作 | 产物 | 期望 |
|:---|:---|:---|:---|
| `SessionStart` | 注入类码表 P/D/X/T + `kind=rule` 硬规则小字条；有界 drain `spool/`；机会性规则抽取 | stdout 注入块 | 恒定极小，不随库增长 |
| `UserPromptSubmit` | 用提问原文检索 `memories`，命中则注入 | stdout 注入块 | 命中注入 ≤ 预算；空命中只留类码表 |
| `PostToolUse` / `Stop` / `SessionEnd` | 有界摘要写 `observations`（**不**调 `mem add`） | `obs-append` 审计 | 不进检索面 |
| 写库失败 | 追加 `~/.v2mem/spool/<session>.jsonl`（0600） | spool 文件 | 下次 SessionStart 补写并清空 |
| 手动 `mem distill` / 规则抽取 | 观测 → 候选 | `candidates`（隔离区） | **不进检索**，须 `mem accept` |

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
| **R2** | 检索命中率 | `1 - prompt-submit.empty 占比` | ≥30% | 10–30% | <10% |
| **R3** | 检索延迟 | 窗口内 `prompt-submit` / `session-start` 的 `ms` 均值（写侧不计入，见下） | <50ms | 50–200ms | >200ms |
| **R4** | 写侧活动 | 窗口内 `manual-add` 数 | ≥1 | 0（库≥20 条时） | — |
| **R5** | 观测落库 | 窗口内 `obs-append` 数 | ≥1 | — | — |
| **R6** | 库规模 | `mem stats` 总条数 vs 上次快照 | 未下降 | 下降 | — |
| **R7** | spool 积压 | `spool/*.jsonl` 数 | 0 | 1–3 | >3 |
| **R8** | 候选积压 | `mem candidates ls --json` 按 `project` 分组后的**单工程** pending 数；窗口取各工程最大值 | ≤5 | 6–15 | >15 |
| **R9** | 候选内容质量 | pending 候选 `--json` 的 `content` 中 transcript 原始 JSON 载荷残留条数 | 0 | 1–3 | >3 |
| **R10** | 写侧延迟 | 窗口内 `stop` 的 `ms` 均值（无样本记 `INFO`） | <1000ms | 1000–3000ms | >3000ms |
| **R11** | WAL 堆积 | `mem.db-wal` 字节数 / 4096（无 -wal 记 0） | ≤1000 帧 | 1001–2000 | >2000 |
| **R12** | 写侧自动决策链路 | 窗口内 `obs-append` → `event=distill`（按 `source` 分流）→ 失败状态数 → `accept` / `reject` 计数；只读 `audit.jsonl` + `mem eval activity --json` | 有蒸馏且失败 0 | 断链或失败 1–3 | 失败 >3 |
| **R13** | 注入体量 | 窗口内读侧（`session-start` / `prompt-submit` / `pre-compact`）审计记录 `inject_bytes` 的**最大值**（无样本记 `INFO`） | ≤3600 | 3601–8000 | >8000 |
| **R14** | 归档体量 | `~/.v2mem/evidence` 下文件数与总字节（`find` / `du`，只读）；detail 附 `mem stats --json` 的 `size_bytes` 作对照 | ≤512MiB | >512MiB | —（不设 FAIL，见依据） |

阈值依据（不凭空定）：

- **R3 的 50ms**：README 声明 200 条库检索平均 ~9ms，实测量级 9–15ms。50ms 是 5 倍冗余，
  越过说明库膨胀或 WAL 写锁竞争。
  **采集口径**：只取 `prompt-submit` / `session-start`（读侧注入）。`stop` 与 `obs-append`
  共用同一个 Stop hook 计时器，其 `ms` 是整个捕获/归档流程耗时，不是检索耗时；
  早期脚本对所有带 `ms` 的事件取均值，把 500ms 级写侧耗时混进来，产生稳定误报。
  脚本另在 R3 detail 展示写侧 Stop 均值供观察，**不对写侧判级**。
- **R2 的 30%**：`docs/design-eval.md` 的口径是「能取到的细节无限深」，命中率随库内容增长而上升；
  初期低是必然。30% 是"库有料但仍常空手"的告警线。
- **库 20 条 / 评测 20 样本**：`mem audit` 自己提示「可评测样本 ≥20 再下结论」，同一口径沿用。
- **R8 的 15**：候选是隔离区，长期不处理会让下次蒸馏覆盖判断；经验值，可随实测调整。
  **分组口径（2026-11-09）**：阈值按**单个 `project`** 计，跨工程候选不相加，窗口值取各工程计数的
  **最大值**（任一工程越线即按该级判）。理由：库本就全局、`project` 是列，别的项目候选是各自会话的
  合法蒸馏产物；旧口径把它们全局累加，会诱导「在 A 工程会话里把 B 工程候选 reject 掉」的误诊
  （[`design-write-governance.md`](./design-write-governance.md) §12）。`mem candidates ls --project P` 与 MCP
  `candidates_ls` 提供就近查看。R9 的采集同步改读 `--json` 的 `content`，检测式与旧版
  `grep '\{"(id|parentId|callId|type)"'` 等价，**判定语义与阈值均未变**。
  `candidates ls --json` 不可解析时 R8/R9 按 `WARN` 观察，不静默判 `PASS`。
- **R9 的 0 / 3**：候选须显式 `mem accept` 才升格（[`design-write-governance.md`](./design-write-governance.md)
  三态），所以结构化载荷残留**不是事实而是污染**，出现即说明蒸馏输入未按
  `proposalEligible` 的准入过滤。判 0 为 PASS；单次 `distill` 实测最多产出 6 条候选
  （`distill_runs` 观测），故 1–3 视为单巡检窗口内可人工清理的余量，>3 判为过滤失效。
  检测为启发式：候选内容含 `{"id"` / `{"parentId"` / `{"callId"` / `{"type"` 等
  transcript 字段即计数。**R9 不随冷启动降级**——污染与库规模无关。
- **R10 的 1000 / 3000ms**：宿主 `Stop` hook 是同步调用，`~/.workbuddy/settings.json`
  给的 `timeout` 为 20s，这是会话收尾的硬上限。1000ms 占预算 5%、3000ms 占 15%，
  越线即应介入，但距中断会话仍有余量。实测对照（`~/.v2mem/audit.jsonl`，
  `obs-append` 全量 n=77）：健康期 11-01~11-05 日均 3–20ms，全量
  p50=81ms / p90=1154ms / p95=2847ms / max=7554ms——两档与 p90/p95 同量级，
  只让最慢的约 10% / 5% 告警，避免长会话的正常批量补写被误判。
  **采集必须只取 `stop`**：`obs-append` 与其共用同一个计时器，两者都取会双计均值。
  R3 已把写侧排除在「检索延迟」之外，R10 是给这部分耗时一个独立判级出口，
  不是把 R3 的口径改回去。
- **R11 的 1000 / 2000 帧**：`internal/store/store.go:70` 只设了 `journal_mode=WAL`、
  `busy_timeout=5000`、`synchronous=NORMAL`，未改 `wal_autocheckpoint`，即 SQLite
  默认 1000 页；超过 1000 帧说明被动 checkpoint 已跟不上写入。2000 帧约为阈值的
  2 倍（按 `page_size=4096` ≈ 8MB）。脚本按 4096 字节/页整除折算，是巡检的近似
  口径（未扣 WAL 头与每帧 24 字节头，误差约 0.6%，不影响分级），detail 同时给出
  原始字节数供复核。实测 11-08 04:19 为 1118 帧 / 4.5MB，
  手动 `PRAGMA wal_checkpoint(TRUNCATE)` 返回 `0|0|0` 并归零，证明可回收。
  **局限**：这是单点采样，刚 checkpoint 完会读到 0；它只反映 checkpoint 状态，
  **不用于推断写侧延迟**——11-08 黑盒实测中 hook 进程退出后 WAL 帧恒为 0，
  而 Stop 耗时仍随 transcript 行数线性上升，两者无因果关系（见
  [`design-write-batch.md`](./design-write-batch.md) §0）。R10 与 R11 各自独立判级。
- **R12 的 0 / 3 与断链口径（2026-11-09 新增）**：这里的「自动决策」指 hook 驱动的
  `observations → distill_runs → candidates` 链，**不含** `accept` / `reject` —— 后者是显式裁决，
  积压由 R8 判级、动作在 §5 R8。R12 只回答「自动段有没有推进」：
  - `obs-append = 0` → `INFO`。这是**无样本**，与 R3 / R10 同类，不是 §4 的冷启动降级。
  - `event=distill = 0` 而有观测 → `WARN`（断链）。成因按 §8.1 的三开关
    （`MEM_DISTILL_SPAWN` / `MEM_PROPOSAL_HOOK` / `MEM_NO_DISTILL`）、
    `startDistillProc` 失败（只打 stderr、不写审计，故库里看不出来）或名单静默回落（§8.3）逐项排查。
    **不判 `FAIL`**：`MEM_NO_DISTILL=1` 完全停掉蒸馏是 §8.4 的合法配置，判 FAIL 会制造恒久误报。
  - 失败状态沿用 R7 / R9 的「单窗口可人工处理余量」分档 `0 / 1–3 / >3`。失败集合取
    `failed=true` 或 `mode ∈ {fail, partial, invalid, backlog}`：Proposal 会写 `noop` /
    `invalid` / `backlog`，而 `audit.Summarize` 只认 `mode=ok` / `mode=fail`，
    `mem eval activity` 的 `distill_fail` 因此有盲区，R12 直接读审计补齐。
    `empty` / `noop` / `ok` 是合法结果，不计失败。
  - `source=proposal` 为 0 **不判级**：默认安装跑旧规则蒸馏，Proposal 是 §8 的灰度项。
    detail 展示分流，是把 §8.3 的「首日确认」从人工 grep 变成每窗口可见。
  - 裁决活性（`accept` / `reject` 计数）与 `mem eval activity --json` 的 `verdict`
    （含「有观测无采纳」）只作 detail，不判级：零裁决已由 R8 的积压判级承接，避免重复告警。
    `eval activity` 读不到或不可解析时 R12 判级不受影响。
  - **采集只读**：`audit.jsonl` + `mem eval activity --json`（`cmdEvalActivity` 不开库），
    不执行 `mem distill` / `accept` / `reject` / `gc`。`distill_runs` 的 `status` / `error` /
    `attempt` 与 `evidence_bundles` 积压留作 §5 的深挖命令，不进脚本采集，避免引入 sqlite 依赖。
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
  「只写的事实源」，一条观测一份 `.jsonl.gz`，**没有任何自动回收路径**：
  `mem gc --obs` 删的是 `observations` 表行，不碰归档文件（`internal/store/observe.go` 的
  `GCObservations` 只发 `DELETE FROM observations`）；而手工删归档会破坏 `memory_evidence`
  的溯源目标——记忆会显示「有证据」却打不开原始记录。因此这条判据回答的是「水位要不要裁决」，
  不是「是否故障」。
  - **512MiB ≈ 10 万份归档**：实测 2026-11-12 为 8215 份 / 41MiB（均值 5.1KB/条）。
    `GCObservations` 的 `maxKeep` 默认 50000 条是产品自己声明的观测保留上限，
    512MiB ≈ 其 2 倍。越过说明归档量已超出「自己认为需要保留」的量级，应显式裁决保留策略。
  - **为什么不设 FAIL 档**：归档按设计只增不减，任何 FAIL 线都会被自然增长永久触发，
    与 R12 断链不判 FAIL 同理——恒久误报会让判据失去意义。越过 512MiB 后按 §5 R14 处置；
    确认接受现状就上调阈值并留记录（口径同 §5 R13）。
  - **口径是磁盘占用，不是条数**：`du -sk` 的块数（APFS 稀疏文件不占块，故样本必须实写）
    加文件数。**不做「折算成多少条观测」的推算**：`gc` 之后行数会降而文件数不降，两者本就会脱钩，
    判据只量落盘量，不量语义。主库 `size_bytes` 仅作对照——主库与归档通常同源增长（观测行），
    但主库会因 freelist 复用而平台化，归档不会。
  - **不随冷启动降级**：它是累计量，与库里有几条记忆无关。

---

## 4. 冷启动期例外（重要）

**库条数 < 20 时，R2 与 R4 降级为 `INFO`。** 理由：此时空注入率高、写侧稀疏都是**设计预期**，
不是故障。用成熟期的阈值去判初期状态，会得到稳定但无意义的 FAIL，最终导致告警疲劳。

判据必须阶段化，否则「符合预期」这四个字没有定义。跨过 20 条后自动启用完整阈值。

**例外只覆盖 R2 / R4。** R9、R10、R11、R12、R13、R14 不降级：候选载荷残留是输入过滤失效、写侧延迟
是同步 hook 的耗时、WAL 堆积是 checkpoint 未跟上、写侧决策断链是自动链路没推进、注入体量是
单次注入的字节数、归档体量是累计落盘量，都与库里有多少条记忆无关，冷启动期同样应报。R12 的 `INFO` 是「窗口内无 `obs-append` 样本」，
与 R3 / R10 / R13 的「无样本」同类，**不是**本节的降级。

---

## 5. 异常处置

| 现象 | 首要怀疑 | 动作 |
|:---|:---|:---|
| R1 = 0（钩子零事件） | 配置写错文件 | 查 `WORKBUDDY_CONFIG_DIR`，确认钩子在 `~/.workbuddy/settings.json` 而**非** `~/.codebuddy/settings.json` |
| R2 低但 R1 正常 | 库稀疏 / 同义词未覆盖 / **找到了但供给弃答** | 先分因，再对症：① `mem eval activity --since 12h` 列出空命中 query；② 手工复跑 `mem search "<query>"`，**也空** → 检索侧（补记忆或 `mem lexicon init`）；**有结果** → 供给侧弃答，直接查结局分布：`sqlite3 -readonly ~/.v2mem/mem.db "SELECT outcome, COUNT(*) FROM read_traces WHERE created_at > strftime('%s','now')-43200 GROUP BY outcome;"`。`abstained_no_hit` 是检索侧没找到，`abstained_low_quality` 是**找到了但被判无支持**（典型：只有 broad 单路支持），`skipped_gate` 是客套跳过（正常）。第三种（低质量弃答）的机制证据与复现见 [`design-eval.md`](./design-eval.md) §17.1 与 `make eval-hook-ab`——**不要**靠调低质量门阈值来让 R2 变好看 |
| R7 持续增长 | 写库失败 | 查 `~/.v2mem` 权限与磁盘；spool 会在下次 SessionStart 重试 |
| R6 下降 | 非预期删除 | 查 `mem gc` / `mem consolidate` 是否自动跑过；库被外部删除后重建也走这条 |
| I2 掉到 1–3 | 宿主升级覆盖配置 | 重跑 `mem init --harness workbuddy`（幂等、带备份） |
| I5 = WARN（`ExecEnv: Sandbox`） | Trae 钩子跑在沙箱，`~/.v2mem` 禁写 | Hooks 面板运行方式改「本地自动运行」；或在沙箱配置放行 `~/.v2mem` |
| I3 缺失 | 连接器开关被关 | 连接器管理页重新开启；**不影响 R 组**，hook 与 MCP 相互独立 |
| R8 候选积压 | 隔离区长期未裁决 | 先看 detail 里的**按工程分组**计数，在**对应工程的会话里**就近裁决：`mem candidates ls --project P --policy` 或 MCP `candidates_ls` 逐条看；可读候选 `mem accept` / MCP `accept`，载荷残留 `mem reject` / MCP `reject`（顶层命令，非 `mem candidates reject`）。不要跨工程批量 reject |
| R9 载荷残留 | 蒸馏输入未按准入过滤 | 先修 `distillOne` 的输入过滤（复用 `proposal.proposalEligible`），再逐条 `mem reject` 清存量；只清不修会在下个窗口复现 |
| R10 写侧延迟越线 | 单次 `Stop` 补写 transcript 过多，每条证据各做一次归档 fsync | 先看窗口内 `stop` 条数与 transcript 增量；实测瓶颈是 `writeArchive` 的逐条 fsync（占 88%），DB 事务仅占 12%，故**批量事务不是治本手段**。治本方案见 [`design-write-batch.md`](./design-write-batch.md)（批量归档，P2 已实现）：在启动宿主的环境导出 `MEM_EVIDENCE_BATCH=1` 灰度开启，默认关；实测 N=93 由 925ms 降到 58ms。`PRAGMA wal_checkpoint(TRUNCATE)` 只回收 WAL、与延迟无因果；**不要**删除归档或观测行来降耗时。注意 `Stop` hook `timeout=20s`，接近上限会阻断宿主会话 |
| R11 WAL 帧数越线 | 被动 checkpoint 未跟上 | `sqlite3 ~/.v2mem/mem.db "PRAGMA wal_checkpoint(TRUNCATE);"` 并记下返回三元组 `(busy,log,checkpointed)`；`busy=1` 说明有长读事务，查各 harness 的 `mem mcp` 连接。**不要**手工删除 `mem.db-wal`——里面有尚未落盘的已提交事务 |
| R12 写侧决策断链 / 失败 | 自动蒸馏没推进，或蒸馏落了失败状态 | **只读诊断，不要重跑 `mem distill` 掩盖问题**。断链（`obs-append` 有量但 `event=distill=0`）：按 §8.1 核对 `MEM_DISTILL_SPAWN` / `MEM_PROPOSAL_HOOK` / `MEM_NO_DISTILL` 是否按预期导出，查宿主 stderr 有无 `startDistillProc` 失败，确认是否按 §8.4 有意关闭；再跑 `sqlite3 -readonly ~/.v2mem/mem.db "SELECT status,error,attempt,started_at FROM distill_runs ORDER BY started_at DESC LIMIT 20;"` 看是否真的没落 run。失败态：查 `distill_runs` 的 `error` / `attempt` 与 `evidence_bundles` 是否 `backlog` / `quarantined` 挡住后续窗口。detail 里 `proposal=0` 且已开灰度 → 按 §8.3 做名单自证，**不要只看「钩子正常」**。detail 里零裁决 → 按 §5 R8 在对应工程会话就近裁决 |
| R13 注入体量越线 | 预算被调高，或 `session-start` 类码表膨胀 | 先查是否有人把 `MEM_PRECOMPACT_BUDGET` 或其他预算变量调高（`pre-compact` 单独读这个环境变量，见 `cmd/mem/hook.go:156-161` 的 `positiveEnvInt` 分支）；再查 `session-start` 的类码表是否膨胀——它恒定注入、不随库增长，是体积主项。确认是有意为之就调阈值并留记录，**不要**靠删记忆来降体积 |
| R14 归档体量越线 | 归档只增不减，且没有回收用例 | 先分因：① **量增长**（文件数与字节同步涨）→ 裁决保留策略；② **单条体积异常**（字节涨、文件数不涨）→ 查 `observeRawMaxRunes`（`cmd/mem/observe.go:34`，32000 rune 上限）是否被抬高，或单条 transcript 本身变大。**不要**直接删 `~/.v2mem/evidence`：里面是 `memory_evidence` 的溯源目标，删了记忆的证据链就指向空处；也**不要**用 `mem gc --obs` 期待回收字节（它只删观测行）。确认接受现状就上调阈值并在提交信息里写明原因 |

处置完成后重跑一次脚本确认级别回落，**不要**在未确认前反复改配置。

---

## 6. 调度与基线

- 定时任务：每 12 小时跑 `scripts/watch-v2mem.sh --window 12h`。
- 基线快照：`~/.v2mem/watch-state.json`（`{ts,total,window}`），供 R6 对比；首次跑记为 `INFO`。
- 脚本**只读**库与配置，唯一写入是上述快照。`--quiet` 只输出非 PASS 项，`--json` 供机器消费。
  R12 额外调用的 `mem eval activity --json` 只读 `audit.jsonl`（`cmdEvalActivity` 不打开数据库），
  不写审计、不写库；该命令不可用时 R12 仍按审计独立判级。
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
| 诊断项固定枚举，便于长期统计同类问题频次 | `doctor-diagnostics.ts` 的 `DIAGNOSTIC_IDS` | 判据 ID（I1…I5 / R1…R14） |
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

## 8. Proposal Hook 观察期 SOP

> 本节只定义**灰度流程与转正判据**，不新增也不修改 §3 的判据 ID 与阈值。
> 引用时一律用 §3 的 R1 / R2 / R4 / R5 / R8 / R9 / R12，不自造 ID。

### 8.1 灰度开关

`MEM_DISTILL_SPAWN=1` 是**常规**蒸馏子进程的总闸（`distillSpawnEnabled`，`cmd/mem/distill_hook.go`）；
`MEM_PROPOSAL_HOOK` 只决定「拉起的是 Proposal 还是旧规则蒸馏」。两者**缺一不生效**：

| 路径 | 入口 | 受 `MEM_DISTILL_SPAWN` 约束 | 走 Proposal 的条件 |
|:---|:---|:---|:---|
| Stop / SessionEnd | `maybeSpawnDistill`（`distill_hook.go`） | **是**：先判 gate，未设直接返回、什么都不做 | `MEM_DISTILL_SPAWN=1` **且** `proposalHookEnabled(host)` 为真 |
| SessionEnd（仅首会话） | `maybeSpawnColdStart`（`coldstart.go`） | **否**：一次性例外，见下 | **不走** Proposal，固定 `--provider none` |
| SessionStart | `distillPendingFailOpen`（`distill_hook.go`） | **是**：要求 gate 与名单同时成立 | 同上；否则**继续往下**内联跑旧规则 `--pending`（200ms 上限，受 `MEM_NO_DISTILL` 约束） |

- `MEM_DISTILL_SPAWN` 未设 / 非 `1` → 常规 Stop / SessionEnd 与 SessionStart 都不拉子进程，SessionStart 退回内联旧规则——这是**默认**。
- **冷启动例外**：每个工程首次 `SessionEnd`、且该工程还没有已采纳记忆时，拉一次 detached 规则蒸馏并把成本移出下一次 `SessionStart`；靠 `coldstart:v1:<project>` 标记保证只尝试一次。它固定零 LLM、只写 candidates，失败由 SessionStart 内联蒸馏兜底。`MEM_NO_COLDSTART=1` 可关，`MEM_NO_DISTILL=1` 同样覆盖它。
- `MEM_PROPOSAL_HOOK` 未设 / 空 / `0` → 全部走旧规则蒸馏，取值见 8.2。
- SessionStart 在 gate 不成立时**不是静默跳过**：内联旧规则仍会蒸馏 pending 会话，所以不会丢蒸馏。

**运维结论：灰度期两个都设**（`MEM_DISTILL_SPAWN=1` + `MEM_PROPOSAL_HOOK=<宿主>`）。
只设 `MEM_PROPOSAL_HOOK` 时三条常规路径全部不动（等于什么都没开），不会出现「半开」——
首会话冷启动仍可能跑一次，但它固定走规则蒸馏，不产生 Proposal 数据，观察期依然白跑。

> 口径变更记录：本节在 `6ed7e8b` 之前写的是「SessionStart 不受 gate 约束」。该不一致已在
> `6ed7e8b` 修复（`distillPendingFailOpen` 纳入同一 gate），现在两个变量是真正的「缺一不生效」。
> **升级影响**：只配了 `MEM_PROPOSAL_HOOK` 而未配 `MEM_DISTILL_SPAWN` 的宿主，SessionStart 会从
> 「拉 Proposal 子进程」退回「内联旧规则」——Proposal 观察数据归零而不是补齐，需按上表两变量同设。

### 8.2 `MEM_PROPOSAL_HOOK` 取值语义（已裁决）

| 取值 | 结果 |
|:---|:---|
| 未设置 / 空串 / `0` | **旧规则蒸馏**（`--session <sid> --provider none`），默认未改变 |
| `1` / `all` / `on`（大小写不敏感） | 全宿主启用 Proposal |
| 其它 | 按**逗号分隔的 harness 名单**灰度 |

名单匹配规则（`proposalHookEnabled`，`cmd/mem/distill_hook.go:69`）：

- 名单项与当前宿主名**都经注册表归一化到规范名后比较**：大小写不敏感、去空格、解析别名。
  因此 `trae` 能命中 `traecode` / `traework`；`claude-code`、`cc` 能命中 `claude`。
- 未显式传 `--harness` 时，宿主名由宿主的 `*_PROJECT_DIR` 推导（`harnessName`，`cmd/mem/hook.go:187`）。
  推导成功 → 灰度照常生效；推导失败（宿主名为空）→ **名单灰度保守失效、回归旧规则蒸馏**。
  全量值（`1` / `all` / `on`）不受影响。

### 8.3 配错自证（已知风险，必须先做）

**名单写错会静默回落。** 写成 `claud`、`trae-code` 这类未注册别名时，归一化后匹配不上，
`proposalHookEnabled` 返回 false，链路**不报错**、照常跑旧规则蒸馏——从「钩子正常」看不出区别。

| 步骤 | 动作 | 通过标准 |
|:---|:---|:---|
| ① 链路自证 | 临时库上设 `MEM_PROPOSAL_HOOK=1`（跳过名单）跑一轮 Stop | 审计出现 `event=distill` + `source=proposal` |
| ② 再改名单 | 换成目标名单，保持 `MEM_DISTILL_SPAWN=1` | 审计同样出现 `source=proposal` |
| ③ 首日确认 | 观察期第 1 天查 R1 钩子事件 | 有 proposal 记录，而不是**只有** `--provider none` 的旧蒸馏 |

```bash
grep '"source":"proposal"' ~/.v2mem/audit.jsonl | tail -5   # 有输出 = Proposal 真的跑了
grep '"source":"distill"'  ~/.v2mem/audit.jsonl | tail -5   # 有输出 = 旧规则蒸馏（对照用）
```

> 旧规则蒸馏的审计同理是 `event=distill`，区别在 `source`：Proposal 写 `proposal`，旧规则写 `distill`
> 且 `provider=none`（`cmd/mem/distill.go:177` / `:562`）。**只看 `event=distill` 分不出两者。**

### 8.4 秒级回退

| 目标 | 动作 |
|:---|:---|
| 回到旧规则蒸馏 | `MEM_PROPOSAL_HOOK=0`，或直接取消该变量 |
| 完全停掉蒸馏 | 取消 `MEM_DISTILL_SPAWN` **且**设 `MEM_NO_DISTILL=1`（缺后者时 SessionStart 仍会内联跑旧规则 `--pending`，见 8.1） |

注意：只把 `MEM_PROPOSAL_HOOK` 设为 `0` 时，`MEM_DISTILL_SPAWN=1` 下**旧规则蒸馏仍会跑**——
关掉的是 Proposal 路径，不是蒸馏本身。回退不需要重启宿主，下一次钩子即按新值生效。

### 8.5 观察期与逐条判据

观察期**至少 7 天 / 14 个 12h 窗口**（对齐 §6 的巡检节奏）。信号全部来自 §3，不新增指标：

| ID | 看到什么算正常 |
|:---|:---|
| **R1** 钩子被真实触发 | 每窗口 ≥1 事件，且窗口内存在 `source=proposal` 的 distill 记录；连续两窗只有 `provider=none` → 按 8.3 查名单 |
| **R2** 检索命中率 | ≥30%（库 ≥20 条）；<20 条时为 `INFO`，不看数值 |
| **R4** 写侧活动 | 库 ≥20 条时每窗口 `manual-add` ≥1；为 0 记 `WARN`，不是 FAIL |
| **R5** 观测落库 | 每窗口 `obs-append` ≥1（归档链路未断） |
| **R8** 候选积压 | 各 `project` 的 `pending` 计数，窗口值取最大值（§3 口径）；≤5 正常，6–15 为 `WARN`，>15 为 `FAIL` |
| **R9** 候选载荷残留 | 恒为 **0**；出现即说明 Proposal 输入未按 `proposalEligible` / `Fidelity` 过滤 |
| **R12** 写侧自动决策链路 | 看 detail 的 `source` 分流：灰度期应出现 `proposal>0`；连续两窗只有 `rules` → 按 8.3 查名单与 `MEM_DISTILL_SPAWN`。断链与失败态按 §5 R12 只读处置，**不改本节的转正阈值** |

`R3` / `R6` / `R7` 照常巡检，但**不参与本节的转正判定**——它们衡量的是检索延迟与库、spool 健康，
与 Proposal 路径无关。

### 8.6 冷启动期例外

库内记忆 **<20 条**时，R2 / R4 按 §4 降级为 `INFO`：**只有 FAIL 需要介入，INFO 不是回退**。
冷启动期 R2 低、R4 为 0 是设计预期；把它当 Proposal 失效去回滚开关，属于误判。
R9 **不降级**：载荷残留与库规模无关，冷启动期同样判 0。

### 8.7 转正判据（可量化）

全部满足才建议切默认；任一条不满足 → **继续观察**，不自动改默认值：

| # | 条件 | 阈值 | 来源 |
|:---|:---|:---|:---|
| 1 | 观察期无 FAIL | 连续 7 天（14 个窗口）全部窗口无 `FAIL`；R8 按工程分组后**任一 `project` 越线即该窗口 FAIL** | §3 判级 |
| 2 | R8 不积压 | **窗口值 = 该窗口各 `project` 计数的最大值**；14 个窗口中 `>5` 的窗口 ≤2 个，且末窗口最大值 ≤ 首窗口最大值（不呈单调增长） | **建议值** |
| 3 | R9 干净 | 14 个窗口恒为 `0` | §3 R9 PASS 线 |
| 4 | 检索无回退 | `make eval-gate` 的 Recall@5 ≥ **0.60**，且较切换前一次实测基线绝对下降 ≤ **0.03** | 0.60 出自 `eval-policy.txt` 的 `recall5_min`；0.03 为**建议值** |

> `make eval-gate` 跑的是沙箱夹具（不读个人库），它守的是**代码级检索回归**，不反映 Proposal 质量——
> Proposal 质量由条件 2 / 3 与人工抽查候选可读性共同判定。两类证据不要互相替代。
> **口径切换**：R8 于 2026-11-09 改为按 `project` 分组取最大值，此前留存的窗口序列是**全局旧口径**，
> 不得与新口径混算；观察期若跨该日期，条件 2 需从切换后的首个窗口重新起算 14 窗。

**切默认值必须单独提一次变更并留档**（改默认 = 改所有宿主的行为），不得在观察期结束时顺手合并。

