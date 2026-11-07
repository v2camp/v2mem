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
| **R8** | 候选积压 | `mem candidates ls` 条数 | ≤5 | 6–15 | >15 |
| **R9** | 候选内容质量 | pending 候选中 transcript 原始 JSON 载荷残留条数 | 0 | 1–3 | >3 |

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
- **R9 的 0 / 3**：候选须显式 `mem accept` 才升格（[`design-write-governance.md`](./design-write-governance.md)
  三态），所以结构化载荷残留**不是事实而是污染**，出现即说明蒸馏输入未按
  `proposalEligible` 的准入过滤。判 0 为 PASS；单次 `distill` 实测最多产出 6 条候选
  （`distill_runs` 观测），故 1–3 视为单巡检窗口内可人工清理的余量，>3 判为过滤失效。
  检测为启发式：候选内容含 `{"id"` / `{"parentId"` / `{"callId"` / `{"type"` 等
  transcript 字段即计数。**R9 不随冷启动降级**——污染与库规模无关。

---

## 4. 冷启动期例外（重要）

**库条数 < 20 时，R2 与 R4 降级为 `INFO`。** 理由：此时空注入率高、写侧稀疏都是**设计预期**，
不是故障。用成熟期的阈值去判初期状态，会得到稳定但无意义的 FAIL，最终导致告警疲劳。

判据必须阶段化，否则「符合预期」这四个字没有定义。跨过 20 条后自动启用完整阈值。

**例外只覆盖 R2 / R4。** R9 不降级：候选里出现 transcript 原始 JSON 是输入过滤失效，
与库里有多少条记忆无关，冷启动期同样应报。

---

## 5. 异常处置

| 现象 | 首要怀疑 | 动作 |
|:---|:---|:---|
| R1 = 0（钩子零事件） | 配置写错文件 | 查 `WORKBUDDY_CONFIG_DIR`，确认钩子在 `~/.workbuddy/settings.json` 而**非** `~/.codebuddy/settings.json` |
| R2 低但 R1 正常 | 库稀疏 / 同义词未覆盖 | `mem eval activity --since 12h` 看空命中 query，补记忆或 `mem lexicon init` |
| R7 持续增长 | 写库失败 | 查 `~/.v2mem` 权限与磁盘；spool 会在下次 SessionStart 重试 |
| R6 下降 | 非预期删除 | 查 `mem gc` / `mem consolidate` 是否自动跑过；库被外部删除后重建也走这条 |
| I2 掉到 1–3 | 宿主升级覆盖配置 | 重跑 `mem init --harness workbuddy`（幂等、带备份） |
| I5 = WARN（`ExecEnv: Sandbox`） | Trae 钩子跑在沙箱，`~/.v2mem` 禁写 | Hooks 面板运行方式改「本地自动运行」；或在沙箱配置放行 `~/.v2mem` |
| I3 缺失 | 连接器开关被关 | 连接器管理页重新开启；**不影响 R 组**，hook 与 MCP 相互独立 |
| R8 候选积压 | 隔离区长期未裁决 | `mem candidates ls --policy` 逐条看：可读候选 `mem accept`，载荷残留 `mem reject`（顶层命令，非 `mem candidates reject`） |
| R9 载荷残留 | 蒸馏输入未按准入过滤 | 先修 `distillOne` 的输入过滤（复用 `proposal.proposalEligible`），再逐条 `mem reject` 清存量；只清不修会在下个窗口复现 |

处置完成后重跑一次脚本确认级别回落，**不要**在未确认前反复改配置。

---

## 6. 调度与基线

- 定时任务：每 12 小时跑 `scripts/watch-v2mem.sh --window 12h`。
- 基线快照：`~/.v2mem/watch-state.json`（`{ts,total,window}`），供 R6 对比；首次跑记为 `INFO`。
- 脚本**只读**库与配置，唯一写入是上述快照。`--quiet` 只输出非 PASS 项，`--json` 供机器消费。
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
| 诊断项固定枚举，便于长期统计同类问题频次 | `doctor-diagnostics.ts` 的 `DIAGNOSTIC_IDS` | 判据 ID（I1…R9） |
| 机器可读 alert token 与人类可读 note 分离 | 同上 `alerts[]` / `notes[]` | `--json` 的 `items[]` |

| 不借鉴 | 理由 |
|:---|:---|
| 进程资源阈值（event loop lag / CPU / RSS） | v2mem 无常驻进程，无对象可测 |
| `stale-pidfile` 类诊断 | 没有 pid 文件，问题不存在 |
| mem0 的 `ADD` 事件遥测 | mem0 走 LLM 抽取 + 向量库，写侧决策模型与 v2mem 的 `candidates → accept` 不同；搬来会对齐错误的写治理语义 |

> mem0 的 `ADDITIVE_EXTRACTION_PROMPT`（写侧抽取）与 agentmemory 的 4-tier 衰减
> 对**检索质量**有意义，但对**运行观测**没有——观测只看链路是否通、量是否合理，不看内容好不好。
> 内容质量归 `docs/design-eval.md`（recall / write 评测），两者不要混在一个指标里。

---

## 8. Proposal Hook 观察期 SOP

> 本节只定义**灰度流程与转正判据**，不新增也不修改 §3 的判据 ID 与阈值。
> 引用时一律用 §3 的 R1 / R2 / R4 / R5 / R8 / R9，不自造 ID。

### 8.1 灰度开关

Proposal 路径由两个环境变量共同决定，缺一不生效：

| 变量 | 取值 | 语义 | 默认 |
|:---|:---|:---|:---|
| `MEM_DISTILL_SPAWN` | `1` | 允许 Stop / SessionEnd 末尾 `Start` 一次短命蒸馏子进程 | **未设 = 不拉起任何蒸馏子进程**（`cmd/mem/distill_hook.go:96`） |
| `MEM_PROPOSAL_HOOK` | 见 8.2 | 决定拉起的是 **Proposal 路径**还是**旧规则蒸馏** | **未设 = 旧规则蒸馏** |

> 口径澄清：`MEM_DISTILL_SPAWN` 只管**子进程**。SessionStart 的 pending 抽取是同步内联、有 200ms
> 时间上限（`distill_hook.go:167`），不受该变量控制；两条路径的选择都由 `MEM_PROPOSAL_HOOK` 决定。

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
| 完全停掉蒸馏 | 再取消 `MEM_DISTILL_SPAWN` |

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
| **R8** 候选积压 | `pending` 候选 ≤5；6–15 为 `WARN`，>15 为 `FAIL` |
| **R9** 候选载荷残留 | 恒为 **0**；出现即说明 Proposal 输入未按 `proposalEligible` / `Fidelity` 过滤 |

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
| 1 | 观察期无 FAIL | 连续 7 天（14 个窗口）全部窗口无 `FAIL` | §3 判级 |
| 2 | R8 不积压 | 14 个窗口中 `>5` 的窗口 ≤2 个，且末窗口值 ≤ 首窗口值（不呈单调增长） | **建议值** |
| 3 | R9 干净 | 14 个窗口恒为 `0` | §3 R9 PASS 线 |
| 4 | 检索无回退 | `make eval-gate` 的 Recall@5 ≥ **0.60**，且较切换前一次实测基线绝对下降 ≤ **0.03** | 0.60 出自 `eval-policy.txt` 的 `recall5_min`；0.03 为**建议值** |

> `make eval-gate` 跑的是沙箱夹具（不读个人库），它守的是**代码级检索回归**，不反映 Proposal 质量——
> Proposal 质量由条件 2 / 3 与人工抽查候选可读性共同判定。两类证据不要互相替代。

**切默认值必须单独提一次变更并留档**（改默认 = 改所有宿主的行为），不得在观察期结束时顺手合并。

