---
name: mem-distill
description: 用 CLI 把观测抽成候选，再人工采纳。accept 和 distill 不在 MCP 里。
---

# mem-distill

蒸馏不进主库。MCP 里没有 distill，也没有 accept。

```bash
mem distill --provider none --session <id>
mem candidates ls
mem accept <id前缀>
mem reject <id前缀>
```

`--provider none` 用规则抽取，不需要 API key。本地模型是可选项，失败也不该挡住会话。
采纳之后 `source=distill-accept`，这时 `mem search` 才查得到。
