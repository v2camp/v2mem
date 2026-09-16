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
  kind           TEXT NOT NULL,          -- preference|decision|pitfall|task|reference
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

## 7. 跨设备归集

```
设备 A: mem export ~/.v2mem/sync/A-<ts>.jsonl
        （同步该文件是安全的：纯文本、行式、可读）
设备 B: mem import ~/.v2mem/sync/A-<ts>.jsonl
```

**禁止**：同步 `mem.db` 本体（写入中途同步会损坏库）。

### 7.1 身份键 = `(content_hash, project)`，不是 `id`

`id` 由 `newID()` 随机生成，两台设备独立写下同一事实必然得到不同 id；而唯一索引 `ux_mem_hash`（`content_hash, project`）只允许一条。**按 id 归并必然漏合**，导入还可能撞唯一索引。因此：

- 导入时忽略远端 `id`，只按 `(content_hash, project)` 定位
- 本地不存在 → 插入并生成**本地** id，`origin_device` 记远端设备（溯源）
- 同一事实落在不同工程 → 视为两条独立记忆，不互相归并

### 7.2 字段级归并规则

全部规则选定为**幂等且可交换**，因此「重复导入」与「双向导入」都收敛。

| 字段 | 规则 | 理由 |
|:---|:---|:---|
| `kind` | 取字典序较小者 | 双方都非空时的确定性让步 |
| `salience` | 取较大者 | 更重要的判断占优 |
| `access_count` | 取较大者 | **不是求和**——求和会随重复导入无限增长，破坏幂等 |
| `created_at` | 取最早者 | 最早的创建时间最接近真相 |
| `updated_at` / `last_seen_at` | 取较晚者 | 最新变动 / 最新命中 |
| `expires_at` | 任一边为 NULL（永不过期）则结果为 NULL，否则取较晚者 | 同步不得**提前**销毁记忆 |
| `tags` | 并集（一键多值全部保留） | 标记是累积的 |
| `origin_device` / `origin_tool` | 仅新插入时写入；已有记录保留本地值 | 本地视角溯源，见 7.3 |
| `superseded_by` | **不在第一遍归并中处理**，由第二遍按身份键重建，见 7.5 | 防悬挂引用 |

一条**刻意的例外**（本地书写形式优先，非可交换，已在文档中固化）：

- `content`：hash 相同即视为同一事实（归一化已折叠大小写与空白），保留本地写法，不重算 `content_idx`

### 7.3 一条已知边界（有意为之，非遗漏）

**`origin_device` 是「首次写入本库的设备」，不随归并收敛。**
共享事实在 A 库记 `mini`、在 B 库记 `laptop`，两边都对——这是本地视角字段。
收敛判据因此排除 `id` 与 `origin_device`，只对内容相关字段断言一致。

### 7.4 导出可复现

`Export()` 按 `(content_hash, project)` 排序，标记按 `(key, value)` 排序。
否则每次导出的 JSONL 都会无谓 diff，跨设备比对失去意义。

### 7.5 取代关系的跨设备重建（M4.1）

**问题**：`superseded_by` 存的是本地随机 id。直接搬过去就是**悬挂引用**；完全不搬，
其他设备就不知道取代关系，会重新看到两条重复 —— 实测在 A 端归并、B 端独立写下同一事实后，
两侧取代关系不一致，**M4 承诺的归集在 M5a 加入后失效**。

**解法**：把取代关系在传输层表达成**身份键**，导入端再解析回本地 id。

```jsonc
// 导出记录新增两个字段
"superseded_by_id": "<本地 id>",                        // 仅供排查，不作为依据
"superseded": { "hash": "<存活者 content_hash>", "project": "<存活者 project>" }
```

导出端用 `LEFT JOIN memories t ON t.id = m.superseded_by` 把本地 id 翻译成身份键。

导入端**必须分两遍**：

1. 第一遍做常规字段归并，并记下「本批记录的身份键 → 本地 id」
2. 第二遍解析取代引用：优先查本批索引，其次回落到库中已有记录（存活者可能不在本文件内）

分两遍的必要性：导出按 `content_hash` 排序，被取代者可能排在存活者**之前**，
一遍扫描会解析不到目标（已用倒序文件固化该断言）。

**解析失败时**（目标既不在文件里也不在本地）：留空并计入 `ImportStats.Unresolved`，
在 CLI 输出里显式报告。宁可缺失也不写悬挂 id。自引用同样被拒并计入 `Unresolved`。

**收敛判据因此收紧为两项**：内容投影一致 **且** 取代关系（按身份键展开）一致。
本地 id 天然不同，不参与比较。

**仍未实现**：`edges` 表的跨设备重映射。当前无写入路径（相似归并不建边），故无实际影响；
一旦 M5b 或后续引入边关系，需按同样方式处理。

## 8. CLI 设计（`mem`）

```
mem add      --kind <k> [--project P | --global] [--tag k=v]... "<原子事实>"
mem search   "<query>" [--project P] [--scope current|global|all] [--tag k=v]... [--limit N] [--json]
mem touch    <id>                  # 命中反馈，刷新 last_seen_at / access_count
mem forget   <id>                  # 显式删除
mem gc                             # 过期清理 + 衰减淘汰
mem consolidate [--threshold 0.7]  # 相似归并：近重复聚簇，留 1 条、其余置 superseded_by
mem export   [out.jsonl]
mem import   <in.jsonl>            # 跨设备归并
mem hook     [--event e] [--harness h] [--limit n] [--max-chars n]
                                   # harness 钩子入口：读 stdin JSON，注入内容写 stdout
mem init     --harness <名字> [--project <目录>] [--scope ...] [--file <路径>] [--dry-run]
                                   # 把钩子写进该工具的配置（合并、幂等、带备份）
mem harness  [--json]              # 列出各工具的接入方式、等级与本机检测结果（● = 已检测到）
mem ingest   <文件.md...>          # 把既有 md 的条目机械化搬进库（幂等；跳过标题/围栏/纯指引；合并缩进续行）
mem budget   --file <文件>         # Level 1 体量守卫：超预算返回非零退出码，供自动化告警
mem stats                          # 条数 / 分布 / 库大小
```

选项位置：Go 的 `flag` 要求**选项写在子命令之后**（`mem add --db X`，不是 `mem --db X add`）。

`--json` 输出供 LLM 解析；默认输出为紧凑纯文本，省 token。
`--scope`：`current` = 当前工程 + 全局；`global` = 只要全局；`all`/省略 = 跨工程（默认）。
`--global` 与 `--project` **互斥**——这是唯一能写入「空工程」记忆的入口，缺了它全局记忆在 CLI 上不可达。
`mem hook` 的 stdout 只放注入内容、诊断走 stderr，且**任何异常都返回 0**（见 §12.4）。

## 9. 已知坑

1. **绝不把 `mem.db` 放进同步目录**——写入中途同步会损坏库。只同步 JSONL。
2. **写入路径要便宜**：写入期只做 hash 去重 + FTS5 索引（微秒级）；embedding 与相似归并放后台。
3. **原子化是检索质量的前提**：实测 340 字符聚焦片段的相似度，比含同样信息的 1.5KB 多主题文档高 **2.3 倍**。库里存**原子事实**，绝不存文档正文。
4. **钩子依赖 LLM 真的去调**：保留少量热事实在 Level 1 做降级；命令要快到「投机性检索也不心疼」。
5. **多进程并发**：开 WAL + `busy_timeout`。

## 10. 里程碑

- [x] M1 骨架：库 + schema + `add` / `search`（FTS5 单路）+ `stats`
- [x] M2 生命周期：`touch` / `gc`（TTL + 衰减）+ `forget`
- [x] M3 标记与工程维度：`--project` / `--global` / `--tag` / `--scope` 全链路
- [x] M4 跨设备：`export` / `import` 归并 + 溯源
- [x] **M4.1 取代关系跨设备重建**：取代引用改为身份键表达，导入端两遍解析（§7.5）
- [x] **M5a 相似归并（纯 Go，无模型）**：字符 n-gram MinHash + 四条护栏 + `consolidate`
- [ ] **M5b-0 模糊检索（建议先做）**：把已有 MinHash 签名接到检索 + RRF 融合，零包体成本（§13.4）
- [ ] M5b-1 向量增强（暂缓）：sqlite-vec 的 Go 绑定当前不可用，见 §13.2
- [x] **M6 Harness 集成**：`mem hook` 统一事件入口 + `mem init` 扫描选择与配置生成（11 个工具入口，见 §12）
- [x] **M6.1 Level 1 运维**：`mem ingest` 机械化搬运 + `mem budget` 体量守卫；AGENTS.md 已接入（见 §14）
- [x] **M7 度量与评测**：`mem audit` 埋点 + `mem eval recall|write`；口径红线与迁移判据见 §15
- [x] **M7.1 召回退化修复**：长问句词组内 AND 导致 0 命中 → 精确优先、空手才放宽（§15.5）

> M5 拆成两半是本次实现中的结论：相似归并属**近重复检测**（经典算法问题），
> MinHash 几十行、微秒级、零依赖即可胜任，不必引入模型与 CGO。
> 于是 M5b 的向量化从「相似归并的前置」降级为「检索召回的另一路」。

## 11. 技术选型（已定：Go）

### 11.1 为什么是 Go

| 候选 | 结论 |
|:---|:---|
| **Go** | ✅ **选定**。默认构建全链路无 CGO；交叉编译内建（`GOOS/GOARCH`）；纯 Go SQLite 驱动；编译秒级 |
| Rust | 备选。ONNX 侧更优（`tokenizers` 是 HF 官方 Rust 实现、`ort` 可静态链入），但 `rusqlite` 的 `bundled` 构建需 `cc`，交叉编译要 `cargo-zigbuild`/`cross`，迭代成本高。纯 Rust 的 Turso/Limbo 仍是 BETA、FTS 实验性，**不能承载长期数据** |
| Python | 否。ONNX/embedding 生态最顺，但 M1–M4 根本不需要模型；需解释器+环境，启动约 50ms |
| Node/TS | 否。同 Python，需 node + node_modules |

**决策判据**：M5（语义 embedding）是**可选**而非必选——相似归并靠 MinHash/SimHash 即可，不需要模型。既然核心路径无 ML，Go 的「无运行时 + 内建交叉编译 + 秒级构建」就成为决定性优势。

### 11.2 依赖清单（当前直接依赖仅 1 个）

| 用途 | 选型 | 需 CGO |
|:---|:---|:---|
| SQLite 驱动 | `modernc.org/sqlite`（SQLite C 转译为 Go） | 否 |
| 全文检索 | SQLite FTS5（内建）+ 自建中文索引 | 否 |
| CLI | 标准库 `flag` | 否 |

> 采用 `modernc.org/sqlite` 而非更高层的 `gosqlite.org`：后者同样基于 modernc 且提供更完整的 typed FTS5/vec/**RRF 融合**，但更晚出现。M1 只需基础 FTS5；待 M5 需要 RRF 与向量时再升级到 `gosqlite.org`，届时是同一引擎，迁移成本低。

### 11.3 中文检索方案（关键决策）

FTS5 自带分词器都不适用于中文，本机实测（sqlite3 3.51）：

| 分词器 | 记忆库(3字) | 记忆(2字) | 目录(2字) |
|:---|:---|:---|:---|
| `unicode61`（默认） | ✗ | ✗ | ✓ 仅整串命中 |
| `trigram` | ✓ | ✗ | ✗ 有 3 字符下限 |

**方案**：写入时由 Go 侧把连续汉字展开为「一元组 + 二元组」存入 `content_idx`，FTS5 只索引该列；检索时编译为 FTS5 表达式——**词组内部 AND（精确）、词组之间 OR（召回优先）**，按 bm25 排序。零额外依赖，且支持单字与二字词查询。

### 11.4 构建与分发

```bash
make build       # CGO_ENABLED=0 → bin/mem（arm64，约 10MB）
make install     # 安装到 ~/go/bin/mem
make build-linux # 交叉编译 linux/amd64，静态链接
make build-onnx  # M5 可选分支：-tags local_onnx，需 CGO + libonnxruntime
```

- macOS 产物仅依赖系统 `libSystem` / `libresolv`（Go 在 macOS 的固有行为，**非 CGO 依赖**）；Linux 产物为完全静态链接。
- 需把 `~/go/bin` 加入 PATH：
  `echo 'export PATH=$HOME/go/bin:$PATH' >> ~/.zshrc`

### 11.5 环境配置（已做）

- Go **1.27.1**（brew 安装）
- `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=sum.golang.google.cn`（`proxy.golang.org` 在本机网络不可达，已用 `go env -w` 持久化）

### 11.6 M1 已完成能力

- `mem add`：原子事实写入；**相同知识覆盖**（归一化 hash，忽略大小写/空白/句尾标点差异）；工程名默认取当前 git 仓库根；支持 `--kind/--project/--tag/--tool/--device/--salience/--ttl`
- `mem search`：FTS5 + bm25；支持中文（含二字词）、英文、混合；`--limit/--project/--kind` 过滤；已过期自动排除
- `mem stats`：条数 / 类型分布 / 工程分布 / 待回收 / 库大小
- 全局：`--db`（默认 `~/.v2mem/mem.db`）、`--json`（供 LLM 解析）
- 并发：`journal_mode=WAL` + `busy_timeout=5000`（已验证）

### 11.7 M2 已完成能力（生命周期闭环）

- `mem touch <id|前缀>`：命中反馈，刷新 `last_seen_at` + 累加 `access_count`；支持 8 位短 ID 前缀，**歧义前缀报错**而非静默选第一条
- `mem forget <id|前缀>`：显式删除，`tags` 经外键 `ON DELETE CASCADE` 级联清理
- `mem gc [--max-idle 720h] [--min-salience 0.2]`：回收两类——TTL 到期（`expired`）、久未命中且低重要性（`decayed`）

**淘汰判据**：`last_seen_at < now - maxIdle AND salience < minSalience`。
关键取舍：**高 salience 的记忆即使久未命中也保留**——重要性应当能抵御遗忘，否则重要但不常用的知识会被误删。

**测试**：TDD 编写（先写失败测试再实现），`go test ./...` 全绿，共 **12 个用例**：

| 范围 | 用例 |
|:---|:---|
| M1 回归 | 相同内容覆盖不重复、中文二字词检索 |
| touch | 刷新 last_seen + 累加计数、支持前缀、歧义前缀报错 |
| forget | 删除记忆并级联删除 tags（CLI 端到端：forget 后检索不到） |
| gc | 回收 TTL 到期、淘汰久未命中低重要性、保留高 salience、保留近期命中（CLI 端到端：unknown ID 报错） |

端到端已验证 `gc` 的 TTL 路径真实执行（`expired=1`），非静默空转。

### 11.8 M3 已完成能力（标记与工程维度）

- `mem add --tag k=v`（可重复）：写入多机 / 多工具 / 多工程标记；同内容覆盖时 **tags 取并集**而非替换（`INSERT OR IGNORE`），避免覆盖丢掉历史标记
- `mem add --global`：写入**空工程**的全局记忆，对所有工程生效。与 `--project` 互斥
- `mem search --tag k=v`（可重复）：**AND 语义**，用 `EXISTS` 子查询逐条断言，避免 JOIN 造成的结果放大
- `mem search --scope current|global|all`：
  - `current` = 当前工程 + 全局（`project = ? OR project = ''`）
  - `global` = 只要全局（`project = ''`）
  - `all` / 省略 = 跨工程（默认）
  - `current` 未显式给 `--project` 时按当前 git 仓库根名自动推断

**设计要点**：`--global` 是唯一能写入空工程记忆的入口。若只有 `--project`，空值会被 `detectProject()` 自动填充，导致库层的全局语义（`project = ''` 对所有工程生效）**在 CLI 上不可达**——即「能力存在但无路径抵达」的静默失效。工程名同时用于 `stats` 的按工程分布。

**测试**：共 **23 个用例**（`go test ./... -v` 实测：store 16 + CLI 7；M2 时点为 12，M3 新增 11）：

| 范围 | 用例 |
|:---|:---|
| 标记过滤 | 单标记命中、多标记 AND 语义（存在+不存在必空）、同内容覆盖时 tags 取并集 |
| 工程作用域 | `current` 含本工程与全局、`current` 排除他工程、默认跨工程返回全部、`global` 只要全局 |
| 全局记忆 | `--global` 写入空工程标记、`--global` 与 `--project` 冲突报错且拒绝写入 |
| CLI 端到端 | `--tag` flag 生效、`--scope current` 缩窄到本工程、`--scope global` 排除工程记忆 |

**e2e 验证矩阵**（临时库，4 条数据覆盖 alpha / beta / 全局 / 双标记）：

| 查询 | 期望 | 结果 |
|:---|:---|:---|
| 默认 all | 4 | 4 ✅ |
| `--scope current`（本工程无记忆） | 1（全局） | 1 ✅ |
| `--scope global` | 1 | 1 ✅ |
| `--project alpha` | 2 | 2 ✅ |
| `--tag machine=laptop` | 1 | 1 ✅ |
| `--tag machine=mini --project beta` | 1 | 1 ✅ |
| `--project alpha --scope current` | 3（alpha 2 + 全局 1） | 3 ✅ |
| 负对照：不存在的 tag / 存在+不存在 tag / 不存在的工程 | 0 / 0 / 0 | 0 / 0 / 0 ✅ |

> 度量教训：首版 e2e 用 `wc -l` 计数，而空结果会打印一行 `（无结果）`，导致负对照恒为 1 —— **负对照抓出的是度量工具失准，不是代码缺陷**。最终改用 `--json` + 长度统计，并先自检仪器（`--limit 1` 应得 1）。

### 11.9 M4 已完成能力（跨设备归集）

- `mem export [路径.jsonl]`：全部记忆导出为 JSONL（省略路径则写标准输出）；给路径时把摘要打到标准输出
- `mem import <路径.jsonl>`：按 `(content_hash, project)` 归并，报告 `新增/合并/跳过`
- 归并规则与边界见 §7.2 / §7.3

**测试**：共 **39 个用例**（`go test ./... -v` 实测；M3 时点为 23，M4 新增 16）：

| 范围 | 用例 |
|:---|:---|
| 导出 | 每条记忆一行且必带 `content_hash` / `tags` / `origin_device`；两次导出完全一致（可复现）；省略路径走标准输出 |
| 导入 · 基础 | 新记录插入并保留远端 `origin_device`；按 hash+project 归并而非按 id 新插；同事实跨工程保持独立 |
| 导入 · 幂等 | 重复导入不改变状态、不产生重复行；`access_count` 取 max 不随重复导入增长；CLI 二次导入报告 `新增=0 合并=1` |
| 导入 · 字段规则 | 标记取并集；`created_at` 取最早、`last_seen_at` 取较晚；`expires_at` NULL 优先（永不过期）与取较晚者两路 |
| 导入 · 收敛 | 双向导入后两侧内容投影完全一致；`origin_device` 保持本地视角（固化 7.3 边界） |

**变异测试（验证断言真有咬合力）**：把 `access_count = MAX(...)` 改成求和后重跑。

- `TestImportTakesMaxAccessCountNotSum` 报警（`got 31`）✅
- `TestImportIsIdempotent` **首次未报警** ❌ —— 该用例两侧计数均为 0，求和与取 max 结果相同，属巧合性通过。
  已修：给两侧预设非零 `access_count` 与 `last_seen_at`，再变异时该用例正确报警。
  **教训**：幂等类断言必须让被测字段取非零值，否则「恒等」会掩盖实现错误。

**e2e 双设备仿真**（A=mini / B=laptop，各 2 条含 1 条共享事实）：

| 判据 | 结果 |
|:---|:---|
| 交叉导入后两侧内容投影一致（排除 `id` / `origin_device`） | 3 条 vs 3 条，完全收敛 ✅ |
| 共享事实字段归并：`salience`、`tags`、`kind` | `0.9`、`{machine:[laptop,mini]}`、`fact` ✅（取 max / 并集） |
| 重复导入后导出逐字节不变 | ✅ |
| 连续 5 次重复导入后状态稳定 | ✅ |
| `origin_device` 本地视角（A 见 `mini`、B 见 `laptop`） | 符合 7.3 设计 ✅ |

> 度量教训（第二次）：首次 e2e 把「导入**前**的导出」与「导入**后**的导出」做字节 diff，误报为幂等失败 —— 对照组本身就不该相等。**幂等必须用「同一状态下的前后对照」，不能拿跨越状态变更的两个快照比。**

### 11.10 M5a 相似归并：为什么单靠阈值不够

用字符 3-gram + MinHash 估 Jaccard（`internal/similarity`）。选择这条路而非 embedding，是为守住
「零 CGO / 零外部服务 / 单文件分发」：近重复检测是经典算法问题，不是语义理解问题。

#### 实测校准矩阵（2026-09-17，阈值 0.7）

| 样本 | 相似度 | 判定 |
|:---|:---|:---|
| 完全相同 | 1.000 | 归并 |
| 仅差尾字（目录 / 目录下） | 0.984 | 归并 |
| 同义改写（SQLite / sqlite 库） | 0.906 | 归并 |
| 补一句（…目录 → …目录，方便统一回收） | 0.781 | 归并 |
| 差两字（目录 / 路径） | 0.750 | 归并 |
| **🔴 数字反转（`CGO_ENABLED=0` / `=1`）** | **0.906** | **拒绝**（护栏拦下） |
| 同主题不同细节 | 0.406 | 不归并 |
| 短句差一字（同步目录 / 共享目录） | 0.078 | 不归并 |
| 负对照：主题无关 / 结构相近 / 完全无关 | 0.000 / 0.031 / 0.000 | 不归并 |

两个致命问题，都不是靠调阈值能解决的：

1. **`CGO_ENABLED=0` 与 `=1` 相似度高达 0.906。** 字符 n-gram 对「一个字符导致语义反转」完全无感。
   若只卡阈值 0.8，两条**互斥的规则**会被合成一条——比漏归并危险得多。
2. **短文本的估计方差不可接受。** 「库不放同步目录」vs「库不放共享目录」只差一字，相似度仅 0.078
   （shingle 集太小）。短文本几乎永远够不到阈值；这既是保护也是缺陷。

#### 归并裁定 = 阈值（提召回）+ 四条护栏（保精度）

| 护栏 | 规则 | 理由 |
|:---|:---|:---|
| 短文本 | 两侧 shingle 数均 ≥ 8（约 10 字符） | 估计方差过大时不做统计推断；完全相同由 hash 覆盖机制负责 |
| 长度比 | 长/短 ≤ 3 | 3-gram 覆盖率会让「一句话被长句包含」也拿到不低的分 |
| 数字 | 抽出的数字串**同序同内容** | 端口 6379/6380、开关 0/1、版本 v2mem/v3mem 都属语义反转 |
| 否定 | 否定词计数相同 | 「可以」与「不可以」必须区分 |

数字与否定只做**计数/集合比较**，不做语义分析——刻意保守：漏归并只是少省一点空间，
误归并会让两条互斥规则合并成错的。

拒绝理由枚举（`similarity.Reason*`）：`too-short` / `length-ratio` / `digits-differ` /
`negation-differs` / `below-threshold`。测试逐条断言理由，保证测到的确实是目标分支。

#### 存活者与幂等

存活者规则必须完全确定（两台设备独立运行也要得到同一结论）：
`salience` 降序 → `access_count` 降序 → `created_at` 升序 → `id` 升序。

幂等来源：被取代者不再进入下一轮的比较范围（查询条件含 `superseded_by IS NULL`），
故重复运行 `取代=0`。归并**不删行**，被取代者保留可追溯，并由 `export` 带走以让其他设备知晓取代关系。

#### 测试（M5a 新增 28 个用例）

| 范围 | 用例 |
|:---|:---|
| similarity 基础 | 完全相同 = 1.0、近重复高分、无关低分、确定性、签名可复现、空串/单字边界、有界且对称 |
| 校准矩阵 | 上表 11 个样本的相似度区间 + 归并裁定，任何调参漂移都会报警 |
| 护栏 | 数字不同拒绝 / 数字一致允许 / 否定反转拒绝 / 长度悬殊拒绝 / 过短拒绝 / 阈值不足拒绝 / 标识符数字参与比较 |
| store 归并 | 高 salience 存活并写 `superseded_by`、取代后从检索消失、幂等、跨工程不归并、**数字反转拒绝（护栏接线验证）**、阈值生效、平手确定性、标记转移、三簇成员、忽略已过期 |
| CLI | 归并并入 + 检索隐藏、`--threshold` 生效 |

**变异测试（验证接线，不只是包内自测）**：

| 变异 | 捕获用例 |
|:---|:---|
| 去掉 `Search` 的 `m.superseded_by IS NULL` | `TestSupersededMemoriesAreExcludedFromSearch`（`got 2`）✅ |
| 护栏跳过数字检查 | `TestConsolidateRefusesDigitReversal` + `TestCalibrationMatrix` + `TestJudgeRejectsWhenDigitsDiffer` ✅ |

变异 B 的输出具体证实了风险：无护栏时 `CGO_ENABLED=0` 与 `=1` 以 **0.8125** 相似度被合并。

**e2e**（7 条播种：3 条近重复 + 1 对危险对 + 1 条无关 + 1 条跨工程同句）：

| 判据 | 结果 |
|:---|:---|
| 3 条近重复 → 1 簇取代 2 条，存活者为最高 salience | `簇=1 取代=2`，存活 0.9 那条 ✅ |
| 危险对 0/1 双双保留 | 2 条均可见 ✅ |
| 跨工程同句各自独立 | 2 条命中 ✅ |
| 幂等 | 二次运行 `扫描=5 取代=0`（扫描数已排除被取代的 2 条）✅ |
| `stats` 口径 | `总条数 7 / 检索可见 5 / 已归并 2` ✅ |

**e2e 失败的一个案例值得记录**：`simBase`/`simVariant` 这组测试数据在构造时先后被数字护栏
（`~/.v2mem` 里的 2）与短文本护栏（8 字 → 6 个 shingle）拦下，测到的是别的分支而非阈值分支。
**多护栏判定链的测试必须逐条控制前面所有护栏的输入条件**，否则「测试通过」不代表目标逻辑被覆盖。

### 11.11 M4.1 验收时发现的缺口与修复

M5a 落地后重跑 M4 的收敛判据，出现 `A 7 条 / B 7 条 -> !! 未收敛`。
根因不是 M4 的实现错误，而是 §7.3 里「记录在案」的那条限制**已被 M5a 激活**：
M4 时代没有 `superseded_by`，故限制无影响；M5a 一旦产生取代关系，归集承诺就破了。

**教训**：把限制「记录在案」不等于它安全。**引入新机制时必须回头复跑依赖它的旧判据** ——
这里正是靠重跑 M4 的收敛判据才发现。仅看 M5a 自己的测试全绿，会漏掉这个跨模块回归。

修复见 §7.5，新增 6 个用例：

| 用例 | 断言 |
|:---|:---|
| `TestExportCarriesSupersessionAsIdentityKey` | 导出携带身份键引用而非裸 id |
| `TestImportRebuildsSupersessionWithLocalIDs` | 导入解析成本地 id，且**不照搬远端 id**、目标在本地确实存在 |
| `TestImportResolvesSupersessionRegardlessOfFileOrder` | 倒序文件也能正确重建（验证「分两遍」的必要性） |
| `TestImportReportsUnresolvedSupersession` | 目标缺失时报 `Unresolved` 且不留悬挂 id |
| `TestImportKeepsAlreadySupersededRecordSuperseded` | 导入自身导出不改变取代关系，检索仍隐去被取代者 |
| `TestCrossDeviceConvergenceWithSupersession` | 一端归并 + 另一端独立写同一事实，交叉导入后内容投影与取代关系**双双收敛** |

**变异测试**：跳过第二遍（`if true { continue }`）后被 4 个用例捕获（`got ""` / `Unresolved:0`）。

**另修一处自查发现的缺陷**：`identity` 索引最初用原始 `r.ContentHash`，
而插入时若文件缺 `content_hash` 会用 `HashOf(content)` 兜底 —— 两者不一致会让索引建出对不上的键。
当时靠 DB 回落掩盖了问题，改用归一化后的 `h` 作键。

**最终 e2e**（A 端归并 1 条 → B 端独立写同一事实 → 交叉导入）：

| 判据 | 结果 |
|:---|:---|
| 内容投影 | A 4 条 / B 4 条，一致 ✅ |
| 取代关系（按身份键） | 两侧均为 `6162dd51/v2mem -> ee30b73c/v2mem` ✅ |
| 检索可见性 | 两侧 `总4 / 可见3 / 已归并1`，检索「隐藏目录」各 1 条 ✅ |
| 幂等 | 再各导入一次 `新增=0`，取代关系不变 ✅ |

## 12. Harness 集成（M6）

目标：把 Level 2 接进用户实际在用的工具，且**用能确定性执行的方式**——不靠「提醒模型记得查」。

### 12.1 事实核实表（2026-09-17 逐家核对官方文档）

不做推测性填写：配置写进不生效的路径是**静默失败**，比不写更糟。

| 工具 | 接入方式 | 事件名 | 项目目录来源 | 用户级配置 | 官方文档 |
|:---|:---|:---|:---|:---|:---|
| Claude Code | native | 同名 6+ 事件 | `CLAUDE_PROJECT_DIR` | `~/.claude/settings.json` | docs.claude.com/…/hooks |
| TraeCode（Trae IDE / SOLO） | native | SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop / Notification | `TRAE_PROJECT_DIR`、`CLAUDE_PROJECT_DIR` | `~/.trae-cn/hooks.json` | docs.trae.cn/ide_hook-configuration-reference |
| CodeBuddy Code | native | 27+ 事件 | `CODEBUDDY_PROJECT_DIR` | `~/.codebuddy/settings.json` | codebuddy.cn/docs/cli/hooks |
| Qoder（IDE / JB / CLI） | native | 12 事件 | `QODER_PROJECT_DIR` | `~/.qoder/settings.json` | docs.qoder.com/en/cli/hooks |
| Qoder CN | native | 同上 | `QODERCN_PROJECT_DIR` | `~/.qoder-cn/settings.json` | help.aliyun.com/zh/lingma/hook |
| QoderWork | native | SessionStart / SessionEnd / UserPromptSubmit / PreToolUse / PostToolUse … | **无（只走 stdin `cwd`）** | `~/.qoderwork/settings.json`（**仅用户级**） | docs.qoder.com/zh/qoderwork/hooks |
| QoderWork CN | native | 同上 | **无** | `~/.qoderworkcn/settings.json` | alibabacloud.com/help/zh/lingma/hook |
| Codex CLI | native | SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / PermissionRequest / PreCompact / PostCompact / SubagentStart / SubagentStop / Stop | **无（只走 stdin `cwd`）** | `~/.codex/hooks.json`（或 config.toml 的 `[hooks]`） | developers.openai.com/codex/hooks |
| DeepSeek Harness (dsh) | **bridge** | Cordis 扩展点：`agent/session-start`、`agent/pre-step`、`tools/pre-execute`、`tools/post-execute`、`session/event` | 插件内 `ctx` | 无 hooks.json；桥接包读 `~/.claude/settings.json` | dsh 官方文档 |
| TraeWork | **instruction** | — | `TRAE_PROJECT_DIR` / `CLAUDE_PROJECT_DIR` | 无公开 hook 文档 | docs.trae.cn/traework/ |
| WorkBuddy | **instruction** | — | — | 无公开 hook 文档 | workbuddy.cn/docs/workbuddy/Overview |

用户点名的 8 个入口 → 注册表名的映射（有测试固化）：
`trae work→traework`、`trae code→traecode`、`work buddy→workbuddy`、`code buddy→codebuddy`、
`qoder→qoder`、`qoder work→qoderwork`、`deepseek harness→dsh`、`codex→codex`。

### 12.2 三个核心设计结论

**① 一条命令通吃。** 六家原生 shell hook 的注入通道在 `SessionStart` / `UserPromptSubmit`
两个事件上是重合的：**stdout 纯文本即被当作附加上下文**（Claude Code、TraeCode、CodeBuddy、
Qoder、QoderWork、Codex 的文档均如此记载）。因此 `mem hook` 输出纯文本，
不按 harness 分支渲染格式 —— 少一层分支就少一类静默失败。

`--harness` 只决定两件事：**从哪个环境变量取项目目录**、**配置写到哪里**。

**② 写一份 Claude 格式，顺带覆盖三家。** TraeCode 官方明确会额外读取
`~/.claude/settings.json` 并**合并执行**；dsh 有官方桥接包
`dsh-hooks-claude-code` 把同一协议翻译到自己的扩展点。故给 `claude` 装一次，
TraeCode 与 dsh 一并受益。

**③ 三级确定性必须对用户可见。** `native`（事件触发即执行，代码强制）→
`bridge`（复用他家协议）→ `instruction`（写指令块靠模型遵守）。
`mem harness` 与 `mem init` 都显式打印等级，并标注「指令级没有代码强制，可靠性低一档」。

### 12.3 注入通道差异（决定输出格式的取舍）

| 工具 | 纯文本 stdout | `hookSpecificOutput.additionalContext` | 备注 |
|:---|:---|:---|:---|
| Claude Code / TraeCode / CodeBuddy | ✅（仅 SessionStart & UserPromptSubmit） | ✅ | 我们只用前一类事件 |
| Qoder / QoderWork | ✅（仅这两个事件） | ✅ | 🔴 JSON 带 `hookSpecificOutput` 时必须同时给 `hookEventName`，否则**整份被拒** |
| Codex | ✅（仅这两个事件） | ✅ | 无环境变量，项目目录取 stdin `cwd` |

退出码语义各家一致：`0` 成功（stdout 按上述规则解析）、`2` 阻断、其他为非阻断错误。
**本实现永远返回 0**：记忆系统不可用不该让用户的会话中断。

### 12.4 `mem hook` 的四条纪律

1. **绝不阻断宿主。** 所有分支返回 nil；解析失败、stdin 是垃圾、库打不开都静默放过。
   这条有专门的用例矩阵（空输入 / 非 JSON / 截断 JSON / 未知事件 / `cwd` 不存在 / 字段类型不符）。
2. **stdout 只放注入内容，诊断走 stderr。** CodeBuddy 文档明确「stderr 仅作为 fallback，
   调试日志可安全写入 stderr，不会污染给 Agent 的反馈」；其余各家在退出码 0 时也只展示 stderr。
3. **每轮注入有字符预算**（默认 1200 字符），且**用法提示预留预算**——它的作用正是告诉模型
   「还有多级记忆可查」，被截断就失去了钩子的意义。
4. **无命中即静默。** `UserPromptSubmit` 检索不到相关内容时不输出任何东西，避免每轮注入噪声。

`SessionStart` 的注入顺序：跨工程硬规则 → 本工程记忆 → 用法提示。
工程推断优先级：指定 harness 的环境变量 → stdin `cwd` → `workspace_roots` → 进程工作目录。
**指定了 harness 就不再兜到别家**——否则在 Trae 里启动的终端会把 `CLAUDE_PROJECT_DIR`
带进 QoderWork 的钩子，工程判断就错了。

### 12.5 `mem init` 的配置生成纪律

| 纪律 | 做法 |
|:---|:---|
| 合并而非覆盖 | 只读写 `hooks` 段；已有的其它 hook 组与无关配置键原样保留；**解析不了就报错退出**，绝不拿默认值盖用户文件 |
| 幂等 | 写入前先摘掉自己上次写的条目（按命令里的 `# v2mem` 标记识别），重复执行不累积 |
| 备份 | `<文件>.v2mem.bak`，**只写一次**，保证它永远是「首次触碰前」的干净状态 |
| 绝对路径 | 命令用当前二进制的绝对路径而非裸 `mem`：harness 拉起钩子时环境往往很干净，没有用户 shell 的 PATH，裸命令会静默失效 |
| 可验证 | 写入后打印该工具的**生效验证方法**（如 Claude 用 `/hooks`、Codex 需 `/hooks` 里 trust）、额外前置条件与已知注意事项 |

**为什么用 `# v2mem` 注释做标记**：最初靠「可执行文件名 == `mem`」识别，测试二进制叫 `mem.test`
就认不出，重复 init 累积了 3 条（被幂等用例抓到）。任何改名都会破坏它。
写成 shell 注释是安全的——各家文档的 command 都经 shell 执行（bash/zsh/sh/powershell 皆以 `#` 为注释），
且不污染实际参数。

### 12.6 本机安装状态与端到端验证

`mem harness` 检测到本机已装并已写入：

| 工具 | 文件 | 结果 |
|:---|:---|:---|
| Claude Code + dsh（桥接） | `~/.claude/settings.json` | 已写入 |
| TraeCode（Trae CN / SOLO CN） | `~/.trae-cn/hooks.json` | 已写入 |
| CodeBuddy Code | `~/.codebuddy/settings.json` | 已写入 |
| Codex CLI | `~/.codex/hooks.json` | 已写入 |
| QoderWork CN | `~/.qoderworkcn/settings.json` | 已写入 |

**端到端验证方式**：从 `~/.claude/settings.json` 里**取出真实命令**并执行（而不是跑一份等价命令），
证明写进配置的那条命令确实可用：

- `SessionStart`（模拟 `cwd` = 真实工程）→ 注入 494 字符，含 3 条硬规则 + 1 条本工程记忆 + 用法提示
- `UserPromptSubmit`（提问含「同步目录」）→ 只注入命中的那 1 条 pitfall

**延迟**（钩子在会话关键路径上，不能有可观开销，`/usr/bin/time -p` 实测）：
`session-start` 0.01s、`prompt-submit` 0.01s、空事件 0.01s、库不存在 0.01s。

### 12.7 测试

`go test ./... -v` 实测：harness 11 + similarity 16 + store 53 + eval 8 + audit 6 + cmd 65 = **159**（M6 完成时点为 119，M5a 时点为 73；口径与新增项见各自小节）。

| 范围 | 用例 |
|:---|:---|
| harness 注册表 | 名字/别名解析、未知名报错带候选、**用户点名的 8 个入口全覆盖**、条目自洽（native 有配置路径 / instruction 有指令文件）、名字与别名全局唯一、事件名归一化容忍大小写与下划线、多候选配置路径选择、环境变量优先级、不注入环境变量的工具被正确标注 |
| `mem hook` | SessionStart 注入硬规则与本工程记忆、教会检索命令、prompt-submit 只注入相关且排除他工程、无命中静默、**异常输入矩阵（8 种）不报错**、库不存在可用、`--limit` 生效、**字符预算生效**、stdin cwd 回退、环境变量优先、显式 `--event` |
| `mem init` / `harness` | 列表含用户工具与等级标注、写出两个事件、绝对路径、**合并不覆盖**、**幂等**、`--dry-run` 不落盘、备份不被二次 init 覆盖、指令级标记块就地替换、`--file` 覆盖、未知 harness 与非法 scope 报错 |
| 扫描与选择 | 不带 `--harness` 进入扫描流程、编号与名字混用、取消（空/`q`/`Q`/空白）不写任何文件、未知选项报错并指名、`--all` 报告跳过项、`--all` 默认只处理检测到的、逗号分隔多选、含未知项报错 |

**变异测试**（逐个把实现改坏，确认有断言报警）：

| 变异 | 捕获用例 |
|:---|:---|
| prompt-submit 去掉工程过滤 | `TestCmdHookPromptSubmitExcludesOtherProjects` ✅ |
| 取消字符预算 | `TestCmdHookRespectsCharBudget` ✅ |
| 不预留用法提示的预算 | `TestCmdHookRespectsCharBudget` ✅ |
| 去掉幂等去重（`withoutOurs`） | `TestCmdInitIsIdempotent`（`got 3`）✅ |
| 备份每次覆盖 | `TestCmdInitBacksUpExistingFile` ✅ |
| 不合并、直接覆盖用户配置 | `TestCmdInitMergesWithoutClobberingExistingConfig` ✅ |

**三处「假通过」的排查记录**（都是断言或数据的问题，不是实现的）：

1. **工程名不对齐** → 一批 prompt-submit 用例返回空，但不是「无命中」而是被工程过滤掉了。
   修法：造带 `.git` 的临时工程，且记忆的 `project` 与钩子推断一致；断言一律要求**有内容**。
2. **宿主环境变量污染** → 工作环境里已设 `CLAUDE_PROJECT_DIR`，钩子优先采纳它，
   测试实际在验证宿主环境。修法：让测试数据构造函数**显式清空**各家环境变量。
   （这一点是靠 `mem hook` 打到 stderr 的诊断行 `project=… dir=…` 定位的 —— 诊断信息的价值。）
3. **预算未成为约束** → 先写 40 条记忆、断言 `--max-chars 800` 生效，但 `--limit 3`
   先把量兜住了，把预算逻辑整段删掉测试也不失败。修法：同时放开 `--limit`，
   并加前置断言「放开采纳量后输出确实远超预算」。
   另：最初 40 条写的是同一个字符串，被「相同知识覆盖」收敛成 1 条 —— 数据必须逐条互异。

### 12.8 不确定项（已知的未知，不做推测）

1. **TraeWork 是否有 shell hook 未经验证。** 官方文档未发布；其 Code 模式构建于 TraeCode SOLO
   之上，若与 TraeCode 共用运行时，则 `~/.trae-cn/hooks.json` 可能同样生效。
   本工具因此按指令级接入，**不据此写入**。
2. **WorkBuddy 无公开 hook 配置。** 其已发布扩展机制是项目记忆文件、技能、自动化任务，
   故走指令级接入。
3. **TraeCode 国际版的全局路径未从文档确认**（文档记 CN 版为 `~/.trae-cn/hooks.json`）。
   注册表同时列 `~/.trae/hooks.json` 作为候选，优先选父目录已存在的那个。
4. **dsh 的桥接包未安装。** 本次只写好 Claude 侧配置；
   需执行 `dsh plugin --profile add dsh-hooks-claude-code` 才生效。
5. **Codex 的 hook 需要信任。** 非托管 hook 首次执行前要在 `/hooks` 里审核，
   信任按 hook 哈希持久化 —— 改动命令后需重新审核。已写入 `~/.codex/hooks.json`，
   但需用户首次确认。
6. **`/hooks` 面板式审核的工具**（CodeBuddy）在面板外的手工改动可能需在面板内确认后才生效。

### 12.9 指令级接入的落点：选 AGENTS.md，不选记忆文件

WorkBuddy 与 TraeWork 走指令级接入。落点有两个候选，结论是 **AGENTS.md**：

| 候选 | 每轮注入 | 判决 |
|:---|:---|:---|
| `.workbuddy/memory/MEMORY.md` | 是 | ❌ 这是「记忆」文件，自带严格注入预算（本机实测约 7800 字符上限），塞说明会挤占真正的记忆 |
| `AGENTS.md` | 是（仓库根每轮注入） | ✅ 语义上就是「在这个仓库怎么做事」，钩子说明天然属于它 |

**实证（在本仓库上跑其自检门禁 `scripts/eval-agents-md.py`）**：

| 指标 | 写入前 | 写入后 |
|:---|:---|:---|
| 规则保留核对 | 84/84 | 84/84 |
| 表格竖线一致性 | OK | OK |
| 单条规则 ≤50 | OK | OK |
| 字符 / 预估 tokens | 6905 / 4538 | 7309 / 4782 |
| 结果 | PASS | **PASS** |

代价是 **+404 字符 / +244 tokens 每轮**——这是该决策的真实成本，属于项目约定，由用户判定是否值得。

**一个必须先检查的坑**：WorkBuddy / CodeBuddy 侧**同层存在 `CODEBUDDY.md` 时 `AGENTS.md` 会被忽略**。
本机仓库根实测无 `CODEBUDDY.md`，故 AGENTS.md 未被遮蔽；注册表里也把这条写进了 Notes。
若某项目两个文件都在，应改用 `--file CODEBUDDY.md`。

### 12.10 扫描 + 交互选择（默认流程）

`mem init` 不带 `--harness` 时进入扫描选择，而不是无差别往所有工具里写：

```
扫描本机：检测到 6 个已安装的工具（共 11 个已知入口）

  [ 1] ● claude         native      /Users/…/.claude/settings.json
  [ 2] ● traecode       native      /Users/…/.trae-cn/hooks.json
  …
  [ 9]   workbuddy      instruction AGENTS.md

选择要注入钩子的工具（编号或名字，逗号分隔；a=全部已检测到；q=取消）：
```

设计取舍：

- **列出全部入口而非只列检测到的**：索引因此稳定（不受本机环境影响），也允许用户选一个
  我们没探到路径的工具（它可能装在别处）。`●` 标记检测到的。
- **编号与名字都接受**，逗号/空格/顿号分隔；`a` 取全部已检测到；空输入或 `q` 取消。
- **未知项报错并指名**，不静默跳过（静默跳过会让用户以为装上了）。
- **`--all` 只处理检测到的**；被排除的**逐条报告**（区分「未检测到」与「当前范围无落点」）——
  否则用户以为「全都装了」。
- **多选时容忍个别工具没有落点**（如 QoderWork 只有用户级、dsh 无独立配置文件），
  报告「跳过 N 个」而不是整批失败。
- `--harness a,b,c` 仍可一次装多个；`--harness` 与 `--all` 互斥。
- 项目级写入（`--project`）不再提示「未检测到配置目录」——项目级本就该在项目里新建文件，
  那时提示纯属误导。

## 13. M5b（向量 + RRF）的 ROI 预算

结论先行：**当前不建议做 M5b**。包体不是主要障碍（实测 +3.76 MB 可接受），
真正的问题是**纯 Go 路线的 sqlite-vec 今天跑不起来**，而向量还需要一个 embedding 来源。
建议先做零成本的替代项（§13.4）。

### 13.1 包体：实测数字（2026-09-17，CGO_ENABLED=0，darwin/arm64）

用三个最小对照程序测「驱动 + 扩展」本身的重量，排除业务代码干扰：

| 组合 | 二进制 | 相对当前 | 备注 |
|:---|:---|:---|:---|
| `modernc.org/sqlite`（当前实现） | 9.45 MB | — | 纯 Go 转译的 SQLite，零 CGO |
| `ncruces/go-sqlite3` + wazero | 13.21 MB | **+3.76 MB（+40%）** | SQLite 作为 WASM 内嵌，wazero 是纯 Go 运行时 |
| `ncruces` + `sqlite-vec` | **构建/运行失败** | — | 见 §13.2 |

实际 `mem` 二进制含业务代码为 **10.63 MB**；若换 wazero 驱动约 **14.4 MB**。
SQLite 扩展本身（sqlite-vec 的 wasm）**不额外增加多少**——它替换整个 wasm 构建，
真正增重的是 wazero 运行时。

### 13.2 🔴 决定性障碍：sqlite-vec 的 Go 绑定当前不可用

逐项实测：

| 组合 | 结果 |
|:---|:---|
| `sqlite-vec-go-bindings` v0.1.6 + `ncruces` **v0.35.5**（最新） | ❌ 编译失败：`undefined: sqlite3.Binary` |
| `sqlite-vec-go-bindings` v0.1.7-alpha.2 + `ncruces` v0.35.5 | ❌ 同样失败（alpha 版仍引用该符号） |
| `sqlite-vec-go-bindings` v0.1.6 + `ncruces` **v0.17.1**（绑定 go.mod 里锁的版本） | ⚠️ 能编译，但**运行时报错**：`invalid function[11] export["sqlite3_soft_heap_limit64"]: i32.atomic.store invalid as feature "" is disabled` —— 连 `CREATE VIRTUAL TABLE ... fts5` 都起不来 |

根因：绑定包通过 `init()` 把自带的 `sqlite3.wasm` 赋给 `sqlite3.Binary` **替换整个 SQLite 构建**，
而该符号在 `ncruces` 主线的某个版本后被移除（v0.35.5 已无，改为 `ext`/`ext.go` 机制）。
绑定仓库的版本跨度是 **v0.17.1 → v0.35.5，相差 18 个小版本**，最新 alpha 也未跟上。

**含义**：这不是「调一下参数就能用」，而是上游生态未对齐。
把它纳入交付物意味着交付一个**无法运行、因而无法验证**的分支 ——
与「完成前必验证」原则直接冲突。

### 13.3 embedding 来源：比包体大一个量级的成本

向量是死数据，必须有 embedding 来源。三条路：

| 方案 | 包体/磁盘 | 依赖 | 可验证性 |
|:---|:---|:---|:---|
| 本地 ONNX 模型 | 模型文件（数十至数百 MB，随模型规模与量化而定）＋ 推理运行时（ONNX Runtime 共享库约 20 MB，或纯 Go 推理但慢） | 需下载模型，可离线 | 可验证 |
| 外部 embedding API | 几乎为零 | **网络 + key，打破「零外部服务」** | 可验证但引入故障面 |
| 不做向量 | **0** | 无 | — |

> ⚠️ **模型体积未在本机实测**：`huggingface.co` 本机不可达（HEAD 超时 10s、状态 000），
> ModelScope 可达但候选路径未解析出文件。故不给具体 MB 数字——按量级评估，
> 任何本地模型的磁盘占用都**远大于**驱动的 +3.76 MB。

### 13.4 建议 M5b-0：零成本的替代项（先做这个）

M5b 的主要收益是「措辞不同也能召回」。而 `internal/similarity` 已经实现了
字符 n-gram + MinHash，`consolidate` 已用它做近重复归并。**同一套签名可以直接接到检索上**：

- 对查询算 MinHash 签名，与库内签名做 LSH/精确 Jaccard，得到「措辞不同但说同一件事」的候选
- 与现有 BM25 结果做 RRF 融合——**RRF 只需两路排名，不需要向量**
- 成本：**零新依赖、零包体增长**，复用已有测试与校准矩阵

真正只有向量能做的是「用词完全不同但语义相关」（如「怎么防重复扣款」↔「幂等设计」）。
这个差距是真实的，但应当**先有证据表明模糊检索不够用**再补。

### 13.5 对「仅支持代码不安装服务是否可控」的回答

机制上可控（build tag 预留 + 可选依赖，默认构建零影响），但有两个前提：

1. **代码必须可运行才算「支持」**。当前 sqlite-vec 组合跑不起来，所以只能做「接口预留」。
   而按本仓库约定，预留必须**显式标注「预留未启用」＋ SKIP 原因**，否则就是静默存在。
2. **embedding 来源不可省略**。没有模型或 API，向量化就是永远无法验证的分支。

⇒ 若坚持要做，正确形态是：`-tags sqlite_vec` 默认不编译 + 文档标注「上游生态未对齐，暂不可用」+
一条 `go build -tags sqlite_vec` 的 CI 探测任务盯着上游何时可用。**但仍不产出可运行能力**。

### 13.6 决策

| 项 | 建议 |
|:---|:---|
| M5b-0 模糊检索（复用 MinHash + RRF） | ✅ **建议做**，零包体成本，覆盖主要收益场景 |
| M5b-1 sqlite-vec + 本地模型 | ⏸ **暂缓**，等上游绑定对齐后再评估；届时成本 = +3.76 MB 驱动 + 模型文件 |
| M5b-2 外部 embedding API | ❌ 不建议，破坏「零外部服务」这一核心设计约束（§3） |
| 默认构建包体 | 维持 10.63 MB 不变 |

## 14. 与 WorkBuddy 结合：解决「频繁压缩记忆」

### 14.1 问题与根因

**症状**：`.workbuddy/memory/MEMORY.md` 每轮注入且有容量上限（本机按 7800 字符控制），
接近上限就要人工「压缩」——蒸馏、下沉、删减，反复做。

**实测现状**（2026-09-17）：

| 文件 | 字节 | 字符（预算口径） | 角色 |
|:---|:---|:---|:---|
| `AGENTS.md` | 13098 | 7431 | 每轮注入（已接入 v2mem 钩子） |
| `.workbuddy/memory/MEMORY.md` | 10617 | **5978 / 7800（77%）** | 每轮注入 |
| `.workbuddy/memory/MEMORY-details.md` | 43273 | — | 长尾细节（不注入） |
| 13 个 `2026-*.md` 日志 | 593607 | — | 过程记录（不注入） |

**根因**：把「每轮必注入的 Level 1」当成知识库在用。细节不断被**人工蒸馏进 MEMORY.md**
（日志 → MEMORY.md 这条链路），文件必然持续增长、逼近上限、需要再压缩。
注意日志本身**不进上下文**，它们的价值是「被蒸馏进 MEMORY.md」才被用上 —— 这条链路才是病灶。

### 14.2 先分清两件事（关键判断）

| 问题 | 性质 | 能否彻底解决 |
|:---|:---|:---|
| 注入文件不断膨胀、需要反复压缩 | **确定性** —— 文件变小那一刻，截断与压缩需求就消失了，不依赖模型行为 | ✅ **可以彻底解决** |
| 模型是否真的会去检索 Level 2 | **概率性** —— 指令级钩子靠模型遵守，WorkBuddy 无公开原生 hook | ❌ 不能保证，只能提高概率 |

这个区分的实用含义：**即使模型从不主动检索，容量问题依然被解决**（省 token、免压缩），
只是少了「自动回忆」那一半收益。所以这件事值得做，且收益下限是确定的。

### 14.3 三层结合方案

```
Level 1   AGENTS.md（已接入钩子）＋ MEMORY.md 瘦身到 ≤2KB/40 行
          ↑ 每轮注入。只留：跨工程硬规则、当前任务态、钩子
Level 1.5 .workbuddy/memory/YYYY-MM-DD.md 日志 —— append-only 的过程记录
          ↑ 不再「蒸馏进 MEMORY.md」，改为 mem ingest 进 Level 2
Level 2   v2mem 库（~/.v2mem/mem.db）—— 全部细节，按需检索，不出现在上下文
```

**核心动作是取消「日志 → MEMORY.md 蒸馏」这一环**，换成：

```
mem ingest .workbuddy/memory/2026-09-17.md --project training-products
```

这条命令是**幂等**的（同内容由 `content_hash` 覆盖），所以可以每天/每周无脑重跑全量日志，
不需要判断「哪些已经搬过了」。实测 13 个日志 → **2630 条候选事实**，
dry-run 可先预览。

### 14.4 一次性迁移怎么做

1. **先看诊断**：`mem budget --file .workbuddy/memory/MEMORY.md --max-chars 7800`
   —— 输出体量、判定、以及库中该工程的记忆条数（迁移收益的对照）
2. **机械化搬运**（可先 `--dry-run` 预览）：
   ```
   mem ingest .workbuddy/memory/2026-*.md            --project <工程>
   mem ingest .workbuddy/memory/MEMORY-details.md    --project <工程>
   ```
   `mem ingest` 会跳过标题、代码围栏、纯指引行与过短行，并把**缩进续行合并**成一条
   （实测本仓库日志 28 处、details 23 处用了硬换行）。
3. **人工重写 MEMORY.md 为 ≤2KB 的 Level 1**：这一步**不能机械做** ——
   哪些是跨工程硬规则、哪些是当前任务态，需要判断。机械搬运只负责内容不丢。
4. **校验信息不丢**：把原 MEMORY.md 的条目逐条在库里搜一次
   （`mem search "<关键词>"`），确认命中后再精简文件。

⚠️ **不要用 `mem ingest` 直接吃 MEMORY.md 然后删掉它**：MEMORY.md 的价值在「每轮注入」，
它的硬规则与任务态是**故意重复**在上下文里的，不因为库里有一份就可以删。

### 14.5 持续保持（防止再度膨胀）

| 机制 | 做法 | 性质 |
|:---|:---|:---|
| 容量纪律 | 已写进 AGENTS.md 的钩子块：细节一律进库，文件只留三样 | 靠模型遵守（概率性） |
| 体量守卫 | `mem budget --file <文件> --max-chars 7800` —— **超预算返回非零退出码**，供自动化判定告警 | **确定性** |
| 定时搬运 | WorkBuddy 自动化任务：定期 `mem ingest` 新日志 + `mem gc` 回收过期 + `mem budget` 守卫 | **确定性** |
| 去重 | `mem consolidate` 归并措辞不同的近重复；跨会话重复搬运由 hash 覆盖天然去重 | **确定性** |

守卫与定时搬运都不依赖模型行为，这部分收益是确定的；只有「模型主动检索」是概率性的。

### 14.6 诚实的边界

1. **WorkBuddy 没有公开的原生 hook 配置**（其已发布扩展机制是项目记忆文件、技能、自动化），
   所以钩子只能是指令级 —— 检索调用是概率性的。
2. 若某天 WorkBuddy 开放 hook，可直接切到 native 级（本工具已支持：`mem init --harness workbuddy`
   会写原生 hooks 配置而非指令块），无需改其他部分。
3. **不要把「彻底解决」理解成「模型一定查」**：彻底解决的是容量与压缩频率，不是检索的确定性。
4. 迁移后 `MEMORY-details.md` 可以不再维护（内容已进库），但**删除前先确认条目都已在库中**。

## 15. 激活模型与评测机制（迁移前的前置判断）

### 15.1 `mem` 不是后台任务，是「需要时激活」

实测确认：**没有任何常驻进程**（`ps` 无 `mem` 进程）、无 `daemon/serve` 子命令、
无 LaunchAgent、无 crontab 条目。二进制是普通的按需调用 CLI。

三条激活路径，性质完全不同：

| 路径 | 触发者 | 保证强度 |
|:---|:---|:---|
| 钩子（SessionStart / UserPromptSubmit） | 宿主工具（Claude Code / TraeCode / CodeBuddy / Codex / QoderWork） | **强**：事件触发即执行，不依赖模型意愿 |
| 模型主动调用（`mem search` / `mem add`） | 模型的判断 | **弱**：指令级，模型可能不遵守 |
| WorkBuddy 定时自动化 | **宿主平台调度** | **弱**：本机没有 cron/launchd；平台没在跑就不触发 |

### 15.2 定时任务不能作为保证 —— 改为挂在「必然发生的事件」上

本机核查：`~/Library/LaunchAgents` 无相关工作项、`crontab` 无 v2mem 条目。
所以「每日维护」实际依赖 WorkBuddy 平台在线触发，**这不是可依赖的保证**（机器关机、应用未开、
平台侧异常都会漏）。

结论：**维护不该以调度器为前提**。设计上应把维护挂到「必然发生的事件」——
SessionStart 是天然的必然事件（用户每次开新会话都会发生），它同时也能承载
「机会性维护」。但要满足三个条件才可行：

1. **增量且有界**：只处理「比上次 ingest 更新的文件」，并设条数/时间预算；
2. **不得阻塞注入**：记忆注入必须先完成并输出，维护要么放在注入之后、要么另起进程；
3. **失败必须静默**：维护失败不能让宿主会话收到错误。

当前实现尚未做机会性维护（`mem hook` 只做检索与注入，实测 1–3 ms）。这是**已知待办**，
在样本积累到能判断收益之前不做 —— 先别为一个未验证的收益去增加会话路径的风险。

### 15.3 两个方向都必须能评（否则替换草率）

| 方向 | 问题 | 手段 |
|:---|:---|:---|
| 用户会话 → mem 查询 → 命中是否准 | 用户换个问法，还找得到吗？ | `mem eval recall --gold`（金标准）／`mem audit`（真实样本沉淀） |
| 用户会话 → mem 记录 → 记录是否准 | 该记的记下了吗？记的全是废物吗？ | `mem eval write --session --gold` |

**三层评测，成本递增、可信度递增**：

| 层 | 手段 | 能证明什么 | 自动 |
|:---|:---|:---|:---|
| L1 索引可用性 | `mem eval recall --auto` | 索引与中文切分没坏 | ✅ |
| L2 检索相关性 | `mem eval recall --gold`（人工标注、来自真实会话的查询） | **唯一能支撑替换决策的指标** | ❌ 需标注 |
| L3 端到端任务成功率 | 有/无记忆两臂对比同一批需要历史知识的问题 | 记忆是否真的提升了任务表现 | ❌ 需设计 A/B |

### 15.4 口径红线（必须先定死，改动须同步本节）

| 项 | 口径 | 理由 |
|:---|:---|:---|
| Recall@k | **宏平均**：Σ(命中 gold 数 / 该样本 gold 数) / N | micro 会让 gold 多的样本主导指标 |
| 命中判定 | **`content_hash` 前缀精确匹配** | 用文本包含会让「换了说法」也算命中，自欺 |
| 分母 N | **有效样本数**；gold 不在库中的样本必须**显式剔除并上报** | 静默剔除等于篡改分母 |
| `EmptyRate` | 只统计「检索完全无结果」 | 与「有结果但没命中」混同，会把「索引挂了」和「排序不好」算成一个数 |
| k | 必须显式；默认 5 | 与 hook 的注入条数（3）**不是一回事**：评测看检索能力上限，hook 看注入预算 |
| `--auto` 的数字 | **不得单独引用**，输出里强制带「下限测试、不代表真实质量」声明 | 查询就是内容本身，不存在「用户换说法」这层困难 |

### 15.5 首次运行就抓到一个真实缺陷（评测机制的价值证明）

审计日志跑起来后立刻发现：

```
prompt-submit  claude  命中 0 空=true  v2mem 的钩子会不会阻断会话
```

定位：**中文查询的词组内是 AND，而词间是 OR**。自然语言长问句没有空格，
整句被当成**一个词组** → 要求全部 bigram 都出现在同一条记忆里 → 必然 0 命中。

实测对比（修复前）：

| 查询 | 命中 |
|:---|:---|
| `日志怎么搬进记忆库` | **0** |
| `日志 记忆库` | 3 |

修复：**精确优先、空手才放宽** —— 先用原表达式；无命中时退化为「全 term OR + bm25 排序」。
保留精度（精确有结果时不放宽，有测试固化），只在完全空手时牺牲精度换召回。

修复后实测：`日志怎么搬进记忆库` 0 → **3 条**，钩子已能注入正确记忆。

> 这是本次「先建评测再迁移」的直接收益：**如果先迁移了，这个缺陷会在真实会话里
> 表现成「记忆系统没用」**，而不是表现为「一个可定位的检索缺陷」。

### 15.6 顺带堵掉的静默陷阱

`mem search "查询" --json` —— Go 的 flag 解析在**首个位置参数处停止**，
所以写在查询之后的选项会被当成查询内容：不报错、也搜不到东西。
已加守卫：查询里出现已知选项名即报错并指明写法（只认已知选项名，
避免把正文里真的含 `--global` 的记忆误判）。

### 15.7 替换决策的判据（现在还不能下结论）

**不能**：L2 需要真实会话样本 + 人工标注，而 `mem audit` 是本次才埋的点，
可评测样本当前为个位数。L1（`--auto`）虽然 100%，但那是下限测试，**不构成证据**。

建议流程：

1. **并行期**：AGENTS.md 钩子已接入（文件与库并存），真实会话开始积累审计样本；
2. **积累到 ≥20–30 条带 query 的样本**后，从中挑选覆盖各类意图的查询做人工标注（gold 用 hash 前缀）；
3. 跑 `mem eval recall --gold`，看 Recall@5/MRR/**最差样本清单**；
4. 若最差样本里出现「明显该召回却没召回」，**先修检索**再谈替换；
5. 同时用 `mem eval write --session --gold` 抽查日志搬运的漏记率；
6. 三项都达标后再精简 MEMORY.md —— 那时替换是有证据的，不是凭感觉的。

在此之前，`MEMORY.md` **保持现状不动**。
