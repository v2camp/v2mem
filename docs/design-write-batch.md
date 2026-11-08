# 批量归档：写侧延迟治本方案（方案 B）

> **状态**：设计定稿。P1 归档层 API 已落地但**默认未接入** `Engine.Capture` / hook；P2 生产切换待灰度。
> 价值判断以 [`VALUE.md`](./VALUE.md) 为准；判据以 [`ops-watch.md`](./ops-watch.md) §3 为准。本文只定义「怎么做」，不重定价值与阈值。
> 起因见 2026-11-08 巡检：R10 写侧延迟从健康期 3–20ms 劣化到 p95 2847ms，而现行判据此前不对写侧判级。

## 0. 为什么不是「批量事务」（实证否决）

原提议是把逐条独立写账合成单事务。**四组独立实测一致否定了它作为治本手段**：

| # | 方法 | 关键结果 |
|:---|:---|:---|
| 1 | 生产 `audit.jsonl` × `observations`（n=145） | 写入行数 vs Stop 耗时 Pearson **r=0.885**；0 行 5.8ms → 1–5 行 32ms → 6–20 行 203ms → 21–50 行 407ms → 51–200 行 1686ms。单次 Stop 均值 25.6 行、最大 93 行 |
| 2 | 真实二进制黑盒（临时 db/audit/transcript，N=0/1/10/50/93） | 26.5 / 32.5 / 104.5 / 488.5 / **1011.5ms**，斜率 **10.6ms/行**，与生产吻合；**每例 WAL 帧均为 0** |
| 3 | C-sqlite 隔离基准（N=93） | 逐条事务 29.0ms、单事务 17.1ms（**仅 1.7×**）、无 segment 1.3ms；WAL 撑到 7058 帧 + 长读者仍 28.5ms |
| 4 | Go 探针（`modernc.org/sqlite` 同驱动，N=93） | 仅 `writeArchive` **7.83ms/条（88%）**；完整 `Engine.Capture` 8.85ms/条 → DB 事务+查重+水位 **1.02ms/条（12%）**；裸 INSERT 事务粒度收益 7.7× 但绝对值仅 2ms/93 条 |

**结论**：把 DB 部分全部消掉，93 行也只从 1011ms → 约 916ms（**−9.5%**），救不了 R10。第 2 组还证伪了最初的「WAL 膨胀导致延迟」假设——hook 进程退出后 WAL 归零，WAL 只是常驻 MCP 连接留下的产物，与延迟无因果。

真正瓶颈是 `internal/evidence/archive.go:writeArchive`：每条执行 `MkdirAll` → `CreateTemp` → gzip → `tmp.Sync()` → `os.Rename` → `syncDir()`，即**每条约 2 次 fsync**，N 条即 2N 次。**一个 gzip JSONL 写 N 行、一次 fsync**，理论上把 7.83ms/条压缩成 7.83ms/批，93 行约 103ms（**−90%**）。

## 1. P0 契约核查（决定方案形态）

| 事实 | 出处 | 对方案的修正 |
|:---|:---|:---|
| 生产代码**没有读归档的路径**；`LookupEvidence` 只查 DB，归档是「只写的事实源」 | `internal/store/evidence.go:153` | 批量归档**不需要改生产读侧**；但 `VALUE.md` §4.1 声称「事实溯源读归档 Raw」目前只在测试里实现，应补产品级读取 API |
| `readArchive` 与 `archivedRaw` 都用 `json.Unmarshal(整个 gzip 流)`，**只认一行**；后者还丢弃 `#seq` 后缀 | `internal/evidence/archive.go:170`、`cmd/mem/fidelity_e2e_test.go:52` | 必须新增按 `<path>#<seq>` 定位**非首行**的读取能力，并升级这两处测试辅助 |
| `evidence_archives.archive_hash` 是 **PRIMARY KEY** | `internal/store/schema.sql:199-211` | N 条账不能指向同一 hash → **必须一批一行账**，不能只改归档层 |

`spool.go:drainOneSpool` 已有逐行 `bufio.Scanner` + `json.Unmarshal` 范式，可直接复用；`segmentRecord` 自带 `event_uid/seq`，`.jsonl.gz` 后缀本就是 JSONL。

## 2. 目标、非目标与不变量

**目标**：单次 Stop 的 transcript 补写从「每条一次归档 fsync + 一次 DB 事务」变为「每批一次归档 fsync + 一次 DB 事务」；R10 连续 3 个 12h 窗口 PASS。

**非目标**：不做常驻进程/异步 flush（违反 `VALUE.md` §4 红线）；不改检索路径；不改 `kind` 枚举；不删观测行或归档来降耗时。

**不变量（改动不得触碰）**：
- 归档 = 打码后的有界长文；单条 Raw 上限 32000 rune（`observeRawMaxRunes`）、证据摘要 512 字节、提案投影 400 rune 的分层口径不变。
- `archive_hash` = 压缩后字节的 sha256；gzip header 固定（`ModTime` 零值、`OS=255`）以保证字节可复现。
- `Fidelity`（full / pointer / none）与 `ProposalEligible` 语义不变；`unparsed` 仍是设计内保真指针，不因批量而改判。
- `RawLocator = "<path>#<seq>"` 格式不变；`event_uid` 仍是先写赢的幂等键。

## 3. 设计

### 3.1 归档层（P1，本轮落地）

新增 `writeArchiveBatch(evs []RawEvent) (archivedSegment, error)`：与 `writeArchive` 同流程，但在一个 gzip 流里按序写 N 行 `json.Marshal(recordOf(ev)) + "\n"`，**只做一次 `tmp.Sync()` 与一次 `syncDir()`**。返回 `{hash, path, size}`，其中 hash 仍是整份压缩字节的 sha256。

新增 `ReadArchiveRecord(locator string) (segmentRecord, error)`：拆出 `#seq`，逐行扫描 gzip 流，按 `rec.Seq == seq` 返回；无后缀时兼容返回首行（保持既有单条归档可读）；seq 不存在返回明确错误。这是补上 §4.1 声称的事实溯源能力，也是批量归档的必要配套。

单条 `writeArchive` **保持原样**，P1 新 API 不接入任何生产调用者。

### 3.2 账本层（P2，本轮不实现）

一批对应 `evidence_archives` **一行**：`from_seq` / `to_seq` 取批的 seq 区间，`raw_count=N`，`raw_hash_concat` 按批内顺序拼接，`UNIQUE(source_id, generation, from_seq)` 正好锁住批起点；`observations` 仍为每事件一行，`raw_ref` 指向同一 `<path>#<seq>`。

需要 `Store.CaptureEvidenceBatch`：从现有 `CaptureEvidence` 抽出 `captureEvidenceTx(tx, in)` 供单条与批量复用，在一个事务内完成「水位推进 → 逐条判重/seq 校验 → 插 N 行 observations → 插 1 行 archive 账」。**判重与 seq gap 语义必须与单条路径逐字一致**，否则重放会分叉。

### 3.3 失败与幂等（P2 的关键约束）

- 顺序仍是「先归档后写账」。DB 失败时保留整份 segment，并**整批写 spool**（spool 已是多记录 JSONL，`drainOneSpool` 可逐行回放）。
- 批内部分重复（重放）时，按 `event_uid` 跳过已存在行、只写新增；**不得整批拒绝**，否则游标推进会卡死。
- 批边界必须确定性：以「单次 Stop 的 transcript 增量 + 游标偏移 + 批序号」定义，同输入同顺序必须得到同一 `archive_hash`，否则重放身份漂移。
- 掉电可见性不能降级：文件 fsync 与目录 fsync 各保留一次，只是从 2N 次降到 2 次。

### 3.4 批次边界与预算

自然批次 = 一次 Stop 的 transcript 增量（实测均值 25.6 / p95 93 行）。沿用 `transcriptStopMaxRecords=5000` 上限，另加**字节预算**（单条 32000 rune × 批可能很大），超限按序切批；切批点记入游标，保证可重放。

## 4. 分阶段与回滚

| 阶段 | 内容 | 状态 |
|:---|:---|:---|
| P0 | 契约核查（见 §1） | 已完成 |
| P1 | `writeArchiveBatch` + `ReadArchiveRecord` + 单测；**默认不接入** | 本轮 |
| P2 | `CaptureEvidenceBatch` + `Engine.CaptureBatch` + hook transcript 接入；`MEM_EVIDENCE_BATCH` 开关灰度，默认关 | 待做 |
| P3 | 连续 2 个 12h 窗口观察后转正、清理单条旧路径 | 待做 |

**回滚**：P2 只需关闭 `MEM_EVIDENCE_BATCH`，单条路径保持完整可回退；P1 未接入，无运行时影响。

## 5. 风险与必守回归

| 风险 | 缓解 |
|:---|:---|
| 批边界不确定 → `archive_hash` 漂移 | 批由游标偏移 + 批序号确定；单测覆盖「同批写两次 hash 相同」 |
| 多行 JSONL 读侧只认首行 | `ReadArchiveRecord` 按 seq 定位；测试覆盖非首行 |
| 批内重复导致整批失败 | 逐条判重跳过；单测覆盖「半批已存在」 |
| 保真语义被顺手改坏 | `fidelity_e2e_test.go` 用例 C、`TestTraeRecordEvidenceFidelity` 为必守回归 |
| 覆盖率被新增代码稀释 | `internal/evidence` 下限 81%、`internal/store` 下限 82%，`make cover-gate` 硬门禁 |

## 6. 验收判据

1. 同款黑盒实验（N=93）：Stop 耗时从 1011ms 降到 **<200ms**。
2. R10 连续 3 个 12h 窗口 PASS；R11 不再因单批写入反复越线。
3. 归档文件数显著下降（每批一个，而非每条一个）。
4. `make test` / `make cover-gate` / `make test-watch` / e2e 全绿，覆盖率下限不下调。
5. 重放两次不新增 observations 行，`archive_hash` 稳定。

## 7. 复现方式

基准脚本（临时产物，不入库）：`/tmp/v2mem-hook-bench.py`（真实二进制黑盒）、`/tmp/v2mem-batch-bench.py`（C-sqlite 隔离对照）。Go 探针为一次性临时测试，跑完即删。原始数据与结论见工作区日志 `.workbuddy/memory/2026-11-08.md`。
