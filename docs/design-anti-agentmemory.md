# 不照搬 agent-memory 的 54 tools / iii

> 关联：[`VALUE.md`](./VALUE.md) 红线、[`design-write-governance.md`](./design-write-governance.md) 三态。
> 对照对象：[agent-memory.dev](https://www.agent-memory.dev/)。这里只写取舍，不写它的实现。

v2mem 默认**零常驻、无 HTTP、无 iii**。可以借鉴的是事件模型、离线 spool、import-jsonl、PreCompact 重注入、secret scrub。实现留在 Go 单二进制里：同库另表 + CLI。

## 五条不搬的理由

1. **定位**。v2mem 是单文件二进制、零服务、FTS 本地检索。agent-memory 依赖 Node 和常驻 iii（:3111 / :3113）。照搬就放弃这个差别。
2. **工具面**。54 个 MCP 工具（leases、signals、routines、graph、team…）和「记忆本身四操作」相反。本仓库 MCP 是 `search` / `add` / `ls` / `touch`，外加一个只读 `recall_context`。没有 accept，没有 distill。
3. **写治理**。对方把 consolidate 收进主记忆。VALUE 要求梦和加工进隔离区。Stop 上直接 consolidate 进 `memories` 是违规。这里是观测 → `candidates` → 显式 `mem accept`。
4. **供应链**。iii pin、多端口、viewer、54 工具矩阵，和「直接依赖只有 modernc sqlite」冲突。
5. **可借鉴、不搬栈**。observations、spool、import-jsonl、PreCompact、secret scrub 用 Go 表和子命令做完。`mem serve` 一类 loopback **不进** `mem init`，本波也不实现。评测沙箱、写时 channel、统一 scrub、观测 event_id 同样留在这个二进制里：临时库、没有常驻进程、没有 HTTP、consolidate 不直接写主库。

## 默认运行时

| 模式 | 行为 |
|:---|:---|
| 默认 | hook 同步写 `observations`。蒸馏靠下次 `SessionStart` 的规则抽取，或用户手动 `mem distill` |
| `MEM_DISTILL_SPAWN=1` | Stop / SessionEnd 末尾再拉起一次短命 `mem distill --provider none`。进程退出即结束，不是服务 |
| spool | 写库失败时追加 `~/.v2mem/spool/<session>.jsonl`（0600）。SessionStart 有界 drain |

`ps` 里不应有常驻的 `mem`。没有监听端口。
