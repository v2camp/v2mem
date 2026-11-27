# AGENTS.md

## 任务流程

本仓库的任务在独立 git worktree 中开发（一任务一分支一 worktree，全部建于 `.worktree/` 下），收尾时合入 main 并清理 worktree 与分支。
**合入并清理后，必须询问是否跑 `make build`，并先确认 PATH 上的 `mem` 来源**：`ls -l "$(command -v mem)"`。
仅当它指向主仓库 `bin/mem`（软链或同源来源）时重建才有意义——worktree 里的构建产物它看不到，不重建验证到的就是旧二进制；
curl 发布版或 `make install` 落地的是真实文件，重建仓库不影响验证，应按该机安装方式更新（重跑安装脚本 / `go install`）。
合并提交正文用**真换行**写 `Base: <sha>` / `Gate: <命令 + 结论 + 关键数值>` / `Closes: <任务号>`，并附本分支测试输出尾部；模板与历次门禁留痕见 [`docs/DEVELOPER.md`](./docs/DEVELOPER.md)「合并门禁与留痕」。只列命令名不算留痕。
版本管理规则：发版在 main 打语义化 tag（vX.Y.Z），只打在通过 `make cover-gate` 的提交上。

## 设计权威

判断「该不该做某项改动」时先读 `docs/VALUE.md`（价值主张 + 红线 + 判据）；它与 `README.md` / `docs/design-*.md` 的表述冲突时，以 `docs/VALUE.md` 为准。

## 测试覆盖率（硬约束，细节见 README「覆盖率要求」）

1. 口径是**包内视角**：每个包由自己的测试负责，跨包调用不算被调方的覆盖率。
2. 下限**只允许上调**；降标须 `make cover-update FORCE=1`，并在提交信息里写明原因。
3. **新包必须在 `coverage-policy.txt` 登记下限**，门禁拒绝任何未登记的包。

提交前跑 `make cover-gate`（阈值单一真理源：`coverage-policy.txt`）。

## 运行观测（装好之后不再重复检查安装）

判据表在 `docs/ops-watch.md`，可执行形态是 `scripts/watch-v2mem.sh`；**改判据必须同步改两处**。

1. **安装只校验一次**：首次 / 换机 / 升级宿主或 mem 之后按 `docs/install.md` 安装并跑
   `scripts/verify-install.sh`（安装态 I 组判据仍以 `docs/ops-watch.md` 为准）。在该机完成
   `mem init --harness <宿主>` 并通过宿主要求的面板确认后，后续会话才交给钩子自动运行
   （`native`，代码强制，不靠模型自觉）。
2. **稳态运行不干预**：`SessionStart` 注入类码表、`UserPromptSubmit` 按 query 检索、`Stop`/`SessionEnd`
   落观测。不要为"让它记住"而手动塞上下文，也不要为"确认它在跑"而每轮查审计。
3. **观测走定时任务**：在该机配置定时任务后每 12h 跑 `scripts/watch-v2mem.sh --window 12h`。只有 `FAIL` 需要人介入，
   `WARN` 观察，`INFO` 忽略（冷启动期库 < 20 条时，R2/R4 按设计降级为 `INFO`）。
4. **判据错误自查**（以本仓库当前提交为准，发布版行为可能不同）：`session-start` 的 `empty` 恒为 false，
   只有 `prompt-submit` 的 `empty` 反映检索质量。

## 自动记忆写入（v2mem）

1. 钩子会自动裁决并写入**未审核**记忆；“已入库”不等于“已验收”，不要跳过复核。
2. 改提示词/模型/提交矩阵后，先看 `mem review stats` 的 confirm/correct/withdraw 与
   `manual_rate`；异常时先 `MEM_NO_AUTOMATION=1` 停自动写侧，再改配置。
3. 错误记忆用 `mem review withdraw <id>`；整批回滚先跑
   `mem review rollback --model-tag <tag> --dry-run` 核对 matched/skipped。
4. 积压接近 15 天 SLA 时优先处理吞吐/drain，不扩大自动采纳范围。

## 文档纪律（AI 友好，硬约束）

文档通常会被整体读入 AI 上下文，越长越容易超限、丢弃或稀释关键信息。以下为硬下限之外的硬上限：

1. **入口文档要瘦**：`README.md`、`AGENTS.md` 等入口文件尽量 ≤ 300 行；超过就要把深度内容下沉到 `docs/` 下的专题文档，入口只留链接和一句话摘要。
2. **单文件文档上限**：任何单个 `.md` 不超过 `500 行 / 60KB`。达到或接近该上限时，按主题拆分为 `docs/<topic>.md`，并改指链接。
3. **不在正文里整段内嵌大文件内容**：引述他人、粘贴日志/数据、复述其他文档时，指向原文路径，不要拷贝整段。
4. **路径引用保持可点击**：新增或移动文档后在 README 更新相对链接，避免死链。
5. **新增文档先登记**：新建 `docs/*.md` 时，同步在 README「文档分工」表登记用途，并先确认没有重复的同主题文档。
