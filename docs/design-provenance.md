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