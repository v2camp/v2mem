# 安装与接入规范（setup guide）

- 日期：2026-09-18
- 状态：草案待复核
- 关联：`docs/DEVELOPER.md`（本地开发部署）、`docs/design-harness-memory-replace.md`（读/写接入）、`docs/design-workbuddy.md`、`docs/design-harness.md`

## 1. 背景与原则：为什么要有这份规范

本仓库工作流遵循「一任务一 worktree」；安装/接入环节却曾长期靠**直接往宿主数据目录写配置**（`~/.trae-cn/hooks.json`、`~/.codebuddy/settings.json`）。实机排查证明这是**侵入性接入**：虽然 JSON 语法正确、命令可执行，但宿主一律**不在面板内确认就不生效**——配置对、却不触发。

本规范将这些教训固化成**合规接入的唯一准则**：

> ### 合规接入五守则
> 1. **不绕过宿主面板**：接入=「产出合规配置 + 引导用户在宿主面板里确认启用」，而不是直接写宿主数据目录。
> 2. **一份配置、一种 schema**：对目标宿主按官方 JSON schema 产出，先校验再提示。
> 3. **引导而非静默**：安装后必须给出「请到 <宿主路径> 执行 <动作>」的可操作清单；不假设外部生效。
> 4. **只读护栏**：读侧（hooks/MCP 读取）绝不因被触发而往 memories 写内容（见 `TestCmdHookReadOnly`）。
> 5. **可回退**：每次写入前备份旧配置；卸载时只摘本工具的条目（`# v2mem` 标记），不碰用户自有配置。

## 2. 接入方式分层

v2mem 对宿主提供**两种合规接入**，定位互补，不可互相替代：

| 接入 | 载体 | 解决什么 | 官方要求 |
|---|---|---:|---|
| **读侧注入（hooks）** | `mem hook` | 会话开始/提问时**自动塞相关记忆进上下文**（"记得住"） | hooks 事件 + 面板启用/审核 |
| **检索能力（MCP）** | `mem mcp` (stdio) | 把 search/add/ls/touch **作为工具暴露**给支持 MCP 的宿主 | `.mcp.json` + 信任确认 |

- hooks 负责**自动喂上下文**（读侧确定性入口）；MCP 负责**能力暴露**（供 agent 显式调用）。互斥？不——v2mem 两条都提供，方向一致。
- 安装动作的目标：**让两条路都"合规产出 + 引导面板确认"**，而非侵入写入。

## 3. 支持工具（harness）清单

v2mem 内置 11 个已知 harness，全部按**应用内置运行时**判定为 `native`（原生 hooks），仅选一个有桥接位。源码：`internal/harness/harness.go`（顺序即展示顺序）。

| Name | 别名 | Label（产品） | Tier | 配置位置 | 实测 |
|:---|:---|:---|:---|:---|:---|
| claude | claude-code, cc | Claude Code | native | `~/.claude/settings.json` | — |
| traecode | trae, trae-cn, solo | TraeCode（Trae IDE/SOLO） | native | `~/.trae-cn/hooks.json` | ✅ 已验证生效 |
| codebuddy | code-buddy, cb | CodeBuddy Code | native | `~/.codebuddy/settings.json` | 待面板审核 |
| qoder | qoder-ide, qoder-cli | Qoder | native | `~/.qoder/settings.json` | — |
| qoder-cn | qodercn, lingma | Qoder CN（通义灵码） | native | `~/.qoder-cn/settings.json` | — |
| qoderwork | qoder-work, qw | QoderWork（桌面） | native | `~/.qoderwork/settings.json` | — |
| qoderwork-cn | qoderworkcn | QoderWork CN | native | `~/.qoderworkcn/settings.json` | — |
| codex | codex-cli | Codex CLI | native | `~/.codex/hooks.json` | — |
| dsh | deepseek-harness | DeepSeek Harness | bridge | 复用 `~/.claude/settings.json` | — |
| traework | trae-work, tw, trae-solo | TraeWork（原 TRAE SOLO） | native | `~/.trae-local` / `~/.trae-cn` / `~/.trae/hooks.json` | ⚠️ 面板未暴露 |
| workbuddy | work-buddy, wb | WorkBuddy | native | `~/.codebuddy/settings.json`（共用） | ⚠️ 待实机验证 |

关键事实：
- **traecode 已验证**：面板启用 + 提问后，审计出现 `prompt-submit/session-start | traecode`——读侧注入闭环打通。
- **traework 面板未暴露 hooks 选项**：应用包内确含 `global_hooks_enabled`、`import_claude_folders` 等运行时开关（静态分析证据），但设置界面没有 hooks 入口——用户实测不可配。hooks 能力在运行时存在，可能只是未出面板或需开关开启。⚠️ 待进一步验证。
- **workbuddy ≡ codebuddy**：共用 `~/.codebuddy/settings.json`，审计中 harness 字段会是 `codebuddy`（事实而非缺陷）。
- **traecode 会额外读 claude 的同份配置**：给 claude 配 hooks 会顺带覆盖 TraeCode。
- **dsh**：原生非 shell hook（Cordis 插件），本工具不产出插件代码，只在池用官方桥接包 `dsh-hooks-claude-code` / `dsh-hooks-codex`。Tier 也因此记为 `bridge`。

## 3A. 各宿主的标准安装/接入流程

> 下表为官方口径，来自各宿主官方文档（TRAE `docs.trae.cn`、CodeBuddy/WorkBuddy `codebuddy.ai/docs`）。执行后仍需到面板确认。

### 3A.1 TRAE（TRAE SOLO CN / Trae CN）

- 全局 hooks：`~/.trae-cn/hooks.json`；项目 hooks：`$PROJECT/.trae/hooks.json`。
- **唯一生效路径**：应用内「设置 > Hooks > 全局 > 勾选已启用 + 运行方式」→ 保存后在会话触发。
- 运行方式建议 **本地自动运行**（沙箱可能限制 mem 访问）。
- 日志：设置 > Hooks > 查看日志（输出面板 "Agent Hooks"）。
- 事件：支持 SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop / Notification；stdout 纯文本注入**仅适用于前两个**。

### 3A.2 WorkBuddy（内置 CodeBuddy 运行时）

- 用户级 hooks：`~/.codebuddy/settings.json`。
- **唯一生效路径**：`/hooks` 面板审查确认后应用；直接编辑 settings.json 的外部修改不生效。
- 注意全局开关：settings.json 里存在 `"disableAllHooks": true` 会禁用全部；frontmatter hooks 需 `allowUntrustedFrontmatterHooks: true`（普通 command hooks 不受此项限制）。
- 已开会话不一定即时生效，需新建会话。

### 3A.3 其他支持 MCP 的宿主

- 产出一份 `.mcp.json`（或宿主等价配置），把 `mem mcp` 注册为 stdio 工具服务器；
- 首次须在宿主里**信任/允许**该 MCP server；
- 之后 agent 可在对话中显式调用 mem 的 search/add/ls/touch。

## 4. `mem setup`（理想接口形态）

> 本规范将安装收敛为单一交互命令，未落地实现，作为后续改造目标。

```
mem setup --harness <trae|workbuddy|codebuddy|mcp>
```

行为：
1. 探测目标宿主的配置路径与版本；
2. 按官方 schema **产出**（而非越权写）hooks 段/MCP 段；
3. 本地**校验 JSON schema + 命令可执行**；
4. 打印「请到 <宿主路径> 执行 <动作>」引导清单；
5. 写入前备份旧配置；幂等合并，带 `# v2mem` 标记以便卸载回退。

验收：`mem setup` 后，用户仅凭引导清单即可在面板完成启用，无需手动编辑配置。

## 5. 卸载与回退

- `mem uninstall --harness <x>`：只摘 `# v2mem` 标记的条目，不动宿主用户自有 hooks/配置。
- 每次安装前自动备份，可 `--restore <备份>` 回退。

## 6. 与其他文档的分工

- `docs/DEVELOPER.md`：本地开发部署（软链、编译-验证循环）。
- `docs/design-harness-memory-replace.md`：读/写接入的架构决策与工作流拆分。
- 本文件：**面向用户/安装动作**的合规接入步骤与验收。