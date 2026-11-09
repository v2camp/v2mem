---
name: mem-distill
description: 用 CLI 把观测抽成候选，再显式采纳；候选可在会话内用 MCP 工具就近裁决，distill 不在 MCP 里。
---

# mem-distill

蒸馏不进主库。MCP 里没有 distill；`accept` / `reject` 是会话内 Agent 的**显式**裁决（不是自动采纳），
与下面 CLI 走同一套 revision CAS 和隔离区升格。`SessionStart` 会提示本工程还有几条 pending，可就近裁决。

```bash
mem distill --provider none --session <id>
mem candidates ls
mem accept <id前缀>
mem reject <id前缀>
```

`--provider none` 用规则抽取，不需要 API key。本地模型是可选项，失败也不该挡住会话。
采纳之后 `source=distill-accept`，这时 `mem search` 才查得到。
