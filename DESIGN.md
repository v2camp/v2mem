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

| 机制 | 实现 |
|:---|:---|
| **相同知识覆盖** | `content_hash` 唯一索引 + `ON CONFLICT DO UPDATE`：刷新 `updated_at`、累加 `access_count`。零成本、写入期完成 |
| **相似知识归并** | **异步** consolidation：余弦 > 0.92 的簇 → 保留最富一条为规范，其余置 `superseded_by` 并建 `merged_into` 边，tags 取并集 |
| **过期机制** | 三种并存：① 硬 TTL（`expires_at`）② 久用衰减 `decay = exp(-ln2 · Δt / halflife)` ③ 被取代（`superseded_by` + `valid_to`，给出时序性而无需图库） |
| **基于标记的归并** | `content_hash` 相同 → tags 取并集、`created_at` 取最早、`updated_at`/`salience` 取最大，`origin_device` 保留为溯源集合 |

**排序**：`score = w1·bm25_rank + w2·(1−cosine) + w3·salience + w4·decay(last_seen_at)`；
或更简单——RRF 融合两路排名后乘衰减因子。

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

**Harness 钩子增强（可选）**：Claude Code 的 `SessionStart` / `UserPromptSubmit` / `Stop`、WorkBuddy 的自动化与 `.githooks`，可做到会话开始自动预检索、会话结束自动回写。文本钩子做可移植兜底，原生钩子做自动化增强，两者并存。

## 7. 跨设备归集

```
设备 A: mem export > ~/.v2mem/sync/A-<ts>.jsonl
        （同步该文件是安全的：纯文本、append-only）
设备 B: mem import ~/.v2mem/sync/A-<ts>.jsonl
        → 按 content_hash upsert，tags 并集，保留双端 origin_device
```

**禁止**：同步 `mem.db` 本体（写入中途同步会损坏库）。

## 8. CLI 设计（`mem`）

```
mem add      --kind <k> [--project P | --global] [--tag k=v]... "<原子事实>"
mem search   "<query>" [--project P] [--scope current|global|all] [--tag k=v]... [--limit N] [--json]
mem touch    <id>                  # 命中反馈，刷新 last_seen_at / access_count
mem forget   <id>                  # 显式删除
mem gc                             # 过期清理 + 衰减淘汰
mem consolidate                    # 异步：相似归并（需 embedding 时才启用）
mem export   [out.jsonl]
mem import   <in.jsonl>            # 跨设备归并
mem stats                          # 条数 / 分布 / 库大小
```

选项位置：Go 的 `flag` 要求**选项写在子命令之后**（`mem add --db X`，不是 `mem --db X add`）。

`--json` 输出供 LLM 解析；默认输出为紧凑纯文本，省 token。
`--scope`：`current` = 当前工程 + 全局；`global` = 只要全局；`all`/省略 = 跨工程（默认）。
`--global` 与 `--project` **互斥**——这是唯一能写入「空工程」记忆的入口，缺了它全局记忆在 CLI 上不可达。

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
- [ ] M4 跨设备：`export` / `import` 归并 + 溯源
- [ ] M5 增强（可选）：sqlite-vec 向量 + `consolidate` 相似归并 + RRF 融合
- [ ] M6 Harness 集成：Level 1 钩子模板 + 原生钩子（自动预检索 / 自动回写）

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
