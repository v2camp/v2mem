# 存储分层：运转逻辑留库，知识内容外置

> 状态：**方案已拍板，P0 契约已落地**（§8 五项决策见下；生产读侧与存量数据未改）。
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

「库大拖慢启动」不成立，别拿它当理由。另以临时基准库（0.24 MB / 1 千行 与 93.5 MB / 40 万行，测完已清理）对照：`connect()` + 读 schema 0.244 → 0.209 ms、索引点查 0.008 → 0.007 ms，只有 `PRAGMA quick_check` 0.4 → 45.4 ms 随体积线性增长。本次改造的真实收益是**主库体积、备份 / 同步成本、内容可读性与保留策略**，不是启动速度。

## 4. 表级映射

| 表 | 留库（状态 / 约束 / 引用） | 外置（内容） |
|:---|:---|:---|
| `observations` | `id` / `event_uid` / `source_id` / `source_seq` / `content_hash` / `archive_hash` / `raw_record_hash` / `raw_ref` / `channel` / `event` / `created_at` / `occurred_at_ns` / `parser_version` | `summary` → JSONL；`summary_idx` → 外部索引 sidecar |
| `evidence_archives` | **整表**（账与引用） | 正文已在 gzip JSONL，无需再动 |
| `evidence_sources` / `memories` / `candidates` / `commit_decisions` / `distill_runs` / `edges` / `hook_dedup` / `db_meta` / `migration_marks` / `tags` | 整表 | — |
| `read_traces` / `read_trace_items` | `feedback` / `feedback_reason` / `feedback_at`、判据依赖的计数列 | `raw_query` / `focus_query` / `gate_reason` / `signals_json` 等只写不改的诊断字段 → JSONL（P2，须先确认 `ops-watch.md` 判据依赖） |
| `fts_mem_*` | 整组保留 | — |
| `fts_obs_*` | 待 §8 第 3 点决策 | 外部检索 sidecar 或精简索引 |
| `memories.content` | **留库**（查询源；仅 18 行 / 0.03 MB） | 单向投影到 Markdown |

`memories` 体量极小，外置正文没有收益；Markdown 只做给人 / 给其他 AI 工具读的投影。

## 5. 契约（正文见 [`contracts/storage-layering.md`](../contracts/storage-layering.md)）

### 5.1 ref 格式与不可变性

`<path>#<seq>`（如 `content/2026/11/23/observations.jsonl.gz#2`）+ 库内单列内容 `sha256`。不用 `#L<seq>` 变体，避免与现有解析分叉；不带 `#seq` 仅兼容读取首行，新写入必须带。分片当日关闭后**不可变**，引用才稳定；活跃分片只 append，禁止重写或改名历史分片。复用 `segmentRecord` 定位与 `ReadArchiveRecord`，不新造格式。

### 5.2 写入顺序：分两类，方向相反

- **引用型内容**：内容先落盘 + 一次 fsync → DB 事务写 ref / 账 / 状态。DB 失败保留 segment 并写 spool 回放；孤儿内容无害，交 GC。
- **派生型审计日志**：DB 状态事务先提交 → 再 append 日志（如 `audit.jsonl`）。日志丢了不影响业务。

两类的顺序相反，判据是「内容是不是真源的一部分」。契约里必须分开写，不能统一成一条规则。

### 5.3 阈值

内容 < 2 KB 内联库，≥ 2 KB 外置；加 `payload_bytes` 记录决策。依据：`summary` 均 449 B、`summary_idx` 均 431 B（内联侧），归档 Raw 上限 32000 rune（外置侧）。避免为一句短摘要也建一个文件和一个 ref。

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
| **P1 写侧外置** | `observations.summary` 外置到 `content/`，DB 留 ref；复用批量归档与 spool 补偿；按 `#seq` 补读取 API | 变体 B 口径：库约 19 MB；`ops-watch` 判据不降级；重放不新增行、哈希稳定 |
| **P2 检索与投影** | `summary_idx` / `fts_obs_*` 的替代方案；Markdown 蒸馏；`read_traces` 诊断字段外置 | 变体 C 口径约 14.5 MB；`mem search --layer observation` 通过既有评测；跨工具可读记忆 |

**非目标**：不删观测行或归档来降耗时（[`design-write-batch.md`](./design-write-batch.md) 已明确）、不改 `mem.db` 的查询源地位、不引入常驻进程（[`VALUE.md`](./VALUE.md) §4 红线）、不改 `kind` 枚举。

## 8. 决策（2026-11-23 拍板）

| 点 | 决议 | 影响 |
|:---|:---|:---|
| ref 定位 | **路径 + `#seq`**；内容 hash 存库内单列 | 复用现有解析与 `ReadArchiveRecord`；分片迁移需同步改 ref |
| 阈值 | **2 KB 统一阈值** + `payload_bytes` 留痕 | 不为字段开特例；现有摘要多为内联 |
| `fts_obs_*` | **保留精简索引** | 收益停在约 36%；外部 sidecar 留到 P2 再评估 |
| Markdown | **单向投影、不回写** | 不做 `source_ref` / `version` 导入协议 |
| `read_traces` | **诊断字段 P2 再拆** | 拆前先核对 `ops-watch.md` 判据依赖 |

> 五项已对齐，P0 按 AGENTS.md 在独立 worktree 实施；契约正文见 `contracts/storage-layering.md`。
