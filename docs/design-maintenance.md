# v2mem R&D · 收尾运维三件套

> 本文是 `docs/DESIGN.md` 的专题切片，供按需阅读；各主题全文目录与关联集中在 [DESIGN 索引页](./DESIGN.md)。

## 17. 收尾运维三件套（2026-09-17 完成，TDD ＋ 覆盖率门禁）

反思「必要功能缺口」后补齐的三项收尾能力，均有测试与门禁覆盖。

### 17.1 `mem uninstall`：一键摘除钩子（init 的逆操作）

`init` 只负责装、没有卸，卸载只能手删配置文件里的条目——不幂等、易误删。
实现对称地放在 `cmd/mem/uninstall.go`：

- **摘除范围与 init 对称**（`uninstallTargets` 复用 `resolveConfigTarget` 的选择规则）：
  hooks 配置（去掉带 `# v2mem` 标记的 hook，组内清空则丢弃整组、事件清空则删键）与
  指令块（`instrBegin..instrEnd`）。`--file` / `--scope global|project|instruction` / `--project`
  均支持，`--all` 一次处理全部。
- **纪律与 init 同源**：只动 v2mem 标记条目、用户自己的 hook 与无关键全保留；解析不了
  的配置报错中止；目标不存在报告「未找到」不新建；`--dry-run` 预览。
- `main.go` 子命令清单 / usage / switch 三处同步登记。

### 17.2 `gc` / `consolidate` 前自动备份库快照

这两个是破坏性操作（删行 / 置 superseded），原先没有任何回退手段。现在：
- `store.Backup` 用 **`VACUUM INTO`** 生成独立快照——库是 WAL 模式，直接 copy `.db`
  会漏掉 `-wal` 未落盘提交，VACUUM INTO 由 SQLite 在事务内产出单文件一致快照。
- `backupBeforeMutating`：库不存在或空库跳过；默认写到 `<库目录>/backup/mem-<时间戳>.db`；
  `--backup-dir` 改址、`--no-backup` 关闭。备份行在 `--json` 下抑制，避免污染机器输出。

### 17.3 审计日志自动轮转

`audit.jsonl` 会随使用无限增长。现在超过 `audit.MaxArchiveBytes`（默认 5MB）后，
下一次 `Append` 前自动轮转：当前日志归档为 `.1`，旧档依次后移，超出 `MaxArchives`（3 份）
的删除并重建空当前日志，维持「当前日志恒存在」不变量。轮转失败被吞掉——审计写失败
绝不能影响注入链路。

### 17.4 覆盖率

| 范围 | 用例 |
|:---|:---|
| store 备份/计数 | 空库 Count、VACUUM INTO 快照可独立打开、空路径报错 |
| cmd 备份开关 | gc/consolidate 前生成快照、空库/缺失不备份、`--no-backup`、`--backup-dir` |
| audit 轮转 | 低阈值不转、超限归档并移位剪枝、`Append` 自动轮转、归档目标被阻塞报错、父路径是文件报错 |
| uninstall | 移除 hooks、保留用户 hook 与无关键、幂等、dry-run、指令块移除、全局路径留在测试 HOME、未知 harness 报错 |

门禁：内审 87.0% ≥ 86%、store 83.2% ≥ 82%、cmd/mem 82.7% ≥ 81%，全绿。
