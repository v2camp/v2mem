# 存储分层契约（P0）

> 来源：[`docs/design-storage-layering.md`](../docs/design-storage-layering.md) §5；判据权威 [`docs/VALUE.md`](../docs/VALUE.md)。
> 状态：P0 契约已落地；§8-2 于 2026-11-24 重开，阈值改为字段硬规定，P1 验收口径按本机实测重写。
> 决策：路径 + `#seq` 定位 · **`summary` / `summary_idx` 一律外置**（其他字段仍走 2 KB） · 保留精简 `fts_obs_*` · Markdown 只读不回写 · `read_traces` 诊断字段 P2 再拆。

## 1. 三层与真源

| 层 | 介质 | 角色 |
|:---|:---|:---|
| 运转逻辑 | SQLite `mem.db` | 唯一查询源：状态机、唯一键、水位线、引用、计数、聚合、身份哈希 |
| 观测内容 | gzip JSONL（`.jsonl.gz`） | 事实源 / 派生物；按日期 × 分类分片，append-only |
| 最终记忆 | Markdown | 单向投影，给人和其他 AI 工具读；不回写 DB |

删掉一个字段后系统仍能正确去重、排序、流转、检索定位、告警判定 → 该字段可外置；否则留库。

## 2. ref 格式与不可变性

- 定位串沿用现有归档契约：`<path>#<seq>`。`path` 是 gzip JSONL 文件路径（可为绝对路径，或相对数据根如 `content/2026/11/23/observations.jsonl.gz`）；`seq` 是分片内 1 起的行序号，与 `segmentRecord.Seq` 一致。
- 不引入设计稿里的 `#L<seq>` 变体；不带 `#seq` 只作兼容读取（等价首行），新写入必须带。
- 内容 `sha256` 单独存在库里的账字段中（与 `archive_hash` 同为压缩后字节的 sha256），不塞进 locator。
- 当日分片只 append；关闭后不可变，禁止重写或改名历史分片。引用才稳定。
- 读取复用 `internal/evidence.ReadArchiveRecord`；不得新造第二套 JSONL 解析格式。

## 3. 写入顺序（两类，方向相反）

| 类型 | 顺序 | 失败处置 |
|:---|:---|:---|
| 引用型内容（如外置 `summary`） | 内容先落盘 + fsync → DB 事务写 ref / 账 / 状态 | DB 失败保留 segment，整批写 spool 回放；孤儿内容交 GC |
| 派生型审计日志（如 `audit.jsonl`） | DB 状态事务先提交 → 再 append 日志 | 日志丢失不影响业务，不回放 |

判据是「内容是不是真源的一部分」。两类不得合并成一条规则。

## 4. 阈值

**例外优先**：`summary` 与 `summary_idx` **一律外置**，不走字节阈值——前者被 `eventSummary` 硬截到 512 B，后者是该输入的一/二元组展开（实测上限 1,523 B），按 2 KB 判定永远不会外置。

其余新增内容字段沿用默认阈值：`payload_bytes < 2048` 内联库，`>= 2048` 外置，并记 `payload_bytes` 留痕。

## 5. Markdown 投影边界

定时任务从 `memories` + JSONL 单向蒸馏出 `memory/YYYY-MM-DD.md` 等文件。人工可改，但**改动不回写 DB**；P0 不定义 `source_ref` / `version` 导入协议。回写须另立契约并受 VALUE.md §4 约束。

## 6. 完整性与巡检

- `RefHealth(locator, wantHash)` 只读校验三件事：文件存在、字节 hash 匹配、`#seq` 可定位；不改库、不删文件。
- P0 巡检只暴露结果，不新增 `ops-watch.md` 判据、不改 R 编号；判据变更须同步文档与脚本两处。
- 备份顺序：冻结写 → `mem.db` 一致性备份 → 复制 JSONL 目录 → 恢复写。观测层允许最终一致。
- 孤儿文件 GC（无引用且超保留期）属于 P1+；删除日期目录前仍需 dry-run 列清单并确认。

## 7. P0 边界

本契约落地范围为：契约文档、`RefHealth` 巡检骨架及单测、设计文档决策状态更新。不迁存量数据、不改 schema、不改 `mem search` / `mem recall` 等生产读路径。
