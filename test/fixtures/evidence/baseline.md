# 阶段 0 基线（2026-11-04）

本文件固定写侧与读侧改造前的事实，供后续 worktree 从 main 继承。所有数值均来自本机只读实测或确定性夹具。

## 1. 本机库快照

| 表 | 数量 |
|:---|---:|
| `memories` | 9 |
| `observations` | 19 |
| `candidates` | 0 |
| `distill_runs` | 19 |
| 其中 `distill_runs.empty` | 13 |
| 其中 `distill_runs.ok` | 6 |

这些值同时写入 `manifest.json.baseline`。

## 2. 夹具

- `events-v1.jsonl`：240 条事件，超过当前蒸馏窗口的 200 条上限。
- `events-v2.jsonl`：10 条事件，代表源轮转后 generation=2、序号重新从 1 开始。
- 前 20 条 `events-v1.jsonl` 使用同一个秒级时间戳，用于验证时间水位不足。
- seq 20 与 seq 21 是同摘要 `stop`、不同 `event_uid`，用于验证内容去重吞并不同事件。
- seq 100 含 12004 字符工具输出，用于验证长输出不被摘要截断替代。
- seq 240 是最后一条助手结论，用于验证 Stop/transcript 证据。
- `spool-segments.jsonl`：4 个独立段、4 个并发 writer，用于验证 append/drain 并发。
- `deleted-source.json`：源路径已删除且要求 archive replay。

`TestEvidenceFixtureCoversKnownLossModes` 验证这些夹具全部存在且自洽。

## 3. 读侧现状回归

`TestReadPoolBaselineNoBackfillAfterRuleDedupOrBudget` 固定两种当前折损：

1. `rule dedup`：store 已有每路 4× 过取与 RRF，但融合结果仍先截到 `limit=3`；notes 去重在 3 条上做减法，后续合格候选不回补。
2. `budget truncation`：长条目在小预算下放不下后，短条目也不补位，最终注入为空。

准确表述是「融合后先截 3 条，再过滤/预算截断且不回补」，不是「没有过取」或「单路召回」。

## 4. 门禁与评测基线

执行：

```text
make test
make cover-gate
make eval-gate
./bin/mem eval recall --gold-dir gold --k 5 --db ~/.v2mem/mem.db
```

结果：

| 项目 | 基线 |
|:---|:---|
| `make test` | 9 个包全部 PASS |
| 覆盖率 TOTAL | 84.1%，下限 83% |
| `cmd/mem` 覆盖率 | 82.5%，下限 81% |
| `internal/store` 覆盖率 | 83.2%，下限 82% |
| `make eval-gate` | GREEN，Recall@5=1，StrictAll@5=1，门槛 0.60 |
| 真实 gold | 9 个有效样本；Recall@5=100%，MRR=0.861，StrictAll@5=100% |

`gold/seed-audit-v0.gold.jsonl` 原先引用的 6 组 hash 已不在当前 9 条记忆中，初次基线为「有效样本 0、6 条被剔除」。阶段 0 已用来自 audit 的真实查询与当前库 hash 刷新该文件，使真实 gold 恢复为可评测基线；旧结果作为「gold 会随库重建失效」的证据保留在本次记录中。

## 5. Host adapter 结论

`docs/design-write-governance.md` §11 记录阶段 0 结论：`host_safe=false`。

当前宿主 CLI 能触发 hooks，但尚无 Proposal run/depth 标记、origin_run_id、深度上限与子会话隔离，自动调度从 Stop 拉起子 Agent 会产生递归风险。阶段 2 契约测试通过前，自动调度走 Local LLM / backlog，Host 仅显式调用。
