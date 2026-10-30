# eval/sandbox —— 发布门禁用的合成夹具

`make eval-gate` 读的是这个目录，不是 `gold/`，也不是 `~/.v2mem/mem.db`。

- `seed.jsonl`：灌进**临时库**的记忆和一条合成观测。正文是虚构的，没有个人信息。
- `recall.gold.jsonl`：查询对应 `content_hash` 前 12 位（`store.HashOf`）。
- 观测行只证明「合成会话可以灌入」；默认 `mem search` 不该靠它得分。

个人标注仍放 `gold/`，用 `mem eval recall --gold-dir gold` 打自己的库。那一步不参与发版。

收据写到 `eval/receipts/`（gitignore）：`manifest.json`、`scores.ndjson`、`evidence.ndjson`。evidence 只有哈希，没有正文。这是开发评测，不是竞品排名。
