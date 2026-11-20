# 安装与接入规范（setup guide）

- 日期：2026-09-18（最近更新 2026-11-12：移除 skill 兜底，补 CLI/MCP 手工速查 §3A.4）
- 状态：草案待复核
- 关联：`docs/DEVELOPER.md`（本地开发部署）、`docs/design-harness-memory-replace.md`（读/写接入）、`docs/design-workbuddy.md`、`docs/design-harness.md`
- 端到端安装协议见 [`install.md`](./install.md)；本文只保留各宿主接入细节。

## 1. 背景与原则：为什么要有这份规范

本仓库工作流遵循「一任务一 worktree」；安装/接入环节却曾长期靠**直接往宿主数据目录写配置**（`~/.trae-cn/hooks.json`、`~/.codebuddy/settings.json`）。实机排查证明这是**侵入性接入**：虽然 JSON 语法正确、命令可执行，但宿主一律**不在面板内确认就不生效**——配置对、却不触发。

本规范将这些教训固化成**合规接入的唯一准则**：

> ### 合规接入五守则
> 1. **不绕过宿主面板**：接入=「产出合规配置 + 引导用户在宿主面板里确认启用」，而不是直接写宿主数据目录。
>    ⚠️ **是否需要面板确认因宿主而异，以 §3A 的实测为准**：TRAE 要开「已配置的 Hooks」总开关；
>    WorkBuddy 的 hooks 不需要面板（内置 CLI 读文件即执行），只有 MCP 需要在连接器页开开关。
>    不要用一个宿主的结论去推断另一个。
> 2. **一份配置、一种 schema**：对目标宿主按官方 JSON schema 产出，先校验再提示。
> 3. **引导而非静默**：安装后必须给出「请到 <宿主路径> 执行 <动作>」的可操作清单；不假设外部生效。
> 4. **只读护栏**：读侧（hooks/MCP 读取）绝不因被触发而往 memories 写内容（见 `TestCmdHookReadOnly`）。
> 5. **可回退**：每次写入前备份旧配置；卸载时只摘本工具的条目（`# v2mem` 标记），不碰用户自有配置。

## 2. 接入方式分层

v2mem 对宿主提供**两种合规接入**，定位互补，不可互相替代：

| 接入 | 载体 | 解决什么 | 官方要求 |
|---|---|---:|---|
| **读侧注入（hooks）** | `mem hook` | 会话开始/提问时**自动塞相关记忆进上下文**（"记得住"） | hooks 事件 + 宿主差异化的启用动作（见 §3A） |
| **检索能力（MCP）** | `mem mcp` (stdio) | 把 search/add/ls/touch/recall_context 与候选裁决 candidates_ls/accept/reject **作为工具暴露**给支持 MCP 的宿主 | `.mcp.json` + 信任确认 |

- hooks 负责**自动喂上下文**（读侧确定性入口）；MCP 负责**能力暴露**（供 agent 显式调用）。互斥？不——v2mem 两条都提供，方向一致。
- 安装动作的目标：**让两条路都"合规产出 + 引导面板确认"**，而非侵入写入。

## 3. 支持工具（harness）清单

v2mem 内置 12 个已知 harness：11 个按**应用内置运行时**判定为 `native`（原生 hooks），
`dsh` 为 `bridge`（复用官方桥接包）；另有 `instruction` 兜底通道，不占清单行。
源码：`internal/harness/harness.go`（顺序即展示顺序）。

> 这张表是**支持矩阵，不是本机安装清单**。`~/.qoder`、`~/.cursor` 等目录存在只说明曾配置过；
> 应用卸载后目录仍会留着。接入前按 [`install.md`](./install.md) S0.1 用 app/CLI/进程核实；
> 残留目录不要执行 `mem init`，否则只是空接入。

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
| workbuddy | work-buddy, wb | WorkBuddy | native | `~/.workbuddy/settings.json` | ✅ 已验证生效（内置 CLI 实测） |
| cursor | cursor-ide | Cursor | native | `~/.cursor/hooks.json` | 六个原生事件，见 README「接入 AI 工具」 |

关键事实：
- **traecode 已验证**：2026-11-02 面板启用（本地自动运行）+ 提问后，审计出现 `prompt-submit | traecode`，`hook_dedup` 同步落库——读侧注入与写侧闭环均已打通。
- **traework 面板未暴露 hooks 选项**：应用包内确含 `global_hooks_enabled`、`import_claude_folders` 等运行时开关（静态分析证据），但设置界面没有 hooks 入口——用户实测不可配。hooks 能力在运行时存在，可能只是未出面板或需开关开启。⚠️ 待进一步验证。
- **workbuddy ≠ codebuddy 的配置**：运行时同是内置 CodeBuddy CLI，但用户级配置读的是 `<WORKBUDDY_CONFIG_DIR>/settings.json`
  = `~/.workbuddy/settings.json`。写 `~/.codebuddy/settings.json` 永远不触发（2026-11-01 实测：挂机 1.5 天，审计 0 条）。
- **WorkBuddy 已完整接入（2026-11-02）**：hooks 四事件写 `~/.workbuddy/settings.json`、MCP 走连接器开关，
  两条通道同时生效；`watch-v2mem.sh` 的 I1–I4 全 PASS，24h 窗口 R1 命中 196 个事件。
  接入步骤与故障对照见 §3A.2。
- **traecode 会额外读 claude 的同份配置**：给 claude 配 hooks 会顺带覆盖 TraeCode。
- **dsh**：原生非 shell hook（Cordis 插件），本工具不产出插件代码，只在池用官方桥接包 `dsh-hooks-claude-code` / `dsh-hooks-codex`。Tier 也因此记为 `bridge`。

## 3A. 各宿主的标准安装/接入流程

> 下表为官方口径，来自各宿主官方文档（TRAE `docs.trae.cn`、CodeBuddy/WorkBuddy `codebuddy.ai/docs`）。执行后仍需到面板确认。

### 3A.1 TRAE（TRAE SOLO CN / Trae CN）

- 全局 hooks：`~/.trae-cn/hooks.json`；项目 hooks：`$PROJECT/.trae/hooks.json`。
- 事件：支持 SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop / Notification；stdout 纯文本注入**仅适用于前两个**。

**一次性接入清单（按序执行，缺一不可）**

1. `mem init --harness traecode`：写入 `~/.trae-cn/hooks.json`，自动备份旧文件，条目带 `# v2mem`。
2. 打开「设置 > Hooks > 全局」，确认事件条目在列，并打开 **已配置的 Hooks** 总开关。
3. **运行方式选「本地自动运行」**。选 Sandbox 会出现“读得到、写不进”的半接入状态。
4. 完全退出 Trae 或新建会话，发一句普通提问并结束该轮（已开会话不即时生效）。
5. 验收（只读）：
   ```bash
   mem audit --tail 5    # 应出现 prompt-submit | traecode 和 stop | traecode
   ```
   或看「设置 > Hooks > 查看日志」（输出面板 "Agent Hooks"）。

**故障对照（2026-11-02 实机）**

| 现象 | 根因 | 处理 |
|:---|:---|:---|
| 日志 `ExecEnv: Sandbox`，stderr 报 `attempt to write a readonly database (8)` 或 `spool: operation not permitted` | 钩子跑在沙箱，`~/.v2mem` 禁写；注入仍成功，但审计/观测为 0 | 第 3 步改为「本地自动运行」；或在沙箱配置放行 `~/.v2mem` |
| 配置存在但审计无 `traecode` | 总开关未开，或旧会话未重建 | 完成第 2、4 步 |
| 审计只有 `workbuddy`/`codebuddy` | 写错了宿主文件 | Trae 只读 `~/.trae-cn/hooks.json` |

- MCP 可选：`mem mcp` 与 hooks 共用同一二进制和库，不冲突，也不是 hooks 接入的前置条件。
  入口：设置 → 插件市场 → 管理 → **MCP** → 「MCP Servers 管理」右上角 **`+ 添加`** → **手动配置**，
  粘贴 `{"mcpServers":{"v2mem":{"command":"/Users/<你>/.local/bin/mem","args":["mcp"]}}}` → 确认。
  Trae 自己写入 `~/Library/Application Support/Trae CN/User/mcp.json`；启用后工具列表出现 8 个 v2mem 工具。
  首次连接若报 `SQLITE_BUSY`，在面板点「重试」即可（多个宿主同时拉起 `mem mcp` 时可能短暂争用）。

### 3A.2 WorkBuddy（内置 CodeBuddy 运行时）

- 用户级 hooks：`~/.workbuddy/settings.json` 的 `hooks` 段（**不是** `~/.codebuddy/settings.json`）。
  判据是 `WORKBUDDY_CONFIG_DIR`：内置 CLI 读 `<该变量>/settings.json`，桌面端把它设成 `~/.workbuddy`。
- 与 TRAE 的关键差异：**hooks 不需要面板确认**——内置 CLI 读到文件即执行。MCP 则相反，必须开面板开关（见第 4 步）。

**一次性接入清单（按序执行）**

1. `mem init --harness workbuddy`：写入 4 个事件（SessionStart / UserPromptSubmit / Stop / SessionEnd），
   条目带 `# v2mem`，首次备份为 `settings.json.v2mem.bak`。
   该文件约 43KB、含 IM 凭据的加密信封，**幂等性已实测**：重复执行 4/4 事件不重复、12 个顶层键完整保留。
2. 检查没有 `"disableAllHooks": true`（它为真时全部钩子都不执行，包括 WorkBuddy 自带插件）。
3. **新开会话**：每轮由新进程读取设置，已开的会话不生效。
4. MCP（可选，与 hooks 相互独立）：在 `~/.workbuddy/mcp.json` 加 `v2mem` 只是**声明，不会激活**。
   条目必须带 `"type": "stdio"`，否则 UI 不加载：
   `{ "type": "stdio", "command": "/Users/<你>/.local/bin/mem", "args": ["mcp"] }`。
   然后到「专家·技能·连接器 → 连接器页 → 右上角自定义连接器」，点「添加 MCP → 保存」，
   再点该行的「信任」，开关才会打开。开启后 `~/.workbuddy/mcp-approvals.json` 出现 `<sha256>::v2mem`，
   会话内即可看到 `mcp__v2mem__search / add / ls / touch / recall_context`，
   以及候选裁决 `candidates_ls / accept / reject`（显式调用，不自动裁决）。
5. 验收（只读）：
   ```bash
   mem audit --tail 5                     # 应有 session-start | workbuddy、prompt-submit | workbuddy
   scripts/watch-v2mem.sh --window 24h    # I1/I2 PASS、R1 ≥1
   ```

**故障对照（2026-11-01 / 11-02 实机）**

| 现象 | 根因 | 处理 |
|:---|:---|:---|
| 审计零 hook 事件，但配置语法正确、`mem hook` 手工可跑 | 钩子写进了 `~/.codebuddy/settings.json`（同名文件，运行时不同源） | 移到 `$WORKBUDDY_CONFIG_DIR/settings.json`，再新开会话 |
| 配置刚改完，当前会话仍无注入 | 会话进程建于改配置之前 | 新开会话；**不要**反复改配置 |
| 连 WorkBuddy 自带插件的钩子也不跑 | `"disableAllHooks": true` | 去掉该键 |
| MCP 工具列表里没有 v2mem | 只写了 `mcp.json`，未开连接器开关 | 第 4 步；此故障**不影响** hooks 注入 |
| hooks 有注入但 MCP 不可用（或反之） | 两条通道相互独立 | 分开排查，不要用一条推断另一条 |

> 判定"是否真的在跑"只看审计：`session-start` / `prompt-submit` 带 `harness=workbuddy`。
> 本机 2026-11-02 的 24h 巡检为 196 个事件（session-start 38 / prompt-submit 79），R1 PASS。

### 3A.3 其他支持 MCP 的宿主

- 产出一份 `.mcp.json`（或宿主等价配置），把 `mem mcp` 注册为 stdio 工具服务器；
- 首次须在宿主里**信任/允许**该 MCP server；
- 之后 agent 可在对话中显式调用 mem 的 search/add/ls/touch/recall_context，并用 candidates_ls/accept/reject 就近裁决本工程候选。

### 3A.4 无 hooks 时的手工兜底（不装 skill）

§3 清单里 11 个 `native` harness 都有原生 hooks（含 Cursor 的六个事件），`dsh` 走桥接，
**没有 hook 时也不需要安装 skill**：直接用 CLI，或按 §3A.3 注册 MCP。仓库原有的三个 `skills/mem-*` 只服务「Cursor 只能配 MCP」的旧路径，
随原生 hooks 落地已退役。命令全表见 `README.md` 「命令一览」，此处只记容易写错的几点：

| 场景 | 要点 |
|---|---|
| 检索 | MCP `search` / `recall_context` 的 `query` 必填；`layer=observation` 才看得到近期痕迹（hook 注入只带已定记忆）。已有 hook 注入时不必每轮重查已定记忆 |
| 写入 | MCP `add` 的 `content` 必填；一条只记一个原子事实，`kind` 取 `preference` / `decision` / `pitfall` / `task` / `fact`。真正用上了某条记忆再 `touch` |
| 蒸馏 | `distill` 不在 MCP 里，走 CLI `mem distill --provider none --session <id>`（规则抽取，无需 API key）；候选必须经 `mem accept` / MCP `accept` **显式**裁决才升格，采纳后 `source=distill-accept` 才可被检索 |

P/D/X/T 类码的触发条件见 `docs/VALUE.md` §1.2；显式裁决的设计约束见
`docs/design-write-governance.md` §12。候选与蒸馏结果不要直接写成永久记忆。

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
  它不会删除宿主目录；确认宿主已卸载后，残留目录需另行清理。
- 每次安装前自动备份为 `<配置>.v2mem.bak`；需要原样回退时手工用该备份覆盖（`mem uninstall` 没有 `--restore` 参数）。

## 6. 与其他文档的分工

- `docs/DEVELOPER.md`：本地开发部署（软链、编译-验证循环）。
- `docs/design-harness-memory-replace.md`：读/写接入的架构决策与工作流拆分。
- 本文件：**面向用户/安装动作**的合规接入步骤与验收。