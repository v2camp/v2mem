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

- 定位串沿用现有归档契约：`<path>#<seq>`。`path` 是 gzip JSONL 文件路径（可为绝对路径，或相对数据根如 `content/2026/11/23/observations.jsonl.gz`）；`seq` 是分片内 1 起的行序号。
- 不引入设计稿里的 `#L<seq>` 变体；不带 `#seq` 只作兼容读取（等价首行），新写入必须带。
- 两种 hash 不能混用：`evidence_archives.archive_hash` 是**不可变归档**压缩后整文件字节的 sha256，由 `RefHealth` 校验；外置 `summary_hash` 是**规范 JSON 行**（不含结尾换行）的 sha256，由 `RefHealthLine` 校验。hash 单独存库，不塞进 locator。
- 当日分片只 append：每次写入增加一个 gzip member，历史 member 永不被重写或改名。整文件 hash 会随 append 变化，所以行级 hash 才是稳定 ref 的依据。
- 归档读取复用 `internal/evidence.ReadArchiveRecord`；外置内容读写统一走 `internal/store/summary_external.go`，不得新造第二套 JSONL 解析格式。

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

- `RefHealth(locator, wantHash)` 只读校验不可变归档：文件存在、压缩后整文件 hash 匹配、`#seq` 可定位；`RefHealthLine(root, locator, wantHash)` 校验外置内容的行级 hash 与 `#seq`，不读整文件 hash。两者都不改库、不删文件。
- 巡检只暴露结果，不新增 `ops-watch.md` 判据、不改 R 编号；判据变更须同步文档与脚本两处。
- 备份顺序：冻结写 → `mem.db` 一致性备份 → 复制 JSONL 目录 → 恢复写。观测层允许最终一致。
- 孤儿文件 GC（无引用且超保留期）属于后续阶段；删除日期目录前仍需 dry-run 列清单并确认。

## 7. 落地边界

P0：契约文档、`RefHealth` 巡检骨架及单测。P1：`summary` / `summary_idx` 全外置、显式 `mem migrate --summary-external`、迁移后 `fts_obs` 独立表、按 ref 回填读侧；不新增 R 判据、不做孤儿 GC、不做 P2 Markdown 投影。
