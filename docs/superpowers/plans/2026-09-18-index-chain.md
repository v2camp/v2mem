# v2mem 索引链补全 — 实施计划（2 工作流并行）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把价值主张 §1「索引链」里尚未落地的一头一尾接上 —— ① L1 里的**类码表**（回答"什么时候该查"）
② `mem asset` **反查入口**（回答"这条记忆还成立吗"）。两者都不新增表 / 不新增列 / 不改 schema 语义，
也都不进检索关键路径。

**Architecture:** 两个 WS 互不写同一文件，可并行。各走独立 worktree（AGENTS.md「任务流程」），
各自过 `make cover-gate` 后再合入 main。

**Tech Stack:** Go（CGO_ENABLED=0）、SQLite JSON1（**本机已实测可用**，见 WS2 现状核实）、
现有 `internal/store` / `cmd/mem` 分层。

**来源 spec：** [`docs/VALUE.md`](../../VALUE.md) §1.1–§1.4、§5；[`docs/design-provenance.md`](../../design-provenance.md) §1.3 / §6。

---

## 前置裁决（开工前必须定，决定 WS1 的任务边界）

VALUE.md §5 写「① 类码表落地（L1 契约改写 + `mem notes` 输出类码）」。但对代码核实后，
`mem notes` 的既有职责是**硬规则小字条**（`kind=rule` 或全局 + 高 salience），
而 VALUE.md §1.2 明确类码表与它是**并列的两条通道**（硬规则管"不能做什么"，类码表管"什么时候该去查"）。
故「`mem notes` 输出类码」有两种读法，**须先裁决**：

| 候选 | 含义 | 影响 |
|:---|:---|:---|
| **A（推荐）** | `mem notes` 扩展为 L1 可注入内容的**统一生成器**：先输出类码表（常数 4 行），再输出硬规则小字条 | 改 `notes.go`：新增类码表段落；`mem hook` 可复用同一来源 |
| **B** | 新增独立命令（如 `mem codes`）专管类码表输出，notes 不动 | 多一个子命令；两条通道在 CLI 层也彻底分开 |

推荐 A 的理由：类码表与硬规则最终都注入 L1，同一命令产出可避免"两块内容两个来源、预算各算一遍"
的重复记账；但 B 在职责上更干净。**该裁决由项目所有者做，不得由执行 agent 自行选定。**

---

## Sequence 总览

| WS | 内容 | 依赖 | 验收判据（VALUE.md §5 原文） |
|:---|:---|:---|:---|
| WS1 | 类码表落地：L1 契约改写 + `mem notes` 输出类码 | 前置裁决 | ① **L1 文件仍 ≤ 2KB** |
| WS2 | `mem asset` 反查：只读命令 + 表达式索引 | 无 | ② **`mem asset <path>` 能列全该文件下的记忆** |

两个 WS 触碰的文件无交集，可并行；最终一次合入 main。

---

# WS1 — 类码表落地

## 现状核实（2026-09-18，对代码对账，非对文档对账）

| 事实 | 证据 |
|:---|:---|
| L1 指令块由 `instructionBlock()` 生成，**当前是命令清单**而非触发条件表 | `cmd/mem/init.go`：段落为「### 容量纪律」「- 查历史决策…」「- 记下新的原子事实…」 |
| DESIGN.md §6 的契约**示范**已要求"写清什么时候查"，但正文示例仍是泛化描述 | `docs/DESIGN.md` §6「钩子必须写清『什么时候查』（触发条件），而非只说『你有记忆库』」 |
| 类码表**当前不存在**于代码、L1 文件或任何文档（只在 VALUE.md §1.2 定义） | 全仓 grep `类码` 仅命中 VALUE.md 与 memory |
| L1 硬约束是 ≤ 2KB / 40 行，且已有体量守卫命令 | `docs/DESIGN.md` §6；`mem budget --file <文件> --max-chars N`（超限非零退出） |
| `mem notes` 输出硬规则小字条，排序稳定（幂等） | `cmd/mem/notes.go`：`collectRules` 按 `(project, content, id)` 排序 |

类码表定义（VALUE.md §1.2，**照抄，不得增删**）：

| 类码 | `kind` | L1 里写的触发条件 |
|:---|:---|:---|
| `P` | `preference` | 涉及用户 / 项目偏好、默认选择、禁用做法时 |
| `D` | `decision` | 要复述「为什么这么定」时 |
| `X` | `pitfall` | 遇到报错、反常现象、曾被坑过的路径时 |
| `T` | `task` | 要确认「上次做到哪 / 还剩什么」时 |

三条不可违背的纪律（VALUE.md §1.2）：**不列举 `fact`**（入库默认值，写不出触发条件）；
**类码只是 `kind` 的别名**（真理源仍是库里的 `kind` 字段，不新增表/列/改 schema/改门禁）；
**类码表是常数**（4 行，不随记忆条数增长）。

## Task WS1.1: 类码表常数落到 Go 侧单一真理源

- [ ] **Step 1: 写失败测试**（新建 `cmd/mem/kindcodes_test.go`）：断言 4 条、顺序为 `P/D/X/T`、
  `kind` 取值与库里一致（`preference/decision/pitfall/task`）、**不含 `fact`**、
  每条触发条件非空且 ≤ 24 个汉字（L1 预算所限）。
- [ ] **Step 2: 跑测试确认失败**：`go test ./cmd/mem/ -run TestKindCodes`。
- [ ] **Step 3: 实现** `kindCodes` 变量（`[]struct{ Code, Kind, Trigger string }`），
  放在 `notes.go` 或新文件 `kindcodes.go`。注释写明"真理源是库里 kind 字段，此表只是短别名"。
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: Commit**（`feat: 类码表常数（P/D/X/T）`）。

## Task WS1.2: `instructionBlock` 改写为类码表

- [ ] **Step 1: 写失败测试**（`cmd/mem/init_test.go` 追加）：
  - 断言生成的指令块含 4 行类码表（`P`/`D`/`X`/`T` 各一行且带触发条件）；
  - 断言**不含** `fact` 的类码行；
  - 断言块内**不再出现**被替换掉的命令清单原文（防两份表述并存）；
  - 断言 `runeLen(block) <= 2048`（判据 ① 的单元级护栏）。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现**：改写 `instructionBlock()` 的钩子段，用类码表替换现有命令清单；
  保留"`mem` 不可用时继续作答并说明未检索"的降级路径（DESIGN.md §6 要求）。
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: 端到端验证判据 ①**：`mem init --dry-run` 取输出 → 写入临时 L1 文件 →
  `mem budget --file <临时文件> --max-chars 2048` 必须退出 0。
- [ ] **Step 6: Commit**（`feat: L1 契约改写为类码表`）。

## Task WS1.3: `mem notes` 输出类码

> 依赖**前置裁决**。按 A 实施则改动 `notes.go` 的 `renderNotes`；按 B 则新增子命令并在 `main.go` 注册。

- [ ] **Step 1: 写失败测试**：断言输出**首段为类码表**（4 行）、**其后为硬规则小字条**；
  断言多次运行输出逐字节一致（沿用既有幂等纪律）；断言 `--json` 模式下两块内容**结构化可分辨**
  （不得靠解析文本切分）。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现。**
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: 回归**：`mem hook` 的注入体量与去重逻辑不受影响（`make cover-gate` + 既有 hook 用例全绿）。
- [ ] **Step 6: Commit**（`feat: mem notes 输出类码表`）。

## Task WS1.4: 文档契约同步

- [ ] **Step 1:** 更新 `docs/DESIGN.md` §6 的 L1 契约示意，把示例里的「## 记忆钩子」段替换为类码表。
- [ ] **Step 2:** 在 `docs/VALUE.md` §1.1 表格的「现状」列，把 ① 类码行从"待升级"改为"已落地"。
- [ ] **Step 3: Commit**（`docs: L1 契约同步为类码表`）。

---

# WS2 — `mem asset` 反查

## 现状核实（2026-09-18，对代码对账）

| 事实 | 证据 |
|:---|:---|
| `provenance` **写侧有、读侧无**：仅 `ingest` 写入并经 `sync` 传播，全仓无任何反查入口 | `cmd/mem/level1.go:112` 构造 `store.Provenance`；`internal/store/sync.go` 传播；grep 无读取方 |
| 存储形态是**单 JSON TEXT 列**（可空，存量 NULL） | `internal/store/store.go`：`ensureColumn(db,"memories","provenance","TEXT")` 幂等 ALTER |
| `File` 是**相对工程根**的路径，非绝对路径 | `store.Provenance` 字段注释；`level1.go` 注释「ingest 在工程目录内运行，直接可用相对路径」 |
| 命令分发表是 `main.go` 的 switch，逐命令一个 `case` | `cmd/mem/main.go:118-162` |
| **SQLite JSON1 实测可用**（本次验证，非推测）：`json_extract` 与 `->>` 均正常；表达式索引 `CREATE INDEX … ON memories(json_extract(provenance,'$.file'))` 建得起来，且查询计划为 `SEARCH memories USING INDEX idx_prov_file (<expr>=?)` | 临时程序实测（4 行样本：命中 2 条、NULL 行正确不参与匹配）；验证脚本用后即删 |

## 边界（VALUE.md §1.3/§1.4，**明确不做**）

- **不比对文件 hash、不自动标记失效、不替 Agent 判断"过期了没"** —— 这是新增红线
  「二级索引只指向、不判定」。理由是架构性的：一旦这级索引承担判定，就需要语义模型，
  模型进关键路径会摧毁检索 ~9ms 的前提。**判定留给 Agent**：它拿到 `file:line` 自己去读源文件。
- 资产只到**文件**一级（`file` + `line` + `preview`）；**不含** git commit、章节锚点、会话 id、外部 URL
  （这些当前没有写入路径，不建空索引 —— 空索引会被误当成"索引里没有 = 不存在"）。

## Task WS2.1: 表达式索引（幂等）

- [ ] **Step 1: 写失败测试**：老库 Open 后索引存在；重复 Open 不报错（幂等）；
  新库建表即带索引。断言方式用 `PRAGMA index_list(memories)` 或 `sqlite_master` 查询。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现**：在 `store.Open` 的 schema 初始化处加幂等的 `CREATE INDEX IF NOT EXISTS`。
  注意**不要**走 `ensureColumn`（那是加列用的），需新增一个同风格的 `ensureIndex` 辅助。
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: Commit**（`feat: provenance.file 表达式索引`）。

## Task WS2.2: store 层按资产反查

- [ ] **Step 1: 写失败测试**（`internal/store/store_test.go` 追加）：
  - 命中：两条 `file=AGENTS.md`（不同 `line`）都能返回，且带 `line` / `preview`；
  - **NULL 不匹配**：无 provenance 的行不得出现在结果里；
  - **工程维度**：同一 `file` 在不同 `project` 下互不串（`project` 为空表示全局，按既有 `List` 的语义）；
  - **大小写与尾斜杠**：`./AGENTS.md`、`AGENTS.md/` 等输入的归一规则与既定选择一致。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现** `func (s *Store) MemoriesByAsset(file, project string, scope string) ([]AssetHit, error)`：
  用 `json_extract(provenance,'$.file') = ?` 精确匹配；`scope` 沿用既有约定
  （`current` = 本工程 + 全局 / `all` = 跨工程 / `global` = 只要全局）。
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: Commit**（`feat: store 按 provenance.file 反查`）。

## Task WS2.3: `mem asset <path>` 命令

- [ ] **Step 1: 写失败测试**（`cmd/mem/asset_test.go`）：
  - 建带 `.git` 的临时工程 → `ingest` 一个 md → `mem asset <该文件相对路径>` 能列出这些记忆；
  - 传**绝对路径**时归一为相对当前工程根后仍能命中；
  - 传未在库中的路径 → 输出"无"且**退出码为 0**（查不到不是错误），`--json` 下为 `[]`；
  - 无 provenance 的库（老库）→ 不报错，输出"无"。
- [ ] **Step 2: 跑测试确认失败。**
- [ ] **Step 3: 实现**：`main.go` 注册 `case "asset":`，新增 `cmdAsset`；
  参数 `--scope`（默认 `current`）、`--project`、`--json` 复用 `common`；
  输出每条含 `id / project / line / preview / content` 摘要，**不输出判定性字样**（如"已失效"）。
- [ ] **Step 4: 跑测试确认通过。**
- [ ] **Step 5: Commit**（`feat: mem asset 反查命令`）。

## Task WS2.4: 端到端判据 ②

- [ ] **Step 1:** 在真实工程上跑一遍：`mem ingest --project <工程> <某 md>` →
  `mem asset <该 md 相对路径>` → 与 `mem ls --project <工程>` 中该文件的条目**逐条对数**，
  确认"能列全"（判据 ② 的字面要求）。
- [ ] **Step 2:** 记录实测条数进提交信息（判据要留证据，不留断言）。
- [ ] **Step 3: Commit**（如无代码改动则并入上一条）。

## Task WS2.5: 文档登记

- [ ] **Step 1:** `README.md`「命令一览」的「写入与检索」表加 `mem asset <path>` 一行，
  说明**只指向、不判定**（不给使用者造成"能判失效"的预期）。
- [ ] **Step 2:** `docs/DEVELOPER.md` 目录结构补 `asset.go`（若新增文件）。
- [ ] **Step 3:** `docs/VALUE.md` §1.3 的"本期补齐方向"改为已落地，并保留"不做"清单。
- [ ] **Step 4: Commit**（`docs: 登记 mem asset 命令`）。

---

## 合入 main 前的总验收

- [ ] `make cover-gate` 通过（新增文件必须**包内视角**达标，不接受靠 `cmd/mem` 的集成测试刷分）。
- [ ] 判据 ①：改写后的 L1 指令块 ≤ 2KB（`mem budget` 退出 0）。
- [ ] 判据 ②：`mem asset <path>` 能列全该文件下的记忆（有实测条数记录）。
- [ ] 红线自查：`mem asset` 的代码与输出里**不存在**任何 hash 比对或失效判定逻辑。
- [ ] 两个 worktree 已清理、分支已删（AGENTS.md「任务流程」收尾要求）。
