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

**Harness 钩子增强（可选）**：Claude Code 的 `SessionStart` / `UserPromptSubmit` / `Stop`、WorkBuddy 的自动化与 `.githooks`，可做到会话开始自动预检索、会话结束自动回写。文本钩子做可移植兜底，原生钩子做自动化增强，两者并存。

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

两条**刻意的例外**（本地书写形式优先，非可交换，已在文档中固化）：

- `content`：hash 相同即视为同一事实（归一化已折叠大小写与空白），保留本地写法，不重算 `content_idx`
- `superseded_by`：已有非空值不被覆盖

### 7.3 两条已知边界（有意为之，非遗漏）

1. **`origin_device` 是「首次写入本库的设备」，不随归并收敛。**
   共享事实在 A 库记 `mini`、在 B 库记 `laptop`，两边都对——这是本地视角字段。
   收敛判据因此排除 `id` 与 `origin_device`，只对内容相关字段断言一致。
2. **`edges` 与 `superseded_by` 的跨设备重映射尚未实现。**
   它们引用本地 `id`，而归并会为同一条知识分配不同 id，跨设备的引用会断。M4 只归并 `memories` + `tags`；
   引用重映射留待 M5（相似归并真正产生 `superseded_by` 时才必需）。

### 7.4 导出可复现

`Export()` 按 `(content_hash, project)` 排序，标记按 `(key, value)` 排序。
否则每次导出的 JSONL 都会无谓 diff，跨设备比对失去意义。

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
- [x] M4 跨设备：`export` / `import` 归并 + 溯源
- [x] **M5a 相似归并（纯 Go，无模型）**：字符 n-gram MinHash + 四条护栏 + `consolidate`
- [ ] M5b 增强（可选）：sqlite-vec 向量 + RRF 融合（**不再是相似归并的前置**，只是检索召回的另一路）
- [ ] M6 Harness 集成：Level 1 钩子模板 + 原生钩子（自动预检索 / 自动回写）

> M5 拆成两半是本次实现中的结论：相似归并属**近重复检测**（经典算法问题），
> MinHash 几十行、微秒级、零依赖即可胜任，不必引入模型与 CGO。
> 于是 M5b 的向量化从「相似归并的前置」降级为「检索召回的另一路」。

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
