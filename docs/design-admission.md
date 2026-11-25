# 准入判定（`mem judge`）

> 目标：把「候选积压」从人工按钮变成自动判定，同时不把未标定的推测写进主库。
> 判据权威：[`VALUE.md`](./VALUE.md) §4「梦的产出不回写主库」及其例外三条。
> 相关：[`design-write-governance.md`](./design-write-governance.md)（三态与矩阵）、[`ops-watch.md`](./ops-watch.md) §3 R8。
>
> **2026-11-25 取代声明**：本文件 §2「模型不能比矩阵更宽」与 §3 影子期转正门槛
> （≥95%/≥200 才放行 `--apply`）已被 [`design-data-lifecycle.md`](./design-data-lifecycle.md)
> §6/§7 取代——候选由模型自动裁决、人工**后审核**。本文件保留判定器实现细节
> （理由码、模型/CLI 接入、`mem batch` 回滚、数据观测）；影子期降级为**标定工具**，
> 不再是放行门槛。改造落点见该文件 §6.3 与 §10。

## 1. 为什么是判定，不是抽取

候选积压的成因不是「判断不了」，是「没人点按钮」：本机实测 74 条候选里 22 条被拒、51 条挂着，
`rationale` **全空**。抽取（蒸馏）已经能自动产出候选，缺的是最后一步的取舍。

因此判定器的职责被收窄成一句：**只回答「这条要不要」，不回答「这条该怎么写」**。
后者是蒸馏的活，混进来会让两层判据互相遮蔽。

## 2. 权限边界：模型不能比矩阵更宽

`internal/commit/policy.go` 的矩阵已经定义了「哪些类必须人工」。判定器沿用同一组判据，
不新造一套：

| 判据 | 模型可自动 accept | 依据 |
|:---|:---|:---|
| `kind ∈ {fact, task}` | 可以 | 矩阵对这些类判 `auto_commit` |
| `kind ∈ {pitfall, preference, decision}` | 不可以，降级 `unsure` | 矩阵判 `human_accept` |
| `scope ∈ {global, cross_project}` | 不可以，降级 `unsure` | 全局知识错了污染面最大 |
| `project` 为空 | 不可以，降级 `unsure` | 无归属即全局 |
| `reject` | 任何类都可以 | 否决只是筛掉噪音，内容仍留在候选表可回捞 |

**模型永远不会独占地「要」一条东西；它只能「不要」，或「问你」。** 这是 C 方案在
「更自动化」与「推测不回写」之间的切分点：判错的代价偏向可回收的一侧。

## 3. 影子期（默认，不可跳过）

```
mem judge                 # 只写 verdict，status 不动，memories 零写入
mem judge --stats         # 人工一致率：verdict=accept 看人工也采纳的比例，reject 看人工也否决的比例
```

- 影子期数据落 `candidates.judge_verdict` / `judge_model`（含 prompt 版本），`rationale` 记理由码。
- 一致率的**分母是人工裁决**；判定器本身没有任何改库入口（`--apply` 已撤），分母不会被自己的产出污染。
- 转正门槛：`accept` 一致率 ≥95%，且样本 ≥200 条。未达标不放开，不因着急下调。
- **`--apply` 仍不开放**：恢复它必须与 `mem judge --rollback <judge_model>` 同批交付（§4 例外条款 (c)）。
  批量清积压走 §3.2 的 `mem batch`——它由人授权、有回滚凭证，与「模型自裁」是两回事。
- 每轮只取 `judge_verdict=''` 的 pending：影子不改 status，按「最旧的 pending」取会永远重判同一批。

## 3.1 积压触发（配好模型后自动开，可一键关）

判定走网络，不能靠「记得去跑」。挂在写侧事件末尾按阈值触发，四条闸门缺一不发。

**开关有两层，环境变量不再是唯一入口**：

| 入口 | 默认 | 作用 |
|:---|:---|:---|
| `model.json` 的 `auto_spawn` | 关 | `mem setup` / `--mode api` 配好模型后写入 `true`；`--mode off` 写回 `false`。钩子由宿主进程启动、不继承 shell 环境，所以授权必须落在文件上 |
| `MEM_JUDGE_SPAWN` | 未设 | 显式总闸：`1` 强制开（脚本/CI），`0` 强制关（一键关闭，GUI 宿主无法 export 时的退路）。其它值退回文件判断 |
| `MEM_JUDGE_MIN_PENDING` | 20 | 全库**未判定**的 pending ≥ 该值才触发。取值略高于 R8 的 FAIL 线（>15），避免刚 WARN 就烧一次调用 |
| `MEM_JUDGE_COOLDOWN_H` | 24 | 冷却小时数。一个会话能 Stop 几十次，没有冷却等于每轮都跑 |

为什么从「默认关」改为「配好模型就开」：用户**主动执行** `mem setup` 已构成显式授权，
关着却只提示 `export MEM_JUDGE_SPAWN=1` 是死路——宿主钩子读不到交互 shell 的变量。
升级后的老用户不受影响：老 `model.json` 没有 `auto_spawn` 字段，读出来就是 `false`。
`auto_spawn` 只开「跑判定」，**不开「改库」**——判定器本身没有改状态入口，影子期的红线不动。

- 只挂 `Stop` / `SessionEnd`。**读路径（PromptSubmit / SessionStart）永不触发**——判定要走网络，
  进了注入链路就直接违约。
- 拉起后不等：`startMemProc` 是 detach 的子进程，钩子立即返回；失败写 `~/.v2mem/spawn.log`。
- 先写 `~/.v2mem/judge-state.json`（上次拉起时间）再 spawn：宁可少跑一次，
  也不要因写状态失败而反复重跑。
- 判定条数取阈值本身（`--limit = MEM_JUDGE_MIN_PENDING`）：一次只清一批，
  不清空积压，避免单次调用把上下文撑爆。

## 3.2 批量准出：AI 出方案，人给授权

积压的成因常常是「没人点按钮」而不是「判断不了」。让 AI 读候选没问题，问题在它**不能同时拥有写库权限**：
`cmdAccept` 把 actor 写死成 `human`，模型批量调它会被算成人工样本，一致率恒 100%，§3 的门槛直接失效。
所以批量走三段，缺一段就退化成「提示词直接改库」：

```
mem candidates ls --status pending --policy --json > /tmp/pending.json   # 1. 只读导出
# 2. AI 读 pending.json，写 /tmp/plan.jsonl（数据文件，不入库）
mem batch --plan /tmp/plan.jsonl --dry-run                              # 3. 人过目
mem batch --plan /tmp/plan.jsonl                                        #    执行一次
mem batch --rollback <归档路径>                                          #    判错了原路退回
```

方案文件是 JSONL，字段只有三个，未知字段直接报错（防止模型把 `verdict` 当 `action` 混进来）：

```json
{"id":"e3d08eb4","action":"accept"}
{"id":"f0971547","action":"reject","reason":"process_narrative"}
```

给 AI 的提示词可以就这几行：

> 读 `/tmp/pending.json`，为每条候选给裁决：`action=human_accept` 且内容是可复用知识 → `accept`；
> `action=defer` 或明显是过程叙述、一次性回执、纯代码/日志清单 → `reject`，`reason` 从闭集里选；
> 拿不准的**不写进方案**，留给人。只输出 JSONL，每行 `{"id","action","reason"}`，不要解释。

三条硬约束：

| 约束 | 实现 | 为什么 |
|:---|:---|:---|
| 记账诚实 | actor 记 `human-reviewed-llm`，`JudgeStats` 只把 `actor='human'` 算人工样本 | 人批准 AI 方案是模型的回声，不能当作标定数据 |
| 看着执行 | 任一行不可执行（非 pending、理由码非法）就整体拒绝，不半跑 | 人看到的方案与实际写库必须是同一份 |
| 可回滚 | 执行结果归档到 `<db 目录>/plans/<时间>-*.jsonl`，内含 `memory_id` / `created_memory`；回滚只删本次新建的记忆，并带 revision 校验 | 批量越大，判错一笔的代价越大；归档之后又被裁决过的候选不按旧档撤 |

注意：`mem batch` 不会让 `--apply` 的标定门槛更快达标——它省的是「逐条敲命令」，不是「证明模型判得准」。
一致率仍要靠人独立裁决的样本来攒。

清积压还有一条**不用方案文件**的近路，只覆盖否决：

```bash
mem batch --reject-judged --dry-run     # 列出「影子判 reject、仍 pending」的候选
mem batch --reject-judged               # 执行：逐条 CommitReject，同样归档可回滚
```

它刻意不接受 accept：**模型可以自动否决，不能自动授予**。判 reject 的行本来就该出隔离区，
移出去不影响主库，判错能原路退回；判 accept 的行要进主库，必须由人给的方案决定。
同理 `--project P` 只能收窄范围，不能改变这条不对称。理由码沿用模型的 `rationale`
（不在闭集内则记 `judge_rejected`），actor 仍是 `human-reviewed-llm`，不进人工标定样本。

## 4. 理由码是语料，不是日志

`reason` 是闭集（`durable` / `process_narrative` / `transient_state` / `payload_residue` /
`no_generalizable`）。自由文本无法聚合，也挡不住提示词漂移。
它的第二个用途是**规则沉淀的输入**：同一理由码出现频次足够高时，才谈把它降级成正则/结构判据。

## 5. 模型接入：配置文件优先，环境变量次之

`~/.v2mem/model.json`（权限建议 `chmod 600`）：

```json
{
  "provider": "deepseek",
  "model": "deepseek-flash",
  "api_key": "sk-...",
  "base_url": "https://api.deepseek.com/v1"
}
```

| 优先级 | 来源 | 场景 |
|:---|:---|:---|
| 1 | `--provider` / `--model` | 临时换模型试一次 |
| 2 | `model.json` | 常态；定时任务不继承交互 shell 环境，只有文件是确定性输入 |
| 3 | `DEEPSEEK_API_KEY` / `DEEPSEEK_BASE_URL` 等环境变量 | 文件没配时的兜底 |
| 4 | `auto`（本地 ollama → openai） | 都没配时 |

- **DeepSeek 走 OpenAI 兼容协议**（`/chat/completions`），只是 base 与 model 名不同；
  代码里复用同一个 `completeOpenAI`，没有第二套客户端。
- 模型名留空且 `provider=deepseek` 时默认 `deepseek-flash`，**不会**落到 ollama 的 `qwen2.5:7b`。
- 密钥写文件而不是走命令行参数：命令行会进 shell history，文件不会。
- 候选正文会发往该端点，这是 `VALUE.md` §4 例外的边界——所以 `provider` 必须显式配，
  不允许「没配就随便挑一个远端」。

- **CLI 路径（`mode=cli`）**：`mem setup` 探测本机已登录的 CLI 后写 `{"mode":"cli","cli":"claude","model_tag":"…"}`。
  优先级上，显式 `--provider` 仍最高；否则 `mode=cli` 压过表中的环境变量与 `auto`（优先级 3/4）。
  `cli` 字段只存白名单名（`claude` / `codex` / `cursor-agent` / `gemini` / `copilot` / `qoder` / `codebuddy` / `traecli`），不存命令串、不拼参数——模板内置在二进制里，
  避免 `model.json` 变成任意命令执行入口。`judge_model` 记 `cli:<name>@<version>+tpl<ver>@judge-v2`
  （如 `cli:claude@2.0.1+tpl1@judge-v2`）：版本取 CLI `--version` 首行，探测不到记 `unknown`，不编造。
- **CLI 工具边界**：claude/codebuddy 用 `--tools ""` 全禁，copilot 用空 `--available-tools`，codex/trae 用只读沙箱，
  gemini 用 plan 只读，qoder 用非交互默认拒绝 + `--max-turns 1`；cursor 官方没有硬禁工具开关，
  `--mode ask` + `--sandbox enabled` 只是只读倾向，标为受限支持。
- **已知不兼容（不进白名单）**：`freebuff` 虽然免费，但厂商声明它是 TUI-only、没有 headless / `-p` 输出模式，
  stdin 被管道接入时仍渲染全屏 alternate buffer，无法满足「prompt 进 / 纯文本出」的 `runCLI` 契约。
  它只登记在「已识别但不支持」里：`mem setup` 探测到会点名说明原因并给替代路径，**不写盘**。
  不做 PTY 兜底——stdout 是 ANSI 全屏，既判断不了一次回答何时结束，也无法可靠提取 JSON，
  放进来等于让 dry-run 这道唯一闸门失效。
- **CLI 路径是 `VALUE.md` §4 例外 (a) 的降级**：隔离维度从「模型版本」变成「CLI + 模板版本」，
  同一个 CLI 升级后可能静默换模型。所以影子期一致率必须按该 tag **分段复核**——
  tag 变了就重新累计 ≥95%，不能拿旧 tag 的样本给新 tag 背书。

## 6. 数据与观测

| 项 | 位置 |
|:---|:---|
| 判定结果 | `candidates.judge_verdict` / `judge_model` / `rationale` |
| 落库记忆 | 走 `CommitAccept`，来源仍是 `distill-accept`（可 JOIN `judge_model` 归因） |
| 批量落库 | `commit_decisions.actor='human-reviewed-llm'`；归档在 `<db 目录>/plans/*.jsonl`，`reopen` 记撤销 |
| 审计 | `event=candidate-judge`：`provider` / `count` / `mode`（固定影子）；批量走 `event=accept|reject`，`source=batch-plan` / `batch-judged` |
| 巡检 | 影子期 `--stats` 的输出；pending 只随人工裁决、`mem batch` 或 GC 下降，判定器不改 status |

**R8 的口径不变**：pending 下降要么是人或批量裁决，要么是 GC 过期；判定只写痕迹，不应被当成「问题解决」。

## 7. 未做（按顺序，且都有前置条件）

1. **规则沉淀**：需要 ≥50 条带理由码的裁决。现在 0 条，先攒语料。产出形态拟为
   `~/.v2mem/admission.jsonl` + dry-run 计数，转正看误杀率（分母来自抽样复核，不是 dry-run 自己）。
2. **判定器级回滚**：`mem batch --rollback` 已能给「人授权的批量落库」兜底；仍缺
   `mem judge --rollback <judge_model>`（按模型版本整体撤销）。后者是恢复 `--apply` 的前置——
   按 §4 例外条款 (c)，单独放行 `--apply` 视为违约。
3. **R8 增列**：影子准确率进 detail，需同步改 `docs/ops-watch.md` 与脚本两处。

## 8. 已知限制

- 没有可用模型时（`auto` 落空、或 `deepseek` 缺 key），判定器明确报「不可用」而不是
  「判定 0 条」——后者会被误读成「候选都干净」。
- 一次调用判一批（默认 20 条），批量大会稀释注意力；超长候选正文截断到 600 rune。
- 模型看不到全库，判不了「这条是否与已有记忆重复」；去重仍由 `ux_cand_hash` 与采纳时的
  overwrite 判定负责，判定器不参与。
