# P4 验收包：eval flow 基线与 U 类走查清单（acceptance-p4）

> **定位**：波 5 的验收交付物。回答两件事——①「准出率/严密度」的 G1–G13 逐跳
> 自动检查在**真实 CLI 动线**上是否全绿（口径见
> [`design-eval-flows.md`](./design-eval-flows.md) §3.1）；②无法从库表自动判定的
> U1–U8 人工走查每条怎么走、看什么、谁签字（§4）。
> **基线 = `079c98d`**（main，S12/S14 合入后由主控复跑，见 §6）。
> **纪律**：本包只读产品代码、不改任何产品行为；全部演练在临时 HOME + 临时库
> （`/tmp/w5-acc-home`）完成，**不触碰 `~/.v2mem`**；U 类里无法在本环境演练的
> 条目已在 §4 逐条标注「偏离」。

## 1. P0–P3 切片完成清单与合入哈希

口径来源 [`design-data-lifecycle.md`](./design-data-lifecycle.md) §10（P0–P4 实施顺序）。
哈希取自 `git log --oneline`（main@079c98d）。

| 阶段 | 切片 | 内容 | 合入哈希 |
|:---|:---|:---|:---|
| 前置 | — | 数据动线设计 + 命令面简化折入 | 74d8cab、686dca2、6dac6e7 |
| P0 | S0 | 测试基建：动线对账纯函数（Reconcile）+ testkit（fake clock/故障注入） | ceaa70f |
| P0 | S1 | 归档生产级回读证明（VerifyRef + `mem audit --read-ref`） | 56918d9 |
| P0 | S2 | 全量引用保护（GC 引用图扩集 + edges 对端回收 + 回读校验拒删） | 89ea5d7 |
| P0 | S3 | 破坏性 GC 前备份 + 巡检只读化（删 `--no-gc`） | cc42a2b |
| P0 | S4 | 积压五指标（pending/未消费证据/Bundle/spool/隔离/最后维护） | e4af351 |
| P1 | S5a | 规则 executor 接入链尾兜底 | 32718de |
| P1 | S5b | 单轨切换——SessionStart 拉起 proposal、灰度开关退役 | 20b933b |
| P1 | S6a | Bundle 有界重试与隔离回捞 | 2f6a93f |
| P1 | S6b | 按源公平调度——头让位不越窗 | 91c94b6 |
| P1 | S6c | 序号洞记录并越过 + spool dead-letter 与公平 drain | 40b54a4 |
| P1 | S7 | eval flow 逐跳验收入闸（G1–G13 进 `make test`） | 349db8c |
| P2 | S8 | 维护门面 `mem maintain` 与 R16 可见性 | be3081e |
| P2 | S9a | 六类故障注入基建（hook/gc/distill，F1–F6） | 0305f72 |
| P2 | S9b | maintain 入口故障注入（F7–F12） | c4e32b3 |
| P2 | S10a | 复核数据面与 `mem review ls` 四态视图（schema v3） | 733d34a |
| P2 | S10b | 复核动作面（撤回/修正/回滚/标定/model-auto） | 77aad23 |
| P2 | S11 | 单行触达面与 MEM_NO_AUTOMATION 总闸 | d879091 |
| P3 | S13 | 保留与终态出口（rejected/Bundle/决策账/文件轮转/R15） | 079c98d |
| — | S12 | consolidate（并行分支 `dev/w5-s12-consolidate`） | **未合入** |
| — | S14 | 存储分层/性能（并行分支 `dev/w5-s14-perf`） | **未合入** |

P4（Judge 扩权）按 §10 不作前置条件，本验收包不覆盖。

## 2. 门禁基线数据（本 worktree，基线 079c98d）

环境：Darwin/arm64，worktree `.worktree/w5-acceptance`，`make build` 产物 `bin/mem`
（18,666,402 字节，CGO_ENABLED=0）。

### make test（watch 90/90 + test-flow + 全包）

```
watch-v2mem 判据回归 90/90 PASS
ok  watch-sync: 21 个判据 ID 双向一致；…；枚举到 R16
CGO_ENABLED=0 go test -count=1 ./test/flow
ok  	github.com/v2camp/v2mem/test/flow	0.412s
CGO_ENABLED=0 go test ./...
ok  	github.com/v2camp/v2mem/cmd/mem	13.347s
ok  	github.com/v2camp/v2mem/internal/audit	0.762s
ok  	github.com/v2camp/v2mem/internal/commit	0.307s
ok  	github.com/v2camp/v2mem/internal/distill	2.178s
ok  	github.com/v2camp/v2mem/internal/eval	0.822s
ok  	github.com/v2camp/v2mem/internal/evidence	2.417s
ok  	github.com/v2camp/v2mem/internal/harness	0.736s
ok  	github.com/v2camp/v2mem/internal/proposal	1.692s
ok  	github.com/v2camp/v2mem/internal/recall	0.406s
ok  	github.com/v2camp/v2mem/internal/scrub	0.310s
ok  	github.com/v2camp/v2mem/internal/similarity	0.309s
ok  	github.com/v2camp/v2mem/internal/store	5.341s
ok  	github.com/v2camp/v2mem/internal/testkit	0.379s
ok  	github.com/v2camp/v2mem/internal/transcript	0.306s
ok  	github.com/v2camp/v2mem/test/flow	0.308s
```

### make cover-gate

```
PACKAGE                ACTUAL     MIN   TARGET    MARGIN  VERDICT
internal/eval           99.5%     98%      98%     +1.5  ok
internal/testkit        98.3%     96%     100%     +2.3  ok
internal/scrub         100.0%    100%     100%     +0.0  ok
internal/transcript     91.5%     89%      94%     +2.5  ok
internal/distill        88.4%     86%      92%     +2.4  ok
internal/proposal       91.0%     85%      90%     +6.0  ok
internal/similarity     97.0%     97%      97%     +0.0  ok
internal/harness        93.8%     93%      96%     +0.8  ok
internal/audit          91.3%     89%      90%     +2.3  ok
internal/recall         97.3%     93%      96%     +4.3  ok
internal/store          82.2%     82%      88%     +0.2  ok
internal/commit         97.5%     97%      98%     +0.5  ok
internal/evidence       81.8%     81%      85%     +0.8  ok
cmd/mem                 82.4%     81%      85%     +1.4  ok
TOTAL                   84.8%     83%      88%     +1.8  ok
✓ 覆盖率门禁通过（下限见 coverage-policy.txt）。
```

### make test-faults

```
CGO_ENABLED=0 go test -tags=faults -count=1 -timeout 300s ./test/faults
ok  	github.com/v2camp/v2mem/test/faults	15.606s
```

### make test-e2e

```
--- PASS: TestE2EFullLifecycle (7.02s)
    --- PASS: 01_安装（install.sh_源码构建） (5.09s)
    --- PASS: 02_初始化（mem_init_幂等_+_备份） (0.02s)
    --- PASS: 03_使用（add_去重_/_search_/_touch_/_stats） (0.12s)
    --- PASS: 04_维护（gc_TTL_回收_+_consolidate_归并） (1.65s)
    --- PASS: 05_优化（lexicon_提取_+_同义词展开生效） (0.06s)
    --- PASS: 06_评测（gold_判据_+_用量视图） (0.06s)
    --- PASS: 07_卸载（uninstall_摘除且不碰用户配置） (0.02s)
ok  	github.com/v2camp/v2mem/test/e2e	7.348s
```

## 3. G1–G13 基线结果

### 3.1 动线构造（全部 CLI 驱动，临时库 `/tmp/w5-acc-home/.v2mem/mem.db`）

复用 `test/flow` 夹具的逐跳形态，但走真实 CLI 与真实退避时钟：

1. 临时 `HOME`，`mem stats` 建库；`model.json` 设 `mode=cli` 指向 PATH 里的假
   `claude`（确定性输出，同 `test/faults` 的 `installFakeCLI` 手法）。
2. 捕获：`mem hook`（stdin JSON）×5——3 条 UserPromptSubmit（session sA，可提案）、
   1 条 UserPromptSubmit（session sF，可提案）、1 条 PostToolUse（session sN，
   不可提案）。
3. 坏输出轮：假 CLI 返回非 JSON → `mem distill --proposal --executor auto --json`
   → sA 的 Bundle `fail`（attempt=1，`invalid` 运行账）。
4. 重试轮：换好输出（1 行合法 proposal），等指数退避（attempt=1 → 1 分钟）后重跑
   → sA `ok` attempt=2（G7 的「失败有重试账」样本）、sF `ok` attempt=1、
   sN `noop`（G6 的合法空结果样本）。
5. 裁决：`mem candidates ls --status pending --json` 取 2 条 → `mem reject <id>` 1 条
   （actor=human）、`mem accept --all-pending` 1 条 → 记忆 + memory_evidence 绑定。
6. 收尾：`mem review ls`、`mem maintain --json`（processed=0，队列干净）、
   `mem gc --retention --dry-run`（零写）。
7. `mem stats`：39 行（candidates=2、commit_decisions=2、distill_runs=4、
   evidence_archives=5、evidence_bundles=3、memories=1、memory_evidence=1）；
   积压五行全 0；「维护 最后成功 0分钟前」。

### 3.2 逐条结果（`mem eval flow --json`，exit=0）

| ID | 检查 | 结果 | detail（基线实测） | 门禁覆盖 |
|:---|:---|:---|:---|:---|
| G1 | 捕获幂等 | PASS | 5 条观测，幂等键无重复 | test/flow E2 + 单点破坏 E3 |
| G2 | 观测归档 | PASS | 5 条观测均有归档账（legacy 豁免 0） | 同上 |
| G3 | 归档账本一致 | PASS | 5 个归档的 raw_count 与观测数一致 | 同上 |
| G4 | 归档序号连续 | PASS | 5 段归档首尾相接 | 同上 |
| G5 | Bundle 连续覆盖 | PASS | 3 个 Bundle 连续覆盖 cursor | 同上 |
| G6 | noop 水位 | PASS | 1 个 noop 窗口均为合法空结果 | 同上 |
| G7 | 失败有重试账 | PASS | 失败窗口均已重试或按上限隔离 | 同上 |
| G8 | 无悬空候选 | PASS | 2 条候选无超窗悬空 | 同上 |
| G9 | 候选裁决闭环 | PASS | 2 条候选均有匹配裁决 | E2/E3 + R8 回滚回归 |
| G10 | 裁决幂等 | PASS | 2 条裁决幂等键唯一 | E2/E3 |
| G11 | 记忆证据绑定 | PASS | 1 条记忆绑定完整（1 条带证据） | E2/E3 |
| G12 | 准出率对账 | PASS | 准出率 100%，2 个窗口全量对账 | E2/E3 + R8 |
| G13 | 重复与严密度 | PASS | 无重复提交、无严密度违例 | E2/E3 |

汇总：`passed 13 / failed 0`，`manual_rate=1`（人工介入率 100%——基线刻意走
`mem accept`/`mem reject` 人工裁决，与 P1 人工验收阶段口径一致；U1 的自动裁决
链路由 `--auto`/矩阵用例另行覆盖）。文本输出尾部：

```
通过 13/13，人工介入率 100.0%
```

> 注：上表「门禁覆盖」指 `test/flow` 的四个用例（E2 全绿夹具 / E3 单点破坏→恰好
> 对应 G 变红 / E4 只读性 / R8 回滚后 G9/G12 仍绿）。CLI 基线证明的是**真实二进制
> + 真实退避时钟**下同口径仍绿；两套互为印证，口径唯一来源是
> `internal/eval/flow_check.go` 与 §3.1 G 表。

## 4. U1–U8 走查清单

每条格式：**目的 / 前置 / 步骤 / 预期 / 证据留痕 / 判定人 / 自动化程度**。
自动化标注约定：✅ 可全自动（已有门禁）；◐ 可脚本化但需人核对输出；✋ 必须人做。
「留痕」列给出核对时看的输出字段或库表。

### U1 装完即用（默认提示词、零人工配置）

- **目的**：安装→init→一次真实会话（捕获→Bundle→候选→自动裁决→记忆）全程无人值守。
- **前置**：干净临时 HOME；真实模型链路可用（`model.json` 指向本地 ollama 或
  `mode=cli`；本基线用假 CLI 等价验证到「候选→裁决」，模型真实调用属偏离，见 §6）。
- **步骤**：
  1. `bash scripts/install.sh --from-source --dir /tmp/u1-bin`（HOME=临时目录）；
  2. `mem init --harness claude`（确认写钩子、生成 `.v2mem.bak`）；
  3. 发起一次会话：UserPromptSubmit + Stop（带 transcript_path）各一条 stdin 给
     `mem hook`；
  4. `mem maintain --json`（SessionStart 在真实链路会自动拉起，此处显式驱动）；
  5. `mem candidates ls --status accepted`、`mem search <关键词>`、`mem stats`。
- **预期**：候选按提交矩阵自动裁决（auto_commit 落库，`--model-auto` 关时人工类型
  留 pending）；`stats` 积压五行归零；检索命中新记忆；全程无手工配置文件编辑。
- **留痕**：`stats` 的「积压」行与「维护」行；`maintain --json` 的 `ok/noop/inserted`；
  `commit_decisions.actor`；audit.jsonl 的 hook/distill 分因记录。
- **判定人**：验收人对照 stats 与 review 输出签字。
- **自动化**：◐ 安装→init→add/search/stats→gc→卸载已由 `make test-e2e`
  （TestE2EFullLifecycle 01–07）覆盖；「模型真实生成」环节需人做或由夹具等价。

### U2 日常多会话

- **目的**：连续 N 个会话准出率 100%；未审核计数经触达面可见。
- **前置**：临时库；≥3 个会话的证据（session_id 不同）。
- **步骤**：
  1. 对 s1/s2/s3 各跑一轮 `mem hook`（UserPromptSubmit ×2 + Stop）；
  2. 每会话结束跑 `mem maintain --json`；
  3. `mem eval flow --db <db>` 与 `--json`；
  4. `mem stats` 看 pending/未审核计数；SessionStart 触发一次 hook 看单行触达面
     （S11）是否把计数带进注入。
- **预期**：G12 准出率 100%、G8 无悬空；stats 积压如实；SessionStart 注入含
  未审核计数单行。
- **留痕**：`eval flow` 的 G8/G12 detail；`stats` 积压行；SessionStart 的 stdout
  单行；audit.jsonl。
- **判定人**：验收人。
- **自动化**：✅ G 项全自动（eval flow）；◐ 触达面可见性需人看一眼注入文本。

### U3 错误记忆撤回

- **目的**：一步 `withdraw` 退出检索；按 `model@promptver` 回滚跳过人工修正条目。
- **前置**：已有至少 1 条检索可见记忆（本基线临时库已演练，命令语法均验证通过）。
- **步骤**（注意：动作子命令的 flag 要写在位置参数之前）：
  1. `mem ls --json` 取 memory id；
  2. `mem review withdraw --db <db> --reason "验收撤回" <memoryID>`；
  3. `mem search --db <db> <关键词>` → 应无结果；
  4. `mem review restore --db <db> <memoryID>` → `mem search` 恢复命中；
  5. `mem review rollback --db <db> --model-tag <TAG> --dry-run` 核对 matched/skipped
     计划 → 去掉 `--dry-run` 执行 → `mem review stats` 看标定。
- **预期**：withdraw 后检索不可见、行与证据保留（`memories.review_state=withdrawn`、
  review_action 留账）；restore 原路恢复；rollback 只撤该版本自动记忆，人工
  confirm/correct 条目进 skipped 并在输出报告。
- **留痕**：`memories.review_state`、`review_action` 表、`commit_decisions`、
  `review stats` 输出。
- **判定人**：验收人。
- **自动化**：✅ store 层动作语义由 cmd/mem 单测覆盖；`test/flow` R8 用例守
  「回滚后 G9/G12 仍绿」；◐ CLI 端到端（本清单 5 步）需人执行一遍。
- **基线演练**：withdraw→search 无结果→restore→search 恢复，已在临时库实测通过；
  rollback dry-run 输出 `matched 0`（临时库无按版本归因样本——真实样本需
  `--model-auto` 裁决链路，属演练面限制，不是缺陷）。

### U4 模型 key 失效

- **目的**：降级规则 executor 不积压 failed；触达面提示；换 key 后补判。
- **前置**：临时库 + 可控的坏模型配置。
- **步骤**：
  1. `model.json` 指向不可用 provider（坏 key / 假端点；本基线用「假 CLI 输出
     非 JSON」等价触发 invalid→fail 链路）；
  2. `mem distill --proposal --executor auto --json` → 观察 `status=invalid`、
     `bundle_status=fail`、attempt=1；
  3. 连跑 `mem maintain --json`：fail 窗口进指数退避（1/2/4/8 分钟），attempt 递增，
     不冒充 noop；`mem stats` 积压行如实；
  4. `mem review ls --state failed` 看 status_reason；
  5. 修复配置（换好 key / 好输出）后 `mem maintain --json`（或退避过后重跑
     distill）→ 补判完成；
  6. `mem eval flow` → G7 必绿。
- **预期**：failed 有界重试（上限 5 次后隔离）、全程退出码/状态如实；恢复后窗口
  补齐、准出率回到 100%。
- **留痕**：`maintain --json` 的 `fail/partial/quarantine`；`candidates ls` 的
  `status_reason` 与 `attempt`；`distill_runs` 账；G7 detail。
- **判定人**：验收人。
- **自动化**：✅ 坏输出/退避/force 重试由 `make test-faults`（F5、F12）覆盖；
  ◐ 「真实 key 失效→换 key」的业务动作需人做（本基线以假 CLI 等价演练，真实
  网络故障未演练，见 §6）。

### U5 故障后追账

- **目的**：`--catch-up` 幂等：连跑两次第二次零重复提交。
- **前置**：有积压的临时库（hook 若干条 + `--executor none` 建 backlog，或直接用
  上一步留下的 fail 窗口）。
- **步骤**：
  1. 造积压：`mem hook`（UserPromptSubmit ×2）→
     `mem distill --proposal --executor none --json`（建窗后进 backlog）；
  2. `mem maintain --catch-up --json` 第一次：恢复过期租约、最老优先重试，
     记下 `processed/ok/inserted`；
  3. 立刻再跑一次 `mem maintain --catch-up --json`：应为 `processed=0, inserted=0`
     （无重复）；
  4. `mem eval flow`：G10（幂等键唯一）、G13（无重复提交）必绿。
- **预期**：第二次零新增、零重复；窗口水位不回退。
- **留痕**：两次 `maintain --json` 输出对比；`candidates` 行数；G10/G13 detail。
- **判定人**：验收人。
- **自动化**：✅ kill -9 后租约接管、attempt 只 +1、候选补齐由 `make test-faults`
  F8 覆盖；◐ catch-up 连跑幂等可脚本化（本基线未单独立项演练，列为可脚本化项）。

### U6 无钩子 Harness

- **目的**：MCP 机会唤醒 / agent 代跑路径有产出（能力矩阵标注上限）。
- **前置**：不配 hook 的宿主（或 `MEM_NO_AUTOMATION=1` 关自动面）；MCP 客户端可连。
- **步骤**：
  1. `MEM_NO_AUTOMATION=1 mem stats` 确认总闸生效（hook 自动面关闭）；
  2. 经 MCP 工具（或手工 `mem maintain` / `mem distill --proposal`）驱动蒸馏与维护；
  3. `mem stats` / `mem review ls` 核对产出；
  4. 在能力矩阵（docs/design-capture.md）核对上限标注是否与实际一致。
- **预期**：无 hook 捕获面 → 无自动捕获（如实，不是故障）；手动/MCP 驱动下
  maintain/distill 正常推进、候选可裁决；总闸下 hook 不再拉起自动蒸馏。
- **留痕**：`stats`、`maintain --json`、MCP 工具返回、能力矩阵文档。
- **判定人**：验收人 + 能力矩阵 owner。
- **自动化**：✋ 需要真实 MCP 客户端配合，本环境无法全自动演练（**偏离**，见 §6）。

### U7 休眠唤醒

- **目的**：超期如实显示 + 追赶完成，水位不伪装已消费。
- **前置**：有退避中窗口的临时库（fail 窗 `not_before=+1m` 或 backlog
  `not_before=+10m`）。
- **步骤**：
  1. 造退避窗口（同 U5 第 1 步或 U4 第 2 步）；
  2. 跨过退避（等 1–10 分钟；真实休眠场景即睡眠唤醒后）；
  3. `mem maintain --json` → 窗口被接管、attempt 递增、候选补齐；
  4. `mem stats` → 「维护 最后成功」如实更新；`mem eval flow` → G5（cursor 连续）、
     G7（重试账）必绿。
- **预期**：水位不伪装已消费（失败窗不被新窗越过，S6b 头让位口径）；超期如实
  显示；追赶后全绿。
- **留痕**：`stats` 维护行（`maintain:last_ok`）；`evidence_bundles.attempt/not_before`；
  G5/G7 detail。
- **判定人**：验收人。
- **自动化**：✅ fake clock 下的退避/接管由单元与 `test/flow` 覆盖；✋ 真实机器
  睡眠唤醒需人做（本基线用退避等待等价演练，未真实休眠，见 §6）。

### U8 高影响抽样复核

- **目的**：global/rule 类约 10% 进复核摘要，其余不打扰。
- **前置**：库里有一批记忆，含 global / cross_project / decision / preference / rule
  类型（抽样口径：`hash(memory_id)%10==0` 确定性抽样，`internal/store/review.go`）。
- **步骤**：
  1. 造样本：`mem add --kind decision` 等多类型若干条（≥10 条才可能命中抽样）；
  2. `mem review ls --state summary --limit 5` → 看计数头是否不受 `--limit` 截断
     （B1 口径）；
  3. `mem review stats` 看按版本标定；
  4. 抽查 `memories` 表：命中抽样的 ID 逐一验证 `hash(id)%10==0`，未命中的
     global/rule 类确认未进 summary。
- **预期**：约 10% 高影响条目进 summary（确定性、可复算）；其余条目不打扰；
  四态计数与实际一致。
- **留痕**：`review ls` 四态计数头、`review stats` 输出、`memories.review_state`。
- **判定人**：验收人。
- **自动化**：✅ 抽样函数与四态投影由 cmd/mem 单测覆盖；✋ 「10% 比例落地按实际
  撤回率调整」的标定判断需人做（本基线临时库样本量 <10，未演练，**偏离**，见 §6）。

## 5. tag 就绪判据

打 tag（`make release`）前逐项确认：

1. **门禁全绿**（合入 main 后由主控复跑，不是只看本 worktree）：
   `make test`（fmt-check + watch 90/90 + test-flow + 全包）、`make cover-gate`、
   `make test-faults`、`make test-e2e` 四项退出码全 0。
2. **G1–G13 全 PASS**：`mem eval flow`（临时库逐跳动线）文本与 `--json` 均
   `passed 13 / failed 0`、exit=0（方法见 §3.1，逐条口径见 §3.1 G 表）。
3. **U 清单签发**：§4 U1–U8 逐条由判定人签字；偏离项（§4 各条「自动化」标注 +
   §6）要么补演练、要么在 tag 报告里显式声明豁免理由。
4. **准出率与严密度**：G12 准出率 100%、G13 严密度违例 0（与
   design-eval-flows §1 的核心度量对齐）。
5. **S12/S14 已合入并复跑基线**（见 §6 第一条）。

## 6. 已知遗留

1. **S12/S14 未合入**：本包基线 = `079c98d`，`dev/w5-s12-consolidate`（consolidate
   与命令面收编）与 `dev/w5-s14-perf`（存储分层/性能）并行开发中。两者合入 main 后
   **由主控复跑**：§2 四项门禁 + §3 eval flow 基线；`internal/store/schema.sql`、
   `store.go`、`coverage-policy.txt`、`Makefile` 被两者触碰，基线数字（覆盖率、
   耗时）允许变化，判据不变。
2. **transcript cursor 终态回收**：`mem gc --retention --dry-run` 输出
   「cursor 报数 N（删除 M）」。只删超过 30 天且 transcript 已不存在或已完全消费
   （`Size <= Offset`）的游标；仍有未读余量或不可读时只报不删
   （见 `cmd/mem/retention.go` 第 (5) 步注释）。
3. **U 类演练偏离**（2026-11-26 续跑已补做 U6/U8，详见 §7）：
   - U1/U4 的真实 key 失效→换 key 仍未演练（codebuddy CLI 实测可用，无法制造真实
     失效；以坏输出假 CLI 等价验证 invalid→fail→重试→ok）。
   - U7 真实机器睡眠唤醒仍未演练（用退避窗口 + `--force` 等价覆盖接管与追赶）。
   - 真实模型对自指验收文本连续返回合法空结果（noop），自动裁决的候选→记忆链路
     由假 CLI 全链演练（§7.3 U1）。
4. **F9 的 `maintain:last_fail_at` 未落地**（design-eval-flows §3.2.1 注）：失败
   可见性目前靠 `maintain --json` 与 R16 巡检，`maintain:last_fail_at` 键留待
   S11/后续补齐。
5. **人工介入率口径**：本基线 `manual_rate=1`（100%）是 P1 人工验收阶段的刻意
   口径（裁决全走 `mem accept`/`mem reject`）；无人值守目标 <1%（§1）依赖
   `--auto`/矩阵自动裁决链路，由 U1 的真实会话演练评估，不在本包 G 类自动账内。
6. **G 类无法覆盖的内容级断言**（design-eval-flows §3.2 末尾清单）：spool 落盘
   重放、32000 rune 截断与 scrub 内容级、双 executor 等价性、revision CAS 并发
   竞态——已分别由故障注入（F2/F6）与单元用例覆盖或显式留待，不在 G1–G13 口径内。
7. **真实环境新发现**（2026-11-26，详见 §7.5）：`memories.scope` 在采纳时未写入，
   global/cross_project 的 scope 抽样分支实际永不命中；G7「无失败样本」判红、
   G11/G13 对 legacy accepted 无证据强写未分桶——均为判据口径或历史数据问题。

## 7. 真实环境走查记录（2026-11-26 续跑）

> WorkBuddy 任务在 U5 发现真实缺陷后因额度耗尽中断（修复子代理未启动）。本轮在
> `.worktree/fix-run-ledger` 接续：先修缺陷（提交 `1306860`），再装到本机、迁移
> 生产库、复跑 U5 并补做 U1–U8。生产库操作前后均有 `.backup` 快照。

### 7.1 安装、宿主与 v3→v4 迁移

- `make install` 落 `~/go/bin/mem`；PATH 上的 `~/.local/bin/mem` 用「先删后拷」
  替换。前序已复现真实坑：旧文件残留 `com.apple.provenance`，`cp` 覆盖后签名失配、
  macOS 直接 SIGKILL（rc=137）。
- 宿主三处（WorkBuddy 4 钩子、WorkBuddy MCP、Trae 3 钩子）均为绝对路径指向
  `~/.local/bin/mem`，无 `MEM_*` 环境变量 → 换二进制即生效，零配置改动。
- 生产库 v3→v4：迁移前备份 31,531,008 字节；首次 `mem stats` 触发迁移，**0.031s**
  完成；279 条 `distill_runs` 行保留，旧唯一键消失，`ux_distill_window` /
  `ix_distill_bundle` 重建，`schema_version=4`。
- 前序失误留痕：SQLite `.backup` 里 `~` 不展开曾导致备份先失败、迁移已先跑
  （幂等加列，无数据风险）；本轮改用绝对路径并在迁移前确认备份成功。

### 7.2 U5 修复验证（生产库，真实数据）

- 修复前（WorkBuddy 实测）：`mem maintain --catch-up --max-bundles 5` 报
  「写 Proposal 运行账失败：started_at 冲突重试耗尽」；库内同 session
  `74/75/76` 连续秒且 `finished_at=started_at`，正是撞键后 +1s 顶出的产物。
- 修复后：一次追账处理 5 个 Bundle、耗时 0.066s，**5 条运行账同一 `started_at=
  1795659507` 全部落库**，时间戳未被篡改；紧接着第二次追账继续处理 5 个新窗口，
  逐 Bundle 运行账仍各 1 条（多运行账 Bundle = 0）。
- 门禁：红 case 旧代码第 4 条即报同一错误；修复后 `make test`、`make cover-gate`
  （store 82.0%，下限 82%）、`make test-faults`、`make test-e2e`、`perf` 全绿。
- 人的视角：夹具通常「一个测试一个窗口」，永远碰不到同秒第 4 条；只有真实库连续
  追账才暴露。这类缺陷是 U 类走查不可替代的价值。

### 7.3 U1–U8 逐条结果

| 条目 | 结果 | 真实 Case / 证据 | 人的视角与遗留 |
|:---|:---|:---|:---|
| U1 | ◐ | `codebuddy` CLI 真实链路可用（3 次均 `executor=cli:codebuddy@2.185.0+tpl2`，合法 noop、0 候选）；假 CLI 全链跑通 ok→候选→accept→search，临时库 `eval flow` **13/13**。真实钩子审计今日 session-start 10 / prompt-submit 13 / stop 11。 | 模型对自指验收文本连续判「无事实」，自动裁决链路需夹具等价；未重跑 `mem init`，避免改写宿主配置。 |
| U2 | ◐ | 3 个会话各 prompt+stop → 每会话 2 条观测；SessionStart 注入含「待人工 1 项（未审核最老 3 天）」+「本工程 1 条 pending 候选」。生产 `eval flow` 8/13。 | 触达面可用；红项见 §7.4，全部为历史数据，不是多会话动线回归。 |
| U3 | ✅ | `c63c5550` withdraw：检索 8→7 条；restore：7→8 条。动作账落 `commit_decisions(action=review_action)`，含 `before_state=confirmed/after_state=withdrawn`。`rollback --dry-run` matched 0（无版本归因样本）；`review stats` 无样本。 | restore 后回到「未审核」而非原 `confirmed`，会重新进待审队列——与「误拒回捞」定位一致，但验收文案「原路恢复」易误读。 |
| U4 | ✅（等价） | 真实 `codebuddy -y -p ...` 探测 rc=0（可用），无法制造真实 key 失效；坏输出假 CLI：invalid→bundle fail attempt=1→退避期内 maintain blocked→`--force` attempt=2→换好输出 attempt=3 ok 并补 1 条候选。 | 失败不冒充 noop、重试账齐全；真实网络故障仍未演练。 |
| U5 | ✅ | 见 §7.2；临时库连跑两次 `--catch-up` 均 `processed=0`，G10/G13 绿。 | 生产库「第二次仍有剩余源」是积压未清空，不是重复提交；幂等按逐 Bundle 运行账验证。 |
| U6 | ✅ | 真实 stdio JSON-RPC：`initialize`（v2mem 0.1.0）、`tools/list` 8 个工具、`review_ls` summary 计数=2 且 `limit=1` 不截断计数、`search` 命中、`maintain_wake` 返回 `waked=true`。 | 无钩子宿主走 MCP 可推进维护，但无自动捕获——与能力上限标注一致。 |
| U7 | ◐ | 退避窗内 maintain 返回 `backlog=1`「所有源的头都在退避或租约中」，未偷跑；`--force` 立即接管、attempt+1、旧 run 不被改写。 | 真实睡眠唤醒未演练；行为等价覆盖。 |
| U8 | ◐ | 40 条 decision/project 自动采纳（actor=model-auto、40 条 auto_commit）→ summary 命中 2 条，两条均满足 `sha256(id)[0]%10==0`；`--limit 1` 时 `counts.summary=2` 不截断。 | **新发现**：`memories.scope` 采纳时未写入（全库为空），global/cross_project 的 scope 抽样永不命中；kind 抽样正常。 |

### 7.4 生产库 eval flow 8/13 的解释

G1–G6、G8、G10 绿；G7/G9/G11/G12/G13 红，逐条归因：

- **G7**：无失败窗口样本被判红——判据把「缺样本」当失败，健康库会恒红；建议缺样本
  降为 INFO/PASS-with-note，而不是让人造一次失败。
- **G9**：2 条 `expired` 无裁决（10 月旧数据，旧过期处置路径）。
- **G11/G13**：6 条 `accepted` 无证据绑定（单轨切换前 unbundled 老候选），
  被同时计为「已采纳均无证据」「无证据强写 6」。
- **G12**：准出率 91%、悬空 6，即上述 unbundled 老候选，不是新窗口漏账。

结论：红项全部是历史数据/判据口径，不是本轮动线回归；新窗口动线在临时库 13/13 绿。
建议给 legacy accepted/expired 建显式豁免桶，避免把历史数据误报成当前故障。

### 7.5 待决与建议

1. 采纳时写入 `memories.scope`（U8 发现），否则 scope 抽样形同虚设。
2. G7 缺样本口径、G11/G13 legacy 豁免分桶（§7.4）。
3. U3 restore 文案与状态语义对齐（「回到未审核」而非「原路恢复」）。
4. 只拷 `mem.db` 不足以复现证据链：`content/` 必须一起搬，否则 maintain 会报
   「打开外置内容失败」（本轮 U2 副本已复现）；建议写进验收操作手册。

