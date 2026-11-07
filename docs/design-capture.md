# design-capture：多 Harness 捕获层与事件契约

- 日期：2026-11-06
- 状态：**部分已落地**（契约冻结与保真度链路 W0/T1–T4、适配器映射表、灰度名单已合入；closeout 规则已裁决但无实现，mem scan 价值已验证为「当前不值得写」，见下方「当前实现对照表」）
- 关联：[`design-harness.md`](./design-harness.md)、[`design-write-governance.md`](./design-write-governance.md)、[`design-eval.md`](./design-eval.md)、[`design-provenance.md`](./design-provenance.md)、[`VALUE.md`](./VALUE.md)
- 边界：`design-harness` 回答「每个工具怎么接」；本文回答「接上以后产出什么事件、如何归一、如何补偿」。不重复钩子安装步骤，也不定义蒸馏算法。

## 当前实现对照表

> 本文原为架构草案；`b1adbd8` → `9911f60` 落地契约冻结与保真度链路，`9ceadb7` 之后补上适配器映射表与灰度名单。
> 左栏是**已合入代码**，右栏是**仍停留在提案**；全文其余章节凡涉及右栏项，均应按设计意图而非现状阅读。
> 「已裁决」只表示规则定了，不表示代码存在；凡无实现处均显式写明。

| 已落地（W0 / T1–T4 / 适配器映射表 / 灰度名单） | 仍为提案（后续） |
|:---|:---|
| **W0 契约冻结**：`internal/evidence/api.go` 新增 `type Fidelity string` + `FidelityFull/Summary/Pointer/None` 四常量 + `const ProposalPayloadRunes = 400`，`RawEvent` / `Ref` 各 append `Fidelity`（types-only、append-only）。 | **P2 artifact 扫描 `mem scan`**：提案，且**本轮价值验证结论为「当前不值得写」**（2026-11 实测，见 §8.5）；机会扫描、指纹幂等、只读有界（本文 §8）均未实现。 |
| **T1 store**：`observations.meta_json` 由单键升级为结构化 `evidenceMeta{proposal_eligible, fidelity, adapter}`；旧行（无新键）按空 `Fidelity` 兼容，`ProposalEligible` 语义不变。 | **Task 级 closeout**：提案，**规则已裁决**（§13.1：默认显式收工、启发式只提醒、Stop 只是 Turn 级），代码**无实现**；P0 仅完成 Trae Stop 字段映射。 |
| **T2 evidence**：`Engine.Capture` 透传 `Fidelity / Adapter` 到 `EvidenceCaptureInput`（`Adapter` 由 `Harness` 承载，写入 `meta_json.adapter`），`toRef` 填 `Ref.Fidelity`；归档 `segmentRecord` 与 `ParserVersion` 一字未改。 | |
| **T3 proposal**：`internal/proposal/parse.go` 的 `proposalEligible` 是唯一准入判定源（full / summary / 空值可提案，pointer / none / 未知值 fail-closed）；`BuildPrompt` 按 `ProposalPayloadRunes = 400` 有界投影；`executor` 与 `runner` 的 `countEligible` / `bundleSession` 统一复用该函数。 | |
| **T4 adapter**：`cmd/mem/hook.go` 的 `hookInput` 补 `last_assistant_message` / `text_content`；`observationText` 不再把 `"stop"` 事件名当正文，按内容标注 `Fidelity`（有正文 full / summary，无正文 none）。 | |
| **适配器映射表（本波 T1）**：`cmd/mem/adapters.go` 的 `adapterTable` 按宿主规范名分派字段映射（`claude` / `traecode` / `traework`，别名经 `harness.Get` 归一；未知宿主走 `defaultAdapter`）。每项只描述「去哪取字段」（`stopReply` 优先级列表、`toolInput` / `toolOutput`、`allowMetaSummary`），Fidelity 判定仍统一在 `observationText`；`cmd/mem/adapters_test.go` 提供每家分派 / 字段映射 / 新增宿主只加表项的 fixture 测试。**不新建 `CaptureEvent` 第四层契约。** | |
| **灰度开关（本波 T2）**：`cmd/mem/distill_hook.go:69` 的 `proposalHookEnabled(host)` 让 `MEM_PROPOSAL_HOOK` 接受逗号分隔宿主名单（如 `MEM_PROPOSAL_HOOK=traecode`），宿主名两侧都经 `normalizeHarness` 归一到规范名；`1` / `all` / `on` 仍为全量。**默认值未改**：未设置 / 空 / `0` → 旧规则蒸馏。 | |

## 0. 关键决策（待复核）

| 决策 | 结论 |
|:---|:---|
| 捕获主链路 | A 原生 hook + B 任务收尾显式写入；C 扫描 / D 导入只做补偿 |
| 事件契约 | 统一 `CaptureEvent`，适配器负责翻译，核心写侧不解析原始载荷 |
| 事件身份 | 用 `event_id` 或 `session + kind + turn_id`；**不用内容 hash 当事件身份** |
| 无内容时 | 显式 `fidelity=none`，禁止回退成 `"stop"` / `"session-end"` 等事件名 |
| 文件监控 | 机会扫描 + 指纹幂等，不做常驻 watcher |
| 唯一真相源 | 拆成「事实源（artifact 正文）」与「查询源（v2mem 记忆与索引）」 |
| 蒸馏 | 独立立项；先定输入保真度契约，再谈抽取算法 |

## 1. 背景与触发

2026-11-02 至 11-04 的 Trae 实测暴露了一个**写侧断点**：

- Trae `Stop` 触发 15 次，全部准时到达（2–20ms，`ExecEnv: Host`）；
- Stop stdin 已带 `last_assistant_message`，内容包含「npx 缓存根因」「MCP 与 hooks 不冲突」等可沉淀事实；
- v2mem 最终只落 3 条 observation，摘要全是字面量 `"stop"`；蒸馏 15 次全部 `empty`，0 候选、0 记忆。

结论不是「日志拿不到」或「时机不准」，而是：

1. **事件到了，内容没按契约取到**：`hookInput` 没解析 Trae 的字段；
2. **内容取到了，事件身份不唯一**：Stop 用内容 hash 去重，同会话后续 Stop 被折叠；
3. **任务边界没定义**：Stop 是「每轮回复结束」，不是「任务结束」，不知道哪一次该沉淀；
4. **源头覆盖不全**：无 hook 的 harness、历史会话、spec/plan/result 文件没有进入捕获面。

多 harness 的真实差异也不是「传输时机」，而是**事件语义与字段契约**：事件名、Stop 是每轮还是每会话、有无 `transcript_path`、最终消息放在哪个字段。这些差异应被关在适配器里，而不是让核心写侧逐个识别日志格式。

## 2. 设计前提

### 2.1 「时机」拆成两层

| 层 | 含义 | 确定性 | 设计应对 |
|:---|:---|:---|:---|
| 传输时机 | hook 是否准时触发 | 高（push 模型） | 以 A 层为主链路 |
| 语义时机 | 哪一次事件代表任务完成 | 低（Stop 每轮触发） | 显式任务收尾（B 层）+ 触发分层 |

### 2.2 源头是事件语义，不是日志文件

日志只是事件的一种载体，会随 harness 版本漂移；事件语义相对稳定。因此：

- **不做**「日志解析器集合」；
- **要做**「统一事件契约的消费者」。

### 2.3 事实源与查询源分开

- **Source of record（事实源）**：spec / plan / result / transcript 的正文，归 harness 或项目目录所有；
- **Source of query（查询源）**：v2mem 是记忆原子与索引的唯一入口。

v2mem 不拥有过程产物的正文，也不回写源文件。否则 harness 与 v2mem 会变成两个写入方，冲突和漂移不可避免。

## 3. 目标与非目标

### 目标

1. 定义统一的 `CaptureEvent` 契约，核心写侧只消费契约，不解析 harness 原始载荷。
2. 定义 A/B/C/D 四级捕获阶梯，把「确定性」和「覆盖率」分开。
3. 定义 artifact 无侵入扫描的边界：可补偿、可回填，但**不常驻、不直接写记忆、不做事实源**。
4. 用适配器一致性测试防止「字段静默丢失」再次发生。
5. 为蒸馏提供有保真度分级的输入，而不是只有事件名。

### 非目标

- 不做常驻文件 watcher / 守护进程；
- 不让 hook 直接写 `memories`；
- 不自动 `accept` 候选；
- 不把向量或 LLM 放进检索关键路径；
- 不做全盘 `~` 通用 glob 扫描；
- 不把 artifact 正文复制成 v2mem 的「唯一真相源」；
- 不在本文定义蒸馏 prompt、模型选型与候选合并算法。

## 4. 核心契约：CaptureEvent

> **本轮落地说明（与草案契约的差异）**：本轮**不新建 `CaptureEvent`**，复用 `internal/evidence`
> 的 `RawEvent` / `Ref`（W0 冻结，append `Fidelity`）；适配器身份不由独立字段承载，而是用
> `RawEvent.Harness` / `Ref.Harness` 承载、写入 `observations.meta_json.adapter`
> （`evidenceMeta.adapter`，T2 由 `Engine.Capture` 把 `Harness` 赋给 `Adapter`）。下面的
> `CaptureEvent` 结构与 4.2 事件枚举只作设计意图阅读：适配器已按 §7 的**字段映射表**落地，
> 并不存在这个结构体；`closeout` 一行是已裁决但**未实现**的事件类型（§13.1）。
>
> `Fidelity` 是 `evidence.Fidelity`，**只允许 `full / summary / pointer / none` 四个常量**；
> 未知字面量在准入层一律按 `FidelityNone` 处理（由 `internal/proposal.proposalEligible` 的
> fail-closed 分支兜底，判定为不可提案），但**落库忠实保留原值**以便排查，不改写
> `meta_json.fidelity`。

```go
type CaptureEvent struct {
	Harness    string            `json:"harness"`     // traecode | workbuddy | ...
	Kind       string            `json:"kind"`        // 见 4.2
	SessionID  string            `json:"session_id"`
	TurnID     string            `json:"turn_id"`     // 每轮事件的身份；Stop 去重靠它
	Project    string            `json:"project"`
	OccurredAt int64             `json:"occurred_at"` // Unix 秒
	Fidelity   string            `json:"fidelity"`    // full | summary | pointer | none
	Payload    string            `json:"payload"`     // 有界正文，可为空
	SourceRef  string            `json:"source_ref"`  // hook event id | file:line | content hash
	Meta       map[string]string `json:"meta"`
}
```

### 4.1 字段约束

| 字段 | 约束 | 说明 |
|:---|:---|:---|
| `Harness` | 必填 | 适配器名，不是宿主显示名 |
| `Kind` | 必填 | 只允许 4.2 的枚举 |
| `SessionID` | 强建议 | 无会话 id 时用 `project + 时间窗` 兜底，并标 `meta.session_id_missing=1` |
| `TurnID` | Turn 级事件必填 | 优先宿主原生 id；否则 `session + kind + ordinal` |
| `Project` | 必填 | 取工程根，不取 cwd |
| `OccurredAt` | 必填 | 事件发生时间，不是落库时间 |
| `Fidelity` | 必填 | 拿不到内容就写 `none`，**禁止静默写成事件名** |
| `Payload` | 按层分离 | 事实源（归档 `Raw`）脱敏后按 32000 rune 保真原文、不丢字节；查询源（提案 / 蒸馏输入）按 `evidence.ProposalPayloadRunes = 400` 有界投影（见下方说明） |
| `SourceRef` | 必填 | 指向原始证据；文件用 `path#Lx-Ly`，hook 用事件 id |
| `Meta` | 可选 | 放 `truncated`、`scrubbed`、`schema_version` 等 |

**Payload 预算按层分离（本轮落地）**：

- **事实源**（归档 `Raw`）：只做脱敏（`internal/scrub`），按 **32000 rune** 保留原文；
  不因提案预算而截断、不丢字节；投影不回写归档，`segmentRecord` 保持字节级幂等。
- **查询源**（提案 / 蒸馏输入）：`internal/proposal.proposalPayload` 折叠空白 → 脱敏 → 按
  `evidence.ProposalPayloadRunes = 400` **rune** 有界投影（省略标记计入预算，输出严格有界）；
  截断只作用于 prompt 文本，`BuildPrompt` 的 `evidence_idx` ordinal 对齐不受影响。

草案里「`Payload` 默认 ≤ 400 字」是单一上限表述，本轮已按上述两层拆开：400 不再是事实源的
上限，只是查询源投影的上限。

### 4.2 事件类型

| Kind | 触发 | 典型来源 | 进入哪一层 |
|:---|:---|:---|:---|
| `session.start` | 会话开始/恢复 | SessionStart | observation |
| `user.prompt` | 用户提交 | UserPromptSubmit | observation |
| `assistant.final` | 一轮回复结束 | Stop / 最终消息 | observation |
| `tool.use` | 工具调用结束 | PostToolUse | observation |
| `artifact.write` | 过程产物落盘 | 文件扫描 | observation / pointer |
| `session.end` | 会话结束 | SessionEnd | observation |
| `closeout` | 任务收尾显式提交 | MCP / CLI / skill | candidate |

`assistant.final` 是**Turn 级**；`closeout` 是**Task 级**。两者不能混用。

### 4.3 保真度分级

| Fidelity | 含义 | 允许的用途 |
|:---|:---|:---|
| `full` | 有完整最终回复/正文（有界截断） | observation 正文、规则蒸馏输入 |
| `summary` | 只有会话摘要/主题摘要 | observation 正文、低置信候选 |
| `pointer` | 只有路径/哈希/行号，正文需回源读 | 索引、按需取回；不直接蒸馏 |
| `none` | 事件存在但无内容 | 只记事件；不生成摘要、不蒸馏 |

**禁止静默降级**：适配器拿不到内容时必须显式返回 `fidelity=none`，不能回退成 `"stop"`、`"session-end"` 这类事件名。事件名不是内容。

### 4.4 事件身份与去重

- 事件身份优先用宿主原生 `event_id`；
- 没有原生 id 时，用确定性键：`session + kind + turn_id`，必要时加 `ordinal`；
- **内容 hash 只用于内容去重，不用于事件身份**；
- 同一事件被多处配置触发时，按 `event_id` 或确定性键去重，不按摘要文本去重。

这条直接修正 Trae 的现状：15 次 Stop 摘要都是 `"stop"`，内容 hash 相同，于是 12 次被当成重复。

## 5. 触发分层

| 层 | 时机 | 产出 | 写不写记忆 |
|:---|:---|:---|:---|
| Turn | 每次 Stop / PostToolUse | observation | 否 |
| Task | 显式 closeout（任务完成时） | candidate / memory | 经显式 `accept` 或显式 `add` |
| Session | SessionStart / SessionEnd | 有界蒸馏、归并 | 否（只到 candidates） |
| Artifact | 机会扫描 / 手动导入 | observation / pointer / candidate | 否 |

设计含义：

- Stop 只负责「这一轮发生了什么」，不负责「这个任务学到了什么」；
- 任务级沉淀必须有显式动作：MCP `add`、`mem closeout` 或等价 skill；
- SessionStart 只做补偿与蒸馏，不承担主写入。

## 6. 捕获阶梯

| 层 | 方式 | 确定性 | 延迟 | 主要产出 | 适用 |
|:---|:---|:---|:---|:---|:---|
| A | 原生 hook push | 高 | 秒级 | observation | 有 hook 的 harness |
| B | 任务收尾显式写入 | 高 | 任务结束时 | candidate / memory | 所有 harness |
| C | 机会扫描 artifact | 中 | 下次启动/手动 | observation / pointer | 无 hook、历史回填 |
| D | 手工导入 | 低 | 人工触发 | candidate | 迁移、取证 |

原则：

1. **A/B 是主链路，C/D 是补偿层**；不能把 C 的延迟当成「时机不确定」的借口。
2. 每层产出必须带 `source`：`hook` / `closeout` / `import-scan` / `import-manual`。
3. 同一内容被 A 与 C 同时看到时，按 `SourceRef` 或 `event_id` 合并，不按文本相似度直接吞并。
4. C/D 永远不直接写 `memories`；只能进 `observations` 或 `candidates`。

## 7. 适配器层

> **状态（本波 T1 已落地）**：「独立适配器层」不再是提案，但落地形态与 §4 的 `CaptureEvent` 草案不同：
> 实现是 `cmd/mem/adapters.go` 的**字段映射表 + 每家 fixture 测试**，不是新建第四层契约。
> `adapterTable` 以宿主规范名为 key（`claude` / `traecode` / `traework`），每项只回答「去哪个字段取正文」；
> Fidelity 判定、去重、落库仍走 `hookInput → observationText → evidence.RawEvent` 原链路。
> 因此这里说的「适配器」= 一次表查找，`CaptureEvent` 结构仍只作设计意图阅读。

### 7.1 职责

适配器只做三件事：

1. 识别本 harness 的事件；
2. 把原生载荷翻译成统一事件（实现上是 `hookInput` 的字段映射，见 §7.2）；
3. 声明本 harness 可扫描的 artifact 路径（路径清单仍属提案，未实现）。

核心写侧不读 `stdin` 原始 JSON，也不判断「这是 Trae 还是 WorkBuddy」。新增宿主 = `adapterTable` 加一行 + 一份 fixture，核心分支不动；`TestAdapterNewHarnessOnlyNeedsTableEntry` 就是这条约束的回归护栏。

### 7.2 映射表（已落地）

| 宿主（规范名） | `stopReply` 取值优先级 | `tool_input` / `tool_output` | `allowMetaSummary` |
|:---|:---|:---|:---|
| `traecode` / `traework` | `last_assistant_message` → `text_content` | 默认字段 | 是 |
| `claude` | 空（最终回复走 transcript，stdin 不带） | 默认字段 | 是（无正文时落到 `status` / `reason` 摘要） |
| 未知 / 空 harness | 同 `defaultAdapter`（保守默认，保持本波之前行为） | 默认字段 | 是 |

Trae 系的字段语义：

| 原生事件 | 事件类型 | Fidelity | 取值 |
|:---|:---|:---|:---|
| `SessionStart` | `session.start` | `none` / `summary` | 无正文时 `none` |
| `UserPromptSubmit` | `user.prompt` | `full` | `prompt` |
| `Stop` | `assistant.final` | `full` | 优先 `last_assistant_message`，回退 `text_content` |
| `PostToolUse` | `tool.use` | `full` | `tool_name` + 有界 `tool_input` / `tool_output` |
| `SessionEnd` | `session.end` | `none` / `summary` | 无正文时 `none` |

关键：Trae 不提供 `transcript_path`，但 Stop 提供了 `last_assistant_message`。**不要因为没有 transcript 就判定「无内容」**。

### 7.3 一致性测试

每个适配器必须通过同一套 fixture 测试（当前落在 `cmd/mem/adapters_test.go`，表驱动）：

- 必填字段齐全；
- `Fidelity` 与 `Payload` 自洽：`full/summary` 必须有正文，`pointer` 必须有 `SourceRef`，`none` 必须为空；
- Turn 级事件必须有 `TurnID`（`hookInput` 的 `session_id` + 轮次身份；transcript 侧用 `HostUID`）；
- 同一会话两次不同 Stop 产生两个不同事件身份；
- 未知字段被忽略而不是导致整条事件丢失；
- 缺字段时显式降级为 `none`，日志里能看到缺口。

fixture 用构造的 `hookInput` 直接跑 `observationText`，不依赖真实 stdin；新增 harness 先加 fixture 再接代码。

## 8. artifact 无侵入扫描（C 层）

### 8.1 定位

用于补两类缺口：

- harness 没有 hook，或 hook 只给摘要不给正文；
- 历史会话回填（例如 Trae 的 task JSONL、`~/.trae-cn/memory/` 下的会话摘要、项目里的 spec/plan/result 文档）。

它是**补偿层**，不是主链路。

### 8.2 正确形态

- **触发**：`SessionStart` 机会扫描，或手动 `mem scan`；不做常驻 watcher。
- **清单**：per-harness `ArtifactSpec`，显式列出路径/glob、kind、parser、大小上限、静默期。
- **指纹**：`path + mtime + size + sha256`，落 manifest；未变化直接跳过。
- **幂等**：同一文件重复扫描不产生重复 observation；以 `source_ref + fingerprint` 为键。
- **稳定性**：mtime 在静默期内、临时后缀、正在写入的文件跳过，等下一次。
- **有界**：每次扫描限文件数、字节数、时间预算；超限留到下次。
- **失败开放**：扫描失败只写 stderr，不阻断 SessionStart。

### 8.3 产出边界

| 项 | 允许 | 说明 |
|:---|:---|:---|
| `observations` | 是 | 摘要 + `raw_ref` + provenance |
| `candidates` | 是 | 从 artifact 抽出的候选，`source=import-scan` |
| `memories` | **否** | 必须显式 `accept` 或 `add` |
| 全文复制 | **否**（默认） | 只存指针 + 有界摘录 |
| 不可变快照 | 可选 | `~/.v2mem/assets/<sha256>`，标记 `derived`，有保留期 |

### 8.4 内容与隐私

- 入库前统一 scrub：密钥、token、URL 密码、PEM 块；
- 跳过二进制、加密 DB、临时文件、超大 transcript；
- 快照必须有保留期与删除入口；
- `provenance` 记录 `file / line / hash / preview`，读侧可回源。

### 8.5 价值假设与判死

**价值假设**：artifact 扫描能补上 hook 覆盖不到的历史与无 hook 工具，让「源头」不止于实时事件。

**判据**：连续两周扫描，若产生的候选采纳率长期趋近 0，或噪声/重复占比过高，则收窄路径清单或停掉该层；不要无限期维护一个只产生垃圾的扫描器。

#### 8.5.1 本轮价值验证结论（2026-11 实测，临时库，未触碰 `~/.v2mem`）

`mem scan` **无实现**，因此无法直接测扫描器本身。本轮用**现成命令** `import-transcript`（C/D 补偿层，
与 scan 同属「把既有 artifact 变成证据」的链路）做一次最小真实验证，测的是同一个价值假设：
**把 artifact 直接喂进证据/候选链路，产出的东西值不值得留下。**

- fixture：`mktemp -d` 下单个 JSONL 共 3 行（1 条 user prompt、1 条 assistant 正文、1 条带原始 JSON
  载荷的 `tool_result`，载荷为 `{"level":"error","msg":"permission denied",...}`）；**未提交**，用完删除。
- 命令：`go run ./cmd/mem import-transcript -file <tmp>/x.jsonl -db <tmp>/probe.db --into both -json`

| 指标 | 实测 | 说明 |
|:---|:---|:---|
| observation 条数 | 3 | 3 行全部入库，`meta_json.fidelity` 均为 `full` |
| 候选条数 | 2 | 规则蒸馏产出（`source=distill`, `status=pending`） |
| 载荷残留 | **1 / 2 = 50%** | 候选正文是 `tool_result {"level":"error",...}` 的原始 JSON，未提炼成事实 |
| 重复率 | **0 / 2 = 0%** | 同文件二次导入 `duplicate=3`、新增候选 0，身份幂等成立 |
| 可采纳候选 | **0 / 2** | 另一条是把用户提问「排查 go test 失败根因」判成 `pitfall`，属噪声 |

对照（`--dry-run` 同参数）：`{"observations":3,"written":3,"duplicate":0,"candidates":0,"dry_run":true}`。
观测计数与非 dry-run 一致（3 = 3）；`candidates` 为 0 是**设计如此**——`import_transcript.go` 里蒸馏
分支带 `&& !*dry`，dry-run 不跑蒸馏，故该字段不可用于对照。

**结论：当前不值得写 `mem scan`。** 理由不是「扫描没价值」，而是这次测出的瓶颈不在覆盖面，而在准入：
原始工具载荷（`tool_result` 的 JSON）以 `fidelity=full` 直通候选，重复率已经不构成问题（0%），
噪声来自「什么内容算事实」这一步，扫描器只会把同样的噪声按更高吞吐复制一遍。

- **判死条件**：在准入收窄（工具载荷不得直出候选）之前，`mem scan` 不启动；若强行实现，以「候选
  残留率 > 20%」或「可采纳候选 < 20%」为停止线。
- **重新评估条件**：① 准入层能区分「工具载荷 / 事实陈述」并通过回归测试；② 有一个 hook 覆盖不到的
  真实历史语料（非合成 fixture）可测，且人工标注的可采纳率 ≥ 30%。两条同时满足再重开 P2。

现在要做的替代动作不是扫描器，而是给 `recordEvidence` / `proposalEligible` 补一条「工具载荷不是
事实」的判定（见 §13.2 仍开放项），属后续提案。

## 9. 事实源与查询源

| 概念 | 归属 | v2mem 的职责 |
|:---|:---|:---|
| 过程产物正文 | harness / 项目目录 | 只索引、只指向、可按需快照 |
| 记忆原子 | v2mem | 拥有、检索、生命周期管理 |
| 观测 | v2mem | 有界保留，默认不进检索 |
| 候选 | v2mem | 隔离区，显式 accept 才升格 |

冲突规则：

- 同一 artifact 两个来源不一致时，两版都保留并打 `provenance`，不静默覆盖；
- v2mem 不回写源文件，不做双向同步；
- 「备份」是耐久性选项，不是事实源转移；快照必须标 `derived`。

这与 [`VALUE.md`](./VALUE.md) 的「二级索引只指向、不判定」一致：资产层负责指向，判断留给 Agent。

## 10. 与蒸馏、评测的关系

**Fidelity 与 Grade 分层（本轮落地，不可互喂）**：`Fidelity`（`full / summary / pointer / none`）
**只管提案准入**，唯一判定源是 `internal/proposal.proposalEligible`，决定一条证据能否进入
`BuildPrompt` 与候选计数；`A/B/C Grade` **只管提交裁决**，由
`internal/store/commit.go:gradeOfEvidence` 决定候选能否升格为记忆，属于 CommitPolicy。
**两者不得互相喂值**：Fidelity 不流入 `gradeOfEvidence` / CommitPolicy，也不由 Grade 反推。
**Fidelity 不写进归档 `segment`**：它只落在 `observations.meta_json`（`evidenceMeta.fidelity`）
与读侧 `Ref.Fidelity`，归档 `segmentRecord` 保持字节级幂等，因此本轮归档重放哈希不变。

- **捕获保真度是蒸馏的上限**。诊断时（2026-11-02~11-04）15 次蒸馏全 `empty`，
  不是蒸馏策略问题，而是输入只有 `"stop"`；本轮 Trae Stop 字段映射与 `Fidelity`
  链路落地后该输入断点已修复（见「当前实现对照表」T4 行），此句只作历史诊断记录。
- 蒸馏独立立项，但输入契约要先定：只有 `full/summary` 可蒸馏；`pointer` 先回源；`none` 跳过。
- 蒸馏产出只进 `candidates`；采纳率是核心指标，不以候选数论英雄。
- 评测至少覆盖：
  - 写侧活动：实质会话是否产生记忆；
  - 观测质量：是否还有裸事件名摘要；
  - 读侧精度：接入前后 Recall/人工相关率不下降；
  - 扫描幂等：重复扫描 0 新增；
  - 回归护栏：巡检能发现「有载荷但摘要为空」。

## 11. 落地顺序

| 阶段 | 内容 | 状态与提交范围 | 验收 |
|:---|:---|:---|:---|
| P0 捕获止血 | Trae Stop 字段映射（本轮范围）；任务收尾写入规则、巡检 R4/R5 内容判据仍属后续 | **已完成**（Trae 字段映射）：`786cf22` → `9911f60`（T4） | 同会话两次 Stop 不再折叠；实质会话写侧 > 0 |
| P1 契约化 / 保真度 | W0 冻结 `Fidelity` 契约 + T1 `meta_json` 结构化 + T2 Engine 透传 + T3 提案准入与 400 字投影 | **已完成**：`51075ed` → `cdeca96`（W0/T1/T2/T3，含 `4e48849` / `2c88424` / `e549b14` 各合并点） | 契约 append-only、归档哈希不变、`proposalEligible` 单一判定、旧行兼容 |
| P1.5 适配器与灰度 | 适配器字段映射表（`cmd/mem/adapters.go`）+ 每家 fixture；`MEM_PROPOSAL_HOOK` 宿主名单灰度 | **已完成**：`ddb18fd`（T1 映射表）、`9ceadb7`（T2 灰度名单，默认值未改） | 新增宿主只加表项；默认仍走旧规则蒸馏 |
| P2 扫描 spike | 手动 `mem scan`，只读、有界、幂等 | **不启动（价值已验证为「当前不值得写」）**：2026-11 实测见 §8.5.1，重建条件与判死线同节 | 重启前须先收窄准入；重复扫描 0 新增；不写 memories |
| P2.5 任务收尾 | `closeout` 显式收工 | **规则已裁决、无实现**：§13.1 | 触发方式为 MCP / CLI / skill 显式调用；启发式只提醒 |
| P3 扩展 | 其他 harness 适配器；快照与保留期 | **部分落地**：映射表已支持 `claude` / `traecode` / `traework`；artifact 路径清单与快照仍为提案 | 新 harness 只加适配器与夹具，不改核心 |

P0 的「任务收尾写入规则 / 巡检判据」与 P1 原列的「抽 `CaptureEvent` + 独立适配器层 + 一致性夹具」：
适配器部分已由 P1.5 的映射表 + fixture 等价落地（不新建第四层契约）；任务收尾写入规则归入 P2.5。
P0 的详细修复项见本次诊断结论；本文只锁定架构边界。

## 12. 验收判据

1. `assistant.final` 的 `Fidelity=full` 时，`Payload` 不得为空；`none` 时不得伪造内容。
2. 同一会话两次不同 Stop 产生两个 observation，不再被内容 hash 吞并。
3. 适配器一致性测试覆盖必填字段、降级、事件身份、未知字段。
4. artifact 扫描重复执行 0 新增；只写 `observations` / `candidates`。
5. 实质会话结束后，写侧活动 > 0；`mem eval activity` 不再长期显示「只查未记」。
6. 巡检能对「Stop 有载荷但摘要为裸事件名」报 WARN/FAIL。
7. artifact 扫描若两周采纳率趋近 0，或实测载荷残留 > 20%，按判死规则收窄或停用（当前结论见 §8.5.1）。

## 13. 开放问题

### 13.1 已裁决：任务边界与「谁有权喊收工」

> 本节只裁决**规则**。`closeout` **当前无任何实现**（无 `mem closeout` 子命令、无对应 MCP 工具、
> 无 skill 调用）；下面出现的命令 / 工具形态都是**建议形态、尚未实现**，不得按已可用阅读。

**裁决一：默认显式收工。** 任务结束必须由一次显式动作声明，而不是由系统猜：

- 建议形态（均未实现）：CLI `mem closeout --session <sid> [--summary <text>]`；MCP 工具 `closeout`；
  skill 在任务收尾时调用上述之一。三者应是同一条写入路径的不同入口，产出 `closeout` 事件（§4.2）。
- 显式动作的语义边界：`closeout` 只产出 **Task 级** 产物（candidate，`source=closeout`）；
  它**不直接写 `memories`**，升格仍要显式 `accept`（§5、§8.3 的同一原则）。

**裁决二：启发式只做提醒，绝不自动触发。** 长时间静默、连续多轮无写入、Stop 后出现「根因 / 决策」
关键词等信号，只能产出**提醒或候选提示**（例如在 hook 输出里加一行提示、或标记 `meta.closeout_hint=1`），
**不得**自动判定收工、不得自动触发 Proposal、不得凭启发式写候选。理由：这些信号在采集侧不可证伪，
误判成本直接落在记忆库上。

**裁决三：`Stop` / `SessionEnd` 仍只是 Turn 级。** `Stop` = 「这一轮回复结束」，`SessionEnd` = 「会话结束」，
两者**都不等于任务边界**。事件类型上 `assistant.final`（Turn）与 `closeout`（Task）互不替代、互不推导。
Trae 一次任务跨多个 Stop 是常态，不能把第 N 个 Stop 当收工。

**裁决四：与 Hook 默认值的上下游关系（默认值一律不动）。**

- Hook 默认仍是**旧规则蒸馏**：`maybeSpawnDistill` 先要求 `MEM_DISTILL_SPAWN == "1"`，否则直接返回；
  该变量**默认关闭**。开关打开后，`proposalHookEnabled(host)` 决定走 `--proposal` 还是
  `--session <sid> --provider none`，其默认（未设置 / 空 / `0`）为 **false**，且名单灰度只影响
  「哪些宿主进 Proposal」，不改变默认值。
- `closeout` 是**独立于 Hook 开关的显式动作**：它由 MCP / CLI / skill 主动调用，不依赖
  `MEM_DISTILL_SPAWN`，也不依赖 `MEM_PROPOSAL_HOOK`。
- `closeout` 落地后，**也不**把 Hook 默认值改成「自动启用 Proposal」。二者的关系是单向补充，
  不是替换：Hook 负责 Turn 级 observation，closeout 负责 Task 级候选。
- 完整链路：**谁触发 closeout**（人或 Agent 在任务收尾显式调用 MCP 工具 / CLI 子命令 / skill）
  → **closeout 触发什么**（写一条 `closeout` 证据 + 一个 Task 级 candidate，不做别的）
  → **什么情况下才会拉起 Proposal**（仅当 `MEM_DISTILL_SPAWN=1` 且 `proposalHookEnabled(host)` 为真、
  且存在 `session_id` 时由 Stop / SessionEnd 触发；与 closeout 无因果关系）。

### 13.2 仍开放

- **工具载荷准入**：`recordEvidence` 对已解析的 `tool_result` 一律标 `fidelity=full` 且
  `ProposalEligible=true`（只有 `unparsed` 降为 `pointer`），§8.5.1 实测因此出现 50% 载荷残留。
  需决定：按事件类型 / 工具名收窄，还是引入「事实陈述 vs 原始载荷」的判定层。
- **artifact 稳定性**：哪些路径是稳定契约，哪些是内部实现（如 Trae `hub_event_cache`、加密 `database.db`）？只把稳定路径写进清单。
- **快照保留**：是否需要不可变快照？保留多久？如何删除？
- **跨来源去重**：A 层与 C 层同时看到同一内容时，按 `source_ref` 合并的边界在哪？
- **无文件 harness**：只有加密 DB 或远端 API 的 harness，是否只能依赖 hook + closeout？
- **事件枚举治理**：`Kind` 新增是否需要版本号与兼容策略？

## 14. 不做清单

- 不做常驻 watcher / 文件守护进程；
- 不做「全盘 `~` 扫描」；
- 不让 hook 直接写 `memories`；
- 不自动 `accept` 候选；
- 不把 artifact 正文复制成唯一事实源；
- 不做双向同步或回写 harness 文件；
- 不把向量/LLM 放进检索关键路径；
- 不为接新 harness 而在核心写侧加 `if harness == ...` 分支。

## 15. 关联文档

- 各工具怎么接： [`design-harness.md`](./design-harness.md)
- 写侧治理与观测/候选边界： [`design-write-governance.md`](./design-write-governance.md)
- 评测与 transcript 取舍： [`design-eval.md`](./design-eval.md)
- 记忆溯源与资产索引： [`design-provenance.md`](./design-provenance.md)
- 价值主张与红线： [`VALUE.md`](./VALUE.md)
- 运行观测判据： [`ops-watch.md`](./ops-watch.md)
