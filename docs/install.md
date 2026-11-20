# 整体安装协议（v2mem）

本文是**端到端安装协议**：装二进制 → 接 hooks → 注册 MCP → 安装后校验 → 回滚。
它不自动写宿主配置；所有写动作仍由 `install.sh`、`mem init` 和宿主面板完成。

- 各宿主的配置落点、面板动作、故障对照：见 [`setup.md`](./setup.md)。
- 装好之后的稳态判据（I/R 组）：见 [`ops-watch.md`](./ops-watch.md)。
- 本文的 A–D 判据只用于「来源 / 冲突 / 接入一致性」，是这四组的唯一真理源。

## 1. 适用与边界

适用：新机器首次安装、换机、升级宿主或 mem 之后的重新校验；同一协议在每台机器各跑一遍。

不适用：日常记忆读写（见 README「命令一览」）、稳态巡检（见 `ops-watch.md`）。

前置：

| 依赖 | 用途 | 缺失时 |
|:---|:---|:---|
| `bash` + 常用文本工具 | 安装与校验脚本 | 无法继续；Windows 用 Git Bash / MSYS |
| `jq` | 校验脚本解析宿主 JSON 配置 | 记 A0 WARN，结构化检查降级为 INFO，其余继续 |
| `git` + `go` | 仅 `--from-source` 回退需要 | 优先走预编译下载 |
| 宿主本体 | 接 hooks / MCP | 只装二进制，接入留到宿主装好后再跑 S3 |

校验脚本有两种运行方式，任选：

```bash
# 仓库内（开发者机器）
bash scripts/verify-install.sh --preflight

# 无仓库的机器：与 install.sh 相同的信任模型
curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/verify-install.sh \
  | bash -s -- --preflight
```

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
| 宿主要求面板确认 | S3 未确认 | 阻止（视为未完成） | TraeCode / CodeBuddy 打开 Hooks 总开关并选「本地自动运行」；WorkBuddy 新开会话即可 |

> B5 只是关键词启发式（`禁止/不要/禁用/不得` × `hook/MCP/记忆/mem`），命中后必须人工读原文，不能以脚本结论代替裁决。

## 3. 分步安装

每一步都有「完成判据」；判据不过，停在原地排查，不跳步。

### S0 盘点现状

```bash
type -a mem; ls -l "$(command -v mem)" 2>/dev/null; go env GOPATH
bash scripts/verify-install.sh --preflight
```

完成判据：知道本机 mem 的来源（没有 / 发布版 / 开发软链 / `go install`），且 §2 的「阻止」项为零。

> `mem harness` 的 `●` 只表示配置目录存在，不等于宿主真的还在用。换机或清理前再核对
> `/Applications` 与运行进程；WorkBuddy 的 `~/.codebuddy` 是内置 CodeBuddy 运行时目录，
> 不是独立的 CodeBuddy 安装，不能按「未使用宿主」删除。

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

```bash
mem init --harness <宿主> --dry-run   # 先看会写什么
mem init --harness <宿主>             # 合并写入，带 # v2mem 标记与 .v2mem.bak 备份
```

完成判据（两段都满足）：

1. 配置里出现 `# v2mem` 条目；`disableAllHooks` 不为 true。
2. 按 `setup.md` 完成宿主面板动作，**新开会话**后 `mem audit --tail 5` 出现对应宿主的
   `session-start` / `prompt-submit`。

失败回退：`mem uninstall --harness <宿主>` 只摘 `# v2mem` 条目，不动用户自有配置。

### S4 注册 MCP（默认包含）

MCP 与 hooks 相互独立；hooks 负责自动注入，MCP 负责会话内显式调用 8 个工具。

| 宿主 | 注册动作 | 还必须做什么 |
|:---|:---|:---|
| WorkBuddy | 在 `~/.workbuddy/mcp.json` 写 `mcpServers.v2mem = { type: "stdio", command: "<mem 绝对路径>", args: ["mcp"] }` | 自定义连接器 → 添加 MCP → 保存 → 点「信任」，开关才会打开（`mcp-approvals.json` 出现 `*::v2mem`） |
| Claude Code | `claude mcp add v2mem -- mem mcp`（或项目 `.mcp.json`） | 宿主首次信任该 server |
| Codex | `~/.codex/config.toml` 的 `[mcp_servers.v2mem]`，`command = "mem"`、`args = ["mcp"]` | 首次信任 |
| Cursor | `~/.cursor/mcp.json` 的 `mcpServers.v2mem` | 宿主 MCP 面板确认 |
| TraeCode / TraeWork | 设置 → 插件市场 → 管理 → MCP → `+ 添加` → 手动配置，粘贴 `{"mcpServers":{"v2mem":{"command":"<mem 绝对路径>","args":["mcp"]}}}` → 确认（Trae 写入 `~/Library/Application Support/Trae CN/User/mcp.json`） | 面板内启用；工具列表出现 8 个 v2mem 工具 |
| Qoder 系 | 宿主 MCP 面板新增 stdio server：命令 `mem`，参数 `mcp` | 面板内启用；脚本只能记 INFO |

完成判据：会话工具列表出现 `mcp__v2mem__search` 等工具；WorkBuddy 另有批准记录。
失败回退：删除对应配置条目或关闭面板开关；不影响 hooks 注入。

> GUI 宿主不继承 shell 的 PATH，`command` 建议直接写绝对路径（如 `~/.local/bin/mem`）；
> 上表的 `mem` 只是简写。多个宿主同时拉起 `mem mcp` 时，首次连接可能短暂报 `SQLITE_BUSY`，
> 在宿主面板点「重试」即可。

> WorkBuddy 实测（5.11.3）：缺 `type: "stdio"` 时文件里虽有 v2mem，UI 不加载；
> 补齐后需在「自定义连接器」里点「添加 MCP → 保存」，再点「信任」，开关才会变为开启。

### S5 安装后校验

```bash
bash scripts/verify-install.sh --harness <宿主>          # A–C 组
bash scripts/verify-install.sh --harness <宿主> --runtime --window 12h   # 追加 D 组
```

完成判据：无 `FAIL`。`WARN` 按条目处理：MCP 未批准去开面板，多份 mem 收敛来源，
事件数不足重跑 `mem init`。`INFO` 只表示当前不构成判据。

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
| **C1** | 所选宿主已有 v2mem 条目 | 有 `# v2mem` 或 `mem hook` | — | FAIL：没有；`--preflight` 时降 INFO |
| **C2** | hooks 事件完整度 | ≥2 个事件（或 instruction 块存在） | 仅 1 个事件 | FAIL：0 个；`--preflight` 时降 INFO |
| **C3** | MCP 声明（不代表宿主已加载） | 已知配置文件中找到 `v2mem` | 未找到（文件型宿主） | INFO：GUI 托管宿主（Trae/Qoder 系）无法只读确认 |
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
- MCP：删除对应配置条目，或在宿主面板关闭 v2mem。
- 二进制：按该机安装方式移除（发布版删 `~/.local/bin/mem`；软链删软链；`go install` 删 `$GOPATH/bin/mem`）。
- 数据库：`~/.v2mem` 保留。确要删除时先 `mem export` 备份，再删目录；不要只删 `mem.db` 留下 WAL/SHM。

## 6. 多机记录

每台机器安装完成后在本地记录一行（不要记凭据）：

| 主机 | OS / 架构 | 安装方式 | 版本或 commit | 宿主 | MCP | DB 路径 | 校验日期 | A–D 摘要 |
|:---|:---|:---|:---|:---|:---|:---|:---|:---|
| 示例：mini | macOS arm64 | 发布版 | v0.5.2 | workbuddy | 已批准 | `~/.v2mem/mem.db` | 2026-11-20 | 无 FAIL，1 WARN |

## 7. 文档分工

- 本文：端到端安装协议、冲突审查、A–D 判据、回滚、多机记录。
- [`setup.md`](./setup.md)：各宿主配置落点、面板动作、故障对照；不重复安装协议。
- [`ops-watch.md`](./ops-watch.md)：装好后的 I/R 稳态判据与巡检节奏；I1–I5 不在本文重算。
- [`../README.md`](../README.md)：面向使用者的安装入口、命令一览、MCP 配置片段。
- [`DEVELOPER.md`](./DEVELOPER.md)：构建、测试、覆盖率与合并门禁。
