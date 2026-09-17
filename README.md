# v2mem

**个人 Agent 记忆系统** —— 一个本地 CLI（命令名 `mem`），存"跨会话该记住的东西"，
让 AI 工具在**需要时**查得到，而不是把它们塞进每轮注入的上下文里。

**轻量** —— 单文件二进制约 **11 MB**：零 CGO、零外部服务、零常驻进程，直接依赖只有 1 个
（`modernc.org/sqlite`，纯 Go 转译）
**快** —— 200 条记忆库检索平均 **~9 ms/次**（FTS5 + bm25：精确优先、空手才放宽）
**多工具** —— 原生接入 **11 个 AI 工具**（Claude Code / Codex / TraeCode / WorkBuddy / CodeBuddy / Qoder …），
另提供 **MCP server**，任何支持 MCP 的工具一行配置即接入
**跨会话** —— 记忆持久在本地 SQLite，会话之间、工程之间共享，不依赖任何云服务
**跨平台** —— mac / linux / windows 预编译单文件，同一套记忆三端通用

```bash
curl -fsSL https://raw.githubusercontent.com/wanghui/v2mem/main/scripts/install.sh | bash
mem add --kind pitfall "活库 mem.db 绝不能放进 iCloud 同步目录，会损坏 SQLite"
mem search "同步目录"
```

---

## 它解决什么问题

AI 工具的"长期记忆"通常是一个每轮都注入的 Markdown 文件（`AGENTS.md`、`MEMORY.md`）。
它有两个硬约束：**有容量上限**，且**灌满即被尾部截断**。于是会陷入循环：

```
细节不断写进该文件 → 逼近上限 → 人工"压缩"（蒸馏/下沉/删减）→ 又慢慢涨回来 → 再压缩
```

根因是把它当成了知识库。本项目的做法是**分成两级**：

| 层 | 载体 | 内容 | 是否进上下文 |
|:---|:---|:---|:---|
| **Level 1** | 会话级文件（`AGENTS.md` / `MEMORY.md`） | 跨工程硬规则、当前任务态、记忆钩子；目标 ≤2KB | ✅ 每轮 |
| **Level 2** | `~/.v2mem/mem.db` | 全部细节：决策、踩坑、数据、路径 | ❌ 按需检索 |

关键推论：**文件不再接收细节的那一刻，截断与压缩需求就消失了** —— 这是确定性的，
不依赖模型是否听话。而"模型会不会主动去查"是概率性的。

---

## 安装

```
v2mem 有两条安装路径，二选一即可：
```

| 路径 | 适用 | 安装 | 更新 |
|:---|:---|:---|:---|
| **远程发布版**（推荐给使用者） | 稳定、随版本走 | `curl -fsSL …/install.sh \| bash` → `~/.local/bin/mem` | 重跑同一行脚本 |
| **本地开发软链**（推荐给开发者） | 跟着代码走，改完即生效 | `make build` → `bin/mem`，软链到 PATH | 重新 `make build`，软链自动跟随 |

```bash
# 远程发布版（推荐）—— 从 GitHub Releases 下载预编译二进制，失败自动回退源码构建
curl -fsSL https://raw.githubusercontent.com/wanghui/v2mem/main/scripts/install.sh | bash
#   --dir ~/bin     指定安装目录（默认 ~/.local/bin）
#   --version v0.1.0 指定版本 tag（默认 latest；未传入时拉最新发布）
#   --from-source   跳过下载，强制源码构建

# 本地开发软链 —— 让 $GOBIN 里的 mem 指向仓库构建产物，值随代码变
make build                                                   # → bin/mem
ln -sf "$PWD/bin/mem" "$(go env GOPATH)/bin/mem"             # 覆盖式软链
```

预编译产物：`mem-darwin-amd64|arm64`、`mem-linux-amd64|arm64`、`mem-windows-amd64.exe`。

> ⚠️ **两条路径会互相覆盖**：`make install`（`go install`）会在 `$GOPATH/bin` 写入**真实文件**，覆盖已存在的软链。
> 想回发布版就重跑 curl 安装；想回开发软链就重跑上面的 `ln -sf`。发布版与软链处于不同安装目录，互不冲突。
> **发布版依赖已打 tag 并上传产物**（当前仓库尚未执行）；打 tag / 传产物见 `docs/DEVELOPER.md`。

---

## 快速开始

```bash
mem stats             # 库概览
mem add --kind decision "记忆库数据固定放 ~/.v2mem，代码与数据分离"
mem add --global --kind preference "自建 Go 工具默认 CGO_ENABLED=0 构建"
mem search "记忆库 数据"
mem search --json "构建方式" --limit 5
mem init              # 扫描本机 AI 工具 → 选择要注入钩子的工具（见下）
```

`--project` 默认取当前 git 仓库名；`--global` 写入"工程标记为空"的记忆，对所有工程生效。

> ⚠️ **选项必须写在位置参数之前**：`mem search --json "查询"`，不是 `mem search "查询" --json`。
> Go 的 flag 在首个非选项参数处停止解析，写反了会把选项当成查询内容（v2mem 会直接报错拦下）。

---

## 三种接入方式

| 方式 | 适合 | 入口 |
|:---|:---|:---|
| **CLI** | 手工记/查，脚本与定时任务 | `mem add` / `mem search` / `mem ls` … |
| **AI 工具钩子** | 你在用的 Agent 自动注入/记录 | `mem init`（下面「接入 AI 工具」） |
| **MCP server** | 任何支持 MCP 的工具按需调用 | `mem mcp`（下面「MCP server」） |

### 接入 AI 工具（钩子）

协议高度同构：stdin 收 JSON、stdout 出内容、退出码定阻断。接入方式分三级，
**可靠性差一个量级，`mem harness` 会明确标注**：

| 等级 | 含义 | 覆盖的工具 |
|:---|:---|:---|
| `native` | 有原生 shell 钩子，事件触发即执行（**代码强制**） | Claude Code、TraeCode、TraeWork、CodeBuddy、WorkBuddy、Qoder / Qoder CN、QoderWork / QoderWork CN、Codex |
| `bridge` | 通过官方桥接包复用他家钩子协议 | DeepSeek Harness（`dsh-hooks-claude-code`） |
| `instruction` | 无原生钩子，把指令写进会话级文件靠模型遵守（**概率性**） | 兜底通道，`mem init --scope instruction` 可强制 |

```bash
mem init                     # 扫描本机 → 显示 11 个入口（● = 已装）→ 让你选
mem init --harness claude    # 直接指定
mem init --dry-run           # 只看会写什么
```

配置写入遵守四条纪律：**合并不覆盖**、**幂等**、**只备份一次**（`.v2mem.bak`）、**绝对路径**。
个别工具需额外动作（Codex 首次要 trust 钩子、QoderWork 改完要重启等），`mem init` 会逐一打印。
全部卸载：`mem uninstall --all`（或 `--harness <名字>` 单个），只摘掉带 `# v2mem` 标记的条目，不动你自己的配置。

### MCP server

`mem mcp` 暴露 4 个工具：`search`（检索）、`add`（写入，相同内容自动覆盖）、
`ls`（列出）、`touch`（记录命中）。在支持 MCP 的工具里加一行进程配置即可：

```json
{ "mcpServers": { "v2mem": { "command": "mem", "args": ["mcp"] } } }
```

- **Claude Code**：`claude mcp add v2mem -- mem mcp`（或写入项目 `.mcp.json` 的 `mcpServers`）
- **Codex**：`~/.codex/config.toml` 的 `[mcp_servers.v2mem] command = "mem" args = ["mcp"]`
- 其余工具看各自的 MCP 配置格式，指向同一个 `mem mcp` 即可

MCP 的检索与写入和 CLI 共享审计埋点，会一并计入 `mem eval activity` 的评测样本。

---

## 命令一览

**写入与检索**

| 命令 | 说明 |
|:---|:---|
| `mem add [--kind K] [--project P \| --global] [--tag k=v] [--salience F] [--ttl DUR] "<事实>"` | 写入一条记忆。**相同内容自动覆盖**（按归一化后的 hash），不会重复 |
| `mem search [--limit N] [--scope S] [--project P] [--kind K] [--tag k=v] "<查询>"` | 全文检索（FTS5 + bm25）。`--scope`：`current`（本工程+全局，按当前目录推断工程）/ `global` / `all`（默认） |
| `mem ls [--project P] [--kind K] [--tag k=v] [--scope S] [--limit N]` | **列出库里的记忆**（不需要查询词）。默认跨工程、按工程分组 |
| `mem touch <id\|前缀>` | 记录一次命中：刷新 `last_seen_at`、累加 `access_count`。衰减机制的输入 |

**生命周期**：`mem gc [--max-idle DUR] [--min-salience F] [--no-backup]`（回收：TTL 到期 + 久未命中**且**低重要性）
· `mem consolidate [--threshold F] [--no-backup]`（相似知识归并，留 1 条、其余置 `superseded_by`）
· `mem forget <id\|前缀>`（删除一条）
`gc` / `consolidate` 是破坏性操作，执行前**自动备份库快照**到 `<库目录>/backup/`；空库或加 `--no-backup` 则跳过。

**跨设备**：`mem export [文件.jsonl]` / `mem import <文件.jsonl>`（按 `(content_hash, project)` 归并，**幂等**）· `mem sync <remote>`（git 拉→三方合并→推，`content_hash+updated_at` 最近者胜、冲突并存打标记，`--abort` 复原；写前自动备份）。
⚠️ **绝不要同步 `mem.db` 本体** —— 写入中途的同步会损坏 SQLite；`export/import` 与 `sync` 都只走导出的 JSONL。

**Level 1 文件运维**：`mem ingest <文件.md...>`（把既有 md 机械化搬进库，幂等；写侧治理含 `--source harness-summary` 锚定"任务收尾汇总"双路径写）
· `mem budget --file <文件> [--max-chars N]`（检查注入文件是否超预算，超限非零退出）
· `mem notes`（读侧硬规则小字条：抽 `kind=rule`/全局高优先记忆，一行一条、幂等；hook 注入去重，`MEM_NO_NOTES=1` 回退到仅原始注入）

**接入与钩子**：`mem harness`（列出 11 个工具入口）· `mem init`（写入钩子配置）· `mem uninstall`（反向移除，`--all` 一键）
· `mem hook`（钩子入口：读 stdin JSON、写 stdout；同一事件重复触发只注入一次，`--dedup-window` 默认 10s）

**度量与评测**：`mem audit [--stats] [--tail N] [--hits=false]`（审计日志与读/写侧计数）· `mem eval`（**无参一键跑 `activity`，24h 窗口**）
· `mem eval recall [--gold 文件 | --gold-dir 目录] [--auto [N]] [--k N]`（检索命中是否准；`--gold-dir` 合并目录下全部金标准，`--auto` 裸用默认抽样 20）
· `mem eval write --session <文件> [--gold 文件]`（记录是否准：漏记/多记/疑似碎片）
· `mem eval activity [--scope period\|task\|session] [--since D] [--session S] [--project P] [--top N]`
（**用量视图**：一段时间/会话/任务内的读/写活动 + 空命中查询 + 四态结论。任务收尾由此产出，写进 `.mem/report.md`）

---

## 数据与隐私

| 路径 | 内容 |
|:---|:---|
| `~/.v2mem/mem.db` | 记忆库本体（SQLite，WAL） |
| `~/.v2mem/audit.jsonl` | 审计日志：每次检索的真实 query、命中的记忆、耗时。超过 5MB 自动轮转并保留 3 份归档（`audit.jsonl.1`…`.3`） |

**审计日志会记录真实提问原文**。它只用于评测，删掉它不影响记忆库。
不便留痕时用 `--no-audit`（`mem search`）或 `--audit -`（`mem hook`）关闭。
`mem.db` **不要放在 iCloud / Dropbox / Syncthing 等同步目录里**。

---

## 设计要点

每条都有实测依据，完整推理见 [`docs/DESIGN.md`](./docs/DESIGN.md)。

| 要点 | 一句话理由 |
|:---|:---|
| 中文检索自建 `content_idx` | FTS5 内置分词器都不适用中文：`unicode61` 把整串汉字当一个词，`trigram` 有 3 字符下限（"目录"这种二字词查不到）。故汉字展开为一元组+二元组（§11.3） |
| 检索"精确优先、空手才放宽" | 自然语言长问句没有空格 → 整句被当成一个词组做 AND → **必然 0 命中**（实测）。无命中时退化为全 term OR + bm25（§15.5） |
| 跨设备身份键是 `(content_hash, project)`，不是 id | id 是随机值，两台设备独立写同一事实必得不同 id；按 id 归并必然漏合（§7.1） |
| 相似归并的准确性靠**四条护栏**而非阈值 | 实测 `CGO_ENABLED=0` 与 `=1` 相似度高达 0.906 —— 只卡阈值会把两条**互斥规则**合成一条（§11.10） |
| 钩子**绝不阻断宿主** | 记忆系统出问题不该让用户会话中断。所有异常静默放过，有 8 种异常输入的用例矩阵（§12.4） |
| 先建度量，再谈替换 | 没有评测就换掉既有机制是凭感觉。`--auto` 是**下限测试**，数字不得单独引用（§15） |

---

## 已知边界

1. **向量检索（M5b）暂缓**：`sqlite-vec` 的 Go 绑定当前不可用（锁在落后 18 个小版本的驱动上，
   唯一能编译的组合运行时报 wazero 特性错）。建议先做零成本替代项：复用已实现的 MinHash 签名 + RRF 融合（§13）
2. **WorkBuddy / TraeWork 钩子路径来自应用包静态分析**（尚未观察到真实会话触发）。
   验证：用一次后看 `mem audit --tail 5` 是否出现 `prompt-submit` 记录
3. **检索退化是启发式**：只在精确表达式 0 命中时才放宽，不做更聪明的查询改写
4. **审计日志记录提问原文**，见上文「数据与隐私」

---

## 文档分工

| 文件 | 读者 | 内容 |
|:---|:---|:---|
| 本文件 | 使用者 | 是什么、卖点、怎么装、怎么用、边界在哪 |
| [`docs/DESIGN.md`](./docs/DESIGN.md) | 深度使用者 | 设计索引页：基础决策 + 各设计专题的「专题地图」 |
| [`docs/design-*.md`](./docs/DESIGN.md) | 深度使用者 | 8 份设计专题（索引与地图见 DESIGN.md） |
| [`docs/DEVELOPER.md`](./docs/DEVELOPER.md) | 开发者 | 构建命令、目录结构、覆盖率纪律、测试纪律、MCP 实现说明 |
| [`docs/design-harness-memory-replace.md`](./docs/design-harness-memory-replace.md) | 使用者/开发者 | 用 v2mem 接替 harness 记忆(workbuddy/traeWork/原生)：读瘦身/写收录/git同步/版本CI门禁 |

设计专题：`design-cross-device` 跨设备归集 · `design-tech-arch` CLI与技术选型 ·
`design-dedup-merge` 相似归并与模糊检索 · `design-harness` Harness 集成 ·
`design-workbuddy` WorkBuddy 结合 · `design-eval` 激活与评测 · `design-maintenance` 收尾运维 ·
`design-write-governance` 写侧治理与短/跨会话记忆分配。
「见 §N」这类跨章节引用按 DESIGN 索引页的专题地图定位文件。

---

面向使用者看本文件即可；要改代码、补测试、跑门禁，见 [`docs/DEVELOPER.md`](./docs/DEVELOPER.md)。
