---
name: mem-remember
description: 把一条原子事实写进 v2mem。一条只记一个事实，并标 kind。
---

# mem-remember

一条只写一个事实，不写长段落。

优先 MCP `add`（`content` 必填）。`kind` 用 `preference` / `decision` / `pitfall` / `task` / `fact`。
降级：`mem add --kind decision|pitfall|preference|fact "<原子事实>"`

用上了某条记忆再 `touch`。不要把候选或蒸馏结果直接写成永久记忆；那要走 `mem accept`。
