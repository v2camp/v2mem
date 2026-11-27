# 已入库记忆的正文提炼（design-content-enhance）

> **定位**：回答「已入库的『无来源 / 过程叙述』记忆怎么变成可执行规则、凭什么可回滚」。
> **读者**：实现者（CLI 与存储写路径）；使用者看 §2 的命令示例即可。
> **判据权威**：[`VALUE.md`](./VALUE.md) §4「梦的产出不回写主库」及例外三条件。
> **相关**：[`design-data-lifecycle.md`](./design-data-lifecycle.md) §6（后审核治理）、
> [`design-admission.md`](./design-admission.md)（判定器与理由码）、
> [`cmd/mem/repair.go`](../cmd/mem/repair.go)（`repair evidence` 现有入口）。
> **状态**：命令契约已冻结（2026-11-28）；CLI 与本文同日推进。实现若与本文冲突，
> 以冻结契约 + 本文件为准，由主控收口。

---

## 0. 一句话

`mem repair evidence` 修**结构**，`mem repair content` 修**语义**：前者确定性补证据链、不动正文；
后者让模型把过程叙述改写成「触发场景 → 判断依据 → 处置」，非确定性、需后审核、按 tag 归档可回滚。

## 1. 定位与职责边界

**为什么需要它**：`internal/proposal` 的 `isProcessNarrative` 只拦**新写入**
（`internal/proposal/parse.go:403`，三条正则：第一人称动作、会话内指代、时点相对词）；
存量里已经进来的这类记忆不会被它清理。直接撤回会丢掉其中仍成立的知识，
一次性改写成规则比「删掉 + 重新交代」便宜。**改写不是新增知识**，因此不产生新行、不进候选表。

### 1.1 与 `repair evidence` 的分工

| 维度 | `repair evidence` | `repair content` |
|:---|:---|:---|
| 目标 | 结构不变式：`memory_evidence` 补齐 | 语义形态：正文可执行、去会话指代 |
| 确定性 | 确定性、幂等、无模型 | 模型、非确定性、每次归档 |
| 写路径 | 只加证据行 | 改 `memories.content` 及派生列 |
| 审核语义 | 不改 `review_state` | 回到未审核，`reviewed_by` 留空；归因在 `provenance.enhance_tag` |
| 回滚 | 不需要（补链可重复） | 按归档逐条校验后还原 |
| 失败面 | 只影响溯源 | 直接影响检索命中的正文 |

### 1.2 范围

**做**：按选择器挑候选；模型逐批改写；单事务落库；归档；按 tag / 文件回滚。

**不做**（边界比功能重要）：

- **不碰证据链**：不增删 `memory_evidence`，不改 `kind` / `scope` / `project` / 时间戳语义。
- **不做近重复归并**：新正文与同工程另一条撞 `content_hash` 时跳过并报告，合并归 `mem consolidate`。
- **不挂自动链路**：不接 `maintain`、不接 Hook、不进 MCP；只手动执行。
- **不产生新记忆 / 候选**：原地改写，库内条数不变。
- **不替代后审核**：改写结果默认回到未审核，等人工确认或修正。

---

## 2. 命令与选择器

```text
mem repair content [id...] [选项]
  --filter=narrative|overlong|all   选择器，默认 narrative（复用 IsProcessNarrative）
  --all                             等价 --filter=all
  --dry-run                         只列候选，不调模型、不写库
  --preview                         调模型出 before/after，不写库
  --limit N                         本次最多处理条数（默认 20）
  --batch N                         单次模型调用条数（默认 10，JSON 一进一出防串扰）
  --model-tag TAG                   归因标签，默认取 model.json 的 model_tag，缺失报错
  --include-confirmed               连人工已确认/修正的条目一起处理（默认跳过）
  --rollback <tag|file>             按归档回滚（逐条校验当前内容仍是本次改写结果）
  --json
```

### 2.1 候选选择

1. **显式 id 优先**：给了 `id...` 就只处理这些 id，忽略 `--filter` / `--all`。
2. **无 id 时按选择器**：
   - `narrative`（默认）：正文命中过程叙述判据（`IsProcessNarrative`）。
   - `overlong`：正文超过实现定义的长度阈值。阈值未在冻结契约里，**以命令输出为准**；本文不另立数字。
   - `all`：不过滤，全部存活候选（`--all` 等价；以实现为准）。
3. **默认跳过**：`review_state=withdrawn` 一律跳过；`confirmed` / `corrected` 默认跳过，
   除非显式 `--include-confirmed`（连人工确认过的也改，风险由操作者承担）。
4. **`--limit`** 是本次总量上限（默认 20），`--batch` 是单次模型调用的条数（默认 10），
   两者互不替代：40 条、`--limit 40 --batch 10` = 4 次调用。
5. **`--model-tag` 缺失即报错**：不静默用空 tag 或主机名兜底——归因缺失时归档失去意义。

### 2.2 三种执行形态

| 形态 | 调模型 | 写库 | 用途 |
|:---|:---|:---|:---|
| 默认（apply） | 是 | 是 | 正式改写并归档 |
| `--dry-run` | 否 | 否 | 只列候选 id 与筛选理由，零副作用 |
| `--preview` | 是 | 否 | 给人看 before/after，确认用 |

`--rollback` 与其他处理形态互斥：它只做还原，不选候选、不调模型。
`--json` 只改变报告形态（候选 / 处理 / 跳过 / 失败计数与逐条状态），不改变写库行为。

---

## 3. 模型交互与提示词（enhance-v1）

- **一进一出**：每次调用最多 `--batch` 条，输入是 JSON 数组，输出必须与输入**按 id 一一对应**。
  缺项、多项、未知 id、正文为空都算该批失败，**不降级成部分写入**——与
  `design-admission.md` §7「一输入必一输出」同一口径。
- **模型只产正文，不产动作**：没有 accept/reject，也不碰状态；写库、审核、归档全部由确定性代码完成。
- **提示词版本 `enhance-v1`** 是归因的一部分，必须随归档可查（见 §4.1 与 §6）。

### 3.1 目标形态

改写后的每条正文应能回答三件事：

```text
触发场景（遇到什么情况） → 判断依据（凭什么这么判断） → 处置（该怎么做 / 不该怎么做）
```

示例（仅示意，不作为断言）：

```text
改前：我刚才试了那个命令，报错了，发现得先 export CGO_ENABLED=0 才行
改后：CGO_ENABLED=0 构建 v2mem 时，若忘记关闭 CGO 会在 sqlite 驱动处报编译错误；构建前显式设置 CGO_ENABLED=0
```

### 3.2 硬约束（写进提示词并逐条校验）

1. **不新增事实**：只能压缩、重排、去指代化；库外知识与推测一律不得引入。
2. **禁止第一人称 / 会话指代 / 时点词**：「我 / 我们」「这个链接 / 那个报错」「刚才 / 本次 / 目前」。
3. **专名原样保留**：命令、路径、函数名、错误码、数字、机器名不改写、不翻译。
4. **不改语义类别**：不把 task 改写成 rule，不把 pitfall 的处置改成偏好；`kind` 由代码原样保留。
5. **模型不可用是常态**：明确报「模型不可用」，不输出「候选 0 条」——后者会被误读成「库很干净」。

---

## 4. 写路径与状态语义

**复用 `review correct` 的核心**（`internal/store/review.go` 的 `ApplyMemoryReview` 修正分支），
逐条事务内完成：

1. **CAS 前置**：只有当前 `content_hash` 仍等于选择时读到的 `old_hash` 才写。
   CLI 把该值作为 `expectHash` 传给 `RewriteMemoryContent`；不等时返回
   `ErrMemoryContentChanged`，报告为 `skipped`（error=`content_changed_since_selection`），
   不覆盖人工修改。期间被人修正 / 撤回 / 再次改写过的条目同样跳过并报告。
2. **正文字段**：`content`、`content_idx`（FTS 触发器据此重建索引）、`content_hash`、
   `mem_sigs`（MinHash 签名，否则模糊检索仍拿旧文本）一并重算。
3. **`provenance.original_hash` 只在首次改写时写入**：连续改写保留最初那次的 hash，
   不被上一次的中间值覆盖（与 `provenanceWithOriginalHash` 同语义）。
4. **另记 `enhance_tag` / `enhanced_at`**：随 `provenance` JSON 落档，不新增 schema 列。
   `enhance_tag` 是回滚与归因的唯一键。
5. **状态回到未审核**：`review_state` 置空，`reviewed_by` 同样留空——模型不是人工复核者，
   改写归因记录在 `provenance.enhance_tag` 与 `enhanced_at`。空状态即「未审核」（S10a 口径），
   可被 `mem review ls` / `mem show` 识别为模型改写产物，**不冒充人工确认**。
6. **冲突守卫**：新正文与同工程另一条撞 `content_hash` 时整条跳过、记 `skipped`，
   不自动合并——合并是 `consolidate` 的职责。

**重复执行不是幂等**：模型每次输出可能不同，每次改写都以当次归档为准；旧归档仍可用于回滚到更早正文。
需要稳定结果时先用 `--preview` 核对，或只对具体 id 执行。

---

## 5. 归档与回滚

### 5.1 归档

- 路径：`<db 目录>/enhance/<unix>-<pid>-<tag>.jsonl`，一次运行一个文件；文件名带 pid，
  同一秒内两个进程跑同一 tag 也不会混写。写入 db 目录的理由与 `plans/*.jsonl` 一致——
  回滚凭证必须与库同生命周期。
- 字段（冻结）：`id` / `kind` / `project` / `old_content` / `old_hash` / `new_content` /
  `new_hash` / `enhance_tag` / `at`。
- 部分失败也归档：成功写入的行必须有对应记录，否则回滚会出现无凭证的改写。
- tag 进文件名前做安全替换（路径分隔符等），**不允许借 tag 写出 db 目录**。

### 5.2 回滚

`--rollback <tag|file>`：值含路径分隔符或以 `.jsonl` 结尾视为文件；否则视为 tag，
处理该 tag 下的全部归档文件（按 `at` 倒序，后写的先还原）。

逐条规则：

1. 读当前记忆；**当前 `content_hash` 必须等于归档里的 `new_hash`** 才还原成 `old_content`。
   不等说明它已被人工修正、被再次改写或已删除——**跳过并报告**，绝不覆盖。
2. 还原时同样重算 `content_hash` / `content_idx` / `mem_sigs`。
3. 清理本次改写留下的 `enhance_tag` / `enhanced_at`；`provenance.original_hash` 仅当它等于
   本条 `old_hash` 时一并清理，否则（来自更早的人工改写）保留。
4. **只还原正文，不删行、不碰证据链与 kind/scope**；配 `--dry-run` 时只报告
   matched / skipped，零写入（沿用 `review rollback` 口径）。
5. 幂等：第二次回滚时 `new_hash` 已不匹配，全部计 skipped。

---

## 6. 治理：`VALUE.md` 例外三条件

正文改写是**模型产出写回主库**，按 `VALUE.md` §4 例外的三条授权，缺一即违约：

| 条件 | 本功能的可执行形态 | 失效后果 |
|:---|:---|:---|
| (a) 版本可归因 | 归档与 `provenance.enhance_tag` 必须能回答「哪个模型 + 哪版提示词」；`--model-tag` 缺失即报错 | 无法按版本定位一批坏改写 |
| (b) 未审核可识别 | `review_state` 置空 + `reviewed_by` 留空；归因在 `provenance.enhance_tag` / `enhanced_at`；人工确认/修正后才离开未审核 | 模型改写被误认为人工已验收 |
| (c) 可回滚 | 归档 + 按 tag/文件逐条校验回滚；人工已改过的条目自动跳过 | 错误正文永久污染检索 |

三条补充：

- **人工修正不被批量回滚覆盖**：回滚以 `new_hash` 匹配为准，人工 `correct` 后 hash 已变，自动跳过；
  改写时默认也跳过 `confirmed` / `corrected`。
- **不进关键路径**：不在 Hook / 检索 / `maintain` 内调用，不违反「向量不进检索关键路径」等红线；
  它只在人显式执行时改库。
- **逐条可追**：`mem show` 能看到 `provenance.enhance_tag` 与 `original_hash`；
  审计事件随实现落地（事件名实现时定，落地后补本节）。

---

## 7. 门禁与未决

### 7.1 门禁

- **代码侧**（CLI 实现者负责）：选择器（三种 filter + id 优先 + 默认跳过）、`--dry-run` 零写入、
  `--preview` 零写入、归档字段完整、回滚 hash 守卫、`--include-confirmed` 开关、
  模型不可用报错；随 `cmd/mem` 包跑 `make cover-gate`（下限 81%，见 `coverage-policy.txt`）。
- **文档侧**：`rg -n "design-content-enhance" README.md docs/` 可查到链接；
  `wc -l docs/design-content-enhance.md` ≤ 300；`design-data-lifecycle.md` 行数不增长。
- **手工验收**（临时库 `--db`，不碰 `~/.v2mem`）：`add` 一条过程叙述 → `--dry-run` 只列 id →
  `--preview` 看 before/after → apply → `show` 验 `enhance_tag` → `--rollback <tag>`
  正文回原样 → 再回滚一次全 skipped。

### 7.2 未决（实现时须收口，不自行发挥）

1. **`overlong` 阈值**未写进冻结契约；文档只写「实现定义」，落地后补数字。
2. **提示词版本落点**：归档字段只有 `enhance_tag`，若 `model.json` 的 `model_tag` 不带版本后缀，
   实现须把 `enhance-v1` 并入落档值（如 `<model>@enhance-v1`），否则不满足例外 (a)。
3. **归档保留期 / GC**：`enhance/*.jsonl` 会随改写次数增长，当前无出口；
   参考 `design-data-lifecycle.md` §8.2 的 `plans/` 口径另立保留策略。
4. **`--rollback` 后的 `review_state`**：冻结契约只规定正文校验，未规定还原后是否回未审核；
   默认按「不改变人工已有状态、无状态则保持空」处理，需主控确认。
5. **同 tag 多次归档**的回滚粒度：本文按「该 tag 全部归档、后写先还原」定义；若实现只取最新文件，
   须同步改本节。
6. **未接自动链路**是刻意决定，不是欠账：`maintain` / Hook / MCP 均不调用，
   防止模型改写进入无人值守写侧。
