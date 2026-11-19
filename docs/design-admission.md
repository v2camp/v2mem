# 准入判定（`mem judge`）

> 目标：把「候选积压」从人工按钮变成自动判定，同时不把未标定的推测写进主库。
> 判据权威：[`VALUE.md`](./VALUE.md) §4「梦的产出不回写主库」及其例外三条。
> 相关：[`design-write-governance.md`](./design-write-governance.md)（三态与矩阵）、[`ops-watch.md`](./ops-watch.md) §3 R8。

## 1. 为什么是判定，不是抽取

候选积压的成因不是「判断不了」，是「没人点按钮」：本机实测 74 条候选里 22 条被拒、51 条挂着，
`rationale` **全空**。抽取（蒸馏）已经能自动产出候选，缺的是最后一步的取舍。

因此判定器的职责被收窄成一句：**只回答「这条要不要」，不回答「这条该怎么写」**。
后者是蒸馏的活，混进来会让两层判据互相遮蔽。

## 2. 权限边界：模型不能比矩阵更宽

`internal/commit/policy.go` 的矩阵已经定义了「哪些类必须人工」。判定器沿用同一组判据，
不新造一套：

| 判据 | 模型可自动 accept | 依据 |
|:---|:---|:---|
| `kind ∈ {fact, task}` | 可以 | 矩阵对这些类判 `auto_commit` |
| `kind ∈ {pitfall, preference, decision}` | 不可以，降级 `unsure` | 矩阵判 `human_accept` |
| `scope ∈ {global, cross_project}` | 不可以，降级 `unsure` | 全局知识错了污染面最大 |
| `project` 为空 | 不可以，降级 `unsure` | 无归属即全局 |
| `reject` | 任何类都可以 | 否决只是筛掉噪音，内容仍留在候选表可回捞 |

**模型永远不会独占地「要」一条东西；它只能「不要」，或「问你」。** 这是 C 方案在
「更自动化」与「推测不回写」之间的切分点：判错的代价偏向可回收的一侧。

## 3. 影子期（默认，不可跳过）

```
mem judge                 # 只写 verdict，status 不动，memories 零写入
mem judge --stats         # 人工一致率：verdict=accept 看人工也采纳的比例，reject 看人工也否决的比例
mem judge --apply         # 真落库：accept 走 CommitAccept，reject 走 CommitReject
```

- 影子期数据落 `candidates.judge_verdict` / `judge_model`（含 prompt 版本），`rationale` 记理由码。
- 一致率的**分母是人工裁决**，所以影子期内不能用 `--apply`，否则分母被判定器自己污染。
- 转正门槛：`accept` 一致率 ≥95%，且样本 ≥200 条。未达标不放开，不因着急下调。

## 4. 理由码是语料，不是日志

`reason` 是闭集（`durable` / `process_narrative` / `transient_state` / `payload_residue` /
`no_generalizable`）。自由文本无法聚合，也挡不住提示词漂移。
它的第二个用途是**规则沉淀的输入**：同一理由码出现频次足够高时，才谈把它降级成正则/结构判据。

## 5. 数据与观测

| 项 | 位置 |
|:---|:---|
| 判定结果 | `candidates.judge_verdict` / `judge_model` / `rationale` |
| 落库记忆 | 走 `CommitAccept`，来源仍是 `distill-accept`（可 JOIN `judge_model` 归因） |
| 审计 | `event=candidate-judge`：`provider` / `count` / `mode`（影子/apply） |
| 巡检 | 影子期 `--stats` 的输出；R8 的 pending 计数在 `--apply` 后才下降 |

**R8 的口径不变**：`--apply` 后 pending 会降，但知识只是从候选区移到主库，由 R6（库规模）承接。
不把 R8 变好当成「问题解决」。

## 6. 未做（按顺序，且都有前置条件）

1. **规则沉淀**：需要 ≥50 条带理由码的裁决。现在 0 条，先攒语料。产出形态拟为
   `~/.v2mem/admission.jsonl` + dry-run 计数，转正看误杀率（分母来自抽样复核，不是 dry-run 自己）。
2. **回滚命令**：影子期不落库，故当前无不可逆风险。`--apply` 转正**必须**与
   `mem judge --rollback <judge_model>` 同批交付——按 §4 例外条款 (c)，单独放行 `--apply` 视为违约。
3. **R8 增列**：影子准确率进 detail，需同步改 `docs/ops-watch.md` 与脚本两处。

## 7. 已知限制

- `auto` 在本机解析为 `none`（无 ollama、无 OPENAI key），判定器会明确报「不可用」而不是
  「判定 0 条」——后者会被误读成「候选都干净」。
- 一次调用判一批（默认 20 条），批量大会稀释注意力；超长候选正文截断到 600 rune。
- 模型看不到全库，判不了「这条是否与已有记忆重复」；去重仍由 `ux_cand_hash` 与采纳时的
  overwrite 判定负责，判定器不参与。
