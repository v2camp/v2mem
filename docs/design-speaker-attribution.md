# 说话人归属：用户请求与 AI 产出怎么进记忆

> 关联：[`VALUE.md`](./VALUE.md)（判据）、[`design-capture.md`](./design-capture.md) §4（捕获层）、
> [`design-write-governance.md`](./design-write-governance.md)（三态）、
> [`design-ref-landscape.md`](./design-ref-landscape.md)（架构对照）。
> 本文件只回答：**一条事实是谁说的，该不该进记忆，进来后怎么标**。

## 1. 起因：一条实测出来的单侧链路

2026-11-13 审查 `~/.v2mem/mem.db`：`user-prompt` / `assistant-text` 证据 **0 / 0**，
可提案证据只有 4 条，全是 `stop` 的 `last_assistant_message`（AI 收尾回复）。
用户原话只完整留在 `audit.jsonl`（审计流水，不参与提纯）。

两个根因：

- `UserPromptSubmit` 只做检索注入，完全不写库（`isObserveHook` 只收
  `post-tool-use` / `stop` / `session-end`）。
- WorkBuddy transcript 的行是 `{"type":"message","role":"user","content":[{"type":"input_text",…}]}`，
  role/content 在顶层；解析器走 Claude 的 `raw["message"]["content"]`，整行落
  `unparsed` → `fidelity=pointer` → 不可提案。本机 510 条 `unparsed` 即由此而来。

结果：唯一进提案的是助手对这一轮的**复述 + 结论**，而这恰是 mem0 提示词里
明确要求跳过的「回声」。

## 2. 参照应用怎么做

| 系统 | 归档范围 | 提炼策略 | 角色标注 | 反回声 |
|:--|:--|:--|:--|:--|
| mem0 | user + assistant，排除 system | 三套提示词：USER-only / AGENT-only / V3 混合 | `attributed_to: user\|assistant` **必填** | `No Echo Extraction` 明文约束 |
| Zep / Graphiti | 全部消息 + JSON + 文档 | 抽实体与事实边 | episode 带 `speaker`；fact 挂 provenance | 靠时间失效，不显式去回声 |
| Letta / MemGPT | 全量对话 + 显式写入 | 按主语分块 | core memory 两块 `human` / `persona` | 靠 Agent 自编辑 |
| ChatGPT | 聊天历史 + saved memories | 后台 dreaming 合成、可审阅修正 | 只面向「关于用户」 | 结构性排除 |
| claude-mem | 5 hooks 全采，含 UserPromptSubmit 存 prompt | tool 输出压成 observation | Stop summary 显式含 `request` 字段 | 结构里保留 request |

mem0 原文（`mem0/configs/prompts.py`）：

> `USER_MEMORY_EXTRACTION_PROMPT`：**GENERATE FACTS SOLELY BASED ON THE USER'S MESSAGES.
> DO NOT INCLUDE INFORMATION FROM ASSISTANT OR SYSTEM MESSAGES.**
>
> `ADDITIVE_EXTRACTION_PROMPT`（V3）：**The user's own words are the primary source —
> if the user said it and the assistant echoed it, extract only once from the user's version.**
> … **No Echo Extraction**: When an assistant message restates, summarizes, or confirms
> information the user already provided, do NOT extract it again.

四条规律：**① 归档层全采**（无人例外，含 tool）；**② 提炼层必须知道「谁说的」**；
**③ 用户原话是主来源，AI 只贡献新增信息**；**④ 记忆主语默认是用户**。

## 3. 决策：方案 B（分源提炼 + 来源标注）

不取「只从用户提」（会丢掉排查出的根因、踩坑这类最值钱的 `pitfall` / `decision`），
也不取「只补采集」（LLM 仍会把回声当事实）。取两者之间：

| 层 | 做法 |
|:--|:--|
| 采集 | ① `UserPromptSubmit` 落一条 `user-prompt` 证据（全宿主可用、prompt 是干净原话）；② transcript 补 WorkBuddy 顶层 `role/content` 解析（可回填历史）。两者可能采到同一句，由提案去重处理 |
| 清洗 | WorkBuddy 把 `system-reminder` 与 `<user_query>` 拼在一条 user 消息里。归档 `Raw` **原文不动**，另用 `SummaryOverride` 让查询源（展示/提案）只剩用户原话；`UserQueryText` 优先取 `<user_query>`，无标签时剥掉 reminder 块。spool segment 也带 `summary_override`（`omitempty`，其余记录字节不变），DB 写失败回放后清洗结果不丢 |
| 提炼 | `BuildPrompt` 分 `USER` / `ASSISTANT` / `OTHER` 三段，**保留 Bundle 原始序号不重排**；提示词写死五条规则（原话优先、回声不提取、同一事实只提一次、`attributed_to`、重复正文去重）。同 Bundle 内 `(speaker, payload)` 完全相同的证据只渲染首次出现 |
| 落库 | `candidates.attributed_to`（`user` / `assistant` / 空=旧行），经 `ensureCaptureSchema` 幂等补列。**以被引证据的 `EventType` 为准**，不信任模型回填；用户与助手混合时 `user` 优先 |
| 规则路径 | `provider=none`：`preference` 只从 `user-prompt` 产，工具载荷 / `unparsed` 不产事实，助手只产 `decision` / `pitfall` / `task`，且需通过 §5 的可复用结论判据；`Fact.AttributedTo` 随候选落库 |
| 历史兼容 | `meta_json` 缺 `proposal_eligible` 键时按 `event` 兜底：`post-tool-use` / `unparsed` 视为不可提案（R9 的 JSON 噪音根因），其余仍放行以免历史窗口永远 pending |

明确**不做**：不把 AI 产出整体排除；不把工具载荷当事实（仍全量归档、仍不可提案）；
不改 hook 默认开关与 `MEM_PROPOSAL_HOOK` 灰度；不新增审计事件（`prompt-submit` 已有
审计行），因此 R1 / R5 / R12 的计数口径不变。

## 4. 验收（真实 WorkBuddy transcript，临时库，新旧二进制对照）

同一份 546 行 transcript（`063320fc…jsonl`），`mem hook --event Stop`：

| 指标 | 旧二进制 | 新二进制 |
|:--|--:|--:|
| `unparsed` 行 | 527 | 509 |
| `user-prompt` / `assistant-text` 可提案证据 | 0 / 0 | **10 / 9** |
| 可提案证据总数 | 1（仅 `stop`） | **20** |
| `user-prompt` 摘要 | — | 只有用户原话，无 `system-reminder` |

三行小夹具端到端（用户偏好 + 助手踩坑 + 工具调用）：旧二进制 `status=empty`、0 候选；
新二进制 2 条候选 —— `preference → user`、`pitfall → assistant`，工具行被挡。
`internal/proposal` 的端到端用例另验证：模型故意把 `attributed_to` 填错时，
落库仍按被引证据纠正为 `assistant`。

后续（未做）：`memories` 表暂不带 `attributed_to`，`accept` 后归属信息留在候选与
`memory_evidence` 链上；若裁决界面需要长期看来源，再单独加列与迁移。

## 5. 2026-11-17 收窄：助手准入判据 + 否定语境

§3 决定「不整体排除助手」，理由是怕丢掉排查出的根因与踩坑。跑了 4 天后实测：
这个口子放进来的是**过程叙述**而不是知识。11-17 巡检时 45 条 pending 里 35 条是
`attributed_to=assistant`，其中 18 条是同一 cron 任务的进度汇报（`已完成 52/65`
→ `65/65`，每轮数字变一下就是一条新候选），5 条是巡检报告本身——**报告被下一轮
蒸馏成候选，再下一轮又成了新的噪音**（自我喂养）。真正 `user` 归属的只有 9 条。

同时另有一个独立根因：`kindRes` 只看关键词、不看语境。上述 18 条**全部**靠否定句
命中 `pitfall`——「日志无 `FAIL`/`ERROR`」「一次也没有**失败**」「失败：0 个」
说的是「一切正常」，却被判成踩坑记录。

两处收窄，都在 `internal/distill/distill.go`，不碰 schema、不改候选/记忆契约：

| 收窄 | 判据 | 挡掉 | 保留 |
|:--|:--|:--|:--|
| 助手准入（`assistantDurable`） | 含可复用结论标记（`根因`/`教训`/`必须`/`不要`/`应该`/`避免`…）**且**不含过程性标记（`进度`/`已完成`/`已落盘`/`本轮`/`DONE`/`52/65`…） | 进度汇报、巡检报告、「已修复并合并」式收尾确认 | 「报错根因是 fts5 分词，必须显式建 unicode61 分词器」 |
| 否定语境（`matchKind`） | 命中点前后 12 字内是否为否定（`无`/`没有`/`未`/`不是`/计数零/`no`），枚举式列举沿分隔符向后传递 | 「无 FAIL」「失败：0」「一次也没有失败」 | 「报错 0x8007 权限不足」「失败了 10 次才定位到」 |

`user` 来源**不加**过程性过滤：用户原话是主来源，宁可留噪也不误删。因此这一层不
解决「用户 prompt 里混进助手正文」——那是 traecode 适配器的采集口径问题（见 §6）。

**验收（临时库副本，同一批会话重放，`--dry-run --provider none`）**：42 → 9 条候选
（-78.6%），剩余 9 条全部 `attributed_to=user`。assistant 侧当前语料下的产出为 0，
说明这批 assistant 正文里没有任何一条构成「可复用结论」——豁免条款存在但没被触发。

**已知代价**：① 不带上述标记的助手结论会漏（如「这个报错是 Studio 自带 SSE 中断，
属开发环境噪音」，只含「报错」）；② 以整条摘要为抽取单元，结论与进度混在一段时
连结论一起丢。两者都是「抽取单元不是原子事实」的后果，由方案 B（session summary
+ 原子化提炼）根治，不在本轮范围。

### 5.1 补漏：引号内的引用不算断言

上表验收「assistant 侧产出为 0」在 13:08 那一轮被打破：唯一一条 assistant 候选
正是**讨论判据本身**的句子——「`决策：…` 这类结构化声明本身就可复用，不该依赖
「必须/不要」等词」。判据被自己引用的文本满足，属自证。

反向的错同时存在：真结论里若引用了字面（「决策：候选必须带 `已完成` 字样」），
引号内的过程词又会把结论判成过程叙述。两个方向同源：判定把**被引述的字面**当成
了**叙述者的断言**。

修法是在 `assistantDurable` 判定前先用 `unquoted` 剥掉成对引用片段（围栏代码块、
行内代码、`「」`/`『』`/`“”`/英文双引号），只留叙述者自己的表述；不成对的半个
定界符原样保留，避免一个反引号吞掉整句。`kindRes` 与否定语境判定仍看原文——
「失败」「报错」出现在引号外时仍是叙述者自己的用词。

与 §5 的关系：这是同一个口子的补丁，不改变「以整条摘要为抽取单元」的既有结论，
方案 B 仍是根治路径。

## 6. 待处理：traecode 的 user-prompt 回声

11-17 实测有 4 条 `user` 归属候选，正文其实是**助手上一轮的收尾语**：
`调研报告已完成落盘：/Users/…`、`failed to add message to history: …`。
`EventType=user-prompt`，`project` 却是 `assistant`。即 traecode 适配器把助手
消息也写进了 user-prompt 证据，§3「用户原话是主来源」的前提在这条通路上不成立。

`attributed_to` 只按 `EventType` 推断，因此这类行会被错标成用户。修法在采集侧
（适配器区分 role），属多 harness 一致性，需先确认再动。

