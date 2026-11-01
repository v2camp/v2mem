# 架构参考：mem0 与 agentmemory

> **定位**：**全面**架构参考，不是只借鉴观测。回答「这两个成熟实现做过哪些架构选择、代价是什么、
> v2mem 该拿哪些不该拿哪些」。
> **对照代码**：`~/code/oss-research/mem0`（v2.2.1，Python 32.5k 行）、
> `~/code/oss-research/agentmemory`（v0.9.31，TS 58k 行）。
> **与其它文档的分工**：[`design-anti-agentmemory.md`](./design-anti-agentmemory.md) 只写「为何不搬常驻 iii 与 54 工具」的取舍；
> 本文写**可借鉴的机制清单**与两侧的坑。两者不重复。
> **纪律**：本文结论的证据是代码路径。读它们的 README / 官网做设计判断会出错——
> mem0 文档里的 temporal reasoning 在 OSS 里直接 `raise ValueError("Platform-only")`。

---

## 0. 先看两者的路线分歧

三个系统对「记忆一致性该在哪一层解决」给出了三种答案：

| | mem0 v3 | agentmemory | v2mem |
|:---|:---|:---|:---|
| 一致性在哪解决 | **读时**：全量累积，检索排序兜底 | **写后异步**：定时 consolidate 归并 | **写时 + 隔离区**：候选须 `accept` |
| LLM 调用次数 | 每次 add 一次 | 会话结束时批量（≤10 次/轮） | 默认零（规则抽取） |
| 常驻进程 | 无（SDK 库） | 有（engine + 3 worker） | 无 |
| 库增长 | 只增不减 | 有淘汰四路径 | 三态 + TTL + 取代 |
| 检索融合 | 加法归一（`maxPossible` 自适应） | **加权 RRF**（k=60）+ 多路奖励 | bm25 → salience → last_seen |

三条路线都成立，代价不同：mem0 把成本推给检索，agentmemory 推给运维，v2mem 推给写侧纪律。
**v2mem 的路线与 `VALUE.md` 一致**（观测 → candidates → accept），不需要动摇。

---

## 1. mem0 v3 那次反转（最值钱的一条）

v3 把「两次 LLM 调用 + ADD/UPDATE/DELETE/NOOP 决策」换成「单次 LLM 调用、只 ADD、批量」。
作者自述的迁移理由与收益（`docs/migration/oss-v2-to-v3.mdx`）：

- 动机：*「模型把容量花在理解输入，而不是 diff 既有状态」*
- 收益：LoCoMo **71.4 → 91.6**、LongMemEval **67.8 → 93.4**，抽取延迟约减半
- 代价（作者自己点明）：*「memories accumulate… 检索负责排序」* → 库只增不减

**对 v2mem 的判断**：不能照搬，但两条副产品直接可用。
v2mem 已经用 `content_hash` 唯一索引在**写时**解决重复，成本为零；mem0 之所以放弃写时决策，
是因为它的写在远端向量库、无法做低成本唯一约束。**这是它的约束，不是普适结论。**
值得拿的是「别让 LLM 做 diff」这条洞察——v2mem 的 `consolidate` 用 MinHash 而非 LLM，方向一致。

---

## 2. 可借鉴（P0：低成本、可直接落地）

| 机制 | 来源 | 为什么值得 | 落到 v2mem 的哪里 |
|:---|:---|:---|:---|
| **整数序号替换 UUID** | `memory/main.py:935-940` | LLM 回填序号而非 UUID，避免捏造/截断 id。成本几乎为零 | 若引入 LLM 抽取/归并，被动引用一律用序号 |
| **时间锚定写进 prompt** | `configs/prompts.py:524-540` | 把「上周」「昨天」解析成绝对日期再落库，防止记忆随时间腐化 | 写侧治理：`add` 前把相对时间归一为 ISO 日期 |
| **错误语义区分** | `main.py:965-971` | `LLM 不可用` ≠ `抽取到 0 条`。旧行为无法区分二者，被迫加 `LLMError` | 任何抽取/蒸馏失败必须与「无产出」分开上报 |
| **写路径三段降级** | `main.py:1052-1076` | 批量 → 逐条 → 全失败报错，且**只把真正落库的算成功** | `mem import` / 批量写：不能对用户谎报成功数 |
| **配置原子写** | `memory/setup.py:44-60` | `tempfile` + `fsync` + `os.replace`，断电不留半个 JSON | 写 `~/.v2mem/config.toml` |
| **分批删的重复批次即停** | `main.py:1942-1946` | 任何「分批删到空」的循环都该防死循环 | `mem gc` / `mem forget --all` 若有分批 |
| **内容寻址 event id** | `agentmemory/capture/event-id.ts:58` | `sha256(session+hook+content)` → 重放天然同 id，无需服务端会话 | 观测幂等：`observations` 的 event_id |
| **淘汰安全阀三件套** | `agentmemory/functions/evict.ts` | `dryRun` 分支 + 单次上限（50/1000） + 删除前版本比对 + 每条写审计 | `mem gc` / `consolidate` 已有备份，可再补 dryRun 与上限 |
| **一致性测试套** | `mem0/integrations/.../conformance/run.py` | CLI / 库 / MCP 三入口用 golden 用例对齐，而非靠文档 | 已有 `test/`；三入口行为对齐可作为独立用例组 |

---

## 3. 可借鉴（P1：需评估，与现有设计有张力）

### 3.1 融合分归一 vs RRF（建议选前者）

| | mem0 | agentmemory |
|:---|:---|:---|
| 公式 | `(semantic + bm25 + entity) / max_possible`，分母随「实际有信号的流」自适应 | 加权 `Σ wᵢ·1/(60+rankᵢ) / maxAttainable`，再乘多路命中奖励 `1+0.05·(n-1)` |
| 位置 | `utils/scoring.py:97-119` | `state/hybrid-search.ts:196-241` |
| 优点 | 分值可解释、单信号不被稀释 | 免疫量纲、天然处理并列 |
| 缺点 | 依赖各分已归一（bm25 需 sigmoid） | 绝对分值不可比，阈值难调 |

`DESIGN.md §3` 原计划用 RRF（k=60，M5b 暂缓）。这两份实现说明：
**两者关键都不是选 RRF 还是加法，而是「分母只统计真正产出结果的流」**——单信号时不补 0。
这一点两边独立收敛到同一结论，值得直接采用。

### 3.2 双路召回（v2mem 当前的真实短板）

**mem0 的坑正好是 v2mem 的风险**：mem0 的候选集**只来自语义结果**
（`main.py:1681-1691`），BM25 与实体分只调排序不扩召回。后果：
精确命中（错误码、文件名、变量名）但语义分低于 `threshold` 的记忆**永远不出现**。
作者自己在迁移文档里写「BM25 是 boost 不是召回扩张器」——这是明确的取舍，不是疏忽。

v2mem 现在的策略是「精确优先、空手才放宽」（`DESIGN.md §检索`），
**本质仍是单路 + 退化**，与 mem0 同构。若要改进，正确形态是**两路各自召回再融合**，
而不是把精确路当作语义路的加分项。这是本文对现有设计最直接的一条挑战。

### 3.3 其余待评估项

| 机制 | 来源 | 评估要点 |
|:---|:---|:---|
| Ebbinghaus retention 打分 | `agentmemory/functions/retention.ts:17-115` | `salience·exp(-λΔt) + σ·Σ(1/daysSinceAccess)`，λ=0.01/天。v2mem 现在只有 `max-idle` + `min-salience` 的**二元判据**，没有连续分。可作为 `salience` 的刷新公式，不必新增表 |
| 图检索预算 | `hybrid-search.ts:244-280` | 500ms race + cancel。v2mem 已有 `edges` 表但未使用；若启用必须带预算，否则图扩展拖垮查询 |
| lesson 置信度衰减 | `lessons.ts:320-380` | 0.05/周，`<0.1 且未强化才软删`（不物理删）。对应 v2mem 的 `salience` 衰减 |
| 多维切片评测 | `mem0 docs/core-concepts/memory-evaluation.mdx` | 按问题类型切片（概念 / 精确事实 / 实体中心 / 时间），比总分更能暴露短板。与 `design-eval.md` 的 recall/write 口径可合并 |

---

## 4. 明确不借鉴

| 不借鉴 | 理由 |
|:---|:---|
| 25 个向量库后端 + 三张 provider 注册表 | 是**分发策略**不是工程必然（每个后端一个生态入口）。代价已经显形：15/25 才有原生 `keyword_search`，同 API 行为不同 |
| iii engine + 3 个 worker + :3111/:3113/:49134 | 真正与记忆质量相关的算法约 2000–2500 行，其余 **约 14,000 行**在为多进程常驻付账（REST 4.2k + MCP 4.2k + CLI 3.7k + 引擎生命周期 1.1k）|
| 54 MCP tools / 143 REST 端点 | 每工具一个薄包装（`mcp/server.ts` 55 个 `case`），其中 13 个是编排层、4 个团队同步，与记忆检索无关 |
| 四层记忆（working/episodic/semantic/procedural） | 对方代码里**自己也没统一**：`semantic`/`procedural` 不进 BM25/向量索引，只出现在 stats 与 viewer 计数。类型存在但检索面只有两档 |
| Reranker | 需额外模型或服务，与单二进制冲突 |
| 遥测 / notices | `notices.py` 1667 行商业提示，对本地工具负价值 |
| `AsyncMemory` 双实现 | 1721 行逐行重复，每个修复要改两遍。Go 用 goroutine + context 即可 |

---

## 5. 两边踩过的坑（v2mem 已避开 / 需警惕）

| 坑 | 现状 | 对 v2mem |
|:---|:---|:---|
| hash 去重只看本次 top-10 召回 | mem0 `main.py:1009-1013`，全库重复仍会入库 | ✅ 已避开：`UNIQUE(content_hash, project)` 是全库约束 |
| 单集合 + payload 隔离靠约定 | mem0 必须主动剥离用户 metadata 里的身份键（issue #6655） | ✅ 已避开：`project` 是列不是 JSON |
| 本地/嵌入式模式跳过索引创建 | `qdrant.py:186-188` 注释 "not supported" | ⚠️ 警惕同类：任何「本地模式功能打折」都要显式告警 |
| `list()` 默认上限导致静默截断 | mem0 `main.py:1932-1933`，「多数向量库上限 100，会静默截断」 | ⚠️ `mem ls` / `export` 应显式分页并断言总数 |
| 健康检查是幽灵路由 | mem0 `server/main.py:59` 引用 `/api/health`，路由不存在 | ⚠️ 已在 `ops-watch.md` 用 I 组真实探测规避 |
| 认证失败判为 rejected 而非 retry | agentmemory `_capture.ts:57`，密钥轮换后整批事件丢弃 | ⚠️ spool 逻辑若加：401 应保留待重试，不能丢 |
| 死代码随大改堆积 | mem0 旧 prompt / `get_update_memory_messages` 全无调用方仍在包内 | ⚠️ 大改时旧路径要连导出一起删 |
| 端口占用不自动 bump | agentmemory 直接拒绝启动（`compose-control.ts:107`） | 参考：确定性优于便利，尤其对「静默改了端口」 |

---

## 6. 结论：复杂度的正确去向

两份代码放在一起看，指向同一个结论：

> **记忆系统的复杂度应该花在读写语义上，而不是传输层。**
> agentmemory 把它花在了传输层（14000 行 / 2500 行 = 5.6 倍），换来 26 个 agent 的通用接入；
> mem0 把它花在了后端矩阵（25 个向量库），换来生态覆盖。

v2mem 选择零常驻、单二进制、MCP 单点接入，在架构上是对的——**代价是放弃跨工具的原生通用接入**，
用「12 个 harness 各写一次钩子 + 一个 MCP server」补足。这个代价比 14000 行运维代码低。

因此优先级建议（与 `VALUE.md` 判据一致，落在「索引链」而非「基础设施」）：

1. **P0**：双路召回 + 融合分母自适应（§3.2、§3.1）——直接改善检索命中，是当前最大短板
2. **P0**：错误语义与写入成功计数（§2）——影响可观测性，成本极低
3. **P1**：Ebbinghaus 作为 `salience` 刷新公式（§3.3）——不新增表
4. **P1**：`provenance` 读侧入口（`VALUE.md §1.3` 的「半条路」）——这是 v2mem 自己的欠账，不在参考范围内
5. **不做**：任何需要常驻进程、额外模型、或后端抽象层的机制
