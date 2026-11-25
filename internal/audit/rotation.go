package audit

// S13：审计轮转可观测与历史合并查询（§8.2「补 .1–.3 合并查询；轮转失败可观测」）。

import (
	"fmt"
	"os"
)

// warnRotate 是轮转失败的告警出口。默认往 stderr 写明确告警（不占宿主读的
// stdout），测试可替换以捕获调用。轮转失败不能静默：否则 .1–.3 会悄悄停更，
// 历史合并查询读到过期归档也无人知晓。审计/轮转失败仍不影响记忆注入，错误
// 只用于可观测、不向上传播到宿主会话。
var warnRotate = func(err error) {
	fmt.Fprintf(os.Stderr, "警告：审计日志轮转失败（%v）\n", err)
}

// ReadHistory 合并当前日志与 .1–.MaxArchives 归档，按写入先后（.N → .1 → 当前）
// 返回；limit>0 时只返回最后 limit 条。
//
// 只查当前文件会在轮转后丢掉整段历史：mem audit / 用量视图的统计会断点。
// 逐档复用 Read 的单文件口径（坏行跳过、空文件返回 nil），不另写一套解析。
func ReadHistory(path string, limit int) ([]Record, error) {
	if path == "" {
		path = Path()
	}
	var all []Record
	// 最旧 .N 先读、当前文件最后读，拼接结果天然是写入时间正序。
	for i := MaxArchives; i >= 1; i-- {
		rs, err := Read(fmt.Sprintf("%s.%d", path, i), 0)
		if err != nil {
			return nil, err
		}
		all = append(all, rs...)
	}
	cur, err := Read(path, 0)
	if err != nil {
		return nil, err
	}
	all = append(all, cur...)
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}
