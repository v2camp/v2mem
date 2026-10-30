---
name: mem-recall
description: 按类码决定要不要查 v2mem。有 SessionStart 注入时不必每轮调用。
---

# mem-recall

命中下面的触发条件再查，不要凭空断言历史决策。

- P preference：涉及用户 / 项目偏好、默认选择、禁用做法时
- D decision：要复述「为什么这么定」时
- X pitfall：遇到报错、反常现象、曾被坑过的路径时
- T task：要确认「上次做到哪 / 还剩什么」时

优先 MCP：`search`，或只读 `recall_context`（`query` 必填）。
降级：`mem search --scope current --json "<关键词>"`

已经有 hook 注入时，不必每轮再调。
