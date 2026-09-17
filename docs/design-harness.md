# v2mem R&D · Harness 十一工具集成（M6）

> 本文是 `docs/DESIGN.md` 的专题切片，供按需阅读；各主题全文目录与关联集中在 [DESIGN 索引页](./DESIGN.md)。

## 12. Harness 集成（M6）

目标：把 Level 2 接进用户实际在用的工具，且**用能确定性执行的方式**——不靠「提醒模型记得查」。

### 12.1 事实核实表（2026-09-17 逐家核对官方文档）

不做推测性填写：配置写进不生效的路径是**静默失败**，比不写更糟。

| 工具 | 接入方式 | 事件名 | 项目目录来源 | 用户级配置 | 官方文档 |
|:---|:---|:---|:---|:---|:---|
| Claude Code | native | 同名 6+ 事件 | `CLAUDE_PROJECT_DIR` | `~/.claude/settings.json` | docs.claude.com/…/hooks |
| TraeCode（Trae IDE / SOLO） | native | SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / Stop / Notification | `TRAE_PROJECT_DIR`、`CLAUDE_PROJECT_DIR` | `~/.trae-cn/hooks.json` | docs.trae.cn/ide_hook-configuration-reference |
| CodeBuddy Code | native | 27+ 事件 | `CODEBUDDY_PROJECT_DIR` | `~/.codebuddy/settings.json` | codebuddy.cn/docs/cli/hooks |
| Qoder（IDE / JB / CLI） | native | 12 事件 | `QODER_PROJECT_DIR` | `~/.qoder/settings.json` | docs.qoder.com/en/cli/hooks |
| Qoder CN | native | 同上 | `QODERCN_PROJECT_DIR` | `~/.qoder-cn/settings.json` | help.aliyun.com/zh/lingma/hook |
| QoderWork | native | SessionStart / SessionEnd / UserPromptSubmit / PreToolUse / PostToolUse … | **无（只走 stdin `cwd`）** | `~/.qoderwork/settings.json`（**仅用户级**） | docs.qoder.com/zh/qoderwork/hooks |
| QoderWork CN | native | 同上 | **无** | `~/.qoderworkcn/settings.json` | alibabacloud.com/help/zh/lingma/hook |
| Codex CLI | native | SessionStart / UserPromptSubmit / PreToolUse / PostToolUse / PermissionRequest / PreCompact / PostCompact / SubagentStart / SubagentStop / Stop | **无（只走 stdin `cwd`）** | `~/.codex/hooks.json`（或 config.toml 的 `[hooks]`） | developers.openai.com/codex/hooks |
| DeepSeek Harness (dsh) | **bridge** | Cordis 扩展点：`agent/session-start`、`agent/pre-step`、`tools/pre-execute`、`tools/post-execute`、`session/event` | 插件内 `ctx` | 无 hooks.json；桥接包读 `~/.claude/settings.json` | dsh 官方文档 |
| TraeWork（TRAE SOLO） | **native**（更正，原判 instruction） | 同名 6 事件 | `TRAE_PROJECT_DIR` / `CLAUDE_PROJECT_DIR` | `<项目>/.trae/hooks.json`；全局为数据目录下 `hooks.json` | 应用包内证据，见 §12.13 |
| WorkBuddy | **native**（更正，原判 instruction） | 同 CodeBuddy | `CODEBUDDY_PROJECT_DIR` | `~/.codebuddy/settings.json`（与 CodeBuddy 共用） | 应用包内证据，见 §12.13 |

用户点名的 8 个入口 → 注册表名的映射（有测试固化）：
`trae work→traework`、`trae code→traecode`、`work buddy→workbuddy`、`code buddy→codebuddy`、
`qoder→qoder`、`qoder work→qoderwork`、`deepseek harness→dsh`、`codex→codex`。

### 12.2 三个核心设计结论

**① 一条命令通吃。** 六家原生 shell hook 的注入通道在 `SessionStart` / `UserPromptSubmit`
两个事件上是重合的：**stdout 纯文本即被当作附加上下文**（Claude Code、TraeCode、CodeBuddy、
Qoder、QoderWork、Codex 的文档均如此记载）。因此 `mem hook` 输出纯文本，
不按 harness 分支渲染格式 —— 少一层分支就少一类静默失败。

`--harness` 只决定两件事：**从哪个环境变量取项目目录**、**配置写到哪里**。

**② 写一份 Claude 格式，顺带覆盖三家。** TraeCode 官方明确会额外读取
`~/.claude/settings.json` 并**合并执行**；dsh 有官方桥接包
`dsh-hooks-claude-code` 把同一协议翻译到自己的扩展点。故给 `claude` 装一次，
TraeCode 与 dsh 一并受益。

**③ 三级确定性必须对用户可见。** `native`（事件触发即执行，代码强制）→
`bridge`（复用他家协议）→ `instruction`（写指令块靠模型遵守）。
`mem harness` 与 `mem init` 都显式打印等级，并标注「指令级没有代码强制，可靠性低一档」。

### 12.3 注入通道差异（决定输出格式的取舍）

| 工具 | 纯文本 stdout | `hookSpecificOutput.additionalContext` | 备注 |
|:---|:---|:---|:---|
| Claude Code / TraeCode / CodeBuddy | ✅（仅 SessionStart & UserPromptSubmit） | ✅ | 我们只用前一类事件 |
| Qoder / QoderWork | ✅（仅这两个事件） | ✅ | 🔴 JSON 带 `hookSpecificOutput` 时必须同时给 `hookEventName`，否则**整份被拒** |
| Codex | ✅（仅这两个事件） | ✅ | 无环境变量，项目目录取 stdin `cwd` |

退出码语义各家一致：`0` 成功（stdout 按上述规则解析）、`2` 阻断、其他为非阻断错误。
**本实现永远返回 0**：记忆系统不可用不该让用户的会话中断。

### 12.4 `mem hook` 的四条纪律

1. **绝不阻断宿主。** 所有分支返回 nil；解析失败、stdin 是垃圾、库打不开都静默放过。
   这条有专门的用例矩阵（空输入 / 非 JSON / 截断 JSON / 未知事件 / `cwd` 不存在 / 字段类型不符）。
2. **stdout 只放注入内容，诊断走 stderr。** CodeBuddy 文档明确「stderr 仅作为 fallback，
   调试日志可安全写入 stderr，不会污染给 Agent 的反馈」；其余各家在退出码 0 时也只展示 stderr。
3. **每轮注入有字符预算**（默认 1200 字符），且**用法提示预留预算**——它的作用正是告诉模型
   「还有多级记忆可查」，被截断就失去了钩子的意义。
4. **无命中即静默。** `UserPromptSubmit` 检索不到相关内容时不输出任何东西，避免每轮注入噪声。

`SessionStart` 的注入顺序：跨工程硬规则 → 本工程记忆 → 用法提示。
工程推断优先级：指定 harness 的环境变量 → stdin `cwd` → `workspace_roots` → 进程工作目录。
**指定了 harness 就不再兜到别家**——否则在 Trae 里启动的终端会把 `CLAUDE_PROJECT_DIR`
带进 QoderWork 的钩子，工程判断就错了。

### 12.5 `mem init` 的配置生成纪律

| 纪律 | 做法 |
|:---|:---|
| 合并而非覆盖 | 只读写 `hooks` 段；已有的其它 hook 组与无关配置键原样保留；**解析不了就报错退出**，绝不拿默认值盖用户文件 |
| 幂等 | 写入前先摘掉自己上次写的条目（按命令里的 `# v2mem` 标记识别），重复执行不累积 |
| 备份 | `<文件>.v2mem.bak`，**只写一次**，保证它永远是「首次触碰前」的干净状态 |
| 绝对路径 | 命令用当前二进制的绝对路径而非裸 `mem`：harness 拉起钩子时环境往往很干净，没有用户 shell 的 PATH，裸命令会静默失效 |
| 可验证 | 写入后打印该工具的**生效验证方法**（如 Claude 用 `/hooks`、Codex 需 `/hooks` 里 trust）、额外前置条件与已知注意事项 |

**为什么用 `# v2mem` 注释做标记**：最初靠「可执行文件名 == `mem`」识别，测试二进制叫 `mem.test`
就认不出，重复 init 累积了 3 条（被幂等用例抓到）。任何改名都会破坏它。
写成 shell 注释是安全的——各家文档的 command 都经 shell 执行（bash/zsh/sh/powershell 皆以 `#` 为注释），
且不污染实际参数。

### 12.6 本机安装状态与端到端验证

`mem harness` 检测到本机已装并已写入：

| 工具 | 文件 | 结果 |
|:---|:---|:---|
| Claude Code + dsh（桥接） | `~/.claude/settings.json` | 已写入 |
| TraeCode（Trae CN / SOLO CN） | `~/.trae-cn/hooks.json` | 已写入 |
| CodeBuddy Code | `~/.codebuddy/settings.json` | 已写入 |
| Codex CLI | `~/.codex/hooks.json` | 已写入 |
| QoderWork CN | `~/.qoderworkcn/settings.json` | 已写入 |

**端到端验证方式**：从 `~/.claude/settings.json` 里**取出真实命令**并执行（而不是跑一份等价命令），
证明写进配置的那条命令确实可用：

- `SessionStart`（模拟 `cwd` = 真实工程）→ 注入 494 字符，含 3 条硬规则 + 1 条本工程记忆 + 用法提示
- `UserPromptSubmit`（提问含「同步目录」）→ 只注入命中的那 1 条 pitfall

**延迟**（钩子在会话关键路径上，不能有可观开销，`/usr/bin/time -p` 实测）：
`session-start` 0.01s、`prompt-submit` 0.01s、空事件 0.01s、库不存在 0.01s。

### 12.7 测试

`go test ./... -v` 实测：harness 11 + similarity 16 + store 53 + eval 8 + audit 6 + cmd 65 = **159**（M6 完成时点为 119，M5a 时点为 73；口径与新增项见各自小节）。

| 范围 | 用例 |
|:---|:---|
| harness 注册表 | 名字/别名解析、未知名报错带候选、**用户点名的 8 个入口全覆盖**、条目自洽（native 有配置路径 / instruction 有指令文件）、名字与别名全局唯一、事件名归一化容忍大小写与下划线、多候选配置路径选择、环境变量优先级、不注入环境变量的工具被正确标注 |
| `mem hook` | SessionStart 注入硬规则与本工程记忆、教会检索命令、prompt-submit 只注入相关且排除他工程、无命中静默、**异常输入矩阵（8 种）不报错**、库不存在可用、`--limit` 生效、**字符预算生效**、stdin cwd 回退、环境变量优先、显式 `--event` |
| `mem init` / `harness` | 列表含用户工具与等级标注、写出两个事件、绝对路径、**合并不覆盖**、**幂等**、`--dry-run` 不落盘、备份不被二次 init 覆盖、指令级标记块就地替换、`--file` 覆盖、未知 harness 与非法 scope 报错 |
| 扫描与选择 | 不带 `--harness` 进入扫描流程、编号与名字混用、取消（空/`q`/`Q`/空白）不写任何文件、未知选项报错并指名、`--all` 报告跳过项、`--all` 默认只处理检测到的、逗号分隔多选、含未知项报错 |

**变异测试**（逐个把实现改坏，确认有断言报警）：

| 变异 | 捕获用例 |
|:---|:---|
| prompt-submit 去掉工程过滤 | `TestCmdHookPromptSubmitExcludesOtherProjects` ✅ |
| 取消字符预算 | `TestCmdHookRespectsCharBudget` ✅ |
| 不预留用法提示的预算 | `TestCmdHookRespectsCharBudget` ✅ |
| 去掉幂等去重（`withoutOurs`） | `TestCmdInitIsIdempotent`（`got 3`）✅ |
| 备份每次覆盖 | `TestCmdInitBacksUpExistingFile` ✅ |
| 不合并、直接覆盖用户配置 | `TestCmdInitMergesWithoutClobberingExistingConfig` ✅ |

**三处「假通过」的排查记录**（都是断言或数据的问题，不是实现的）：

1. **工程名不对齐** → 一批 prompt-submit 用例返回空，但不是「无命中」而是被工程过滤掉了。
   修法：造带 `.git` 的临时工程，且记忆的 `project` 与钩子推断一致；断言一律要求**有内容**。
2. **宿主环境变量污染** → 工作环境里已设 `CLAUDE_PROJECT_DIR`，钩子优先采纳它，
   测试实际在验证宿主环境。修法：让测试数据构造函数**显式清空**各家环境变量。
   （这一点是靠 `mem hook` 打到 stderr 的诊断行 `project=… dir=…` 定位的 —— 诊断信息的价值。）
3. **预算未成为约束** → 先写 40 条记忆、断言 `--max-chars 800` 生效，但 `--limit 3`
   先把量兜住了，把预算逻辑整段删掉测试也不失败。修法：同时放开 `--limit`，
   并加前置断言「放开采纳量后输出确实远超预算」。
   另：最初 40 条写的是同一个字符串，被「相同知识覆盖」收敛成 1 条 —— 数据必须逐条互异。

### 12.8 不确定项（已知的未知，不做推测）

1. **TraeWork 是否有 shell hook 未经验证。** 官方文档未发布；其 Code 模式构建于 TraeCode SOLO
   之上，若与 TraeCode 共用运行时，则 `~/.trae-cn/hooks.json` 可能同样生效。
   本工具因此按指令级接入，**不据此写入**。
2. **WorkBuddy 无公开 hook 配置。** 其已发布扩展机制是项目记忆文件、技能、自动化任务，
   故走指令级接入。
3. **TraeCode 国际版的全局路径未从文档确认**（文档记 CN 版为 `~/.trae-cn/hooks.json`）。
   注册表同时列 `~/.trae/hooks.json` 作为候选，优先选父目录已存在的那个。
4. **dsh 的桥接包未安装。** 本次只写好 Claude 侧配置；
   需执行 `dsh plugin --profile add dsh-hooks-claude-code` 才生效。
5. **Codex 的 hook 需要信任。** 非托管 hook 首次执行前要在 `/hooks` 里审核，
   信任按 hook 哈希持久化 —— 改动命令后需重新审核。已写入 `~/.codex/hooks.json`，
   但需用户首次确认。
6. **`/hooks` 面板式审核的工具**（CodeBuddy）在面板外的手工改动可能需在面板内确认后才生效。

### 12.9 指令级接入的落点：选 AGENTS.md，不选记忆文件

WorkBuddy 与 TraeWork 走指令级接入。落点有两个候选，结论是 **AGENTS.md**：

| 候选 | 每轮注入 | 判决 |
|:---|:---|:---|
| `.workbuddy/memory/MEMORY.md` | 是 | ❌ 这是「记忆」文件，自带严格注入预算（本机实测约 7800 字符上限），塞说明会挤占真正的记忆 |
| `AGENTS.md` | 是（仓库根每轮注入） | ✅ 语义上就是「在这个仓库怎么做事」，钩子说明天然属于它 |

**实证（在本仓库上跑其自检门禁 `scripts/eval-agents-md.py`）**：

| 指标 | 写入前 | 写入后 |
|:---|:---|:---|
| 规则保留核对 | 84/84 | 84/84 |
| 表格竖线一致性 | OK | OK |
| 单条规则 ≤50 | OK | OK |
| 字符 / 预估 tokens | 6905 / 4538 | 7309 / 4782 |
| 结果 | PASS | **PASS** |

代价是 **+404 字符 / +244 tokens 每轮**——这是该决策的真实成本，属于项目约定，由用户判定是否值得。

**一个必须先检查的坑**：WorkBuddy / CodeBuddy 侧**同层存在 `CODEBUDDY.md` 时 `AGENTS.md` 会被忽略**。
本机仓库根实测无 `CODEBUDDY.md`，故 AGENTS.md 未被遮蔽；注册表里也把这条写进了 Notes。
若某项目两个文件都在，应改用 `--file CODEBUDDY.md`。

### 12.10 扫描 + 交互选择（默认流程）

`mem init` 不带 `--harness` 时进入扫描选择，而不是无差别往所有工具里写：

```
扫描本机：检测到 6 个已安装的工具（共 11 个已知入口）

  [ 1] ● claude         native      /Users/…/.claude/settings.json
  [ 2] ● traecode       native      /Users/…/.trae-cn/hooks.json
  …
  [ 9]   workbuddy      instruction AGENTS.md

选择要注入钩子的工具（编号或名字，逗号分隔；a=全部已检测到；q=取消）：
```

设计取舍：

- **列出全部入口而非只列检测到的**：索引因此稳定（不受本机环境影响），也允许用户选一个
  我们没探到路径的工具（它可能装在别处）。`●` 标记检测到的。
- **编号与名字都接受**，逗号/空格/顿号分隔；`a` 取全部已检测到；空输入或 `q` 取消。
- **未知项报错并指名**，不静默跳过（静默跳过会让用户以为装上了）。
- **`--all` 只处理检测到的**；被排除的**逐条报告**（区分「未检测到」与「当前范围无落点」）——
  否则用户以为「全都装了」。
- **多选时容忍个别工具没有落点**（如 QoderWork 只有用户级、dsh 无独立配置文件），
  报告「跳过 N 个」而不是整批失败。
- `--harness a,b,c` 仍可一次装多个；`--harness` 与 `--all` 互斥。
- 项目级写入（`--project`）不再提示「未检测到配置目录」——项目级本就该在项目里新建文件，
  那时提示纯属误导。

### 12.13 🔴 重大更正：WorkBuddy 与 TraeWork 都是 native（原判 instruction 是错的）

**错在哪**：最初的判断依据是「官方文档有没有发布钩子配置」——两者都没有，于是归入指令级。
这个判据太浅：它只看了**应用自己的设置文件**，没看**它内置的 Agent 运行时是谁的**。

**怎么查出来的**（方法可复用，不必依赖公开文档）：

```bash
# 1) 先列全应用（注意 glob 大小写：*[Tt]rae* 匹配不到全大写的 TRAE）
ls /Applications | grep -i trae            # → TRAE SOLO CN.app（即 TraeWork）、Trae CN.app

# 2) 在主程序里搜钩子关键词
grep -a -c 'UserPromptSubmit' "/Applications/TRAE SOLO CN.app/Contents/Resources/app/modules/ai-agent/libharness.dylib"

# 3) 在 JS 资源里找资产表/路径声明
grep -o -E '.{80}hooks\.json.{80}' ".../workbench.desktop.main.solo-lite-slim.js"
```

**TraeWork（`TRAE SOLO CN.app`，bundle id `cn.trae.solo.app`）的证据**：

| 证据 | 内容 |
|:---|:---|
| workbench JS 资产表 | `{assetType:"hook", scope:3, projectRelPath:".trae/hooks.json", globalRelPath:"hooks.json"}` |
| `libharness.dylib` | 含 `SessionStart` / `UserPromptSubmit` / `PreToolUse` / `PostToolUse` / `Stop` / `SessionEnd` / `Notification` |
| 配置键 | `HooksConfiguration{ enabled_folders, run_mode, import_claude_folders, global_hooks_enabled, global_import_claude_enabled }` |
| 执行上下文 | `HookExecContext{ event_name, additional_context, text_stdout, blocking_error, stop_reason }` —— 与 TraeCode 同一套协议 |
| 附带发现 | 支持**直接导入 Claude Code 的钩子目录**（`import_claude_folders`） |

**WorkBuddy 的证据**：

| 证据 | 内容 |
|:---|:---|
| 应用包内含 CodeBuddy CLI 及中文文档 | `hooks.md` 明确列出用户级 `~/.codebuddy/settings.json`、项目级 `<项目根>/.codebuddy/settings.json`、项目本地 `.codebuddy/settings.local.json` |
| 内置插件用的是 CodeBuddy 格式 | `builtin-plugins/sheetagent/hooks/hooks.json` → `type:"command"` + `${CODEBUDDY_PLUGIN_ROOT}` |
| 环境变量 | `CODEBUDDY_PROJECT_DIR` / `CODEBUDDY_PLUGIN_ROOT` / `CODEBUDDY_SKILL_DIR` |
| 结论 | WorkBuddy 的 Agent 运行时**就是 CodeBuddy**，故与 `codebuddy` 共用同一份用户级配置 |

**两点必须讲清**：

1. **两者共用同一运行时**，故钩子侧无法区分 WorkBuddy 与 CodeBuddy Code。配置里保留运行时本名
   `codebuddy` 作为标签，审计中的 `harness` 字段也会是 `codebuddy`。这是事实而非缺陷。
2. **路径与事件来自静态分析，尚未观察到真实会话触发。** 验证方法：
   用一次之后 `mem audit --tail 5` 看是否出现 `prompt-submit` 记录；有即生效。
   注册表的 `Notes` 里保留了「⚠️ 待实机验证」，并有测试断言这句话必须在。

**已安装**：TraeWork 项目级 → `<repo>/.trae/hooks.json`（该目录已被本仓库 gitignore，属本地文件）；
WorkBuddy → 复用 `~/.codebuddy/settings.json`（无需单独安装）。

**副作用（留给用户判断）**：既然两者原生钩子已就位，写进 `AGENTS.md` 的指令块就是**冗余兜底**，
代价 +344 tokens/轮。**待实机验证钩子确实触发后**，可考虑移除那一块省 token；
在此之前保留 —— 原生钩子未验证时它是唯一靠得住的通道。

### 12.14 钩子的幂等去重：同一事件被多处配置触发时只注入一次

**问题**：同一事件可能被**多处配置**并行触发。实例：TraeWork 的项目级
`<项目>/.trae/hooks.json` 与全局 `~/.trae-cn/hooks.json` 都定义了
`SessionStart` / `UserPromptSubmit`，而宿主的合并语义是
「不同作用域的 hooks **合并**而非覆盖，同一事件的所有匹配 hooks **并行执行**」
⇒ `mem hook` 跑两次、同样内容注入两遍、审计里出两条。

**可行性与落点**：用幂等解决是可行的，**且这是正确的落点** ——
用户的环境本来就可能有多个作用域（全局 / 项目 / 项目本地 / 企业策略），
工具侧对重复触发免疫比要求用户"只装一处"更稳。

**做法**：把「同一事件只让一个调用者注入」变成**数据库级原子声明**（`hook_dedup` 表）：

```sql
INSERT INTO hook_dedup(key, ts) VALUES(?, ?)
  ON CONFLICT(key) DO UPDATE SET ts = excluded.ts
  WHERE hook_dedup.ts < ?      -- 仅当旧记录已过期才认领
```

`RowsAffected() > 0` ⇒ 归我处理；`= 0` ⇒ 别人刚处理过 ⇒ 抑制注入。

**四条设计约束**（每条都有对应测试与变异验证）：

| 约束 | 为什么 |
|:---|:---|
| 🔴 **键不含 harness** | 重复触发恰恰来自两处配置用了**不同的 harness 名**（`traework` / `traecode`）。把 harness 纳入键，去重会**永远失效** —— 那正是要防的场景。同理不含 `--limit` / `--max-chars` 等工具旋钮 |
| **必须有时间窗口**（默认 10s） | 同一会话里用户**可能合理地重复同一句话**（两次「继续」），那是两个真实事件、应各注入一次。而重复触发发生在**毫秒级**，秒级窗口足以区分。窗口内被抑制的那次，内容与刚注入的完全相同、仍在上下文里，**抑制的损失可忽略** |
| 🔴 **必须原子** | 宿主是**并行执行**两处钩子。若先 `SELECT` 再 `INSERT`，两个进程都读到「不存在」而双双注入。实测：把实现改成「先查后插」，并发用例立刻报 `got 2` 赢家 |
| **失败开放** | 判不出是否重复时**照常注入**。注入两次只是啰嗦；**漏**注入是功能缺失 —— 两者代价不对等 |

**副产品：抑制本身是度量**。被抑制的触发会写一条 `suppressed=true` 的审计记录，
`mem audit` 会显示「重复触发：已抑制 N 次」。N>0 就是「配置里确实存在重复」的直接证据 ——
这比靠推断强，也给了清理冗余配置的依据。

**验证**（真机模拟两处配置）：
第 1 次（`traework`）注入 3 条；第 2 次（`traecode`，同一 stdin）**无任何输出**；
第 3 次换提问正常注入；审计里恰好 1 条 `suppressed=true, harness=traecode, hits=0`。

⚠️ **去重是防御，不该成为留冗余配置的理由**：它让工具对重复配置免疫，
但冗余配置本身仍浪费一次进程启动。看到抑制数 >0 时，建议顺手清掉一处。

