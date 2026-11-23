# 设计：模型来源配置（api canonical / cli 便利路径）

- 日期：2026-11-23
- 状态：已批准（用户确认「通过，按推荐写 spec」）
- 分支：`feat/model-provider-setup`（worktree `.worktree/model-provider-setup/`）
- 权威：[`docs/VALUE.md`](../../VALUE.md) §4 例外三条、[`docs/design-admission.md`](../../design-admission.md) §5、[`AGENTS.md`](../../../AGENTS.md)

## 1. 问题与范围

本机诊断（2026-11-23）：源码 HEAD `v0.8.1` 已含 `mem judge`，但 PATH 上是 `v0.6.1` 旧二进制、
`~/.v2mem/model.json` 从未创建、三条 judge 相关环境变量均未设，判定器一次都没跑过。
**能力合入 ≠ 配置完成**：现在缺一个「零填写」的落盘动作，用户要自己读 `design-admission.md` §5
手搓 JSON、自己申请 key、自己 `chmod 600`。

本 spec 只做**第一块子项目**：api + cli 配置整合、`mem setup` 向导、`docs/install.md` 落地。
**更新能力（标准更新 / 自动检查 / 自动更新）另立第二份 spec**，本文件不设计、不实现。

范围外：ACP provider；`--apply` 转正与 rollback；MCP / hooks 判据改动；修改 `VALUE.md` 红线。

## 2. 决策记录

| # | 问题 | 结论 |
|:---|:---|:---|
| D1 | 拆几份 spec | 拆两份，先做本份（judge 配置/向导），更新能力下一轮 |
| D2 | CLI 接管范围 | **judge + 蒸馏共用同一份 CLI 配置**（不采纳「只接管 judge」） |
| D3 | 向导命名 | **复活 `mem setup`**，作为「本机模型来源」总向导；hooks 接入仍归 `mem init` |
| D4 | 交互深度 | **全自动探测、零选择**：零填写、零提问 |
| D5 | 配置整合形态 | **方案 A：单文件 + `mode` 判别**，老文件无 `mode` 归一化为 `api` |
| D6 | 零选择 × 红线 | **零填写 ≠ 零告知**：主动执行 `mem setup` 即显式授权，但必须打印数据外发告知、dry-run、回退命令 |
| D7 | model_tag | 记为 `cli:<name>@<cli-version>+tpl<ver>@judge-v2`，并在设计文档标注为 §4(a) 的**降级** |
| D8 | CLI flag | 具体 flag / 输出格式 / 能否取 model 字段**全部标「未测」**，实现期本机实测 |

被否决的方案：B 双文件（两份配置 + 优先级规则扩散，「零选择」下状态更多）、
C 扩 `provider`（污染 `ModelSpec` 与 `--provider` 枚举）、纯静默零选择（违反 §5「provider 必须显式配」）。

## 3. 现状勘误（实现前必读）

| 路径 | 实际配置来源 | 后果 |
|:---|:---|:---|
| `cmd/mem/judge.go:101` | `LoadModelConfig(model.json)` | 唯一读 `model.json` 的地方 |
| `cmd/mem/distill.go:46` | `distill.LoadConfig(config.toml)` 只取 model / timeout | **不读 `model.json`** |
| `cmd/mem/distill.go:68,84` | Proposal local 硬编码 `ollama`、byok 硬编码 `openai` | CLI 无法只靠改 judge 生效 |
| `cmd/mem/distill.go:446` | `distill.Run(... Options{Provider: provider})` | 走 `resolve()` → `completeLLM`，不走 `PromptCompleter` |
| `internal/distill/completer.go:31` | `PromptCompleter` 也调 `resolve()` | 与 `Run` **共用同一个 `resolve()`** |

结论：不要在每个调用点各自判断 mode。把 CLI 分派放进 `resolve()` 的入口，
`PromptCompleter` 与 `Run` 两条路径自动同时受益，调用方只需把配置字段填进 `Options`。
`config.toml` 保留，继续只承担 model / timeout；**不再承担 provider 职责**（它本来也没有）。

## 4. 配置 schema（`~/.v2mem/model.json`，权限 0600）

```json
{
  "mode": "cli",
  "cli": "claude",
  "model_tag": "claude@2.1.34+tpl1",
  "provider": "deepseek",
  "model": "deepseek-flash",
  "base_url": "https://api.deepseek.com/v1",
  "api_key": "sk-..."
}
```

| 字段 | 语义 |
|:---|:---|
| `mode` | `api`（缺省）\| `cli` \| `off`。老文件无此字段一律按 `api`，**完全保持现有行为** |
| `cli` | 内置白名单里的 CLI 名，**不是命令串**；配置里绝不存可执行命令 |
| `model_tag` | 由 `mem setup` 探测 `cli --version` + 模板版本写入，供 `judge_model` 归因 |
| `provider` / `model` / `base_url` / `api_key` | `mode=api` 时沿用现状；`mode=cli` 时忽略但**保留不删**，便于 `--mode api` 切回 |

归一化规则：
- 未知 `mode` 值 → 报错，不静默退化成 `api`。
- `mode=off` → judge 明确报「判定器不可用」；蒸馏退回 `--provider none` 规则路径。
- `mode=cli` 但缺 `cli` 或 `model_tag` → 报错并提示重跑 `mem setup`。
- `mode=cli` 时**不得**读取 `api_key` / `base_url` 出网，避免两条通道混用；
  配置文件里的这四个字段在该模式下不参与解析（但保留不删）。

优先级（不变）：`--provider` / `--model` / `--config` flag > `model.json` > 环境变量 > `auto`。
`mode=cli` 下用户显式 `--provider deepseek|none|ollama` 时以 flag 为准（临时试一次），不改写配置文件。

**优先级的单一实现点**：新增 `distill.ResolveOptions(cfg ModelConfig, flagProvider, flagModel string) Options`，
所有调用点都经它构造 `Options`。规则是「flag 显式给了 provider → 清空 `CLI`，按 flag 走；
否则 `mode=cli` → 填 `CLI`/`ModelTag`；否则按现有 `mode=api` 逻辑」。
不允许各调用点自行判断 flag 是否显式给过——5 处构造点各判一次必然漂移。
`resolve()` 本身只看 `Options.CLI` 是否非空，不重复判优先级。

## 5. 组件

| 文件 | 改动 |
|:---|:---|
| `internal/distill/modelconfig.go` | `ModelConfig` 加 `Mode` / `CLI` / `ModelTag`；`LoadModelConfig` 归一化老文件与校验；新增 `SaveModelConfig`（0600、写前备份、原子替换）与 `ResolveOptions()`（§4 优先级单一实现点） |
| `internal/distill/cli.go`（**新文件，同包**） | 内置模板表（含 `tplVer`）、`Probe()`、`runCLI()`、`VersionTag()` |
| `internal/distill/distill.go` | `Options` 加 `CLI` / `ModelTag`；`ModelSpec.Kind` 增加 `"cli"`；`resolve()` 入口分派 |
| `internal/distill/llm.go` | `completeLLM` 按 `spec.Kind` 走 `runCLI`（原按 Kind 分派 ollama / openai） |
| `internal/distill/completer.go` | 无需改动（自动继承 `resolve()`） |
| `cmd/mem/judge.go` | 构造 `Options` 改走 `distill.ResolveOptions`；`mode=off` 保持现有「判定器不可用」报错 |
| `cmd/mem/distill.go` | 4 处 `Options` 构造全部改走 `distill.ResolveOptions`；`config.toml` 仍只提供 model / timeout |
| `cmd/mem/setup.go`（新文件） | 新命令，见 §7 |
| `cmd/mem/main.go` | `knownSubcommands`、`switch`、`usageText` 三处同步（现有护栏断言单一清单） |
| `docs/install.md` | 新增 S6；见 §8 |
| `docs/design-admission.md` | §5 增补 CLI 路径、优先级、model_tag 降级说明 |

**不新增包** → 不动 `coverage-policy.txt`；但 `internal/distill`（下限 86）与 `cmd/mem`（下限 81）不得下降。

### 5.1 CLI 执行契约

- 模板表内置于代码，白名单含本机常见 harness CLI（`claude` / `codex` / `cursor-agent` / `gemini` / `copilot` / `qoder` / `codebuddy` / `traecli`，按固定顺序探测）。
  工具策略：claude/codebuddy 用 `--tools ""` 全禁；copilot 用空 `--available-tools`；codex/trae 用只读沙箱；gemini 用 plan 只读；qoder 用非交互默认拒绝 + `--max-turns 1`；cursor 官方无硬禁工具开关，`--mode ask` + `--sandbox enabled` 只是只读倾向，标为受限支持。
  每条模板含：argv、禁工具 flag、prompt 传输方式（stdin，或官方示例要求的 argv 占位位置）、stdout 出纯文本、`tplVer`。
- `runCLI`：`exec.CommandContext`，prompt 按模板走 stdin 或替换 argv 占位；stdout 收集、stderr 截断；
  超时复用 `opt.Timeout`（judge 默认 90s，distill 默认 45s）；非零退出归 `ErrLLMUnavailable`。
- 输出**不新增解析器**：仍交给 `parseJudgeVerdicts`（截首个 `[` 到末个 `]`）与 `ParseFacts`。
  dry-run 的职责就是证明模板输出落在现有解析器容差内。
- `Probe()` 用 `exec.LookPath` 按白名单顺序取第一个可用；不提问、不列编号。
- 换 flag 即升 `tplVer`，历史 `judge_model` 自动分段，不合并统计。

### 5.2 CLI 模式下的 Proposal 链

`mode=cli` 且 `--executor auto` 时只装配 `deps.Local`（CLI completer），**不装配 `deps.BYOK`**：
两者会指向同一个端点，装两级降级没有意义且会重复外发。显式 `--executor byok/host/none` 时
保持原有链语义，只是 prompt 函数由 CLI 提供。此规则需有单测断言。

## 6. 数据流

```
mem setup  → Probe() → dry-run(judge prompt + 蒸馏真实 prompt) → 通过才写 model.json(0600)
                                                            ↘ 打印外发告知 + 回退命令

mem judge  → LoadModelConfig → ResolveOptions() → resolve() → runCLI
           → parseJudgeVerdicts → 影子期（默认，只写 verdict）→ --apply 才落库
mem distill→ LoadModelConfig → ResolveOptions() → resolve() → runCLI → ParseFacts → candidates
Stop/SessionEnd → startDistillProc（detach） / maybeSpawnJudge（MEM_JUDGE_SPAWN 仍默认关）
```

硬约束：`mem setup` 不写 hooks、不写 MCP、不写 memories、不写 candidates、不启动常驻进程。
CLI 只在写侧 detach 子进程里被调用，**绝不进 PromptSubmit / SessionStart 读路径**。

## 7. `mem setup` 行为规范

```
mem setup                 # 默认：全自动探测、零选择
mem setup --dry-run       # 只探测 + 验证，不落盘
mem setup --status        # 显示 mode / cli / model_tag / 路径 / 权限 / 来源，不联网
mem setup --mode api      # 显式切回 API；key 从 stdin 不回显读入，或沿用已探测到的环境变量
mem setup --mode cli      # 显式指定 CLI 路径
mem setup --mode off      # 显式关闭
mem setup --undo          # 恢复最近一次写盘前的备份；无备份则报错并提示 --mode off
```

默认流程（有序，不可跳步）：
1. 读现状。已有配置**默认不覆盖**（幂等），打印现状并返回；要改必须显式 `--mode`。
2. `Probe()` 按白名单顺序探测本机 CLI；取第一个 `LookPath` 命中的候选。
3. dry-run 各跑一次：judge 合成候选 prompt、蒸馏真实 prompt。验证四点：
   退出码 0、`parseJudgeVerdicts` 可解析、`ParseFacts` 可解析、**stdin 关闭后不等待输入**（证明非交互、不弹权限）。
4. 候选失败 → 试下一个；全失败 → **不写盘**，逐条列出失败原因，退出码 0（可选步骤不阻塞安装），
   提示可用 `--mode api` 或 `--mode off`。
5. 通过 → `VersionTag()` 取 `cli --version`，写 `model.json`（0600、写前 `.v2mem.bak`、原子替换）。
6. **必须打印告知**（不是提问）：
   > 已将判定器与蒸馏的模型来源设为 `<cli>`。judge 候选正文与蒸馏观测摘要会发往该 CLI 所连的远端模型。
   > 回退：`mem setup --undo`。自动触发仍默认关（`MEM_JUDGE_SPAWN` 未设 1）。

与 §5 红线的调和：用户**主动执行** `mem setup` 构成显式授权；探测范围限定本机已安装且
dry-run 通过的 CLI，不是「没配就随便挑一个远端」；有告知、有 dry-run、有回退、有默认关。
因此**不修改 `VALUE.md`**，`design-admission.md` §5 只做增补。

## 8. `docs/install.md` 落地

- 新增 **S6「可选：配置模型来源（判定器 / 蒸馏）」**，放在 S5 安装后校验之后，
  **不挡 hooks / MCP 主链路**。内容：一行 `mem setup`、输出示例、完成判据
  （`mem setup --status` 显示 mode；`mem judge --stats` 不再报「判定器不可用」）。
- §1 原则补一条「零填写但需告知」。
- 删除末尾「`mem setup` 尚未实现，不要执行」，改为指向 S6。
- **不新增 A–D 判据**：避免同步改 `verify-install.sh` 与 `test-verify-install.sh`（YAGNI）。
  完成判据以文字写在 S6 内。
- 当前 `install.md` 319 行，S6 约 25 行，仍在 500 行 / 60KB 上限内。
- README「安装」表加一行「模型配置（可选）」；`docs/design-admission.md` §5 增补 CLI 路径。
- 本 spec 位于 `docs/superpowers/specs/`。按 AGENTS.md「新增文档先登记」，
  在 README「文档分工」表增加一行登记本 spec（读者：所有者 / 开发者；用途：模型来源配置与 setup 向导设计）。
  先确认无重复同主题文档：`docs/design-admission.md` 讲准入判定原理、`docs/install.md` 讲安装步骤，
  均不与本 spec 重复。实现阶段若再新增正式 `docs/*.md`，同样按 AGENTS.md 登记。

## 9. 错误处理

| 场景 | 行为 | 写盘 | 退出码 |
|:---|:---|:---|:---|
| 探测不到任何 CLI | 提示 `mem setup --mode api` 或 `--mode off` | 否 | 0 |
| dry-run 解析失败 / 超时 / 非零退出 | 换下一个候选，逐条列因 | 否 | 0 |
| 全部候选失败 | 打印「未配置，判定器/蒸馏 LLM 不可用」 | 否 | 0 |
| 已有配置 | 幂等跳过，打印现状 | 否 | 0 |
| 配置 JSON 非法 / 未知 mode | 报错，不拿默认值覆盖用户文件 | 否 | 1 |
| `--undo` 无备份 | 报错并提示 `--mode off`，不猜、不删用户配置 | 否 | 1 |
| 运行期 CLI 失败 | 沿用 `ErrLLMUnavailable` / `ErrBadResponse`，不改「判定器不可用」语义 | — | 按调用方 |

## 10. model_tag 与 §4(a) 的降级（诚实标注）

CLI 拿不到真实模型名（能否从输出取：**未测**）。`judge_model` 记
`cli:<name>@<cli-version>+tpl<ver>@judge-v2`——按 **CLI 版本 + 模板版本**隔离，而非模型版本。

映射链必须显式实现，不能靠字符串巧合：`VersionTag()` 把 `cli@<ver>+tpl<ver>` 写进
配置的 `model_tag` → `ResolveOptions` 填进 `Options.ModelTag` → `resolve()` 的 cli 分支
返回的 provider name 取 `cli:` + `ModelTag` → `cmd/mem/judge.go` 现有拼接
`providerName + "@" + judgePromptVersion` 原样不动，得到最终 `judge_model`。
因此 **不修改 `judge_model` 的拼接逻辑**，只让它拿到的 name 带上版本分量。

这是 `VALUE.md` §4 例外 (a)「按模型版本隔离」的降级，必须在 `design-admission.md` 明写：
影子期 `--stats` 的一致率按该 tag 分段；转正标定前须人工复核同一 tag 下样本是否同质；
CLI 升级导致 tag 变化时不得合并统计。若复核认为不可接受，则 CLI 路径不进入 `--apply` 转正范围。

## 11. 测试与门禁

| 文件 | 覆盖 |
|:---|:---|
| `internal/distill/cli_test.go`（新） | 模板表白名单与 `tplVer`、`Probe` 顺序、`runCLI` stdin/stdout、超时、非零退出、stderr 截断、注入式 fake exec |
| `internal/distill/modelconfig_test.go`（扩） | 老文件无 mode 归一化、未知 mode 报错、cli 缺字段报错、0600 写入、备份、原子替换；`ResolveOptions` 的 flag > cli > api 优先级与 5 处调用点一致性 |
| `internal/distill/completer_test.go`（扩） | `resolve()` 在 `Options.CLI` 非空时优先返回 `Kind=cli`；`Options.CLI` 为空时不分派；`Run` 与 `PromptCompleter` 两条路径都命中 |
| `cmd/mem/setup_test.go`（新） | 临时 HOME + 假 CLI 桩：零选择成功、全失败不写盘、已有配置不覆盖、`--mode api/cli/off`、`--undo`、`--dry-run`、`--status`、0600、告知输出含外发范围 |
| `cmd/mem/distill_test.go`（扩） | cli 模式下 `deps.BYOK` 为 nil（§5.2） |
| `cmd/mem/main_test.go`（扩） | `knownSubcommands` 护栏同步 |

门禁：`make test`、`make cover-gate`（distill 86 / cmd/mem 81 不得下降）、`make test-e2e`、
`make test-install`、`make test-verify-install`（确认未误改判据）。
合并提交正文按 AGENTS.md 用真换行写 `Base:` / `Gate:` / `Closes:` 并附测试输出尾部；
合入 main 并清理 worktree 后，先 `ls -l "$(command -v mem)"` 再询问是否 `make build`。

## 12. 未测项（实现期本机实测，不得据常识补写）

1. 白名单各 CLI 的非交互 flag 具体名称、输出格式、是否等 stdin、能否取 model 字段；除 `codebuddy` 外均未在本机实测。
2. `cli --version` 输出格式，以及取不到版本时 `VersionTag()` 的兜底串。
3. CLI 冷启动耗时是否超出 45s（distill）/ 90s（judge）默认超时。
4. CLI 在无 TTY 的 detach 子进程里是否会弹权限或要求登录；dry-run 断言「stdin 关闭后不等待」能否稳定证伪。
5. 蒸馏在 Stop 钩子被拉起时，`runCLI` 与现有 `distill_runs` 记账是否兼容。
6. `freebuff` 是否已提供 headless 入口。**2026-11 已核实为不支持**：官方博客明写
   「There is no headless mode. The CLI is a terminal UI with no non-interactive mode」，
   README 只有交互式 `freebuff`，第三方集成实测管道接入仍渲染全屏 ANSI。
   故它只进「已识别但不支持」表，供 `mem setup` 点名解释，不进白名单；不尝试 PTY 兜底。
   若官方后续交付 `exec -p`，按 §5.1 重新实测后再纳入。

以上任一项实测不符合契约，实现应回退到「该 CLI 模板不进入白名单」，**不得**放宽 §7 的验证条件。

## 13. 后续（第二份 spec，本文件不展开）

标准更新支持（与 `install.sh` 配套、版本可查）→ 自动更新检查 → 自动更新机制。
需单独处理：三种安装来源（发布版 / 开发软链 / `go install`）的更新动作差异、
`VALUE.md`「不做常驻进程」、本机无 cron/launchd 而定时走宿主平台调度、
`verify-install.sh` A1 已有的「库比二进制新 → 提示升级 mem」挂钩点，以及二进制替换的回滚。
