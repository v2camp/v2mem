# v2mem

**个人 Agent 记忆系统** —— 一个本地 CLI（命令名 `mem`），存"跨会话该记住的东西"，
让 AI 工具在**需要时**查得到，而不是把它们塞进每轮注入的上下文里。

**轻量** —— 单文件二进制约 **17 MB**（各平台 16.7–17.9 MB）：零 CGO、零外部服务、零常驻进程，直接依赖只有 1 个
（`modernc.org/sqlite`，纯 Go 转译）
**快** —— 200 条记忆库检索平均 **~9 ms/次**（FTS5 + bm25：精确优先、空手才放宽）
**多工具** —— 原生接入 **12 个 AI 工具**（Claude Code / Codex / Cursor / TraeCode / WorkBuddy / CodeBuddy / Qoder …），
另提供 **MCP server**，任何支持 MCP 的工具一行配置即接入
**跨会话** —— 记忆持久在本地 SQLite，会话之间、工程之间共享，不依赖任何云服务
**跨平台** —— mac / linux / windows 预编译单文件，同一套记忆三端通用（Windows 路径尚未在 runner 实测，见安装节）

```bash
curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash
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

完整的分步安装（含冲突审查、MCP 注册与安装后校验）见 [`docs/install.md`](./docs/install.md)。

```
v2mem 有两条安装路径，二选一即可；模型配置是可选项，装完再配也不迟。
```

| 路径 | 适用 | 安装 | 更新 |
|:---|:---|:---|:---|
| **远程发布版**（推荐给使用者） | 稳定、随版本走 | `curl -fsSL …/install.sh \| bash` → `~/.local/bin/mem` | 重跑同一行脚本 |
| **本地开发软链**（推荐给开发者） | 跟着代码走，改完即生效 | `make build` → `bin/mem`，软链到 PATH | 重新 `make build`，软链自动跟随 |
| **模型配置**（可选） | judge / LLM 蒸馏 | `mem setup` | 重跑 `mem setup`（回退用 `--undo`） |

```bash
# 远程发布版（推荐）—— 下载预编译二进制；失败则浅克隆到临时目录再构建
curl -fsSL https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh | bash
# 参数要写在 bash -s -- 后面（写在 bash 后的 --version 是 bash 自己的选项）：
#   curl -fsSL …/install.sh | bash -s -- --version v0.1.0 --dir ~/bin
#   --from-source   跳过下载。默认克隆 main；--version 指定 tag 时克隆该 tag
# 国内模块超时：export GOPROXY=https://goproxy.cn,direct 后再跑
# （脚本不改写已有的 GOPROXY / GOSUMDB；机器上没有 go 会直接说明原因）

# 本地开发软链 —— 让 $GOBIN 里的 mem 指向仓库构建产物，值随代码变
make build                                                   # → bin/mem
ln -sf "$PWD/bin/mem" "$(go env GOPATH)/bin/mem"             # 覆盖式软链
```

预编译文件名：`mem-darwin-amd64`、`mem-darwin-arm64`、`mem-linux-amd64`、`mem-linux-arm64`、`mem-windows-amd64.exe`，
另有 `checksums.txt`（五个文件的 SHA256）。`latest` 下载 `…/releases/latest/download/<文件名>`；`--version vX.Y.Z` 下载 `…/releases/download/vX.Y.Z/<文件名>`。

安装脚本会下载 `checksums.txt` 校验目标二进制：哈希不符即拒绝落地并回退源码构建；**v0.5.1 及更早的 Release 没有该清单**，此时打印一次 WARN 并跳过校验（向后兼容）。
清单与二进制同源于同一个 Release，只防传输损坏与错配，**不等于发布者签名**。

这些文件由 [`.github/workflows/release.yml`](.github/workflows/release.yml) 在推送 `vX.Y.Z` tag，或于 Actions 里手动跑 `release` 时上传。main 上自动打 tag 仍先过 `eval-gate`；托管 runner 没有金标准对应的记忆库时这一步会失败，不会上传。推 tag 或手动跑 workflow 只再要求覆盖率门禁。某个版本的 Release 里还没有这些产物、或 SHA256 校验不符时，安装脚本回退到源码构建（需要本机已有 `git` 和 `go`）。

Windows 只有 amd64 预编译，且脚本要在 Git Bash / MSYS 里跑（cmd.exe 不行）。默认 `~/.local/bin` 通常不在 Windows Path 中，请加 `--dir`。这条路径没有在 Windows runner 上实测。

> ⚠️ **两条路径会互相覆盖**：`make install`（`go install`）会在 `$GOPATH/bin` 写入**真实文件**，覆盖已存在的软链。
> 想回发布版就重跑 curl 安装；想回开发软链就重跑上面的 `ln -sf`。发布版与软链处于不同安装目录，互不冲突。
> ⚠️ **worktree 里构建不生效**：软链固定指向主仓库的 `bin/mem`，在 `.worktree/<任务>/` 下 `make build`
> 只更新该 worktree 自己的产物。要验证分支行为，得在该 worktree 里重跑一次上面的 `ln -sf`（`$PWD` 即该 worktree），
> 切回主仓库后再跑一次。也因此**分支合入 main 并清理后要重建主分支**，否则验证到的仍是旧二进制（见 `AGENTS.md` 任务流程）。
> 发版步骤见 [`docs/DEVELOPER.md`](./docs/DEVELOPER.md)。

### 安装失败排查

一行 `curl | bash` 若卡在拉取脚本本身（`raw.githubusercontent.com` 不可达），脚本没执行就不会有任何回退提示。拆成分步执行，可以看清是哪一段断了：

```bash
curl -fsSL -o /tmp/v2mem-install.sh https://raw.githubusercontent.com/v2camp/v2mem/main/scripts/install.sh
bash /tmp/v2mem-install.sh --dry-run
bash /tmp/v2mem-install.sh
```

`--dry-run` 只打印将使用的 URL、校验清单 URL 与安装目录，不联网、不建目录。源码回退同样需要 `git` + `go` 且要能访问 GitHub；机器没有 Go 时，优先保证预编译下载通道可达，或从 Release 页面手动下载对应平台的二进制，改名为 `mem` 放进 PATH。

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
| `native` | 有原生 shell 钩子，事件触发即执行（**代码强制**） | Claude Code、TraeCode、TraeWork、CodeBuddy、WorkBuddy、Qoder / Qoder CN、QoderWork / QoderWork CN、Codex、Cursor |
| `bridge` | 通过官方桥接包复用他家钩子协议 | DeepSeek Harness（`dsh-hooks-claude-code`） |
| `instruction` | 无原生钩子，把指令写进会话级文件靠模型遵守（**概率性**） | 兜底通道，`mem init --scope instruction` 可强制 |

```bash
mem init                     # 扫描本机 → 显示 12 个入口（● = 配置目录存在）→ 让你选
mem init --harness <宿主>    # 直接指定（推荐，先按 S0.1 确认真装）
mem init --dry-run           # 只看会写什么
```

`●` 只代表配置目录存在，不代表宿主还装着；卸载后的残留目录也会显示 `●`。
先按 [`docs/install.md`](./docs/install.md) S0.1 核实真实安装，**只对真装宿主点名 `--harness`**，
不要用 `--all` 扫全量，否则会产生空接入。

配置写入遵守四条纪律：**合并不覆盖**、**幂等**、**只备份一次**（`.v2mem.bak`）、**绝对路径**。
写出配置 ≠ 已启用：TraeCode 要开 Hooks 总开关并选「本地自动运行」，Codex 要 trust，
CodeBuddy 要在面板审核，QoderWork 要重启；`mem init` 会打印各宿主的额外动作。
Cursor 用 `mem init --harness cursor` 注册六个原生事件（不靠只配 MCP）。
`postToolUse` / `stop` / `sessionEnd` 只把有界摘要写入同库 `observations`，**不**调用 `mem add`。这些痕迹可以用 `mem search --layer observation` 查到，默认的 `mem search` 和开场注入仍然只看已定记忆。写库失败时落到 `~/.v2mem/spool`，下次会话开始再补写；`SessionStart` 会机会性拉起一次短命 proposal 蒸馏把证据抽成候选（候选仍不进检索，自动提交矩阵负责落库），每个工程的首次 `SessionEnd` 另有一次性冷启动蒸馏。自动化的唯一开关是总闸 `MEM_NO_AUTOMATION=1`（S12 已退役 `MEM_DISTILL_SPAWN` / `MEM_NO_DISTILL` / `MEM_NO_COLDSTART` 散闸）。默认没有常驻进程。
全部卸载：`mem uninstall --all`（或 `--harness <名字>` 单个），只摘掉带 `# v2mem` 标记的条目，不动你自己的配置。

### MCP server

`mem mcp` 暴露 8 个工具：`search`（检索；`layer` 为 `memory` / `observation` / `both`，默认只查已定记忆）、`add`（写入，相同内容自动覆盖）、
`ls`（列出）、`touch`（记录命中）、`recall_context`（只读，把类码和命中拼成一段文本；`query` 必填，`layer` 与 search 相同），
以及复核视图三件（S12 §12.3 统一后取代候选裁决三件）：`review_ls`（只读一屏四态复核队列）、`review_withdraw`（把已入库的可疑记忆撤回，退出检索、行与证据保留）、`maintain_wake`（无钩子 Harness 的维护兜底，唤醒一次幂等的 `mem maintain`）。
`distill` 仍不进 MCP。撤回是人工唯一剩余的否定动作：出错撤回，而不是逐条裁决。
MCP **不是安装脚本的一部分**：`install.sh` 只放二进制，`mem init` 只写 hooks；MCP 要完成「声明 + 宿主信任/开关」两步才算接入。

```json
{ "mcpServers": { "v2mem": { "type": "stdio", "command": "mem", "args": ["mcp"] } } }
```

GUI 宿主不继承 shell 的 PATH，实际配置时建议把 `command` 写成绝对路径（如 `~/.local/bin/mem`）。

- **Claude Code**：`claude mcp add v2mem -- mem mcp`（或写入项目 `.mcp.json` 的 `mcpServers`），首次需信任
- **Codex**：`~/.codex/config.toml` 的 `[mcp_servers.v2mem]`，首次需信任
- **WorkBuddy**：`~/.workbuddy/mcp.json` 必须带 `"type": "stdio"`；再到「自定义连接器 → 添加 MCP → 保存 → 信任」
- **Trae / Qoder 系**：走宿主 MCP 面板，不靠手写文件；步骤见 [`docs/install.md`](./docs/install.md) S4

MCP 的检索与写入和 CLI 共享审计埋点，会一并计入 `mem eval activity` 的评测样本。
清单内宿主都走 hooks 通道（11 个 native + 1 个桥接，含 Cursor 的六个原生事件），不需要 skill 兜底；无 hook 的临时环境直接用 CLI 或 MCP，命令要点见 [`docs/install.md`](./docs/install.md) §8.4。

---

## 命令一览

**写入与检索**

| 命令 | 说明 |
|:---|:---|
| `mem add [--kind K] [--project P \| --global] [--tag k=v] [--salience F] [--ttl DUR] "<事实>"` | 写入一条记忆。**相同内容自动覆盖**（按归一化后的 hash），不会重复 |
| `mem search [--limit N] [--scope S] [--layer L] [--project P] [--kind K] [--tag k=v] "<查询>"` | 全文检索（FTS5 + bm25）。`--layer`：`memory`（已定记忆，默认）/ `observation`（近期痕迹）/ `both`（两层交错）。`--scope`：`current`（本工程+全局，按当前目录推断工程）/ `global` / `all`（默认） |
| `mem ls [--project P] [--kind K] [--tag k=v] [--scope S] [--limit N]` | **列出库里的记忆**（不需要查询词）。默认跨工程、按工程分组 |
| `mem touch <id\|前缀>` | 记录一次命中：刷新 `last_seen_at`、累加 `access_count`。衰减机制的输入 |

**生命周期**：`mem gc [--max-idle DUR] [--min-salience F] [--no-backup]`（回收：TTL 到期 + 久未命中**且**低重要性）
· `mem gc --obs [--max-age DUR]`（只删观测行）· `mem gc --evidence [--evidence-grace DUR]`（删**已无观测引用**的归档段与磁盘文件；可与 `--obs` 合跑，顺序固定为观测在前）
· `mem gc --audit [--audit-age DUR]`（只删审计账本：超期 `distill_runs` / `read_traces`，默认 720h；**每个会话留一条蒸馏游标**，活跃会话的轨迹不动）
· `mem consolidate [--threshold F] [--no-backup]`（相似知识归并，留 1 条、其余置 `superseded_by`）
· `mem forget <id\|前缀>`（删除一条）
`gc` / `consolidate` 是破坏性操作，执行前**自动备份库快照**到 `<库目录>/backup/`；空库或加 `--no-backup` 则跳过。
证据归档**按引用准出**：观测还在、段就必须留（否则记忆会显示「有证据」却打不开），所以 `--evidence` 单跑在观测未回收时释放 0 字节，属预期。
审计账本**按会话结束准出**：`read_traces` 是注入去重依据（按 session 无时间窗读），`distill_runs` 是蒸馏游标（每会话 `MAX(finished_at)`），两者都不能按年龄一刀切。

**跨设备**：`mem export [文件.jsonl]` / `mem import <文件.jsonl>`（按 `(content_hash, project)` 归并，**幂等**）· `mem sync <remote>`（git 拉→三方合并→推，`content_hash+updated_at` 最近者胜、冲突并存打标记，`--abort` 复原；写前自动备份）。
⚠️ **绝不要同步 `mem.db` 本体** —— 写入中途的同步会损坏 SQLite；`export/import` 与 `sync` 都只走导出的 JSONL。

**Level 1 文件运维**：`mem ingest <文件.md...>`（把既有 md 机械化搬进库，幂等；写侧治理含 `--source harness-summary` 锚定"任务收尾汇总"双路径写）
· `mem budget --file <文件> [--max-chars N]`（检查注入文件是否超预算，超限非零退出）
· `mem notes`（先输出类码表 P/D/X/T，再输出硬规则小字条：`kind=rule`/全局高优先，一行一条、幂等；正文由 SessionStart 注入承载，每轮检索侧再去重，`MEM_NO_NOTES=1` 让检索侧也回退到原始注入）
· `mem distill --proposal`（从证据 Bundle 抽候选，**不**写永久记忆；`--quarantined` / `--requeue` / `--gaps` 为运维出口）· `mem review ls [--state pending|all]`（一屏四态复核队列，按工程就近查看）· `mem review withdraw <memoryID>`（误入库撤回）· `mem accept` / `mem reject`（兼容保留，单条裁决；下一轮随 P4/灰度删除）
· `mem import-transcript --file <claude.jsonl>`（只写观测；候选由 proposal 蒸馏产出）
· `mem gc --candidates`（pending 14 天过期，已采纳行 90 天后删除，不动记忆）

**接入与钩子**：`mem harness`（列出 12 个工具入口）· `mem init`（写入钩子配置）· `mem uninstall`（反向移除，`--all` 一键）
· `mem hook`（钩子入口：读 stdin JSON、写 stdout；同一事件重复触发只注入一次，`--dedup-window` 默认 10s）

**度量与评测**：`mem audit [--stats] [--tail N] [--hits=false]`（审计日志与读/写侧计数）· `mem eval`（**无参一键跑 `activity`，24h 窗口**）
· `mem eval recall [--sandbox 目录 | --gold 文件 | --gold-dir 目录] [--auto [N]] [--k N] [--receipts 目录]`（检索命中是否准。`--sandbox` 用临时库和仓库内合成夹具，是发布门禁；`--gold` 打个人库，不参与发版。`--auto` 裸用默认抽样 20）
· `mem eval write --session <文件> [--gold 文件]`（记录是否准：漏记/多记/疑似碎片）
· `mem eval activity [--scope period\|task\|session] [--since D] [--session S] [--project P] [--top N]`
（**用量视图**：一段时间/会话/任务内的读/写活动 + 空命中查询 + 四态结论。任务收尾由此产出，写进 `.mem/report.md`）

**同义词词库**：`mem lexicon init --project <X> [--terms "词A,词B"]`（从该工程记忆提取同义词写入 `synonyms.txt`：以既有检索为
"行为 oracle"算命中集、按 Jaccard 相似度聚簇，泛词护栏 + `[0.40,0.5)` 复核区间防止跨概念误合；`--dry-run` 只预览不写）
· `--auto`（实验性：从记忆正文自动提取 bigram 作候选，须跨 ≥2 条记忆，精度低于 `--terms`，输出走复核）
同义词的作用域与迭代闭环见 [`docs/design-synonym-library.md`](./docs/design-synonym-library.md)。

---

## 数据与隐私

| 路径 | 内容 |
|:---|:---|
| `~/.v2mem/mem.db` | 记忆库本体（SQLite，WAL） |
| `~/.v2mem/audit.jsonl` | 审计日志：每次检索的真实 query、命中的记忆、耗时。超过 5MB 自动轮转并保留 3 份归档（`audit.jsonl.1`…`.3`） |

**审计日志会记录真实提问原文**。它只用于评测，删掉它不影响记忆库。
不便留痕时用 `--no-audit`（`mem search`）或 `--audit -`（`mem hook`）关闭。
观测摘要、候选正文、`mem add` 和审计 query 共用一套密钥打码；本地调试可加 `--no-scrub`。MCP `add` 始终打码。
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
2. **TraeWork 钩子路径来自应用包静态分析**（面板未暴露 hooks 选项，尚未观察到真实会话触发）。
   验证：用一次后看 `mem audit --tail 5` 是否出现 `prompt-submit` 记录。
   WorkBuddy 已实机验证：用户级钩子读 `~/.workbuddy/settings.json`，**不是** `~/.codebuddy/settings.json`
   （判据 `WORKBUDDY_CONFIG_DIR`；写错文件的症状是「配置正确但审计零记录」）
3. **检索退化是启发式**：只在精确表达式 0 命中时才放宽，不做更聪明的查询改写
4. **审计日志记录提问原文**，见上文「数据与隐私」

---

## 文档分工

| 文件 | 读者 | 内容 |
|:---|:---|:---|
| 本文件 | 使用者 | 是什么、卖点、怎么装、怎么用、边界在哪 |
| [`docs/VALUE.md`](./docs/VALUE.md) | 所有者 / 未来的 agent | **价值主张**：为什么这么做、红线在哪、下一步与判据（改设计前先读） |
| [`docs/DESIGN.md`](./docs/DESIGN.md) | 深度使用者 | 设计索引页：基础决策 + 各设计专题的「专题地图」 |
| [`docs/design-*.md`](./docs/DESIGN.md) | 深度使用者 | 设计专题（索引与地图见 DESIGN.md） |
| [`docs/DEVELOPER.md`](./docs/DEVELOPER.md) | 开发者 | 构建命令、目录结构、覆盖率纪律、测试纪律、MCP 实现说明 |
| [`docs/design-harness-memory-replace.md`](./docs/design-harness-memory-replace.md) | 使用者/开发者 | 用 v2mem 接替 harness 记忆(workbuddy/traeWork/原生)：读瘦身/写收录/git同步/版本CI门禁 |
| [`docs/design-synonym-library.md`](./docs/design-synonym-library.md) | 使用者/开发者 | 同义词词库：起步配置/查询期展开/有效迭代闭环/初始化 agent 提取管线/自动维护闭环（§8：LLM 提取 × 试用回测） |
| [`docs/design-provenance.md`](./docs/design-provenance.md) | 使用者/开发者 | 记忆溯源：`provenance` 字段走向、ALTER 迁移、ingest 写入、sync 传播与后续资产/词库联动设想 |
| [`docs/design-capture.md`](./docs/design-capture.md) | 所有者 / 开发者 | **捕获层架构**：多 harness 的 `CaptureEvent` 契约、A/B/C/D 捕获阶梯、适配器一致性测试、artifact 扫描边界与「事实源/查询源」划分 |
| [`docs/design-speaker-attribution.md`](./docs/design-speaker-attribution.md) | 所有者 / 开发者 | **说话人归属**：用户请求与 AI 产出怎么进记忆；参照应用对照、分源提炼、`attributed_to`、反回声规则与验收数据 |
| [`docs/design-write-governance.md`](./docs/design-write-governance.md) | 所有者 / 开发者 | **写侧治理**：三态（观测/候选/采纳）、写必由 LLM 决策、channel 与来源标记；§12 候选裁决自动化闭环（触发点、MCP 显式裁决、R8 分组、采纳率前提） |
| [`docs/design-admission.md`](./docs/design-admission.md) | 所有者 / 开发者 | **准入判定**：判定器实现（理由码、模型/CLI 接入、批量回滚）；事前门槛已由 `design-data-lifecycle` §6 取代，影子期降级为标定工具 |
| [`docs/design-data-lifecycle.md`](./docs/design-data-lifecycle.md) | 所有者 / 开发者 | **数据动线与统一维护**：准入→准出唯一证据流、`mem maintain`、Harness 唤醒拓扑、处理 SLA、模型自动裁决 + 人工后审核、引用保护与终态出口、命令面简化、P0–P4 实施顺序（改生命周期/GC/提交矩阵/命令面前先读） |
| [`docs/design-eval-flows.md`](./docs/design-eval-flows.md) | 开发者 | **动线级 e2e 评测（TDD）**：准出率 100% 与严密度口径、U1–U8 用户动线矩阵、逐跳内容流转 case、账本对账形态、随 P 阶段交付节奏 |
| [`docs/design-write-batch.md`](./docs/design-write-batch.md) | 所有者 / 开发者 | **写侧延迟治本**：批量归档方案 B 的实证依据、契约核查、账本/幂等设计与分阶段灰度（改 `writeArchive` 前先读） |
| [`docs/install.md`](./docs/install.md) | 使用者 / 所有者 | **安装与接入唯一文档**：六条原则、冲突审查、S0–S6、A–D 校验、宿主矩阵与细节、回滚、多机记录 |
| [`docs/ops-watch.md`](./docs/ops-watch.md) | 所有者 / 未来的 agent | **运行观测**：装好之后的动线与判据表、冷启动期例外、异常处置、12h 巡检调度（改判据先读） |
| [`docs/exp-project-seed.md`](./docs/exp-project-seed.md) | 所有者 / 开发者 | **实验**：工程记忆覆盖度对注入的影响（0%→57%）、写入粒度与配额竞争的实测、已部署种子与回退方式 |
| [`docs/design-ref-landscape.md`](./docs/design-ref-landscape.md) | 所有者 / 开发者 | **架构参考**：mem0 与 agentmemory 的全面对照，可借鉴/不借鉴/待评估三档，两边踩过的坑 |
| [`docs/design-anti-agentmemory.md`](./docs/design-anti-agentmemory.md) | 所有者 / 开发者 | 为何不照搬常驻 iii 与 54 个 MCP 工具；默认零常驻 |
| [`docs/design-db-stamp.md`](./docs/design-db-stamp.md) | 开发者 | **库版本台账**：`db_meta` 写入者版本戳、库比二进制新时拒绝打开、与 `user_version` 门控的边界 |
| [`docs/design-storage-layering.md`](./docs/design-storage-layering.md) | 所有者 / 开发者 | **存储分层方案**：运转逻辑留库、知识内容外置为 JSONL / Markdown；实测体积、表级映射、五项决策与相对比例验收（P0/P1 已落地） |
| [`contracts/storage-layering.md`](./contracts/storage-layering.md) | 开发者 | **存储分层 P0 契约**：ref 格式、两类写入顺序、`summary` / `summary_idx` 一律外置（其他字段 2 KB）、Markdown 只读边界与 `RefHealth` 巡检范围 |
| [`docs/superpowers/specs/2026-11-23-model-provider-setup-design.md`](./docs/superpowers/specs/2026-11-23-model-provider-setup-design.md) | 所有者 / 开发者 | **模型来源配置设计**：api canonical + CLI 便利路径、`mem setup` 零填写向导、install.md S6 落地（第一份 spec；更新能力另立） |

设计专题：`design-cross-device` 跨设备归集 · `design-tech-arch` CLI与技术选型 ·
`design-dedup-merge` 相似归并与模糊检索 · `design-harness` Harness 集成 ·
`design-capture` 多 harness 捕获层与事件契约 · `design-speaker-attribution` 说话人归属与分源提炼 ·
`design-workbuddy` WorkBuddy 结合 ·
`design-eval` 激活与评测 · `design-maintenance` 收尾运维 ·
`design-write-governance` 写侧治理与短/跨会话记忆分配 · `design-provenance` 记忆溯源 ·
`design-write-batch` 写侧延迟治本（批量归档） · `design-db-stamp` 库版本台账与降级守卫 · `design-storage-layering` 运转逻辑留库、知识内容外置。
「见 §N」这类跨章节引用按 DESIGN 索引页的专题地图定位文件。

---

面向使用者看本文件即可；要改代码、补测试、跑门禁，见 [`docs/DEVELOPER.md`](./docs/DEVELOPER.md)。
