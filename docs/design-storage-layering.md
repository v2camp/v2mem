# 存储分层：运转逻辑留库，知识内容外置

> 状态：**P0 已落地；§8-2 于 2026-11-24 重开**（`summary` / `summary_idx` 一律外置，P1 验收口径按实测重写）。生产读侧与存量数据仍未改。
> 判据权威：[`VALUE.md`](./VALUE.md)（冲突时以它为准）。本方案不新增红线，只把既有的「事实源 / 查询源分离」推广到全部知识内容。
> 相关：[`design-capture.md`](./design-capture.md) §2.3 / §9、[`design-write-batch.md`](./design-write-batch.md)、[`design-cross-device.md`](./design-cross-device.md)、[`design-db-stamp.md`](./design-db-stamp.md)、[`ops-watch.md`](./ops-watch.md)。
> 实施按 [`AGENTS.md`](../AGENTS.md) 的任务流程在独立 worktree 进行；P0 契约正文见 [`contracts/storage-layering.md`](../contracts/storage-layering.md)。

## 1. 判据

**DB 存「系统据此做决策的最小状态」，外部文件存「系统不据此做决策的内容」。**

判断一个字段该留还是该走，只问一句：删掉它之后，系统还能不能正确地做去重、排序、状态流转、检索定位、告警判定？能 → 外置；不能 → 留库。

| 层 | 装什么 | 介质 | 特性 |
|:---|:---|:---|:---|
| 运转逻辑 | 状态机、唯一键 / 幂等键、水位线与序号、引用与账本、计数器、聚合结果、身份哈希 | SQLite `mem.db` | 要约束、要事务、要 O(1) 判定 |
| 观测 / 记录内容 | 原始 transcript、DOM、完整输出、错误堆栈、事件流、归档 Raw | JSONL（`.jsonl` / `.jsonl.gz`） | 机器生成、append-only、按时间 × 分类分目录、可回放 |
| 最终记忆（人读） | 每日记忆、决策记录、可读快照 | Markdown | 长期沉淀、有章节、人读可改 |

**三层不是三份真源**：DB 是唯一查询源；JSONL 是事实源与派生物；Markdown 是投影。

## 2. 现状：原则已部分成立

v2mem 已经在做这件事，本文是收敛与补全，不是另起一套：

- 归档正文是 gzip JSONL（`segmentRecord`），`evidence_archives` 表是**账**（`archive_hash` 主键 + `source_id` / `generation` / `from_seq` / `to_seq` / `path`），不是正文；`observations.raw_ref` 已是外部路径引用。见 [`design-write-batch.md`](./design-write-batch.md) §2。
- 写入顺序已是「**先归档后写账**」：DB 失败时保留整份 segment 并整批写 spool 回放。这正是 §5.2 要固化的契约。
- 跨设备只同步 `mem export` 产出的 JSONL，禁止同步 `mem.db` 本体。见 [`design-cross-device.md`](./design-cross-device.md)。
- [`VALUE.md`](./VALUE.md) §5 已写「事实源与查询源按层分离：事实溯源读归档 Raw（32000 rune），提案输入只取 400 rune 投影」；[`design-capture.md`](./design-capture.md) §2.3 定义了 source of record / source of query。

**缺口**：`observations.summary`（有界摘要）、`summary_idx`（供 FTS 的一元组 + 二元组）与 `fts_obs_*` 索引体仍在库内；Markdown 投影层没有统一约定；`read_traces` 含可回写的 `feedback`，其「日志 / 状态」边界未明确。

## 3. 实测依据（2026-11-23，本机，只读）

方法：`sqlite3.Connection.backup()` 取含 WAL 的一致性副本 → `dbstat` 取逐表 / 逐索引页占用 → 在副本上做变体并 `VACUUM` 实测体积。**原库未修改**，副本测完已清理。

基线 `~/.v2mem/mem.db` 页占用 **29.97 MB**，`observations` 9,576 行，另有 WAL 约 4.2 MB。

| 表 / 索引 | 占用 | 占比 | 性质 |
|:---|---:|---:|:---|
| `observations` | 18.42 MB | 61.5% | 混合：身份 / 哈希是状态，`summary` 是内容 |
| `evidence_archives` | 3.09 MB | 10.3% | **账 / 引用** → 必须留库 |
| `fts_obs_data` | 2.60 MB | 8.7% | 观测全文索引（派生） |
| 观测相关索引（`ix_obs_*` / `ux_obs_*` / 主键索引） | 2.37 MB | 7.9% | 去重 / 检索定位 → 留库 |
| `read_traces` + `read_trace_items` | 0.63 MB | 2.1% | 读取轨迹，含可回写 `feedback` |
| `memories` / `candidates` / `distill_runs` / … | < 0.3 MB | ~1% | 状态骨架 |

`observations` 内知识内容分布：`summary` 4.07 MB（均 449 B）、`summary_idx` 3.90 MB（均 431 B）、`raw_ref` 1.04 MB（引用列，不动）。

> **更正一处粗分类**：早先把 `evidence_archives` 当作「可外置的归档」测过 -20.9%。读 `internal/store/schema.sql` 后确认它是账与引用，按 §1 判据属于运转逻辑，**不得外置**。下列变体全部保留账表、引用列、去重键与状态列。

| 变体 | 体积 | 缩小 | 功能影响 |
|:---|---:|---:|:---|
| 基线 | 29.97 MB | — | — |
| A 只外置 `summary` | 24.20 MB | 19.3% | 命中后展示摘要需读外部文件 |
| B 外置 `summary` + `summary_idx` | 19.09 MB | **36.3%** | 同上；FTS 源文本需改读外置内容 |
| C = B + `DROP TABLE fts_obs` | 14.50 MB | 51.6% | **理论上限**：观测全文检索须替换为外部索引，不能直接删 |

结论：**不动检索架构的现实收益是 19%~36%；超过 50% 要连检索层一起改造。**

> 2026-11-24 本机复核（10,097 行，基线 31.54 MiB；`backup()` 副本上把两列置空 + `VACUUM`，外置内容按日期分片 gzip 后计入净额）：A → 25.51 MiB（DB -19.1%，外部 1.35 MiB，净 -14.8%）；B → 20.09 MiB（DB **-36.3%**，外部 2.36 MiB，净 **-28.8%**）。副本已清理。

「库大拖慢启动」不成立，别拿它当理由。另以临时基准库（0.24 MB / 1 千行 与 93.5 MB / 40 万行，测完已清理）对照：`connect()` + 读 schema 0.244 → 0.209 ms、索引点查 0.008 → 0.007 ms，只有 `PRAGMA quick_check` 0.4 → 45.4 ms 随体积线性增长。本次改造的真实收益是**主库体积、备份 / 同步成本、内容可读性与保留策略**，不是启动速度。

## 4. 表级映射

| 表 | 留库（状态 / 约束 / 引用） | 外置（内容） |
|:---|:---|:---|
| `observations` | `id` / `event_uid` / `source_id` / `source_seq` / `content_hash` / `archive_hash` / `raw_record_hash` / `raw_ref` / `channel` / `event` / `created_at` / `occurred_at_ns` / `parser_version` | `summary` / `summary_idx` → JSONL（一律外置，见 §8-2） |
| `evidence_archives` | **整表**（账与引用） | 正文已在 gzip JSONL，无需再动 |
| `evidence_sources` / `memories` / `candidates` / `commit_decisions` / `distill_runs` / `edges` / `hook_dedup` / `db_meta` / `migration_marks` / `tags` | 整表 | — |
| `read_traces` / `read_trace_items` | `feedback` / `feedback_reason` / `feedback_at`、判据依赖的计数列 | `raw_query` / `focus_query` / `gate_reason` / `signals_json` 等只写不改的诊断字段 → JSONL（P2，须先确认 `ops-watch.md` 判据依赖） |
| `fts_mem_*` | 整组保留 | — |
| `fts_obs_*` | **整组保留精简索引**（见 §8-3） | — |
| `memories.content` | **留库**（查询源；仅 18 行 / 0.03 MB） | 单向投影到 Markdown |

`memories` 体量极小，外置正文没有收益；Markdown 只做给人 / 给其他 AI 工具读的投影。

## 5. 契约（正文见 [`contracts/storage-layering.md`](../contracts/storage-layering.md)）

### 5.1 ref 格式与不可变性

`<path>#<seq>`（如 `content/2026/11/23/observations.jsonl.gz#2`）+ 库内单列 `summary_hash`。不用 `#L<seq>` 变体；不带 `#seq` 仅兼容读取首行，新写入必须带。分片按日 append，每次写入增加独立 gzip member，历史 member 永不被重写——所以 `summary_hash` 是**规范 JSON 行**（不含结尾换行）的 sha256，由 `RefHealthLine` 校验；`RefHealth` 仍只服务不可变归档的整文件 hash。不新造第二套解析格式。

### 5.2 写入顺序：分两类，方向相反

- **引用型内容**：内容先落盘 + 一次 fsync → DB 事务写 ref / 账 / 状态。DB 失败保留 segment 并写 spool 回放；孤儿内容无害，交 GC。
- **派生型审计日志**：DB 状态事务先提交 → 再 append 日志（如 `audit.jsonl`）。日志丢了不影响业务。

两类的顺序相反，判据是「内容是不是真源的一部分」。契约里必须分开写，不能统一成一条规则。

### 5.3 阈值

**例外优先**：`summary` / `summary_idx` 一律外置（2026-11-24 重开 §8-2），不走字节阈值——前者被 `eventSummary` 硬截到 512 B，后者是其一/二元组展开、实测上限 1,523 B，按 2 KB 判定永远不会命中。其余新增内容字段仍是 `< 2 KB` 内联、`≥ 2 KB` 外置，并记 `payload_bytes`。归档 Raw 上限 32000 rune 属外置侧。

### 5.4 Markdown 是投影，不是真源

定时任务从 `memories` + JSONL 单向蒸馏为 `memory/*.md`，人可读可改，**改动不回写 DB**。若将来要回写，须在 frontmatter 定义 `source_ref` + `version` 走显式导入，并受 [`VALUE.md`](./VALUE.md) §4「梦的产出不回写主库」及其实例三条约束。否则会出现「人改了 md、DB 不知道」的双写冲突。

### 5.5 完整性与回收

`RefHealth`（文件存在 + hash 匹配 + `#seq` 可定位）P0 已提供只读骨架，不改库；巡检判据沿用 [`ops-watch.md`](./ops-watch.md)，P0 不新增 R 编号。孤儿文件 GC（无引用且超保留期）；备份顺序：冻结写 → `mem.db` 一致性备份 → 复制 JSONL 目录 → 恢复写。观测层也可接受最终一致。

## 6. 目录布局（沿用现有 `~/.v2mem/`）

```
~/.v2mem/
├── mem.db                     唯一查询源（状态 / 约束 / 引用 / 计数）
├── audit.jsonl                派生审计日志（已有）
├── evidence/                  归档 Raw，gzip JSONL（已有）
├── spool/                     写入失败补偿（已有）
├── content/YYYY/MM/DD/        新增：外置的知识内容，不可变
│   └── observations.jsonl.gz
├── memory/                    新增：Markdown 投影
│   ├── YYYY-MM-DD.md
│   └── decisions/ADR-*.md
└── derived/                   新增：外部检索 sidecar / 聚合结果
```

按时间 × 分类分目录的价值在这里：**保留策略 = 删一个日期目录**，不用 `DELETE` + `VACUUM`；纯文本可直接同步 / diff / grep。删目录前仍需 dry-run 列出待删清单并确认。

## 7. 分阶段

| 阶段 | 动作 | 验收 |
|:---|:---|:---|
| **P0 契约先行（已完成）** | `contracts/storage-layering.md` 定 ref 格式、两类写入顺序、阈值、Markdown 投影边界；`internal/evidence.RefHealth` 巡检骨架；**不改生产读侧** | 契约文档 + `ref_health_test.go`；`make cover-gate` 通过 |
| **P1 写侧外置（已落地）** | `observations.summary` / `summary_idx` 一律外置到 `content/`，DB 留 ref / hash / payload_bytes；`fts_obs` 改 contentless 独立表 + `obs_fts_map` 行号映射（保留精简索引，不存列值副本）；`mem migrate --summary-external [--dry-run]` 显式迁移；读侧按 ref 回填摘要 | **相对比例验收**（不写死绝对 MB）：同一快照 `backup()` 取基线，迁移后 `VACUUM`，`dbstat` 取 `sum(pgsize)` → DB 降幅 ≥30%（本机 36.3%）；外置内容按日期分片 gzip 计入后净降幅 ≥25%（本机 28.8%）；`ops-watch` 判据不降级；`mem search --layer observation` 通过既有评测；重放不新增行、hash 稳定 |
| **P2 检索与投影** | `fts_obs_*` 替代方案（若仍要冲压）；Markdown 蒸馏；`read_traces` 诊断字段外置 | 变体 C 只是理论上限（2026-11-23 基线约 14.5 MB），**不作验收线**；Markdown 投影跨工具可读；`mem search --layer observation` 通过既有评测 |

**非目标**：不删观测行或归档来降耗时（[`design-write-batch.md`](./design-write-batch.md) 已明确）、不改 `mem.db` 的查询源地位、不引入常驻进程（[`VALUE.md`](./VALUE.md) §4 红线）、不改 `kind` 枚举。

> 现状（2026-11-26，S13）：终态保留出口已由 `mem gc --retention` 落地（rejected 90 天 / 完成 Bundle 30 天 / 孤立决策账 / 文件侧轮转），但它只清库内账行与文件凭证，**不删 evidence 归档文件**——本设计的「归档只增不减」口径不变。

## 8. 决策（2026-11-23 拍板；§8-2 于 2026-11-24 重开）

| 点 | 决议 | 影响 |
|:---|:---|:---|
| ref 定位 | **路径 + `#seq`**；内容 hash 存库内单列 | 复用现有解析与 `ReadArchiveRecord`；分片迁移需同步改 ref |
| 阈值 | **2026-11-24 重开：`summary` / `summary_idx` 一律外置**；其余字段仍走 2 KB + `payload_bytes` | 两字段有 512 B / <2 KB 硬上限，按阈值永不外置；实测 DB -36.3%、净 -28.8% |
| `fts_obs_*` | **保留精简索引** | 收益停在约 36%；外部 sidecar 留到 P2 再评估 |
| Markdown | **单向投影、不回写** | 不做 `source_ref` / `version` 导入协议 |
| `read_traces` | **诊断字段 P2 再拆** | 拆前先核对 `ops-watch.md` 判据依赖 |

> 五项已对齐，P0 按 AGENTS.md 在独立 worktree 实施；契约正文见 `contracts/storage-layering.md`。

## 9. 大库基准与热点索引（S14，2026-11-26）

> 口径：**没有测量就不加索引**。用 `test/perf` 的合成数据生成器（临时目录，测完即删）造大库，
> 逐一测 8 个场景的基线耗时，`EXPLAIN QUERY PLAN` 确认计划后只把提速 ≥20% 的索引落库。
> 数字是同一次运行内的前后对比（Apple Silicon 本机，绝对值受机器负载影响，只看相对变化）。
> 运行：`make perf`（= `go test -tags=perf ./test/perf -count=1 -v`），CI 不跑。

数据规模：memories 20,000（10% withdrawn）、observations 99,000（165 源 × 600 seq、每源 2 个序号洞）、
evidence_sources 165、evidence_archives 825、evidence_bundles 3,135（七成终态/含退避与租约）、
candidates 10,000（复核四态分布）、commit_decisions 20,000、distill_runs 500、审计日志 37.6 MB（当前档 + .1–.3）。
大库生成约 4s。

| # | 场景 | 基线 | 优化后 | 索引 | EXPLAIN |
|:---|:---|---:|---:|:---|:---|
| 1 | `mem stats --json`（Stats+Backlog 积压单快照） | 12ms | 12ms | 无 | 全程走既有索引，提速 <20% 不加 |
| 2 | `mem search`（memory 层三路召回） | 4–8ms | 4ms | 无 | FTS/bm25 已走 fts_mem |
| 3 | proposal 调度（NextRunnable/Open） | 1ms | 1ms | 无 | autoindex(source_id,generation) 已覆盖头查找 |
| 4 | `NextEvidenceBundle` 出窗（含越洞） | 8–9ms/次 | ≤1ms/次 | `ix_obs_source_seq` | `SCAN o` → SEARCH ix_obs_source_seq |
| 5 | `mem review ls` 四态 | **3,340ms** | **14ms** | `ix_commit_memory_actor` | `SCAN d2`（逐行全表扫裁决账）→ SEARCH |
| 6 | `mem eval flow`（13 检查全表拉取） | ~98ms | ~94ms | 无 | 全表拉取无 WHERE，索引帮不上 |
| 7 | audit `ReadHistory`（37.6 MB 四档合并） | ~600ms | ~550ms | 无 | 纯文件 I/O，不涉库 |
| 8 | `mem gc --retention` 库侧（Bundle 回收为主） | 163ms | 23ms | `ix_cand_evidence_evidence` + `ix_mem_evidence_evidence` + `ix_distill_bundle` | 引用保护三处 `SCAN` → SEARCH；GCCandidates 17ms / 决策账 1ms 不变 |

落地 5 条索引：`ix_commit_memory_actor`（commit_decisions）、`ix_cand_evidence_evidence`（candidate_evidence）、
`ix_mem_evidence_evidence`（memory_evidence）、`ix_distill_bundle`（distill_runs）、`ix_obs_source_seq`（observations）。
新库由 schema.sql 建（列随表原生的四条）；`ix_obs_source_seq` / `ix_distill_bundle` 的列是后加列，
只经 `ensurePerfIndexes`（store.go Open 时幂等补齐），存量库重开即得、二次 Open 幂等（用例 `TestPerfIndexesSurviveLegacyReopen`）。

**候而不加**：`ix_evbundle_status_created`（evidence_bundles(status, created_at)）在 3k Bundle 规模下
场景 1/3 提速 <20%（12ms→12ms / 1ms→1ms），写放大不值；Bundle 数涨两个数量级后再测。

**建议**（不在本片改，产品查询语句冻结）：
- 场景 5 的摘要外层仍要全量扫 `commit_decisions`（actor 无前导索引，子查询走新索引后只剩这一次 O(N) 扫描，14ms 可接受）；
  若将来裁决账到百万行，可考虑 `(actor, memory_id)` 前导索引或查询改写。
- 场景 7 是 JSONL 逐行解析的文件 I/O，索引无关；若嫌慢应改流式聚合，不是加索引。

**存量库风险**：`CREATE INDEX` 在 Open 时逐条执行，SQLite 短暂持写锁；按本机 10 万行观测建
`ix_obs_source_seq` 实测 <100ms，busy_timeout 5s 足够。并发打开的旧进程不受影响（WAL 读写不互斥）。
