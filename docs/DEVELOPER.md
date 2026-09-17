# 开发者指南（v2mem）

面向要改代码、补测试、跑门禁的开发者。使用者入口见 [README](../README.md)。

## 构建与常用命令

```bash
make test        # go test ./...（CGO_ENABLED=0）
make build       # → bin/mem
make smoke       # 造一个临时库跑 add/search/stats
make build-linux # 交叉编译（Go 内建，无需交叉工具链）
make build-win
make cover       # 覆盖率报告：终端汇总 + coverage.html 逐行可视化
make cover-gate  # 覆盖率门禁：逐包校验下限，不达标退出 1
make cover-update # 用当前实测值刷新下限（棘轮：只允许上调；降标须 FORCE=1）
```

提交前必跑 `make cover-gate`。阈值单一真理源是 `coverage-policy.txt`，不在 README 重复。

## 安装 / 更新 / 部署

### 本地调试部署（开发时推荐）

**本地只要管"调试部署"，发布的版本走远程路径见下节。** 软链让 `$GOPATH/bin/mem` 指向仓库构建产物，值随代码变。

1. **装（一次性）**：`make build` → `bin/mem`，再软链进 PATH：
   ```bash
   make build                                    # → bin/mem
   ln -sf "$PWD/bin/mem" "$(go env GOPATH)/bin/mem"
   ```
2. **每次改动的「改 → 编 → 验」循环**：
   ```bash
   make build      # 重新编译 → bin/mem（软链自动跟到新产物）
   mem --help      # 子命令清单应包含本次改动项
   make smoke      # 独立临时库跑 add→search→stats，先拦明显回归
   ```
3. **观测读/写是否按预期留痕**（写侧治理是否有回归就看这里）：
   ```bash
   mem audit --tail 5     # 最近审计事件：read（search/注入）与 write（add）各归其位
   ```
4. **调试隔离**：别把测试数据写进真实长期库，用 `--db` 指独立测试库（`--db` 对所有数据命令通用，`-json` 便于脚本解析）：
   ```bash
   DB=/tmp/v2mem-debug.db; rm -f "$DB" "$DB-wal" "$DB-shm"
   mem add    --db "$DB" --kind decision "隔离测试：不进长期库"
   mem search --db "$DB" "隔离测试"
   ```
5. **切回发布版 / 撤销本地**：`make install`（写真实文件）或重跑 curl 安装即脱离软链。
   ⚠️ 软链状态下别跑 `make install`：`go install` 会写真实文件**覆盖软链**，之后改动不再自动跟随；
   想再回开发软链就重跑第 1 步的 `ln -sf`。

### 远程发布版（使用者路径）

`scripts/install.sh` 从 GitHub Releases 下载 `mem-$OS-$ARCH` 预编译产物，失败回退源码构建。升级=重跑同一行 curl。

### 发版（对外）

过 `make cover-gate` → `git tag vX.Y.Z` → 上传 `mem-darwin-{amd64,arm64}` /
`mem-linux-{amd64,arm64}` / `mem-windows-amd64.exe` 到对应 Release
→ 验证 `curl …/install.sh | bash --version vX.Y.Z`。

## 目录结构

```
cmd/mem/            CLI 入口（命令名 mem）
  main.go           add/search/touch/forget/gc/export/import/stats + help
  hook.go           钩子入口（读 stdin、注入、审计埋点）
  init.go           配置生成：扫描选择、合并写入、指令块
  level1.go         ingest（机械化搬运）与 budget（体量守卫）
  measure.go        audit / eval recall / eval write / eval activity
  mcp.go            MCP stdio server（initialize/tools list/call，search/add/ls/touch）
internal/
  store/            SQLite + FTS5、schema、生命周期、跨设备归并、相似归并
  similarity/       字符 n-gram + MinHash + 四条护栏（纯算法，无 IO）
  harness/          11 个工具入口的接入注册表（纯数据）
  audit/            审计日志读写与汇总
  eval/             评测指标（纯函数，口径可独立测试）
scripts/
  install.sh        curl 一键安装：下载预编译二进制，失败回退源码构建
  cover-gate.sh     覆盖率门禁实现
```

## 覆盖率纪律

覆盖率不是目标，是**防回退的闸门**。门槛写在 `coverage-policy.txt`（单一真理源），
由 `make cover-gate` 逐包校验 —— 任一项低于下限即非零退出。

门槛按**代码性质**分层，而不是给全仓一刀切一个数字：

| 包 | 下限 | 目标 | 为什么是这个量级 |
|:---|---:|---:|:---|
| `internal/eval` | 97% | 98% | 纯函数指标（命中率/漏记/碎片），输入空间可穷举 |
| `internal/similarity` | 96% | 97% | 纯算法、无 IO，n-gram 与护栏都可用构造数据打满 |
| `internal/harness` | 93% | 96% | 纯数据注册表，条目自洽性本身就可断言 |
| `internal/audit` | 86% | 90% | 日志读写；剩余主要是 IO 错误分支 |
| `internal/store` | 82% | 88% | SQL 与事务，含大量 sqlite 错误路径 |
| `cmd/mem` | 81% | 85% | CLI 编配层，职责是接线而非承载逻辑 |
| **整体** | **83%** | **88%** | |

三条纪律：

- **口径是包内视角**（`go test ./... -coverprofile`）：每个包由自己的测试负责，
  跨包调用产生的覆盖不计入被调方。所以「`cmd/mem` 的测试跑到了 `harness` 的代码」
  不算 `harness` 的覆盖率 —— 它需要自己的单元测试。这是 Go 的标准语义，
  也是唯一能让「哪个包该补测试」有确定答案的口径
- **棘轮只允许上调**：`make cover-update` 用实测值刷新下限，但拒绝下调。
  确需降标要显式写 `make cover-update FORCE=1`，并在提交信息里说明原因 ——
  门槛的松动必须是人为决定，不能顺手发生
- **新包必须登记**：门禁拒绝任何未出现在 `coverage-policy.txt` 里的包，
  不允许存在无门槛的包（同 Makefile 里「宁可没有目标，也不留静默的假目标」）

当前 0% 覆盖的只剩 4 个 CLI 入口与调度函数：`main`、`cmdGC`、`cmdStats`、`cmdEval`。
其中 `cmdStats` 由 `make smoke` 端到端跑到，其余是进程入口与低频子命令 ——
**已知欠账，不是遗漏**：给它们写单元测试等于重测 flag 解析，收益低于维护成本。

## 测试纪律

不是建议，是踩过坑后固化的：

- **测试必须隔离 `HOME`**：`mem init` 在未给 `--project` 时写"用户级配置"路径。
  曾因未隔离，测试二进制路径被写进了真实的 `~/.claude/settings.json`。
  现在 `TestMain` 把 `HOME` 指向临时目录，并有护栏用例固化这一点
- **变异测试要能编译**：用 `if false` 屏蔽分支会让变量变成未使用 → 编译失败 →
  测试根本没跑（失败行还会被 `grep` 过滤掉），得到的是假结论。变异要保留变量使用
- **负向断言要枚举全库**，不能只看检索返回的条目 —— 标题行本就不会被任何查询命中，
  用它做负向断言等于没检查

## MCP server 实现说明

`mem mcp` 暴露给支持 MCP 的 AI 工具：检索与写入走标准协议，配置只需一行进程启动。

- **自实现而非引 SDK**：项目纪律是「直接依赖只有 1 个、零外部服务」。
  MCP stdio 只是 newline-delimited JSON-RPC 2.0 的一个小子集（`initialize` /
  `tools/list` / `tools/call` / `ping`），自实现约 250 行且可纯函数单测；
  引 mcp-go 会破坏单文件轻量卖点，换来的流式/采样能力当前用不上
- **工具面刻意最小**：只暴露记忆本身的四种操作（`search`/`add`/`ls`/`touch`）。
  评测/运维（audit、eval、gc、consolidate）不进工具面，保持工具语义干净
- **审计复用**：MCP 的检索/写入与 CLI 共享 `manual-search`/`manual-add` 埋点，
  MCP 通道的使用也会进入 `mem eval activity` 的评测样本，不会被悄悄漏掉
- **测试分层**：协议纯函数（`dispatch`）直接喂 map 断言响应；传输层（`serve`）
  用字符串管道测逐行 round-trip 与解析错误；`cmdMCP` 经真实 stdin/stdout 端到端
