# gold/ —— 金标准（gold）起步集

`mem eval recall` 的**人工标注**检索测评样本，与 `--gold`/`--gold-dir` 配合使用，
是支撑「这套记忆系统到底准不准」的唯一指标来源（自召回 `--auto` 只是下限自查，不具说服力）。

## 格式（JSONL）

每行一条 `GoldCase`：

```json
{"query": "<真实会话里的查询>", "gold": ["<content_hash 前缀>", "..."], "note": "可选说明"}
```

- `query`：应来自真实 audit 记录（`mem audit` 的 query），而非模型臆造的查询。
- `gold`：该查询在库里**应该命中**的记忆 `content_hash` 前 8~16 位。用哈希而非文本
  包含匹配，避免「同一事实换个说法就算命中」的自欺（见 `internal/eval` 口径）。

## 来源

起步集从 `~/.v2mem/audit.jsonl` 的真实会话查询里挑选，人工核对 `mem audit --tail`
还原出的内容后标注。**禁止**用 20 条 `--auto` 自召回当金标准。

## 版本化管理

- 文件命名 `v<N>-seed.gold.jsonl`，与发布 tag 对齐；改动随代码进 repo 版本化。
- 发版门禁（`make eval-gate`）读的是 `eval-policy.txt` 的 `recall5_min`，
  用 `--gold-dir gold` 合并本目录全部 `*.gold.jsonl` 后评 Recall@5。

> 起步期 gold 为空目录时，`eval-gate` 会因「目录下无 *.gold.jsonl」判定 RED ——
> 这是**fail-closed**：没有标注金标准就不允许发版，等补齐真实样本后再放行。