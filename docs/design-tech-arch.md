# v2mem R&D · CLI 设计与技术选型

> 本文是 `docs/DESIGN.md` 的专题切片，供按需阅读；各主题全文目录与关联集中在 [DESIGN 索引页](./DESIGN.md)。

## 8. CLI 设计（`mem`）

```
mem add      --kind <k> [--project P | --global] [--tag k=v]... "<原子事实>"
mem search   "<query>" [--project P] [--scope current|global|all] [--tag k=v]... [--limit N] [--json]
mem touch    <id>                  # 命中反馈，刷新 last_seen_at / access_count
mem forget   <id>                  # 显式删除
mem gc                             # 过期清理 + 衰减淘汰
mem consolidate [--threshold 0.7]  # 相似归并：近重复聚簇，留 1 条、其余置 superseded_by
mem export   [out.jsonl]
mem import   <in.jsonl>            # 跨设备归并
mem hook     [--event e] [--harness h] [--limit n] [--max-chars n]
                                   # harness 钩子入口：读 stdin JSON，注入内容写 stdout
mem init     --harness <名字> [--project <目录>] [--scope ...] [--file <路径>] [--dry-run]
                                   # 把钩子写进该工具的配置（合并、幂等、带备份）
mem harness  [--json]              # 列出各工具的接入方式、等级与本机检测结果（● = 已检测到）
mem ingest   <文件.md...>          # 把既有 md 的条目机械化搬进库（幂等；跳过标题/围栏/纯指引；合并缩进续行）
mem budget   --file <文件>         # Level 1 体量守卫：超预算返回非零退出码，供自动化告警
mem stats                          # 条数 / 分布 / 库大小
```

选项位置：Go 的 `flag` 要求**选项写在子命令之后**（`mem add --db X`，不是 `mem --db X add`）。

`--json` 输出供 LLM 解析；默认输出为紧凑纯文本，省 token。
`--scope`：`current` = 当前工程 + 全局；`global` = 只要全局；`all`/省略 = 跨工程（默认）。
`--global` 与 `--project` **互斥**——这是唯一能写入「空工程」记忆的入口，缺了它全局记忆在 CLI 上不可达。
`mem hook` 的 stdout 只放注入内容、诊断走 stderr，且**任何异常都返回 0**（见 §12.4）。

## 9. 已知坑

1. **绝不把 `mem.db` 放进同步目录**——写入中途同步会损坏库。只同步 JSONL。
2. **写入路径要便宜**：写入期只做 hash 去重 + FTS5 索引（微秒级）；embedding 与相似归并放后台。
3. **原子化是检索质量的前提**：实测 340 字符聚焦片段的相似度，比含同样信息的 1.5KB 多主题文档高 **2.3 倍**。库里存**原子事实**，绝不存文档正文。
4. **钩子依赖 LLM 真的去调**：保留少量热事实在 Level 1 做降级；命令要快到「投机性检索也不心疼」。
5. **多进程并发**：开 WAL + `busy_timeout`。

## 10. 里程碑

- [x] M1 骨架：库 + schema + `add` / `search`（FTS5 单路）+ `stats`
- [x] M2 生命周期：`touch` / `gc`（TTL + 衰减）+ `forget`
- [x] M3 标记与工程维度：`--project` / `--global` / `--tag` / `--scope` 全链路
- [x] M4 跨设备：`export` / `import` 归并 + 溯源
- [x] **M4.1 取代关系跨设备重建**：取代引用改为身份键表达，导入端两遍解析（§7.5）
- [x] **M5a 相似归并（纯 Go，无模型）**：字符 n-gram MinHash + 四条护栏 + `consolidate`
- [ ] **M5b-0 模糊检索（建议先做）**：把已有 MinHash 签名接到检索 + RRF 融合，零包体成本（§13.4）
- [ ] M5b-1 向量增强（暂缓）：sqlite-vec 的 Go 绑定当前不可用，见 §13.2
- [x] **M6 Harness 集成**：`mem hook` 统一事件入口 + `mem init` 扫描选择与配置生成（11 个工具入口，见 §12）
- [x] **M6.1 Level 1 运维**：`mem ingest` 机械化搬运 + `mem budget` 体量守卫；AGENTS.md 已接入（见 §14）
- [x] **M7 度量与评测**：`mem audit` 埋点 + `mem eval recall|write`；口径红线与迁移判据见 §15
- [x] **M7.1 召回退化修复**：长问句词组内 AND 导致 0 命中 → 精确优先、空手才放宽（§15.5）

> M5 拆成两半是本次实现中的结论：相似归并属**近重复检测**（经典算法问题），
> MinHash 几十行、微秒级、零依赖即可胜任，不必引入模型与 CGO。
> 于是 M5b 的向量化从「相似归并的前置」降级为「检索召回的另一路」。

## 11. 技术选型（已定：Go）

### 11.1 为什么是 Go

| 候选 | 结论 |
|:---|:---|
| **Go** | ✅ **选定**。默认构建全链路无 CGO；交叉编译内建（`GOOS/GOARCH`）；纯 Go SQLite 驱动；编译秒级 |
| Rust | 备选。ONNX 侧更优（`tokenizers` 是 HF 官方 Rust 实现、`ort` 可静态链入），但 `rusqlite` 的 `bundled` 构建需 `cc`，交叉编译要 `cargo-zigbuild`/`cross`，迭代成本高。纯 Rust 的 Turso/Limbo 仍是 BETA、FTS 实验性，**不能承载长期数据** |
| Python | 否。ONNX/embedding 生态最顺，但 M1–M4 根本不需要模型；需解释器+环境，启动约 50ms |
| Node/TS | 否。同 Python，需 node + node_modules |

**决策判据**：M5（语义 embedding）是**可选**而非必选——相似归并靠 MinHash/SimHash 即可，不需要模型。既然核心路径无 ML，Go 的「无运行时 + 内建交叉编译 + 秒级构建」就成为决定性优势。

### 11.2 依赖清单（当前直接依赖仅 1 个）

| 用途 | 选型 | 需 CGO |
|:---|:---|:---|
| SQLite 驱动 | `modernc.org/sqlite`（SQLite C 转译为 Go） | 否 |
| 全文检索 | SQLite FTS5（内建）+ 自建中文索引 | 否 |
| CLI | 标准库 `flag` | 否 |

> 采用 `modernc.org/sqlite` 而非更高层的 `gosqlite.org`：后者同样基于 modernc 且提供更完整的 typed FTS5/vec/**RRF 融合**，但更晚出现。M1 只需基础 FTS5；待 M5 需要 RRF 与向量时再升级到 `gosqlite.org`，届时是同一引擎，迁移成本低。

### 11.3 中文检索方案（关键决策）

FTS5 自带分词器都不适用于中文，本机实测（sqlite3 3.51）：

| 分词器 | 记忆库(3字) | 记忆(2字) | 目录(2字) |
|:---|:---|:---|:---|
| `unicode61`（默认） | ✗ | ✗ | ✓ 仅整串命中 |
| `trigram` | ✓ | ✗ | ✗ 有 3 字符下限 |

**方案**：写入时由 Go 侧把连续汉字展开为「一元组 + 二元组」存入 `content_idx`，FTS5 只索引该列；检索时编译为 FTS5 表达式——**词组内部 AND（精确）、词组之间 OR（召回优先）**，按 bm25 排序。零额外依赖，且支持单字与二字词查询。

### 11.4 构建与分发

```bash
make build       # CGO_ENABLED=0 → bin/mem（arm64，约 10MB）
make install     # 安装到 ~/go/bin/mem
make build-linux # 交叉编译 linux/amd64，静态链接
make build-onnx  # M5 可选分支：-tags local_onnx，需 CGO + libonnxruntime
```

- macOS 产物仅依赖系统 `libSystem` / `libresolv`（Go 在 macOS 的固有行为，**非 CGO 依赖**）；Linux 产物为完全静态链接。
- 需把 `~/go/bin` 加入 PATH：
  `echo 'export PATH=$HOME/go/bin:$PATH' >> ~/.zshrc`

### 11.5 环境配置（已做）

- Go **1.27.1**（brew 安装）
- `GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=sum.golang.google.cn`（`proxy.golang.org` 在本机网络不可达，已用 `go env -w` 持久化）

### 11.6 M1 已完成能力

- `mem add`：原子事实写入；**相同知识覆盖**（归一化 hash，忽略大小写/空白/句尾标点差异）；工程名默认取当前 git 仓库根；支持 `--kind/--project/--tag/--tool/--device/--salience/--ttl`
- `mem search`：FTS5 + bm25；支持中文（含二字词）、英文、混合；`--limit/--project/--kind` 过滤；已过期自动排除
- `mem stats`：条数 / 类型分布 / 工程分布 / 待回收 / 库大小
- 全局：`--db`（默认 `~/.v2mem/mem.db`）、`--json`（供 LLM 解析）
- 并发：`journal_mode=WAL` + `busy_timeout=5000`（已验证）

### 11.7 M2 已完成能力（生命周期闭环）

- `mem touch <id|前缀>`：命中反馈，刷新 `last_seen_at` + 累加 `access_count`；支持 8 位短 ID 前缀，**歧义前缀报错**而非静默选第一条
- `mem forget <id|前缀>`：显式删除，`tags` 经外键 `ON DELETE CASCADE` 级联清理
- `mem gc [--max-idle 720h] [--min-salience 0.2]`：回收两类——TTL 到期（`expired`）、久未命中且低重要性（`decayed`）

**淘汰判据**：`last_seen_at < now - maxIdle AND salience < minSalience`。
关键取舍：**高 salience 的记忆即使久未命中也保留**——重要性应当能抵御遗忘，否则重要但不常用的知识会被误删。

**测试**：TDD 编写（先写失败测试再实现），`go test ./...` 全绿，共 **12 个用例**：

| 范围 | 用例 |
|:---|:---|
| M1 回归 | 相同内容覆盖不重复、中文二字词检索 |
| touch | 刷新 last_seen + 累加计数、支持前缀、歧义前缀报错 |
| forget | 删除记忆并级联删除 tags（CLI 端到端：forget 后检索不到） |
| gc | 回收 TTL 到期、淘汰久未命中低重要性、保留高 salience、保留近期命中（CLI 端到端：unknown ID 报错） |

端到端已验证 `gc` 的 TTL 路径真实执行（`expired=1`），非静默空转。

### 11.8 M3 已完成能力（标记与工程维度）

- `mem add --tag k=v`（可重复）：写入多机 / 多工具 / 多工程标记；同内容覆盖时 **tags 取并集**而非替换（`INSERT OR IGNORE`），避免覆盖丢掉历史标记
- `mem add --global`：写入**空工程**的全局记忆，对所有工程生效。与 `--project` 互斥
- `mem search --tag k=v`（可重复）：**AND 语义**，用 `EXISTS` 子查询逐条断言，避免 JOIN 造成的结果放大
- `mem search --scope current|global|all`：
  - `current` = 当前工程 + 全局（`project = ? OR project = ''`）
  - `global` = 只要全局（`project = ''`）
  - `all` / 省略 = 跨工程（默认）
  - `current` 未显式给 `--project` 时按当前 git 仓库根名自动推断

**设计要点**：`--global` 是唯一能写入空工程记忆的入口。若只有 `--project`，空值会被 `detectProject()` 自动填充，导致库层的全局语义（`project = ''` 对所有工程生效）**在 CLI 上不可达**——即「能力存在但无路径抵达」的静默失效。工程名同时用于 `stats` 的按工程分布。

**测试**：共 **23 个用例**（`go test ./... -v` 实测：store 16 + CLI 7；M2 时点为 12，M3 新增 11）：

| 范围 | 用例 |
|:---|:---|
| 标记过滤 | 单标记命中、多标记 AND 语义（存在+不存在必空）、同内容覆盖时 tags 取并集 |
| 工程作用域 | `current` 含本工程与全局、`current` 排除他工程、默认跨工程返回全部、`global` 只要全局 |
| 全局记忆 | `--global` 写入空工程标记、`--global` 与 `--project` 冲突报错且拒绝写入 |
| CLI 端到端 | `--tag` flag 生效、`--scope current` 缩窄到本工程、`--scope global` 排除工程记忆 |

**e2e 验证矩阵**（临时库，4 条数据覆盖 alpha / beta / 全局 / 双标记）：

| 查询 | 期望 | 结果 |
|:---|:---|:---|
| 默认 all | 4 | 4 ✅ |
| `--scope current`（本工程无记忆） | 1（全局） | 1 ✅ |
| `--scope global` | 1 | 1 ✅ |
| `--project alpha` | 2 | 2 ✅ |
| `--tag machine=laptop` | 1 | 1 ✅ |
| `--tag machine=mini --project beta` | 1 | 1 ✅ |
| `--project alpha --scope current` | 3（alpha 2 + 全局 1） | 3 ✅ |
| 负对照：不存在的 tag / 存在+不存在 tag / 不存在的工程 | 0 / 0 / 0 | 0 / 0 / 0 ✅ |

> 度量教训：首版 e2e 用 `wc -l` 计数，而空结果会打印一行 `（无结果）`，导致负对照恒为 1 —— **负对照抓出的是度量工具失准，不是代码缺陷**。最终改用 `--json` + 长度统计，并先自检仪器（`--limit 1` 应得 1）。

