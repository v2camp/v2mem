# 整体安装与接入（v2mem）

本文是 v2mem **唯一的安装/接入文档**：装二进制 → 接 hooks → 注册 MCP → 安装后校验 →（可选）配模型来源 → 回滚。
它不自动写宿主配置；写动作由 `install.sh`、`mem init`、宿主面板或手工 MCP 配置完成。

- 装好之后的稳态判据（I/R 组）：见 [`ops-watch.md`](./ops-watch.md)。
- 构建、测试、覆盖率、发版：见 [`DEVELOPER.md`](./DEVELOPER.md)。
- 日常命令与 MCP 片段：见 [`README.md`](../README.md)。

## 1. 原则与接入分层

> 1. **不绕过宿主面板**：接入 = 产出合规配置 + 引导用户在宿主面板确认启用。
>    是否需要面板因宿主而异（见 §8）：Trae 要开 Hooks 总开关并选「本地自动运行」；
>    WorkBuddy 的 hooks 不需要面板，但 MCP 要在连接器页开开关。
> 2. **一份配置、一种 schema**：按目标宿主的官方 schema 产出，先校验再提示。
> 3. **引导而非静默**：安装后必须给出「到哪个面板做什么」的可操作清单，不假设外部生效。
> 4. **只读护栏**：hooks / MCP 的读侧被触发时，绝不往 memories 写内容。
> 5. **可回退**：每次写入前备份；卸载只摘本工具的 `# v2mem` 条目，不碰用户自有配置。
> 6. **零填写但需告知**：模型来源可以零填写（探测本机已登录的 CLI），但发出去的是什么必须写清楚；
>    不配也能装完用起来，只是判定器与 LLM 蒸馏不可用。

v2mem 有两条互补通道，不能互相替代：

| 接入 | 载体 | 解决什么 | 完成条件 |
|:---|:---|:---|:---|
| **hooks** | `mem hook` | 会话开始/提问时自动注入相关记忆 | 配置写入 + 宿主启用动作 + 审计出现事件 |
| **MCP** | `mem mcp`（stdio） | 把 search/add/ls/touch/recall_context 与候选裁决暴露为工具 | 宿主侧声明 + 信任/开关 |

## 2. 安装前冲突审查

先跑 `scripts/verify-install.sh --preflight`，再按命中项逐行裁决。
三档判定：**可合并**（直接继续）、**人工裁决**（用户决定）、**阻止**（不解决不得安装）。
preflight 对「尚未接入」的缺项（C1/C2/C3）只报 `INFO`，不用于安装前判失败。

| 冲突源 | 检测信号 | 判定 | 动作 |
|:---|:---|:---|:---|
| 已有其他 mem / 多个安装来源 | A3 多份来源、A5 hooks 指向另一份 mem | 人工裁决 | PATH 只保留一种来源：发布版实文件、开发软链、`go install` 三选一；hooks 重跑 `mem init` 指向当前 mem |
| 宿主已有 hooks | B1 同事件有非 v2mem 命令 | 可合并 | 不同事件可直接共存；同事件注意注入重复，必要时关掉旧注入 |
| `disableAllHooks` | B2 为 true | 阻止 | 先去掉该键，否则 v2mem 与宿主自带钩子都不执行 |
| MCP 同名 server | B3 `v2mem` 的 command 不是 `mem mcp` | 人工裁决 | 改成 `mem` + `["mcp"]`，或改名避免覆盖 |
| 用户规则禁止 hooks / MCP / 自动注入 | B5 规则文件命中禁令 | 人工裁决 | 只装 CLI；或改规则后再装；不得静默绕过 |
| 用户规范指定唯一记忆系统 / 记忆文件 | B5 或人工阅读规则文件 | 人工裁决 | v2mem 只做 Level 2 检索库，或放弃安装；不要双写同一事实 |
| 组织统一安装协议（如 qifuskills install.md） | 规则文件或组织文档 | 人工裁决 | 本协议不写 `.agents/skills`、不改仓库，通常不冲突；若组织要求登记/评审，先走组织流程 |
| DB 路径不可写 / 在同步盘 | A4 | 阻止 | `~/.v2mem` 必须在本地可写目录；绝不放 iCloud / Dropbox / OneDrive / 坚果云等同步盘 |
| 宿主要求面板确认 | S3 未确认 | 阻止（视为未完成） | 按 §8 完成对应宿主的启用动作 |

> B5 只是关键词启发式（`禁止/不要/禁用/不得` × `hook/MCP/记忆/mem`），命中后必须人工读原文，不能以脚本结论代替裁决。

## 3. 分步安装

每一步都有「完成判据」；判据不过，停在原地排查，不跳步。

### S0 盘点现状

```bash
type -a mem; ls -l "$(command -v mem)" 2>/dev/null; go env GOPATH
bash scripts/verify-install.sh --preflight
```

完成判据：知道本机 mem 的来源（没有 / 发布版 / 开发软链 / `go install`），且 §2 的「阻止」项为零。
`verify-install.sh` 的 C0 会用 app/CLI 证据再查一遍；S0.1 是人工复核，两者一致才继续。

### S0.1 再确认宿主真的装了

`mem harness` 的 `●` 只代表**配置目录存在**：卸载后残留的 `~/.qoder`、`~/.cursor` 也会显示为已装。
只对下面能对上号的宿主执行 S3/S4；对残留目录执行 `mem init` 只会产生空接入。

| 宿主 | 真装证据（任一） |
|:---|:---|
| TraeCode / TraeWork | `/Applications/Trae CN.app`（或国际版 `Trae.app`）、`trae` CLI |
| WorkBuddy | `/Applications/WorkBuddy.app`、运行中的 WorkBuddy 进程 |
| CodeBuddy | `CodeBuddy.app` 或 `codebuddy` CLI；**WorkBuddy 的 `~/.codebuddy` 不算** |
| Codex | `codex` CLI |
| Cursor | `Cursor.app` 或 `cursor` CLI |
| Qoder / QoderWork / CN | 对应 app 或 CLI |
| Claude Code | `claude` CLI |
| dsh | `dsh` CLI + `dsh-hooks-claude-code` 桥接包 |

```bash
ls /Applications | grep -iE 'trae|workbuddy|codebuddy|cursor|qoder'
for c in trae codebuddy codex cursor qoder qoderwork claude dsh; do command -v "$c"; done
ps aux | grep -iE 'Trae|WorkBuddy|Cursor|Qoder|CodeBuddy' | grep -v grep
```

三条纪律：

1. 只用 `mem init --harness <名字>` 点名安装，**不要用 `--all` 扫全量**。
2. 配置目录存在、但上面查不到 → 视为残留：先清理，不安装、不写入。
3. WorkBuddy 的 `~/.codebuddy` 是内置 CodeBuddy 运行时目录，保留；不要当作独立 CodeBuddy。

### S1 选择安装方式

| 方式 | 适用 | 落地 | 更新方式 |
|:---|:---|:---|:---|
| 发布版（推荐给使用者） | 稳定、随版本走 | `~/.local/bin/mem` 真实文件 | 重跑 `install.sh` |
| 开发软链（推荐给开发者） | 跟代码走 | `$GOPATH/bin/mem` → 仓库 `bin/mem` | 重新 `make build` |
| `go install` | 临时验证 | `$GOPATH/bin/mem` 真实文件 | 重跑 `make install` |

同一台机器只保留一种；后两种会互相覆盖（见 README「安装」警告）。

### S2 装二进制

```bash
# 发布版
curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash

# 开发软链
make build
ln -sf "$PWD/bin/mem" "$(go env GOPATH)/bin/mem"
```

完成判据：`command -v mem` 有输出，且 `mem help` 返回 0。
失败回退：预编译下载失败会自动源码回退；GitHub 不可达时按 README「安装失败排查」分步执行。

### S3 接 hooks

`mem init` 只写 hooks 配置，**不代表宿主已启用**；先过 S0.1，只对真装宿主执行。

```bash
mem init --harness <宿主> --dry-run   # 先看会写什么
mem init --harness <宿主>             # 合并写入，带 # v2mem 标记与 .v2mem.bak 备份
```

完成判据（三段都满足）：

1. 配置里出现 `# v2mem` 条目；`disableAllHooks` 不为 true。
2. 完成该宿主的启用动作（见 §8.1–§8.4）。
3. **新开会话**后 `mem audit --tail 5` 出现对应宿主的 `session-start` / `prompt-submit`。

> TraeCode 与 TraeWork 共用 `~/.trae-cn/hooks.json`，装一次即可，不要对两个名字重复 `mem init`。
> Claude Code 与 TraeCode 也共用读取关系：给 claude 配 hooks 会顺带覆盖 TraeCode；两边都写会双份注入，二选一。

补充：Codex 还需在 `~/.codex/config.toml` 确保 `[features] hooks = true`（默认开启），首次使用在 `/hooks` trust；
Cursor 的 `hooks.json` 需要 `version: 1`、事件名 camelCase（`mem init` 已处理）。

失败回退：`mem uninstall --harness <宿主>` 只摘 `# v2mem` 条目，不动用户自有配置。

### S4 注册 MCP（默认包含）

**MCP 不是 `install.sh` 或 `mem init` 装的**：前者只放二进制，后者只写 hooks。
MCP 必须两步都完成才算接入：宿主侧声明（文件或面板）+ 宿主侧信任/开关。
暴露 8 个工具：`search` / `add` / `ls` / `touch` / `recall_context` / `candidates_ls` / `accept` / `reject`。
只写 `mcp.json` 而没做第二步 = 未接入。各宿主的声明位置与面板动作见 §8。

完成判据：会话工具列表出现 `mcp__v2mem__search` 等工具；WorkBuddy 另有批准记录。
失败回退：删除对应配置条目或关闭面板开关；不影响 hooks 注入。

> GUI 宿主不继承 shell 的 PATH，`command` 建议直接写绝对路径（如 `~/.local/bin/mem`）。
> 多个宿主同时拉起 `mem mcp` 时，首次连接可能短暂报 `SQLITE_BUSY`，在宿主面板点「重试」即可。

### S5 安装后校验

```bash
bash scripts/verify-install.sh --harness <宿主>          # A–C 组
bash scripts/verify-install.sh --harness <宿主> --runtime --window 12h   # 追加 D 组
```

完成判据：无 `FAIL`。`WARN` 按条目处理：MCP 未批准去开面板，多份 mem 收敛来源，
事件数不足重跑 `mem init`。`INFO` 只表示当前不构成判据。

### S6 可选：配置模型来源（判定器 / 蒸馏）

不配也能用 hooks 与 MCP；只是 `mem judge` 会报「判定器不可用」、`mem distill` 不能走 LLM。

```bash
mem setup              # 探测本机 CLI，dry-run 通过才写 ~/.v2mem/model.json（0600，带备份）
mem setup --status     # 查看当前来源（不联网、不调用 CLI）
mem setup --undo       # 回退到上一次写盘前
```

`mem setup` 按白名单（`claude` → `codex` → `cursor-agent` → `gemini` → `copilot` → `qoder` → `codebuddy` → `traecli`）逐个验证 judge 与 distill 两条真实路径，首个通过者胜出；
验证时各跑一次真实调用，可能消耗额度与时间。全都不可用时退出 0、不写盘，安装流程不因此中断。
想显式指定分支：`--mode api`（把 API 来源写入 `model.json`，密钥取 setup 时环境里的
`DEEPSEEK_API_KEY` / `OPENAI_API_KEY`，需先 export）、`--mode cli`（必须探测成功，否则报错）、
`--mode off`；`--dry-run` 只验证不写盘。

完成判据：`mem setup --status` 显示 `mode`（`cli` / `api` / `off`）；真正的 `mem judge` 不再报「判定器不可用」。

**已知不兼容**：`freebuff` 免费，但官方声明是 TUI-only、没有 headless / `-p` 输出模式，
不能当非交互后端，因此不在上面的白名单里。本机装了它时 `mem setup` 会点名说明，而不是静默忽略；
想要零成本可改用本地 `ollama`（`mem judge --provider ollama --model <tag>`）。

**数据边界**：judge 的候选正文与蒸馏的观测摘要会发往该 CLI 所连的模型端点；本机不另存副本。
不配置只会让 judge / LLM 蒸馏不可用，不影响 hooks、MCP、检索与人工写入。

## 4. 校验判据 A–D

脚本 `scripts/verify-install.sh` 是执行形态，本表是唯一真理源；改判据必须同步改两处。
级别：`PASS` / `WARN` / `FAIL` / `INFO`。退出码：有 `FAIL` = 1，否则 0；用法错 = 2。

| ID | 检查 | PASS | WARN | FAIL / INFO |
|:---|:---|:---|:---|:---|
| **A0** | `jq` 可用性 | 存在 | 缺失：结构化检查降级为 INFO | — |
| **A1** | `mem` 可执行、库可读 | `mem help` 返回 0；库存在时 `mem stats` 返回 0 | — | FAIL：不可执行或库版本高于二进制（提示升级 mem） |
| **A2** | PATH 上 mem 的落点 | 真实文件 | 软链指向 `.worktree/` | FAIL：软链悬空 |
| **A3** | 安装来源数量 | 仅一份 | 多份：列出路径与 PATH 顺序 | — |
| **A4** | DB 目录 | 可写且不在同步盘 | `mem.db` 是软链 | FAIL：不可写或在同步盘；INFO：目录尚未创建 |
| **A5** | hooks 里的 mem 路径 | 与当前 mem 同源 | 指向另一份或已删除的 mem | — |
| **B1** | 宿主已有非 v2mem hooks | — | — | INFO：列出条数，提示注入可能重复 |
| **B2** | `disableAllHooks` | 未开启或不存在 | 为 true：全部钩子不执行 | — |
| **B3** | 同名 MCP 的 command | `mem`（可带绝对路径） | 不是 `mem`：可能覆盖或错配 | — |
| **B4** | 同一事件重复挂载 v2mem | 每事件 1 条 | 同一事件 >1 条 | — |
| **B5** | 用户规则禁令 | 无命中 | — | INFO：命中文件:行，需人工裁决 |
| **C0** | 宿主真实安装证据 | 所选宿主能对上 app / CLI | — | WARN：仅配置目录存在（可能是残留）；INFO：未选宿主 |
| **C1** | 所选宿主已有 v2mem 条目 | 有 `# v2mem` 或 `mem hook` | — | FAIL：没有；`--preflight` 时降 INFO |
| **C2** | hooks 事件完整度 | ≥2 个事件（或 instruction 块存在） | 仅 1 个事件 | FAIL：0 个；`--preflight` 时降 INFO |
| **C3** | MCP 声明（不代表宿主已加载） | 已知配置文件中找到 `v2mem`；WorkBuddy 还要求 `type: stdio`，且 `disabled` 不是 `true` | 未找到（文件型宿主）、WorkBuddy 缺 `type`、或条目 `disabled: true`（连接器页开关关着） | INFO：GUI 托管（Qoder 系）；Trae 的 `User/mcp.json` 已纳入扫描 |
| **C4** | WorkBuddy MCP 批准 | `mcp-approvals.json` 有 `*::v2mem` | 有声明未批准 | INFO：未选 WorkBuddy |
| **C5** | 面板确认清单 | — | — | INFO：输出所选宿主的 `verify` 提示 |
| **D1** | 运行态判据 | 引用 `watch-v2mem.sh` 的 I/R 结论 | watch 报 WARN | FAIL：watch 报 FAIL；INFO：未启用 `--runtime` 或 watch 缺失 |

## 5. 回滚

按安装顺序反向拆，数据库默认保留：

```bash
mem uninstall --harness <宿主>        # 只摘 # v2mem 条目
mem uninstall --all                  # 全部宿主
```

- 配置回退：`mem uninstall` 没有 `--restore`；需要原样恢复时手工用同目录的
  `<配置>.v2mem.bak` 覆盖（仅在确认该备份是本工具产生时）。
- `mem uninstall` 不会删除宿主目录；确认宿主已卸载后，残留目录需另行清理。
- MCP：删除对应配置条目，或在宿主面板关闭 v2mem。
- 二进制：按该机安装方式移除（发布版删 `~/.local/bin/mem`；软链删软链；`go install` 删 `$GOPATH/bin/mem`）。
- 数据库：`~/.v2mem` 保留。确要删除时先 `mem export` 备份，再删目录；不要只删 `mem.db` 留下 WAL/SHM。

## 6. 多机记录

每台机器安装完成后在本地记录一行（不要记凭据）：

| 主机 | OS / 架构 | 安装方式 | 版本或 commit | 宿主 | MCP | DB 路径 | 校验日期 | A–D 摘要 |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| 示例：mini | macOS arm64 | 发布版 | v0.5.2 | workbuddy | 已批准 | `~/.v2mem/mem.db` | 2026-11-20 | 无 FAIL，1 WARN |

## 7. 附录 A：支持矩阵

v2mem 内置 12 个已知 harness：11 个 `native`（原生 hooks），`dsh` 为 `bridge`；另有 `instruction` 兜底通道。
源码：`internal/harness/harness.go`（顺序即展示顺序）。

> 这是**支持矩阵，不是本机安装清单**。`~/.qoder`、`~/.cursor` 等目录存在只说明曾配置过；
> 接入前按 §3 S0.1 用 app/CLI/进程核实；残留目录不要执行 `mem init`。

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
| cursor | cursor-ide | Cursor | native | `~/.cursor/hooks.json` | 六个原生事件 |

关键事实：

- **traecode 已验证**：面板启用（本地自动运行）+ 提问后，审计出现 `prompt-submit | traecode`。
- **traework 面板未暴露 hooks 选项**：应用包内有 `global_hooks_enabled` 等开关，但设置界面没有入口；新开会话后用审计确认。
- **workbuddy ≠ codebuddy 的配置**：运行时同是内置 CodeBuddy CLI，但用户级配置读 `~/.workbuddy/settings.json`；写 `~/.codebuddy/settings.json` 永远不触发。
- **traecode 会额外读 claude 的同份配置**：给 claude 配 hooks 会顺带覆盖 TraeCode；两边都写会双份注入。
- **dsh**：原生不是 shell hook，本工具不产出插件代码；需装官方桥接包
  `dsh plugin --profile add dsh-hooks-claude-code`（或 `dsh-hooks-codex`）。

## 8. 附录 B：宿主接入细节

### 8.1 TraeCode / TraeWork

- 全局 hooks：`~/.trae-cn/hooks.json`；项目 hooks：`$PROJECT/.trae/hooks.json`。
- 事件：SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop / Notification；
  stdout 纯文本注入仅适用于前两个。

一次性接入清单（按序执行，缺一不可）：

1. `mem init --harness traecode`：写入 `~/.trae-cn/hooks.json`，自动备份，条目带 `# v2mem`。
2. 打开「设置 > Hooks > 全局」，确认条目在列，并打开 **已配置的 Hooks** 总开关。
3. 运行方式选 **「本地自动运行」**；选 Sandbox 会出现「读得到、写不进」的半接入状态。
4. 完全退出 Trae 或新建会话，发一句普通提问并结束该轮。
5. 验收：`mem audit --tail 5` 应出现 `prompt-submit | traecode` 和 `stop | traecode`。

| 现象 | 根因 | 处理 |
|:---|:---|:---|
| 日志 `ExecEnv: Sandbox`，报 `readonly database` 或 `operation not permitted` | 钩子跑在沙箱，`~/.v2mem` 禁写 | 改为「本地自动运行」，或给沙箱放行 `~/.v2mem` |
| 配置存在但审计无 `traecode` | 总开关未开，或旧会话未重建 | 完成第 2、4 步 |
| 审计只有 `workbuddy` / `codebuddy` | 写错了宿主文件 | Trae 只读 `~/.trae-cn/hooks.json` |

MCP：设置 → 插件市场 → 管理 → **MCP** → 「MCP Servers 管理」右上角 **`+ 添加`** → **手动配置**，
粘贴 `{"mcpServers":{"v2mem":{"command":"<mem 绝对路径>","args":["mcp"]}}}` → 确认。
Trae 自己写入 `~/Library/Application Support/Trae CN/User/mcp.json`；启用后工具列表出现 8 个 v2mem 工具。
首次连接若报 `SQLITE_BUSY`，在面板点「重试」即可。

### 8.2 WorkBuddy（内置 CodeBuddy 运行时）

- 用户级 hooks：`~/.workbuddy/settings.json` 的 `hooks` 段（**不是** `~/.codebuddy/settings.json`）。
  判据是 `WORKBUDDY_CONFIG_DIR`：内置 CLI 读 `<该变量>/settings.json`，桌面端把它设成 `~/.workbuddy`。
- **hooks 不需要面板确认**：内置 CLI 读到文件即执行。MCP 则必须开连接器开关。

一次性接入清单（按序执行）：

1. `mem init --harness workbuddy`：写入 4 个事件（SessionStart / UserPromptSubmit / Stop / SessionEnd），
   条目带 `# v2mem`，首次备份为 `settings.json.v2mem.bak`。该文件约 43KB、含 IM 凭据的加密信封；
   幂等性已实测：重复执行不重复、12 个顶层键完整保留。
2. 检查没有 `"disableAllHooks": true`（它为真时全部钩子都不执行）。
3. **新开会话**：每轮由新进程读取设置，已开的会话不生效。
4. MCP：在 `~/.workbuddy/mcp.json` 加 `v2mem` 只是声明，不会激活。条目必须带 `"type": "stdio"`：
   `{ "type": "stdio", "command": "<mem 绝对路径>", "args": ["mcp"] }`。
   然后到「专家·技能·连接器 → 连接器页 → 右上角自定义连接器」，点「添加 MCP → 保存」，
   再点该行「信任」，开关才会打开。开启后 `mcp-approvals.json` 出现 `<sha256>::v2mem`。
5. 验收：`mem audit --tail 5` 应有 `session-start | workbuddy`、`prompt-submit | workbuddy`；
   `scripts/watch-v2mem.sh --window 24h` 的 I1/I2 PASS、R1 ≥1。

| 现象 | 根因 | 处理 |
|:---|:---|:---|
| 审计零 hook 事件，但配置语法正确 | 钩子写进了 `~/.codebuddy/settings.json` | 移到 `~/.workbuddy/settings.json`，再新开会话 |
| 配置刚改完，当前会话仍无注入 | 会话进程建于改配置之前 | 新开会话；不要反复改配置 |
| 连自带插件的钩子也不跑 | `"disableAllHooks": true` | 去掉该键 |
| MCP 工具列表没有 v2mem | 只写了 `mcp.json`，未开连接器，或条目缺 `type: "stdio"` | 补齐 type → 添加 MCP → 保存 → 信任 |
| hooks 有注入但 MCP 不可用（或反之） | 两条通道相互独立 | 分开排查 |

> 判定「是否真的在跑」只看审计：`session-start` / `prompt-submit` 带 `harness=workbuddy`。

### 8.3 其他支持 MCP 的宿主

- Codex：`~/.codex/config.toml` 的 `[mcp_servers.v2mem]`，`command = "<mem 绝对路径>"`、`args = ["mcp"]`；
  首次在 `/hooks` 或 MCP 面板 trust。
- Cursor：`~/.cursor/mcp.json` 的 `mcpServers.v2mem`；在宿主 MCP 面板确认。
- Claude Code：`claude mcp add v2mem -- mem mcp`（或项目 `.mcp.json`）；首次信任。
- 其他宿主：产出 `.mcp.json`（或宿主等价配置），把 `mem mcp` 注册为 stdio server，并在宿主里信任/允许。

### 8.4 无 hooks 时的手工兜底（不装 skill）

没有 hook 时也不需要安装 skill：直接用 CLI，或按 §8.3 注册 MCP。
仓库原有的三个 `skills/mem-*` 只服务旧路径，随原生 hooks 落地已退役。命令全表见 README「命令一览」，易错点：

| 场景 | 要点 |
|---|---|
| 检索 | MCP `search` / `recall_context` 的 `query` 必填；`layer=observation` 才看得到近期痕迹 |
| 写入 | MCP `add` 的 `content` 必填；一条只记一个原子事实；真正用上了再 `touch` |
| 蒸馏 | `distill` 不在 MCP；走 CLI `mem distill --provider none --session <id>`，候选必须显式 `accept` / `reject` |

P/D/X/T 类码见 [`VALUE.md`](./VALUE.md) §1.2；显式裁决见 [`design-write-governance.md`](./design-write-governance.md) §12。
