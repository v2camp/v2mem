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

## 和个人库的关系

这里的 hash 来自某份本机记忆库。托管 CI 上没有那份库。

**发布门禁不读本目录。** `make eval-gate` 用 [`eval/sandbox`](../eval/sandbox/README.md) 的合成 seed + 金标准，灌进临时库。本目录只留给本机：

```
mem eval recall --gold-dir gold --k 5
```

那一步打的是 `--db`（默认 `~/.v2mem/mem.db`），和发版无关。