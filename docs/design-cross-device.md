# v2mem R&D · 跨设备归集（M4 / M4.1）

> 本文是 `docs/DESIGN.md` 的专题切片，供按需阅读；各主题全文目录与关联集中在 [DESIGN 索引页](./DESIGN.md)。

## 7. 跨设备归集

```
设备 A: mem export ~/.v2mem/sync/A-<ts>.jsonl
        （同步该文件是安全的：纯文本、行式、可读）
设备 B: mem import ~/.v2mem/sync/A-<ts>.jsonl
```

**禁止**：同步 `mem.db` 本体（写入中途同步会损坏库）。

### 7.1 身份键 = `(content_hash, project)`，不是 `id`

`id` 由 `newID()` 随机生成，两台设备独立写下同一事实必然得到不同 id；而唯一索引 `ux_mem_hash`（`content_hash, project`）只允许一条。**按 id 归并必然漏合**，导入还可能撞唯一索引。因此：

- 导入时忽略远端 `id`，只按 `(content_hash, project)` 定位
- 本地不存在 → 插入并生成**本地** id，`origin_device` 记远端设备（溯源）
- 同一事实落在不同工程 → 视为两条独立记忆，不互相归并

### 7.2 字段级归并规则

全部规则选定为**幂等且可交换**，因此「重复导入」与「双向导入」都收敛。

| 字段 | 规则 | 理由 |
|:---|:---|:---|
| `kind` | 取字典序较小者 | 双方都非空时的确定性让步 |
| `salience` | 取较大者 | 更重要的判断占优 |
| `access_count` | 取较大者 | **不是求和**——求和会随重复导入无限增长，破坏幂等 |
| `created_at` | 取最早者 | 最早的创建时间最接近真相 |
| `updated_at` / `last_seen_at` | 取较晚者 | 最新变动 / 最新命中 |
| `expires_at` | 任一边为 NULL（永不过期）则结果为 NULL，否则取较晚者 | 同步不得**提前**销毁记忆 |
| `tags` | 并集（一键多值全部保留） | 标记是累积的 |
| `origin_device` / `origin_tool` | 仅新插入时写入；已有记录保留本地值 | 本地视角溯源，见 7.3 |
| `superseded_by` | **不在第一遍归并中处理**，由第二遍按身份键重建，见 7.5 | 防悬挂引用 |

一条**刻意的例外**（本地书写形式优先，非可交换，已在文档中固化）：

- `content`：hash 相同即视为同一事实（归一化已折叠大小写与空白），保留本地写法，不重算 `content_idx`

### 7.3 一条已知边界（有意为之，非遗漏）

**`origin_device` 是「首次写入本库的设备」，不随归并收敛。**
共享事实在 A 库记 `mini`、在 B 库记 `laptop`，两边都对——这是本地视角字段。
收敛判据因此排除 `id` 与 `origin_device`，只对内容相关字段断言一致。

### 7.4 导出可复现

`Export()` 按 `(content_hash, project)` 排序，标记按 `(key, value)` 排序。
否则每次导出的 JSONL 都会无谓 diff，跨设备比对失去意义。

### 7.5 取代关系的跨设备重建（M4.1）

**问题**：`superseded_by` 存的是本地随机 id。直接搬过去就是**悬挂引用**；完全不搬，
其他设备就不知道取代关系，会重新看到两条重复 —— 实测在 A 端归并、B 端独立写下同一事实后，
两侧取代关系不一致，**M4 承诺的归集在 M5a 加入后失效**。

**解法**：把取代关系在传输层表达成**身份键**，导入端再解析回本地 id。

```jsonc
// 导出记录新增两个字段
"superseded_by_id": "<本地 id>",                        // 仅供排查，不作为依据
"superseded": { "hash": "<存活者 content_hash>", "project": "<存活者 project>" }
```

导出端用 `LEFT JOIN memories t ON t.id = m.superseded_by` 把本地 id 翻译成身份键。

导入端**必须分两遍**：

1. 第一遍做常规字段归并，并记下「本批记录的身份键 → 本地 id」
2. 第二遍解析取代引用：优先查本批索引，其次回落到库中已有记录（存活者可能不在本文件内）

分两遍的必要性：导出按 `content_hash` 排序，被取代者可能排在存活者**之前**，
一遍扫描会解析不到目标（已用倒序文件固化该断言）。

**解析失败时**（目标既不在文件里也不在本地）：留空并计入 `ImportStats.Unresolved`，
在 CLI 输出里显式报告。宁可缺失也不写悬挂 id。自引用同样被拒并计入 `Unresolved`。

**收敛判据因此收紧为两项**：内容投影一致 **且** 取代关系（按身份键展开）一致。
本地 id 天然不同，不参与比较。

**仍未实现**：`edges` 表的跨设备重映射。当前无写入路径（相似归并不建边），故无实际影响；
一旦 M5b 或后续引入边关系，需按同样方式处理。

### 11.9 M4 已完成能力（跨设备归集）

- `mem export [路径.jsonl]`：全部记忆导出为 JSONL（省略路径则写标准输出）；给路径时把摘要打到标准输出
- `mem import <路径.jsonl>`：按 `(content_hash, project)` 归并，报告 `新增/合并/跳过`
- 归并规则与边界见 §7.2 / §7.3

**测试**：共 **39 个用例**（`go test ./... -v` 实测；M3 时点为 23，M4 新增 16）：

| 范围 | 用例 |
|:---|:---|
| 导出 | 每条记忆一行且必带 `content_hash` / `tags` / `origin_device`；两次导出完全一致（可复现）；省略路径走标准输出 |
| 导入 · 基础 | 新记录插入并保留远端 `origin_device`；按 hash+project 归并而非按 id 新插；同事实跨工程保持独立 |
| 导入 · 幂等 | 重复导入不改变状态、不产生重复行；`access_count` 取 max 不随重复导入增长；CLI 二次导入报告 `新增=0 合并=1` |
| 导入 · 字段规则 | 标记取并集；`created_at` 取最早、`last_seen_at` 取较晚；`expires_at` NULL 优先（永不过期）与取较晚者两路 |
| 导入 · 收敛 | 双向导入后两侧内容投影完全一致；`origin_device` 保持本地视角（固化 7.3 边界） |

**变异测试（验证断言真有咬合力）**：把 `access_count = MAX(...)` 改成求和后重跑。

- `TestImportTakesMaxAccessCountNotSum` 报警（`got 31`）✅
- `TestImportIsIdempotent` **首次未报警** ❌ —— 该用例两侧计数均为 0，求和与取 max 结果相同，属巧合性通过。
  已修：给两侧预设非零 `access_count` 与 `last_seen_at`，再变异时该用例正确报警。
  **教训**：幂等类断言必须让被测字段取非零值，否则「恒等」会掩盖实现错误。

**e2e 双设备仿真**（A=mini / B=laptop，各 2 条含 1 条共享事实）：

| 判据 | 结果 |
|:---|:---|
| 交叉导入后两侧内容投影一致（排除 `id` / `origin_device`） | 3 条 vs 3 条，完全收敛 ✅ |
| 共享事实字段归并：`salience`、`tags`、`kind` | `0.9`、`{machine:[laptop,mini]}`、`fact` ✅（取 max / 并集） |
| 重复导入后导出逐字节不变 | ✅ |
| 连续 5 次重复导入后状态稳定 | ✅ |
| `origin_device` 本地视角（A 见 `mini`、B 见 `laptop`） | 符合 7.3 设计 ✅ |

> 度量教训（第二次）：首次 e2e 把「导入**前**的导出」与「导入**后**的导出」做字节 diff，误报为幂等失败 —— 对照组本身就不该相等。**幂等必须用「同一状态下的前后对照」，不能拿跨越状态变更的两个快照比。**

### 11.11 M4.1 验收时发现的缺口与修复

M5a 落地后重跑 M4 的收敛判据，出现 `A 7 条 / B 7 条 -> !! 未收敛`。
根因不是 M4 的实现错误，而是 §7.3 里「记录在案」的那条限制**已被 M5a 激活**：
M4 时代没有 `superseded_by`，故限制无影响；M5a 一旦产生取代关系，归集承诺就破了。

**教训**：把限制「记录在案」不等于它安全。**引入新机制时必须回头复跑依赖它的旧判据** ——
这里正是靠重跑 M4 的收敛判据才发现。仅看 M5a 自己的测试全绿，会漏掉这个跨模块回归。

修复见 §7.5，新增 6 个用例：

| 用例 | 断言 |
|:---|:---|
| `TestExportCarriesSupersessionAsIdentityKey` | 导出携带身份键引用而非裸 id |
| `TestImportRebuildsSupersessionWithLocalIDs` | 导入解析成本地 id，且**不照搬远端 id**、目标在本地确实存在 |
| `TestImportResolvesSupersessionRegardlessOfFileOrder` | 倒序文件也能正确重建（验证「分两遍」的必要性） |
| `TestImportReportsUnresolvedSupersession` | 目标缺失时报 `Unresolved` 且不留悬挂 id |
| `TestImportKeepsAlreadySupersededRecordSuperseded` | 导入自身导出不改变取代关系，检索仍隐去被取代者 |
| `TestCrossDeviceConvergenceWithSupersession` | 一端归并 + 另一端独立写同一事实，交叉导入后内容投影与取代关系**双双收敛** |

**变异测试**：跳过第二遍（`if true { continue }`）后被 4 个用例捕获（`got ""` / `Unresolved:0`）。

**另修一处自查发现的缺陷**：`identity` 索引最初用原始 `r.ContentHash`，
而插入时若文件缺 `content_hash` 会用 `HashOf(content)` 兜底 —— 两者不一致会让索引建出对不上的键。
当时靠 DB 回落掩盖了问题，改用归一化后的 `h` 作键。

**最终 e2e**（A 端归并 1 条 → B 端独立写同一事实 → 交叉导入）：

| 判据 | 结果 |
|:---|:---|
| 内容投影 | A 4 条 / B 4 条，一致 ✅ |
| 取代关系（按身份键） | 两侧均为 `6162dd51/v2mem -> ee30b73c/v2mem` ✅ |
| 检索可见性 | 两侧 `总4 / 可见3 / 已归并1`，检索「隐藏目录」各 1 条 ✅ |
| 幂等 | 再各导入一次 `新增=0`，取代关系不变 ✅ |

