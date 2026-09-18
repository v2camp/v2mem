# v2mem · 个人 Agent 记忆系统设计

> 项目名 `v2mem`，CLI 命令 `mem`。
> 目标：轻量、单文件、零服务、跨工程 / 跨工具 / 跨设备的个人记忆层。

## 0. 路径约定（关键：代码与数据分离）

| 用途 | 路径 | 理由 |
|:---|:---|:---|
| 代码 | `~/code/v2mem/` | 可 git 管理、可删可重建 |
| **数据** | `~/.v2mem/` | 个人长期资产，绝不能随代码目录被 `git clean` / 删库带走 |
| 主库 | `~/.v2mem/mem.db` | SQLite 单文件 |
| 配置 | `~/.v2mem/config.toml` | device_id、halflife、权重、embedding 开关 |
| 导出 | `~/.v2mem/sync/*.jsonl` | 跨设备归集用的事实日志（唯一可同步物） |

**纪律：活的 `mem.db` 永远不进任何同步目录（iCloud / Dropbox / Syncthing）。**

## 1. 核心洞察

记忆体系的设计取决于两件事：**怎么存** 与 **怎么用**。

- **怎么用**由 Harness 策略决定（Claude Code / WorkBuddy / Cursor 各自注入什么、何时注入），**我们无法控制**。
- 唯一可控的杠杆：**在「会话级」被注入的那个文件里放钩子**，告诉 LLM「还有多级记忆可以查询」。

因此采用 **两级机制**：

- **Level 1 — 项目记忆文件**：极小，每轮必注入，只承载「钩子 + 硬规则 + 当前任务态」。
- **Level 2 — 本地存储**：真正的事实仓库，按需检索，**不进上下文**。

## 2. 三层「存」→ 物理两层的映射

| 逻辑层 | 物理实现 | 说明 |
|:---|:---|:---|
| 会话级 | Level 1 注入文件 | 唯一被 Harness 保证在上下文里的东西 |
| 项目级 | Level 1 的 scope + Level 2 的 `project` 标记 | 文件按项目放；事实按 project 标记入库 |
| 设备级 | **Level 2 上的一次归并操作**，非独立物理层 | 导出 JSONL → 按 `content_hash` 归并，保留 `origin_device` 溯源 |

> 设备级不是新加一层数据库，而是一个 `merge` 子命令——这是本设计保持轻量的关键。

## 3. 存储底座

**主底座：SQLite 3.51 + FTS5 + sqlite-vec（可选）**

- 单文件、零服务、无 Docker、进程内；本机 sqlite3 3.51.0 可用。
- **FTS5**：BM25 关键词检索。对错误码、路径、变量名、缩写这类**必须精确匹配**的内容远强于向量。
- **sqlite-vec**（可选增强）：向量 KNN，一个可加载扩展，不引入第二个数据库。
- 融合用 **RRF（Reciprocal Rank Fusion, k=60）**，天然免疫「BM25 分值 0–25 vs 余弦 0–1」的量纲问题。

**v1 建议：检索只走 FTS5，零模型依赖。** 向量作为后续可选增强——这样 v1 不需要 embedding 模型，启动即最快。

**本地 embedding（M5 才需要）：用 ONNX Runtime——它是进程内库，不是服务，无需 Docker。**

- 安装即普通依赖：Python `pip install onnxruntime`；Node `npm i onnxruntime-node`。
- 模型就是一个 `.onnx` 文件（中文小模型 `bge-small-zh-v1.5` 约 90MB / 512 维），存 `~/.v2mem/models/`，首次运行下载一次。
- macOS Apple Silicon 可走 **CoreML Execution Provider**，硬件加速（Neural Engine / GPU）。
- 单句推理约 10–30ms（CPU），int8 量化更快；常驻内存约 200–400MB。
- **因此 embedding 不得进 `mem search` 的关键路径**：检索保持 FTS5（<10ms），向量只用于后台 `mem consolidate` 的相似归并。
- 易混淆点：需要 Docker 的是「模型服务器」（TEI / vLLM / Ollama）和「向量数据库」（Qdrant / Milvus）；ONNX Runtime 是**把模型直接嵌进进程**的那条路，正是反 Docker 的选项。

**图关系：先用 SQLite 递归 CTE，不引入图库。**

| 选项 | 结论 |
|:---|:---|
| SQLite 递归 CTE + `edges` 表 | ✅ 默认。零新依赖，1-hop 与简单遍历足够 |
| Kuzu | ⚠️ 正是「SQLite for graphs」（嵌入式 / Cypher / 内置向量+全文），但 **2025-10 已归档、被 Apple 收购、终版 0.11.3、仅余社区 fork**，不作为长期个人资产 |
| DuckDB + duckpgq | 备选：嵌入式单文件、SQL/PGQ；但列存偏 OLAP，小库不如 SQLite 自然 |

**决策规则**：查询多为 1-hop（"关于 X 我知道什么"）→ 递归 CTE 足够；真出现「A 经 C 到 B」的多跳且反复出现 → 再评估。

## 4. 数据模型

```sql
PRAGMA journal_mode = WAL;      -- 多工具并发读
PRAGMA busy_timeout = 5000;

CREATE TABLE memories (
  id             TEXT PRIMARY KEY,
  content        TEXT NOT NULL,          -- 原子事实，不是文档
  kind           TEXT NOT NULL,          -- preference|decision|pitfall|task|fact
  content_hash   TEXT NOT NULL,          -- 归一化哈希 → 相同知识覆盖
  project        TEXT,                   -- 工程标记（路径哈希）
  salience       REAL NOT NULL DEFAULT 0.5,
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL,
  last_seen_at   INTEGER NOT NULL,       -- 最近命中 → 衰减
  expires_at     INTEGER,                -- 硬 TTL（可空 = 不过期）
  superseded_by  TEXT REFERENCES memories(id),  -- 被谁取代 → 时序性
  origin_device  TEXT NOT NULL,
  origin_tool    TEXT,                   -- claude-code|workbuddy|cursor|cli
  access_count   INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX ux_mem_hash ON memories(content_hash, IFNULL(project,''));
CREATE INDEX ix_mem_project ON memories(project);
CREATE INDEX ix_mem_seen   ON memories(last_seen_at);

CREATE TABLE tags (                      -- 多机 / 多工具 / 多工程标记
  memory_id TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  key       TEXT NOT NULL,
  value     TEXT NOT NULL,
  PRIMARY KEY (memory_id, key, value)
);
CREATE INDEX ix_tags_kv ON tags(key, value);

CREATE TABLE edges (                     -- 图关系（递归 CTE 遍历）
  src TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  dst TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  rel TEXT NOT NULL,                     -- supersedes|related_to|merged_into|derived_from
  valid_from INTEGER,
  valid_to   INTEGER,
  PRIMARY KEY (src, dst, rel)
);

CREATE VIRTUAL TABLE fts_mem USING fts5(
  content, content='memories', content_rowid='rowid'
);
-- 可选增强（v2）：
-- CREATE VIRTUAL TABLE vec_mem USING vec0(embedding float[768]);
```

## 5. 四大机制

以下四条均已落地（M1–M5a）。标记 ✅ 为已实现，⏳ 为未实现。

| 机制 | 实现 | 状态 |
|:---|:---|:---|
| **相同知识覆盖** | `content_hash` 唯一索引（`(content_hash, project)`）。同一条命中即刷新 `updated_at`/`last_seen_at`、`access_count + 1`、`salience` 取 max、tags 取并集。零额外成本，写入期完成 | ✅ |
| **相似知识归并** | `mem consolidate`：字符 3-gram MinHash 估 Jaccard（**不是余弦，不用 embedding**），同 `project` 内聚簇，留 1 条、其余置 `superseded_by`，tags 转移给存活者 | ✅ |
| **过期机制** | 三种并存：① 硬 TTL（`expires_at`）② 久用衰减（`gc --max-idle` + `--min-salience`）③ 被取代（`superseded_by`，从检索中隐去但保留可追溯） | ✅ |
| **基于标记的归并** | 跨设备 JSONL 导入的字段级规则：tags 并集、`created_at` 取最早、`updated_at`/`last_seen_at` 取较晚、`access_count`/`salience` 取 max、`expires_at` NULL 优先。详见 §7.2 | ✅ |

**相似归并的准确性来自护栏而非阈值**（实现细节见 §11.10）。检索排序当前为
`bm25 ASC, salience DESC, last_seen_at DESC`；RRF 融合向量路排名留待 M5b ⏳。

## 6. 检索接口与 Level 1 钩子契约

**Level 1 文件硬约束：≤ 2KB / 40 行**，只放三样：硬规则、当前任务态、钩子。
多一行知识都是负债（当前 WorkBuddy 记忆 7800 字符被尾部截断，正是此病）。

```markdown
# MEMORY · Level 1（严格 ≤2KB）
## 硬规则（不得外置）
- ...
## 当前任务态
- ...
## 记忆钩子
你有一个本地二级记忆库（v2mem，<10ms）。遇到以下情况必须先查再答：
- 用户提到「之前 / 上次 / 我们说过」、或涉及偏好·决策·踩坑 → `mem search "<query>"`
- 产出有结论的结果 → `mem add --kind decision "<原子事实>"`
不要凭空回忆。若 `mem` 不可用，继续作答并说明未检索。
```

要点：**钩子必须写清「什么时候查」（触发条件），而非只说「你有记忆库」**；并给出 `mem` 缺失时的降级路径。

**Harness 原生钩子（已落地，见 §12）**：不再依赖「提醒模型记得查」——`mem init` 把
`SessionStart` / `UserPromptSubmit` 钩子写进各工具的原生配置，由 harness 在事件点执行
`mem hook`，确定性注入。文本钩子仍保留为可移植兜底（也是 WorkBuddy / TraeWork 这类
无原生钩子的工具的唯一接入方式）。


## 本文档定位

`DESIGN.md` 是设计索引页：只保留**最常被读**的基础决策（路径约定、核心洞察、存储、
数据模型、四大机制、检索契约），其余已按主题拆分到 `docs/design-*.md`，供在做相关
改动时按需阅读。每个专题页头部有指针，回到本索引。

## 专题地图（原 DESIGN 的章节去向）

| 原章节 | 主题 | 移入文件 |
|:---|:---|:---|
| §7 | 跨设备归集 | [`design-cross-device.md`](./design-cross-device.md) |
| §8 · §9 · §10 · §11.1–11.8 | CLI 设计与技术选型 | [`design-tech-arch.md`](./design-tech-arch.md) |
| §11.9 · §11.11 | 跨设备归集（已落地能力与 M4.1 修复） | [`design-cross-device.md`](./design-cross-device.md) |
| §11.10 · §13 | 相似归并（M5a）与模糊检索（M5b-0） | [`design-dedup-merge.md`](./design-dedup-merge.md) |
| §12 | Harness 十一工具集成（M6） | [`design-harness.md`](./design-harness.md) |
| §14 | 与 WorkBuddy 结合：缓解记忆压缩 | [`design-workbuddy.md`](./design-workbuddy.md) |
| §15 · §16 | 激活模型、评测机制、report 定位 | [`design-eval.md`](./design-eval.md) |
| §17 | 收尾运维三件套（uninstall/备份/轮转） | [`design-maintenance.md`](./design-maintenance.md) |
| 写侧治理 | 写必由 LLM 决策、短/跨会话记忆分配、bulk 打标 | [`design-write-governance.md`](./design-write-governance.md) |

> 正文里若出现「见 §12.x」这类跨章节引用，按上表定位文件即可。

## 阅读建议

- 第一次读 / 想了解全局：读本节下列基础章节 + `design-*.md` 的标题即可。
- 改检索：`design-dedup-merge.md`；改钩子：`design-harness.md`；改评测：
  `design-eval.md`；改生命周期维护：`design-maintenance.md`。
