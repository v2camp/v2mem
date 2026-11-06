# design-capture：多 Harness 捕获层与事件契约

- 日期：2026-11-06
- 状态：**部分已落地**（本轮已合入契约冻结与保真度链路 W0/T1–T4；P2/P3 仍为提案，见下方「当前实现对照表」）
- 关联：[`design-harness.md`](./design-harness.md)、[`design-write-governance.md`](./design-write-governance.md)、[`design-eval.md`](./design-eval.md)、[`design-provenance.md`](./design-provenance.md)、[`VALUE.md`](./VALUE.md)
- 边界：`design-harness` 回答「每个工具怎么接」；本文回答「接上以后产出什么事件、如何归一、如何补偿」。不重复钩子安装步骤，也不定义蒸馏算法。

## 当前实现对照表

> 本文原为架构草案；本轮（基线 `b1adbd8` → `9911f60`）已落地契约冻结与保真度链路。
> 左栏是**已合入代码**，右栏是**仍停留在提案**；全文其余章节凡涉及右栏项，均应按设计意图而非现状阅读。

| 已落地（本轮 W0 / T1–T4） | 仍为提案（后续） |
|:---|:---|
| **W0 契约冻结**：`internal/evidence/api.go` 新增 `type Fidelity string` + `FidelityFull/Summary/Pointer/None` 四常量 + `const ProposalPayloadRunes = 400`，`RawEvent` / `Ref` 各 append `Fidelity`（types-only、append-only）。 | **P2 artifact 扫描**：`mem scan` 机会扫描、指纹幂等、只读有界（本文 §8），本轮未实现。 |
| **T1 store**：`observations.meta_json` 由单键升级为结构化 `evidenceMeta{proposal_eligible, fidelity, adapter}`；旧行（无新键）按空 `Fidelity` 兼容，`ProposalEligible` 语义不变。 | **Task 级 closeout**：`closeout` 事件与显式任务收尾写入（本文 §4.2 / §5），本轮未实现；P0 仅完成 Trae Stop 字段映射。 |
| **T2 evidence**：`Engine.Capture` 透传 `Fidelity / Adapter` 到 `EvidenceCaptureInput`（`Adapter` 由 `Harness` 承载，写入 `meta_json.adapter`），`toRef` 填 `Ref.Fidelity`；归档 `segmentRecord` 与 `ParserVersion` 一字未改。 | **独立适配器层抽象**：本文 §4 的 `CaptureEvent` 结构、§7 适配器层与一致性夹具；本轮复用 `evidence.RawEvent / Ref`，不新建该层（见 §4 落地说明）。 |
| **T3 proposal**：`internal/proposal/parse.go` 的 `proposalEligible` 是唯一准入判定源（full / summary / 空值可提案，pointer / none / 未知值 fail-closed）；`BuildPrompt` 按 `ProposalPayloadRunes = 400` 有界投影；`executor` 与 `runner` 的 `countEligible` / `bundleSession` 统一复用该函数。 | |
| **T4 adapter**：`cmd/mem/hook.go` 的 `hookInput` 补 `last_assistant_message` / `text_content`；`observationText` 不再把 `"stop"` 事件名当正文，按内容标注 `Fidelity`（有正文 full / summary，无正文 none）。 | |

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
> `CaptureEvent` 结构与 4.2 事件枚举仍是「独立适配器层」的设计意图（见头部对照表右栏），
> 不代表已落库字段。
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

### 7.1 职责

适配器只做三件事：

1. 识别本 harness 的事件；
2. 把原生载荷翻译成 `CaptureEvent`；
3. 声明本 harness 可扫描的 artifact 路径。

核心写侧不读 `stdin` 原始 JSON，也不判断「这是 Trae 还是 WorkBuddy」。

### 7.2 Trae 映射示例

| 原生事件 | CaptureEvent.Kind | Fidelity | 取值 |
|:---|:---|:---|:---|
| `SessionStart` | `session.start` | `none` / `summary` | 无正文时 `none` |
| `UserPromptSubmit` | `user.prompt` | `full` | `prompt` |
| `Stop` | `assistant.final` | `full` | 优先 `last_assistant_message`，回退 `text_content` |
| `PostToolUse` | `tool.use` | `full` | `tool_name` + 有界 `tool_input` / `tool_output` |
| `SessionEnd` | `session.end` | `none` / `summary` | 无正文时 `none` |

关键：Trae 不提供 `transcript_path`，但 Stop 提供了 `last_assistant_message`。**不要因为没有 transcript 就判定「无内容」**。

### 7.3 一致性测试

每个适配器必须通过同一套 fixture 测试：

- 必填字段齐全；
- `Fidelity` 与 `Payload` 自洽：`full/summary` 必须有正文，`pointer` 必须有 `SourceRef`，`none` 必须为空；
- Turn 级事件必须有 `TurnID`；
- 同一会话两次不同 Stop 产生两个不同事件身份；
- 未知字段被忽略而不是导致整条事件丢失；
- 缺字段时显式降级为 `none`，日志里能看到缺口。

测试夹具放 `testdata/capture/<harness>/`，表驱动；新增 harness 先加夹具再接代码。

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
| P2 扫描 spike | 手动 `mem scan`，只读、有界、幂等 | **待办（提案）** | 能补历史缺口；重复扫描 0 新增；不写 memories |
| P3 扩展 | 其他 harness 适配器；快照与保留期 | **待办（提案）** | 新 harness 只加适配器与夹具，不改核心 |

P0 的「任务收尾写入规则 / 巡检判据」与 P1 原列的「抽 `CaptureEvent` + 独立适配器层 + 一致性夹具」
本轮未纳入，分别归入头部对照表右栏（Task 级 closeout / 独立适配器层抽象）与 P3；
本轮 P1 以「W0 冻结契约 + 保真度链路」这一等价实现路径完成。
P0 的详细修复项见本次诊断结论；本文只锁定架构边界。

## 12. 验收判据

1. `assistant.final` 的 `Fidelity=full` 时，`Payload` 不得为空；`none` 时不得伪造内容。
2. 同一会话两次不同 Stop 产生两个 observation，不再被内容 hash 吞并。
3. 适配器一致性测试覆盖必填字段、降级、事件身份、未知字段。
4. artifact 扫描重复执行 0 新增；只写 `observations` / `candidates`。
5. 实质会话结束后，写侧活动 > 0；`mem eval activity` 不再长期显示「只查未记」。
6. 巡检能对「Stop 有载荷但摘要为裸事件名」报 WARN/FAIL。
7. artifact 扫描若两周采纳率趋近 0，按判死规则收窄或停用。

## 13. 开放问题

- **任务边界**：closeout 由模型显式调用，还是用启发式（如 Stop 后出现决策/根因关键词）自动触发？默认显式，启发式只做提醒。
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
