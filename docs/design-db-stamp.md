# 数据库写入者版本戳与降级守卫

## 为什么需要

同一个库 `~/.v2mem/mem.db` 会被多个入口打开：CLI、hook（每次会话多次）、MCP server，
以及不同机器 / 不同 harness 上的二进制。多入口并存是常态——`~/go/bin/mem` 软链、
某个 harness 里写死的绝对路径、一份忘删的旧 release，都可能同时存在。

库升级后，旧二进制仍会照常打开新库、按旧假设读数据：**不报错，但读出来不对**。
所以要把「这个库是哪个版本写的」写进库，并在「库比二进制新」时明确拒绝打开。

## 为什么不用 `PRAGMA user_version`

`user_version` 已被 `markLegacyOnce`（`internal/store/evidence_schema.go`）当作
**一次性迁移门控**占用：`>=1` 就早退，跑完无条件写 `=1`。它表达的是「这段迁移跑过没有」，
不是「这个库是什么版本」。两套语义挤一个整数位必然打架：门控会把版本戳覆盖回 1，
版本戳也会让门控误判成已迁移而跳过真正的数据修补。所以用键值表 `db_meta(k, v)`：
`schema_version` / `written_by` / `first_seen`，后续加 `last_open_by`、`migrated_at`
都不用再动 DDL，也不必再抢一个 PRAGMA 整数位。

DDL 写在 Go 里（`EnsureWriterStamp`），**不写进 `schema.sql`**：`schema.sql` 只对新建库
生效，存量库没有这张表，而存量库恰恰是最需要认版本的那批。用 `ensureColumn` 同款幂等 DDL 兜。

## 四个分支

| 库里 `schema_version` | 行为 |
|:---|:---|
| 不存在 | 写入 `=CurrentSchemaVersion`、`written_by`、`first_seen`；`Fresh=true` |
| `== Current` | 只更新 `written_by`，保留 `first_seen`；`Fresh=false` |
| `< Current` | 就地升到 `Current` 并更新 `written_by`。**不跑迁移**：表结构迁移仍由 `Open` 里的 `ensureColumn` / `ensureEvidenceSchema` 系列负责，本函数只维护台账 |
| `> Current` | 返回 `ErrDBSchemaNewer`，拒绝打开，不改写库 |

建表、读、写在同一事务内完成：hook 会并发多次打开库，分多次自动提交会互相覆盖 `first_seen`。

## 为什么解析失败要容错而不是拒绝

`schema_version` 存了非整数或空串时，按「不存在」覆盖处理并返回 `Fresh=true`。
理由：这个库可能是用户唯一的记忆数据，认不出台账就拒绝打开会让人彻底无法恢复。
语义退化成「以当前二进制为准，重新认领这个库」——失去一次降级保护，换一条总能走通的路。

## 与 `markLegacyOnce` 的边界

- `markLegacyOnce`：**门控**。回答「某段一次性数据修补跑过没有」，用 `user_version`，
  只增不减，且只影响 legacy 行的标记。
- `EnsureWriterStamp`：**台账**。回答「这个库最后被哪个二进制、以哪个 schema 版本打开」，
  每次打开都刷新 `written_by`。两者互不读写对方的状态。

## 什么时候必须 +1 `CurrentSchemaVersion`

任何改变「读出来的数据形状」的迁移：加列且新代码依赖它、改索引语义、改 NULL 含义、
拆表/合表、改变已有字段的单位或取值域。只加新表 / 新索引（旧二进制读不到也不受影响的）
可不加。判断标准只有一条：**旧二进制读到迁移后的库，会不会静默错读**。会，就必须 +1。

## 版本史：v2 → v3（S10a 复核数据面）

`CurrentSchemaVersion` 从 2 升到 3。变化的是「读出来的数据形状」——新代码依赖这些列
（`visibilityWhere` 看 `memories.review_state`、复核视图看 `candidates.attempt`），
旧二进制读到迁移后的库会按旧形状解释新数据，故必须 +1。

| 表 | 新列 |
|:---|:---|
| `memories` | `review_state` / `reviewed_at` / `reviewed_by` |
| `candidates` | `status_reason` / `judge_reason` / `attempt` |
| `commit_decisions` | `model_tag` / `created_memory` |

**幂等 ALTER**：列与索引不写进 `schema.sql`（它只对新建库生效，且索引写在建表脚本里会
先于 ALTER 执行、旧库还没有列时直接让 `Open` 失败）。`Open` 里由 `ensureReviewSchema`
用 `ensureColumn` / `ensureColumnAdded` 逐列补，补完才建 `ix_mem_review_state` 与
`ix_commit_model`（`CREATE INDEX IF NOT EXISTS`，新建库与存量库走同一条路径）。
二次 `Open` 全部命中「列已存在」，不重跑任何 DDL。

**回填规则**：`backfillDirectWriteReviews` 只在 `review_state` 这一列**这次刚加上**时跑一次，
把 `review_state = ''` 且 `source IN ('human','llm','ingest','harness-summary','bench','test')`
的行标成 `confirmed`（`reviewed_at = created_at`、`reviewed_by = 'direct-write'`）。
只认可证明的直写来源：`distill-accept` 与空 `source` 的 legacy 行一律留未审核——
全库历史一律标已审核会让复核摘要永远看不到模型写入的行。挂在「刚加上」而不是每次
`Open`，是为了不让人工撤回过的行在下次打开时被悄悄改回 `confirmed`。

## `BinVersion()` 的口径

用 `runtime/debug.ReadBuildInfo()`：`Main.Version` 有值（release tag 或 Go 1.24+ 的
VCS 伪版本）就用它，否则回退 `devel`（可带 `+<rev 前 7 位>`）。它和
`scripts/derive-version.sh` 不是一回事：那边推导「下一个发布 tag」，这里读
「正在跑的二进制自述」。选 build info 是因为不需要 ldflags 注入，worktree 里
`go run` / `go build` 也成立。
