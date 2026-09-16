# v2mem

个人 Agent 记忆系统。一个本地 CLI（命令名 `mem`），存"跨会话该记住的东西"，
让 AI 工具在**需要时**查得到，而不是把它们塞进每轮注入的上下文里。

```
mem add --kind pitfall "活库 mem.db 绝不能放进 iCloud 同步目录，会损坏 SQLite"
mem search "同步目录"

# 装进你在用的 AI 工具（扫描本机 → 让你选）
mem init
```

- **零 CGO、零外部服务、零常驻进程**：单文件二进制约 11 MB，可交叉编译，数据是自己的一个 SQLite 文件
- **直接依赖只有 1 个**（`modernc.org/sqlite`，纯 Go 转译）
- 代码 4633 行 / 测试 4330 行，**164 个用例**（`make test` 可复现），全部 TDD 编写

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

## 快速开始

```bash
make install          # → ~/go/bin/mem（需 PATH 含 $GOPATH/bin）
mem stats             # 库概览

mem add --kind decision "记忆库数据固定放 ~/.v2mem，代码与数据分离"
mem add --global --kind preference "自建 Go 工具默认 CGO_ENABLED=0 构建"
mem search "记忆库 数据"
mem search --json "构建方式" --limit 5

mem harness           # 看本机装了哪些 AI 工具、各是什么接入方式
mem init              # 扫描 → 选择要注入钩子的工具
```

`--project` 默认取当前 git 仓库名；`--global` 写入"工程标记为空"的记忆，对所有工程生效。

> ⚠️ **选项必须写在位置参数之前**：`mem search --json "查询"`，不是 `mem search "查询" --json`。
> Go 的 flag 在首个非选项参数处停止解析，写反了会把选项当成查询内容（v2mem 会直接报错拦下）。
> 同理 `mem ingest --project <工程> <文件>`。

---

## 命令一览

### 写入与检索

| 命令 | 说明 |
|:---|:---|
| `mem add [--kind K] [--project P \| --global] [--tag k=v] [--salience F] [--ttl DUR] "<事实>"` | 写入一条记忆。**相同内容自动覆盖**（按归一化后的 hash），不会重复 |
| `mem search [--limit N] [--scope S] [--project P] [--kind K] [--tag k=v] "<查询>"` | 全文检索（FTS5 + bm25）。`--scope`：`current`（本工程+全局，**按当前目录推断工程**）/ `global` / `all`（默认） |
| `mem touch <id\|前缀>` | 记录一次命中：刷新 `last_seen_at`、累加 `access_count`。这是衰减机制的输入 |

### 生命周期

| 命令 | 说明 |
|:---|:---|
| `mem gc [--max-idle DUR] [--min-salience F]` | 回收：TTL 到期 + 久未命中**且**低重要性。高 salience 的记忆能抵御遗忘 |
| `mem consolidate [--threshold F]` | 相似知识归并：把措辞不同的近重复聚簇，留 1 条、其余置 `superseded_by` |
| `mem forget <id\|前缀>` | 删除一条记忆 |

### 跨设备

| 命令 | 说明 |
|:---|:---|
| `mem export [文件.jsonl]` | 导出全部记忆（省略路径写标准输出）。按 `(content_hash, project)` 排序，可复现 |
| `mem import <文件.jsonl>` | 按 `(content_hash, project)` 归并。**幂等**：重复导入不改变状态 |

**绝不要同步 `mem.db` 本体** —— 写入中途的同步会损坏 SQLite 库。跨设备只同步导出的 JSONL。

### Level 1 文件运维

| 命令 | 说明 |
|:---|:---|
| `mem ingest <文件.md...>` | 把既有 md 的条目机械化搬进库。跳过标题/代码围栏/纯指引行，**合并缩进续行**；幂等，可重复跑全量 |
| `mem budget --file <文件> [--max-chars N]` | 检查注入文件是否超出预算。**超预算返回非零退出码**，供定时任务告警 |

### 接入 AI 工具

| 命令 | 说明 |
|:---|:---|
| `mem harness` | 列出支持的 11 个工具入口：接入方式、配置路径、本机是否已装 |
| `mem init [--harness 名字] [--all] [--project 目录] [--file 路径] [--dry-run]` | 把钩子写进该工具配置。不带参数则进入扫描选择流程 |
| `mem hook` | **钩子入口**：读 stdin JSON，把要注入的内容写 stdout。由 AI 工具调用，一般不手工执行 |

### 度量与评测

| 命令 | 说明 |
|:---|:---|
| `mem audit [--stats] [--tail N]` | 审计日志：钩子激活次数、空注入率、**可评测样本数**、**读侧/写侧计数**（判断"到底用起来没有"看这两个数，不看总次数） |
| `mem eval recall [--gold 文件] [--auto N] [--k N]` | 测「检索命中是否准」 |
| `mem eval write --session <文件> [--gold 文件]` | 测「记录是否准」（漏记/多记/疑似碎片） |

---

## 接入 AI 工具

协议高度同构：stdin 收 JSON、stdout 出内容、退出码定阻断。接入方式分三级，
**可靠性差一个量级，`mem harness` 会明确标注**：

| 等级 | 含义 | 覆盖的工具 |
|:---|:---|:---|
| `native` | 有原生 shell 钩子，事件触发即执行（**代码强制**） | Claude Code、TraeCode、TraeWork、CodeBuddy、WorkBuddy、Qoder / Qoder CN、QoderWork / QoderWork CN、Codex |
| `bridge` | 通过官方桥接包复用他家钩子协议 | DeepSeek Harness（`dsh-hooks-claude-code`） |
| `instruction` | 无原生钩子，把指令写进会话级文件靠模型遵守（**概率性**）。当前**无工具默认走这条**，它作为兜底通道保留 | `mem init --scope instruction` 可强制 |

> **WorkBuddy 与 TraeWork 原先被判为 instruction —— 那是错的。** 判断依据不该是「官方文档有没有写」，
> 而要看他**内置的 Agent 运行时是谁的**：WorkBuddy 内置 CodeBuddy（用户级钩子就是
> `~/.codebuddy/settings.json`，与 CodeBuddy Code 共用）；TraeWork(TRAE SOLO) 的项目级钩子是
> `.trae/hooks.json`、全局是数据目录下的 `hooks.json`。两者均是代码强制执行。

```bash
mem init                     # 扫描本机 → 显示 11 个入口（● = 已装）→ 让你选
mem init --harness claude    # 直接指定
mem init --dry-run           # 只看会写什么
mem harness                  # 不带参数只看清单
```

配置写入遵守四条纪律：**合并不覆盖**（解析不了就报错退出）、**幂等**（重复执行不累积）、
**只备份一次**（`.v2mem.bak` 保存首次触碰前的状态）、**绝对路径**（钩子环境往往没有你的 PATH）。

写入后 `mem init` 会打印**该工具怎么验证生效**。几个需要额外动作的：

- **Codex**：首次要在 `/hooks` 里 trust，否则钩子不执行；需 `[features] hooks = true`
- **QoderWork / QoderWork CN**：不支持热加载，**改完要重启**
- **CodeBuddy**：面板外的手工改动可能需在 `/hooks` 面板内确认
- **dsh**：需先装桥接包 `dsh plugin --profile add dsh-hooks-claude-code`
- **WorkBuddy**：与 CodeBuddy Code 共用 `~/.codebuddy/settings.json`，无需单独安装
- **TraeWork**：项目级写 `<项目>/.trae/hooks.json`（与 TraeCode 项目级同路径）；全局是数据目录下的 `hooks.json`
- **兜底**（任何工具）：`mem init --harness <名字> --scope instruction [--file <路径>]` 把指令写进文件

卸载：删掉配置文件里带 `# v2mem` 标记的 hook 条目即可。

---

## 数据与隐私

| 路径 | 内容 |
|:---|:---|
| `~/.v2mem/mem.db` | 记忆库本体（SQLite，WAL） |
| `~/.v2mem/audit.jsonl` | 审计日志：每次检索的真实 query、命中的记忆、耗时 |

**审计日志会记录真实提问原文**。它只用于评测，删掉它不影响记忆库。
不便留痕时用 `--no-audit`（`mem search`）或 `--audit -`（`mem hook`）关闭。

`mem.db` **不要放在 iCloud / Dropbox / Syncthing 等同步目录里**。

---

## 设计要点

每条都有实测依据，完整推理见 [`DESIGN.md`](./DESIGN.md)。

| 要点 | 一句话理由 |
|:---|:---|
| 中文检索自建 `content_idx` | FTS5 内置分词器都不适用中文：`unicode61` 把整串汉字当一个词，`trigram` 有 3 字符下限（"目录"这种二字词查不到）。故汉字展开为一元组+二元组（§11.3） |
| 检索"精确优先、空手才放宽" | 自然语言长问句没有空格 → 整句被当成一个词组做 AND → **必然 0 命中**（实测）。无命中时退化为全 term OR + bm25，精确有结果时不放宽（§15.5） |
| 跨设备身份键是 `(content_hash, project)`，不是 id | id 是随机值，两台设备独立写同一事实必得不同 id；按 id 归并必然漏合（§7.1） |
| 取代关系用身份键表达、导入端两遍解析 | 直接搬本地 id 会产生悬挂引用；完全不搬则其他设备不知道取代关系（§7.5） |
| 相似归并的准确性靠**四条护栏**而非阈值 | 实测 `CGO_ENABLED=0` 与 `=1` 相似度高达 0.906 —— 只卡阈值会把两条**互斥规则**合成一条。护栏：短文本/长度比/数字/否定（§11.10） |
| 钩子**绝不阻断宿主** | 记忆系统出问题不该让用户会话中断。所有异常静默放过，有 8 种异常输入的用例矩阵（§12.4） |
| 每轮注入有字符预算，且提示预留预算 | 用法提示的作用正是告诉模型"还有多级记忆可查"，被截断就失去意义（§12.4） |
| 先建度量，再谈替换 | 没有评测就换掉既有机制是凭感觉。口径红线见 §15.4；`--auto` 是**下限测试**，数字不得单独引用（§15） |

---

## 开发

```bash
make test        # go test ./...（CGO_ENABLED=0）
make build       # → bin/mem
make smoke       # 造一个临时库跑 add/search/stats
make build-linux # 交叉编译（Go 内建，无需交叉工具链）
make build-win
```

```
cmd/mem/            CLI 入口
  main.go           add/search/touch/forget/gc/export/import/stats + help
  hook.go           钩子入口（读 stdin、注入、审计埋点）
  init.go           配置生成：扫描选择、合并写入、指令块
  level1.go         ingest（机械化搬运）与 budget（体量守卫）
  measure.go        audit / eval recall / eval write
internal/
  store/            SQLite + FTS5、schema、生命周期、跨设备归并、相似归并
  similarity/       字符 n-gram + MinHash + 四条护栏（纯算法，无 IO）
  harness/          11 个工具入口的接入注册表（纯数据）
  audit/            审计日志读写与汇总
  eval/             评测指标（纯函数，口径可独立测试）
```

**测试纪律**（不是建议，是踩过坑后固化的）：

- **测试必须隔离 `HOME`**：`mem init` 在未给 `--project` 时写"用户级配置"路径。
  曾因未隔离，测试二进制路径被写进了真实的 `~/.claude/settings.json`。
  现在 `TestMain` 把 `HOME` 指向临时目录，并有护栏用例固化这一点
- **变异测试要能编译**：用 `if false` 屏蔽分支会让变量变成未使用 → 编译失败 →
  测试根本没跑（失败行还会被 `grep` 过滤掉），得到的是假结论。变异要保留变量使用
- **负向断言要枚举全库**，不能只看检索返回的条目 —— 标题行本就不会被任何查询命中，
  用它做负向断言等于没检查

---

## 已知边界

1. **向量检索（M5b）暂缓。** 包体不是障碍（换 wazero 驱动实测 +3.76 MB），但
   `sqlite-vec` 的 Go 绑定当前**不可用**：它锁在落后 18 个小版本的驱动上，唯一能编译的
   组合运行时报 wazero 特性错。且向量还需要 embedding 来源。建议先做零成本的替代项
   （复用已实现的 MinHash 签名 + RRF 融合）。见 §13
2. **WorkBuddy 与 TraeWork 的钩子路径来自应用包静态分析**（尚未观察到真实会话触发）。
   验证方法：用一次之后看 `mem audit --tail 5` 是否出现 `prompt-submit` 记录 —— 有即生效。
   未生效时的兜底：`mem init --scope instruction` 把指令写进 `AGENTS.md`（概率性，且占每轮 token）
3. **检索退化是启发式**：只在精确表达式 0 命中时才放宽，不做更聪明的查询改写
4. **指令级接入的默认落点需要你判断**：写进 `AGENTS.md` 会占每轮 token（实测 +344/轮）；
   若项目另有带预算的记忆文件，用 `--file` 指定落点
5. **审计日志记录提问原文**，见上文「数据与隐私」

---

## 文档分工

| 文件 | 内容 |
|:---|:---|
| 本文件 | 是什么、怎么用、边界在哪 |
| [`DESIGN.md`](./DESIGN.md) | 设计推理、实测数据、被否决的方案、踩过的坑。§12 各工具钩子事实、§13 M5b 预算、§14 与 WorkBuddy 结合、§15 度量与评测 |
