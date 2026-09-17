# v2mem R&D · 激活模型与评测机制

> 本文是 `docs/DESIGN.md` 的专题切片，供按需阅读；各主题全文目录与关联集中在 [DESIGN 索引页](./DESIGN.md)。

## 15. 激活模型与评测机制（迁移前的前置判断）

### 15.1 `mem` 不是后台任务，是「需要时激活」

实测确认：**没有任何常驻进程**（`ps` 无 `mem` 进程）、无 `daemon/serve` 子命令、
无 LaunchAgent、无 crontab 条目。二进制是普通的按需调用 CLI。

三条激活路径，性质完全不同：

| 路径 | 触发者 | 保证强度 |
|:---|:---|:---|
| 钩子（SessionStart / UserPromptSubmit） | 宿主工具（Claude Code / TraeCode / CodeBuddy / Codex / QoderWork） | **强**：事件触发即执行，不依赖模型意愿 |
| 模型主动调用（`mem search` / `mem add`） | 模型的判断 | **弱**：指令级，模型可能不遵守 |
| WorkBuddy 定时自动化 | **宿主平台调度** | **弱**：本机没有 cron/launchd；平台没在跑就不触发 |

### 15.2 定时任务不能作为保证 —— 改为挂在「必然发生的事件」上

本机核查：`~/Library/LaunchAgents` 无相关工作项、`crontab` 无 v2mem 条目。
所以「每日维护」实际依赖 WorkBuddy 平台在线触发，**这不是可依赖的保证**（机器关机、应用未开、
平台侧异常都会漏）。

结论：**维护不该以调度器为前提**。设计上应把维护挂到「必然发生的事件」——
SessionStart 是天然的必然事件（用户每次开新会话都会发生），它同时也能承载
「机会性维护」。但要满足三个条件才可行：

1. **增量且有界**：只处理「比上次 ingest 更新的文件」，并设条数/时间预算；
2. **不得阻塞注入**：记忆注入必须先完成并输出，维护要么放在注入之后、要么另起进程；
3. **失败必须静默**：维护失败不能让宿主会话收到错误。

当前实现尚未做机会性维护（`mem hook` 只做检索与注入，实测 1–3 ms）。这是**已知待办**，
在样本积累到能判断收益之前不做 —— 先别为一个未验证的收益去增加会话路径的风险。

### 15.3 两个方向都必须能评（否则替换草率）

| 方向 | 问题 | 手段 |
|:---|:---|:---|
| 用户会话 → mem 查询 → 命中是否准 | 用户换个问法，还找得到吗？ | `mem eval recall --gold`（金标准）／`mem audit`（真实样本沉淀） |
| 用户会话 → mem 记录 → 记录是否准 | 该记的记下了吗？记的全是废物吗？ | `mem eval write --session --gold` |

**三层评测，成本递增、可信度递增**：

| 层 | 手段 | 能证明什么 | 自动 |
|:---|:---|:---|:---|
| L1 索引可用性 | `mem eval recall --auto` | 索引与中文切分没坏 | ✅ |
| L2 检索相关性 | `mem eval recall --gold`（人工标注、来自真实会话的查询） | **唯一能支撑替换决策的指标** | ❌ 需标注 |
| L3 端到端任务成功率 | 有/无记忆两臂对比同一批需要历史知识的问题 | 记忆是否真的提升了任务表现 | ❌ 需设计 A/B |

### 15.4 口径红线（必须先定死，改动须同步本节）

| 项 | 口径 | 理由 |
|:---|:---|:---|
| Recall@k | **宏平均**：Σ(命中 gold 数 / 该样本 gold 数) / N | micro 会让 gold 多的样本主导指标 |
| 命中判定 | **`content_hash` 前缀精确匹配** | 用文本包含会让「换了说法」也算命中，自欺 |
| 分母 N | **有效样本数**；gold 不在库中的样本必须**显式剔除并上报** | 静默剔除等于篡改分母 |
| `EmptyRate` | 只统计「检索完全无结果」 | 与「有结果但没命中」混同，会把「索引挂了」和「排序不好」算成一个数 |
| k | 必须显式；默认 5 | 与 hook 的注入条数（3）**不是一回事**：评测看检索能力上限，hook 看注入预算 |
| `--auto` 的数字 | **不得单独引用**，输出里强制带「下限测试、不代表真实质量」声明 | 查询就是内容本身，不存在「用户换说法」这层困难 |

### 15.5 首次运行就抓到一个真实缺陷（评测机制的价值证明）

审计日志跑起来后立刻发现：

```
prompt-submit  claude  命中 0 空=true  v2mem 的钩子会不会阻断会话
```

定位：**中文查询的词组内是 AND，而词间是 OR**。自然语言长问句没有空格，
整句被当成**一个词组** → 要求全部 bigram 都出现在同一条记忆里 → 必然 0 命中。

实测对比（修复前）：

| 查询 | 命中 |
|:---|:---|
| `日志怎么搬进记忆库` | **0** |
| `日志 记忆库` | 3 |

修复：**精确优先、空手才放宽** —— 先用原表达式；无命中时退化为「全 term OR + bm25 排序」。
保留精度（精确有结果时不放宽，有测试固化），只在完全空手时牺牲精度换召回。

修复后实测：`日志怎么搬进记忆库` 0 → **3 条**，钩子已能注入正确记忆。

> 这是本次「先建评测再迁移」的直接收益：**如果先迁移了，这个缺陷会在真实会话里
> 表现成「记忆系统没用」**，而不是表现为「一个可定位的检索缺陷」。

### 15.6 顺带堵掉的静默陷阱

`mem search "查询" --json` —— Go 的 flag 解析在**首个位置参数处停止**，
所以写在查询之后的选项会被当成查询内容：不报错、也搜不到东西。
已加守卫：查询里出现已知选项名即报错并指明写法（只认已知选项名，
避免把正文里真的含 `--global` 的记忆误判）。

### 15.7 替换决策的判据（现在还不能下结论）

**不能**：L2 需要真实会话样本 + 人工标注，而 `mem audit` 是本次才埋的点，
可评测样本当前为个位数。L1（`--auto`）虽然 100%，但那是下限测试，**不构成证据**。

建议流程：

1. **并行期**：AGENTS.md 钩子已接入（文件与库并存），真实会话开始积累审计样本；
2. **积累到 ≥20–30 条带 query 的样本**后，从中挑选覆盖各类意图的查询做人工标注（gold 用 hash 前缀）；
3. 跑 `mem eval recall --gold`，看 Recall@5/MRR/**最差样本清单**；
4. 若最差样本里出现「明显该召回却没召回」，**先修检索**再谈替换；
5. 同时用 `mem eval write --session --gold` 抽查日志搬运的漏记率；
6. 三项都达标后再精简 MEMORY.md —— 那时替换是有证据的，不是凭感觉的。

在此之前，`MEMORY.md` **保持现状不动**。

### 15.8 衔接状态核实（2026-09-17 实测，纠正一处想当然）

「装上了」不等于「会生效」。逐项核实结果：

| 工具 | 接入方式 | 配置文件 | 是否会产生审计样本 |
|:---|:---|:---|:---|
| Claude Code | native | `~/.claude/settings.json` ✅ | ✅ 已装 |
| TraeCode | native | `~/.trae-cn/hooks.json` ✅ | ✅ 已装 |
| CodeBuddy Code | native | `~/.codebuddy/settings.json` ✅ | ✅ 已装 |
| Codex CLI | native | `~/.codex/hooks.json` ✅ | ⚠️ 需用户先在 `/hooks` 里 **trust**，否则不执行 |
| QoderWork CN | native | `~/.qoderworkcn/settings.json` ✅ | ⚠️ 需**重启**（不支持热加载） |
| **WorkBuddy** | instruction | AGENTS.md 的指令块 ✅ | ❌ **无原生钩子**（见下） |
| **TraeWork** | instruction | 同上（AGENTS.md 共享） | ❌ **无原生钩子** |
| **DeepSeek Harness** | bridge | 需 `dsh plugin --profile add dsh-hooks-claude-code` | ❌ **dsh CLI 未安装**，桥接包未装 |

**WorkBuddy 的核实结论**：`~/.workbuddy/settings.json` 存在，但内容是应用自身配置
（IM 通道绑定、凭据、连接方式），**没有 `hooks` 段**。故 WorkBuddy 确实没有用户可配的
shell 钩子，指令级判断成立。

> 附带的两次教训：
> 1. 首次核查用 `ls | head -6` 列目录，把 `settings.json` 截掉了，差点得出错误结论。
>    **列目录不要截断**，否则「没看到」会被当成「不存在」。
> 2. 该 settings.json 以**明文**保存了第三方 appSecret 与 botToken。这不在本任务范围内，
>    但值得单独收紧（文件权限或凭据外置）。

### 15.9 🔴 已修的关键缺口：`mem search` 原先不写审计

**问题**：审计原先只在 `mem hook` 里写。而 WorkBuddy / TraeWork 是**指令级**接入 ——
它们没有原生钩子，模型只能靠手工调 `mem search` 来用记忆库。于是：

> 在 WorkBuddy 里用一个月，审计日志**一条都不会增加**，回来也标注不出任何东西。

这使「先并行跑一段时间再评测」的计划对**最主要的两个工具失效**。

**修复**：`mem search` 也写审计（`event=manual-search`，默认开启，`--no-audit` 关闭），
记录真实 query、命中记忆、耗时。审计写失败静默 —— 它只是观测，不是功能。

**实测**：模拟指令级路径检索 3 次后，审计 5 → 8 条，`按事件: manual-search=3 prompt-submit=4`。
两类事件现在都能积累：`prompt-submit`（原生钩子工具）与 `manual-search`（指令级工具）。

### 15.10 所以「随便用哪个都会生效」不成立，差异如下

| 你用什么 | 钩子自动注入 | 审计积累 |
|:---|:---|:---|
| Claude Code / TraeCode / CodeBuddy | ✅ 自动 | ✅ |
| Codex | ✅（先 trust 一次） | ✅ |
| QoderWork CN | ✅（重启后） | ✅ |
| **WorkBuddy / TraeWork** | ❌ 靠模型遵守指令 | ✅ 仅当模型主动 `mem search`（修复后） |
| **dsh** | ❌ 未衔接（CLI 未装） | ❌ |

结论：**「回来激活评测」这件事，前提是你主要在原生钩子已生效的工具里工作**。
若主要在 WorkBuddy 里工作，样本只能来自「模型主动检索」，量会少得多，
且依赖模型是否遵守 AGENTS.md 的指令 —— 这本身也是可以观察的：`mem audit` 里
`manual-search` 的占比就是「指令遵守率」的一个代理指标。

### 15.13 有钩子之后，什么变确定了、什么没有

**不能笼统回答「都能确定性触发」。** 把链路拆成五段，确定性各不相同：

| 阶段 | 由谁执行 | 有钩子后 | 说明 |
|:---|:---|:---|:---|
| **检索** | 钩子调 `mem hook` | ✅ **确定** | 事件触发即执行，不依赖模型意愿 |
| **注入进上下文** | 钩子 stdout | ✅ **确定** | 文本确实进入了模型上下文 |
| **模型采用注入内容** | 模型 | ❌ 概率 | **注入 ≠ 使用**。放进上下文不等于它会照做 |
| **记录（写入库）** | 目前靠模型主动 `mem add` | ❌ 概率 | 没有哪个事件能替模型决定「哪句话值得长期记」 |
| **观测（审计）** | 钩子 ＋ CLI | ✅ **确定** | 读侧写侧都留痕，可事后度量 |

精确说法：**「用」的前半段（检索＋注入）确定，后半段（模型照做）不确定；
「记」整体仍不确定** —— 这是二者最大的不对称。

**为什么「记」难以确定化**：钩子能捕获的都是「发生了某事」的通知
（SessionStart / UserPromptSubmit / Stop / SessionEnd）。
但「这段对话里哪句话是值得长期保留的原子事实」是**语义判断**，shell 钩子没有这个能力。

**唯一能把「记」也确定化的途径**（有代价，故未采用）：

用 `Stop` / `SessionEnd` 钩子 ＋ stdin 里的 `transcript_path` 做**机械搬运**：
把会话记录交给 `mem ingest` 抽条目入库。它把「记」变成确定，但：

- 得到的是**无判断力的机械抽取**（碎片、噪声），需后续 `consolidate` 清理
- **`transcript_path` 的可用性因工具而异**：Claude Code / Codex / Qoder / QoderWork
  的 stdin 提供该字段；而 **TraeCode / TraeWork 文档记的字段只有
  `session_id` / `cwd` / `hook_event_name` / `workspace_roots` —— 没有 transcript_path**，
  这两家根本做不了
- `Stop` 在部分工具是**每轮**触发而非每会话，直接搬运会放大噪声

⇒ **不做**，除非有证据表明「模型不记」是主要瓶颈。而**观测已经就位**：
`mem audit` 的读/写两侧计数就是判断依据。

### 15.14 写侧审计（本次补齐的观测缺口）

此前只有读侧写审计，于是「模型到底记了什么」**完全不可观测** ——
而「用户会话 → mem 记录 → 准确记录」这条评测方向正需要它。

现在 `mem add` 也留痕（`event=manual-add`），并区分 `mode`：

| 字段 | 用途 |
|:---|:---|
| `mode=create` | 新增。说明模型记了库中没有的东西 |
| `mode=overwrite` | 命中「相同知识覆盖」。**重复记录率**＝overwrite/写侧总数 |

`mem audit` 的输出相应拆成两侧：

```
读侧/写侧 : 17 / 1 次（读/写比 1700%）
写侧构成  : 新增 1，命中覆盖 0（重复记录率 0%）
```

**为什么要分开看**：只看总次数看不出问题。**只记不查**（写了一堆但从不检索）
与**只查不记**（检索但从不沉淀）是两种完全不同的故障，各自对应不同的修法。

### 15.12 指引本身是坏的：选项位置踩坑（含扩散修复）

`mem search "<关键词>" --scope current --json` 与 `mem ingest <文件> --project <工程>`
这两条命令都**执行失败**，而它们出现在**每次注入的用法提示**与**指令级接入块**里 ——
等于把坏命令教给模型。Go 的 flag 在**首个位置参数处停止解析**，选项被当成位置参数：

| 写法 | 结果 |
|:---|:---|
| `mem search "<关键词>" --scope current --json` | 被 §15.6 的守卫拦下并报错（提示选项要在前） |
| `mem ingest <文件> --project <工程>` | `错误: open --project: no such file or directory` |

**扩散范围（四处）**：`hook.go` 的 `hintBlock()`、`init.go` 的 `instructionBlock()`
（search 与 ingest 各一处）、`level1.go` 的 `budget` 建议行。其中 `instructionBlock`
那一处已被写进 `training-products/AGENTS.md` 并合入 main，需要重新生成。

**修复**：全部改为选项在前；`AGENTS.md` 用 `mem init --file` 就地替换重新生成
（差异仅那两行，门禁 PASS，tokens 4882 → 4962）。

**护栏**：新增 `TestGuidanceCommandsAreExecutable` —— 抽取 `hintBlock()` 与
`instructionBlock()` 里所有 `mem` 命令，断言**选项必须写在位置参数之前**；
另有 `TestGuidanceNamesRealSubcommands` 断言引用的子命令真实存在。
两个用例都做了变异验证（把写法改回错误形式即报警）。

> 写这个护栏时自己也踩了两次：抽取器第一版只认反引号（`hintBlock` 用的是双引号），
> 校验器第一版把**子命令名**与**选项的值**（如 `--scope current` 的 `current`）都当成了
> 位置参数，导致所有命令被误判违规。这本身就是个提醒：**校验器的正确性也要被验证** ——
> 所以用例里加了「至少校验 N 条命令，否则视为抽取器失效」的前置断言，
> 防止它退化成空转。

### 15.11 隐私提示

审计日志记录**真实用户提问原文**（`prompt-submit` 的 `query`）。它落在
`~/.v2mem/audit.jsonl`，仅本机。若某些提问不便留痕，用 `--audit -`（钩子）
或 `--no-audit`（手工检索）关闭；也可定期清理该文件（它只是评测样本，不影响记忆库）。

## 16. 「报」的定位：eval 的用量视图（report 消解）

> 2026-09-17 定稿。起因：training-products 曾用 AGENTS.md 指令要求「任务收尾输出
> mem 评测结论」，不生效；调查报告钩子时发现该需求与 harness 事件的匹配是错的，
> 遂把「报」从「任务收尾约定」重新定位为「eval 的用量视图」。
> 2026-09-17 收口：`mem report` **直接删除**（不做别名），收尾结论由 task-finish.sh
> 调 `mem eval activity` 产出并**追加写进项目根 `.mem/report.md`**。

### 16.1 三类功能定位不同，要求也不同

| 功能 | 所属层 | 对 harness 依赖 | 确定性 | 未来方向 |
|:---|:---|:---|:---|:---|
| 查 | 记忆核心（检索＋注入） | 高：SessionStart / UserPromptSubmit 钩子注入 | ✅ 确定（注入侧） | **替代 harness 自身 memory**：跨会话、跨 Harness、跨机器共享 |
| 记 | 记忆核心（沉淀） | 高：但没有事件能替模型做「总结提炼」的语义判断 | ❌ 概率（这是核心能力，不是缺陷） | 同上；语义判断依赖模型不变 |
| 报 | 观测／评测层 | **零**：纯函数作用于审计日志 | ✅ 确定 | 归并进 eval，消解独立功能 |

**结论：报不该与 harness 有钩子关系。** 查、记是记忆系统的功能，注定长在 harness
里（替代其原生记忆机制）；报是评测，和 eval 同级，评测不依赖宿主的事件。

### 16.2 为什么报告钩子不成立（调研结论，2026-09-17）

对 Claude Code / TraeCode / CodeBuddy / Qoder / QoderWork / Codex / TraeWork
逐一核对官方文档后：

1. **没有通用的「任务结束」事件。** Stop 是**每轮**回复结束触发；SessionEnd 各家
   有（TraeCode 官方事件表**没有**，TraeWork 的来自静态分析未经实机验证），但
   「会话结束」≠「任务结束」；Qoder 的 `TaskCompleted` 是唯一接近的，仅一家有。
2. **SessionEnd 的输出用户看不见。** Claude Code 文档明确其 stdout **被丢弃**；
   Stop 的「注入上下文」≠「用户看到」，仍要模型复述（概率环节），除非 exit 2
   阻断（每轮扰民）。
3. **报告不是注入型钩子。** `mem hook` 的两种事件承载「注入内容」；报告承载的是
   「分析结果」——钩子能提供的只是「何时触发」，而「何时」恰恰是 harness 给不了的。

**数据层与触发层分离**：报的输入（审计日志）由查、记的埋点写入（`prompt-submit`
钩子 ＋ `manual-search` CLI，均确定）；报的分析与输出不需要任何事件——它只是
审计日志的纯函数。所以剥离报告钩子不损失任何确定性，只去掉了一套错误的触发机制。

### 16.3 消解设计：report = eval 的用量视图

把 report 折叠进 eval：eval 成为「**质量 × 粒度**」的评测面，`report` 的名字与
独立生命周期消失。

```
mem eval recall   --gold/--auto --k                       # 质量族：检索准不准（已有）
mem eval write    --session --gold                        # 质量族：记录准不准（已有）
mem eval activity --scope period|session|task             # 用量族：读/写活动＋空命中＋四态结论
                   [--since D] [--session S] [--project P] [--top N]
```

| 粒度 | 语义 | 数据来源 | 现状 |
|:---|:---|:---|:---|
| `period` | 一段时间内的汇总 | audit 时间戳 | ✅ `mem eval activity --scope period --since` |
| `session` | 单个会话的细钻 | audit 的 `session_id` | ✅ `mem eval activity --scope session --session` |
| `task` | 单个任务的细钻 | 外部锚点（见 §16.4） | ✅ 显式窗口（task-finish.sh 落地） |

归并理由：period 粒度与 eval 的时间窗本就重叠；归并后减掉 report 的 CLI 入口、
帮助文案、以及「任务收尾要单独输出一段」的整套约定。

**`mem report` 直接删除，不做别名**（用户定稿）。原因：保留别名等于给旧命令留
两条语义通路，而 activity 已完整承接其能力（时间窗/会话过滤、空命中查询、四态
结论），别名只增加维护面。删干净：`main.go` 子命令清单、usage、`init.go` 指引、
README、测试全部同步移除。

### 16.3b 收尾结论的落盘：`.mem/report.md`

「报」的输出简化成**项目内一个文件**：`task-finish.sh` 调
`mem eval activity --scope task --since <任务时长>`，把结论**覆盖写**进项目根
`.mem/report.md`（目录 `.mem/` 不入库，见 training-products 的 `.gitignore`）。

- 固定文件名 = 「最新一次任务的结论」，旧结论随每次收尾被顶掉，不需要版本管理
- 文件是给**下一次任务的人/AI 看的留痕**，也是 period 级细钻的原料
- 不用 `.txt`：`.md` 在工具里可点击预览；无 markdown 渲染的环境里纯文本同样可读
- 未安装 mem 时写一行说明（「未安装 mem，本段由 task-finish.sh 自动产出」），
  文件始终存在，引用它的约定不会因环境缺失而悬空

### 16.4 任务粒度的锚点（唯一需要设计的地方）

任务粒度无法从 harness 事件或 audit 直接推出，需要外部锚点：

| 方案 | 做法 | 优点 | 代价 |
|:---|:---|:---|:---|
| **显式窗口**（推荐 v1） | `--scope task --since <起点>` 由调用方给出 | 零新增状态；可移植 | 任务无「身份」，无法跨次聚合 |
| 任务登记文件 | task-start 写 `~/.v2mem/tasks/<project>/<name>.json`，eval 用 `--task <name>` 查 | 有身份，可直接聚合「最近 N 个任务」 | 新状态；跨机器同步要处理 |
| git 推断 | 自动取当前分支首提交 | 零参数 | 仅 git 项目成立；分支 ≠ 任务，不做通用默认 |

**v1 用显式窗口**。training-products 的落地已按此实现：`task-finish.sh` 取分支
首个提交时间 → `--since <窗口>`（分支在收尾清理时删除，需在删分支前记住起点）。
登记文件等「任务身份聚合」需求出现再做。

**细钻／上卷闭环**：原达标判据「连续 10 次任务多数为两侧都有活动」变为——
每次 task-finish 产出任务级结论留痕，`--scope period` 视图聚合最近 N 个任务的
结论分布。确定性来自脚本，不再依赖模型自觉。

### 16.5 连带变化与明确不做

- `mem init` **保持只写两个事件**（SessionStart / UserPromptSubmit）；报告钩子不做。
  `harness.go` 里已建模的 `EventStop` / `EventSessionEnd` 保留归一化能力，暂不消费。
- **明确不做**：Stop / SessionEnd ＋ `transcript_path` 机械搬运（沿用 §15.13 判断——
  transcript_path 各家可用性不一，TraeCode / TraeWork 没有；Stop 每轮触发放大噪声）。
- **诚实边界**：没有 task-start/finish 这类确定性任务边界的项目，任务级自动退化为
  session / period 级；「记」的语义判断依赖模型的事实不变——那是核心能力，不归评测层。

### 16.6 实施记录（2026-09-17 完成，TDD ＋ 覆盖率门禁）

1. `cmd/mem/measure.go`：**删除 `cmdReport` / `reportVerdict`**；`cmdEval` 增加
   `activity` 视图（`--scope period|task|session` 三粒度，`activityVerdict` 复用
   原四态结论口径，`emptyQueries` 保留）
2. `cmd/mem/main.go`：usage / 子命令清单 / switch 三处删除 report；
   `cmd/mem/init.go`：指引块改指 task-finish.sh 自动产出；`hook.go` **未动**
   （验证报告钩子确实无需落地）
3. README 命令一览：eval 表补 activity 行；report 行删除
4. training-products：`task-finish.sh` 改调 `mem eval activity --scope task --since …`，
   结论**覆盖写** `.mem/report.md`；AGENTS.md 指引同步；`.gitignore` 加 `.mem/`
5. 测试：activity 视图的粒度过滤（period / session / task）、四态结论口径复用、
   「读不到日志≠无活动」红线；覆盖门禁照旧（cmd/mem 81.5% ≥ 81%）

