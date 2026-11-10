# 设计：记忆溯源（provenance）

> 目的：让记忆可低成本追溯回"它来自哪个工程资产/哪次会话/哪条链路"，为工程资产变更联动、词库反查、跨设备一致提供来源锚点。
> 范围：本设计只覆盖**记忆层**的溯源；词库条目的来源引用（别名 → 支撑记忆 id）是后续增量，不在本期。

## 1. 问题

`memories` 表已有 `project / origin_device / origin_tool / source / content_hash / created_at / superseded_by` 等字段，能回答"哪条工程、哪台设备、哪条写入链路、何时写入"。

真正缺失的是**内容级溯源**：一条记忆**具体来自哪个源文件、第几行**（如 cad-bot/AGENTS.md 第 12 行）。`ingest` 提取记忆时，源文件信息被丢弃，只留下"来自 ingest"。这导致：

- 源文件变更时无法反向判定"这条记忆是否已失效 / 需要重摄取"。
- 无法把一条记忆挂到它支撑的词库条目上（词库联动的前置）。

## 2. 方案：单字段 `provenance` JSON

新增一个 TEXT 列 `provenance`，存紧凑 JSON，可空（存量数据为 NULL，兼容老版本不迁移数据）。

```json
{"file": "AGENTS.md", "line": 12, "source": "asset", "preview": "钩子只读护栏"}
```

| 字段 | 含义 | 说明 |
|:---|:---|:---|
| `file` | 源文件相对路径 | 相对工程根；不存绝对路径，避免跨设备失效 |
| `line` | 锚点行号（可选） | 精确到行；无行信息可省略 |
| `source` | 溯源类型 | `asset`（工程资产）/ `session`（会话总结）/ `query`（查询反馈）… |
| `preview` | 源片段预览（可选） | 一眼可读，省去反查文件 |

### 选型理由：为什么不拆列 / 不用纯文本

- **拆多列**（file/line/…）schema 臃肿，且溯源基本不按列过滤，SQL 层索引价值低。
- **纯文本串**（"AGENTS.md:12"）最简单，但难扩展（将来加 preview、类型判定要解析字符串）。
- **单 JSON 列**：写入即对象，解析成本低，字段可扩展，是溯源"低频读取、写入即定格"场景的合理形态。

## 3. Schema 迁移

已有真实库（2923 条）用 **ALTER TABLE 加 nullable 列**，不动存量数据：

```sql
ALTER TABLE memories ADD COLUMN provenance TEXT;
```

- 老版本二进制打开的字段不含 provenance，不感知该列，行为不变（前向兼容）。
- 新库 `CREATE TABLE` 显式带上该列。
- 迁移在 `store.Open` 的 schema 初始化处做：检测缺列即 `ALTER`，幂等。

## 4. 写入路径

`store.Add` 的 `AddInput` 增 `Provenance *Provenance`（或字符串），INSERT/UPDATE 一并写入。默认空。

`ingest` 逐个候选提取时，已知当前文件路径与该候选在文件内的行号，`extractCandidates` 返回 `(candidate, line)`，调用 `Add` 时填：

```go
store.AddInput{..., Provenance: &store.Provenance{
    File: "AGENTS.md", Line: line, Source: "asset",
}}
```

## 5. 纳入 sync 传播

`mem sync` 的三方合并要把 `provenance` 一并携带：导出/导入的 JSONL 结构加 `provenance` 字段，`insert` 分支写入它。

- 冲突策略沿用现有约定：以 `content_hash + updated_at` 判定，provenance 属"写入即定格"字段，合并时不参与 content 冲突判定，仅随记录传播。
- 已有记录当 provenance 为空而同步侧非空时，采纳同步侧值（补齐溯源）。

## 6. 联动价值（非本期实现）

- **资产联动**：源文件 hash 变更 → 反查 `provenance.file` 命中条 → 提示重摄取 / 标记失效。
- **词库联动**：词库条目带 `source_memories: [id,...]`，反查支撑它的记忆及其 `provenance`，形成 资产→记忆→词库 双向追溯。

## 7. 验收

- 迁移：老库 Open 后 `provenance` 列存在、存量行为 NULL；新库建表即带列。
- 写入：`ingest` 后可读到 `provenance.file` 与 `line`；`add` 未传时为空。
- sync：跨库 sync 后 provenance 随记录到达目标库，冲突时按约定规则。
- 门禁：`make cover-gate` 通过，新列相关命中率达标。

## 8. 与 channel 的分工

`provenance` 回答「来自哪个文件第几行」。`channel`（`user|agent|tool|import`）回答「谁写的」，和 `source` 并存。约定与捕获幂等见 [`design-write-governance.md`](./design-write-governance.md) §10。channel 随 export/import 传播：本地为空才采纳对端，不参与内容冲突判定。

## 9. 资产联动：`mem asset`

§6 的第一条联动（源文件变更 → 反查记忆）落地为只读命令：

```
mem asset <path> [--db ...] [--json] [--project P] [--limit N]
```

### 为什么按文件反查

语义检索（`mem search`）对「这个文件」这种**结构化条件**无能为力：一条讲 `hook.go` 预算的记忆里未必出现 "hook.go"。而 Agent 改文件前最自然的问法恰恰是「关于这个文件，记住过什么」——按 `provenance.file` 精确匹配，精确率是语义检索给不了的。

### 为什么库里仍存相对路径

绝对路径跨设备失效（§2 表格）。反查的成本因此被推到读侧：用户给什么形态，就在 CLI 层投影成一组候选字面量。

### 路径归一化 = 投影成多候选

`normalizeAssetPaths`（`cmd/mem/asset.go`）把输入投影成：

1. 相对工程根的路径（斜杠分隔）——今天写入的主流形态；
2. 输入本身（`Clean` 后）——覆盖「cwd 正好是工程根」的写法；
3. 绝对路径——覆盖历史数据与外部工具直接写绝对路径的情形。

工程根用 `git rev-parse --show-toplevel` 求，起点是**源文件所在目录**而非进程 cwd（查别的工程的文件时，cwd 的工程名会把结果滤空）；`~` 只展开开头，不做 shell 展开。

### 为什么不解析符号链接

投影**只做字符串运算**：`~` 展开 → `Clean` → 非绝对时 `Join(cwd)` → `ToSlash`，不调 `EvalSymlinks`。

- 解析会把路径**移出**工程根：工程经 symlink 暴露时（例如 `~/work/proj` 链到 `/Volumes/x/proj`），唯一真正有用的「相对工程根」候选直接失效。
- 解析结果依赖本机布局（不存在的路径解析失败；macOS 的 `/home` 是 autofs 指到 `/System/Volumes/Data/home`），投影会多出与用户输入无关的候选，也让「纯函数」变成不可测的机器相关函数。
- 代价是「库里存的是 symlink 一侧、用户给另一侧」时相对候选对不上，绝对路径候选仍可用。溯源记录的就是写入当时的字面量，投影只负责对齐常见写法，不为符号链接做路径规范化。

### 为什么不做目录前缀匹配

`mem asset ./cmd/mem/` 不做「该目录下所有文件」的匹配。前缀匹配是隐藏的全表 `LIKE`，召回一堆弱相关内容；且「这个目录下有哪些记忆」已由 `mem ls --project` 承担。范围收窄成单文件的代价是可解释。

### 全表扫描的代价

`provenance` 无索引，`json_extract` 是整表扫描。本地库千级量级下为毫秒级，且 `asset` 是按需命令、不在 hook 热路径上，因此**不**为此加生成列或表达式索引（那是 schema 变更，属于迁移任务）。查询只读取检索可见的记忆（复用 `visibilityWhere`），与 `mem search` 口径一致：被归并取代、已过期的条目不会出现在这里。

### 边界

- `provenance` 为 NULL / 空串 / 非法 JSON 一律视为不命中，单条脏数据不会让整次查询失败。读取 `file`/`line`/`preview` 的每个表达式都先过 `json_valid`：SQLite 的 `json_extract` 遇到非法 JSON 是**抛错**而不是返回 NULL，守卫不能只加在 `WHERE` 用到的那个上。
- 未记录锚点行时（`$.line` 不存在）`json_extract` 返回 NULL，输出渲染为 `L-`；排序里 NULL 在 0 之前，即「只有文件级结论」的条目排在最前。
- 无命中时打印提示并**以 0 退出**，与 `mem search` 的「无结果」语义一致：空结果是答案的一部分，不是错误。
