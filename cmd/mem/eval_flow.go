// mem eval flow：逐跳验收（docs/design-eval-flows.md §3.1 的 G 类检查）。
//
// 只读：打开库只为建表/stamp 与查询，不写任何业务表；有失败项时返回 error，
// main 统一映射为退出码 1（make test / eval-gate 据此拦截）。
package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/v2camp/v2mem/internal/eval"
	"github.com/v2camp/v2mem/internal/store"
)

func cmdEvalFlow(args []string) error {
	var c common
	fs := flag.NewFlagSet("eval flow", flag.ContinueOnError)
	c.register(fs)
	window := fs.Duration("window", 15*24*time.Hour,
		"悬空候选的处理 SLA（design-data-lifecycle §5，默认 15 天）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := store.Open(c.db)
	if err != nil {
		return err
	}
	defer st.Close()
	snap, err := eval.LoadFlowSnapshot(st.DB(), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("读取动线快照失败: %w", err)
	}
	rep := eval.CheckFlow(snap, *window)
	if c.json {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		for _, chk := range rep.Checks {
			status := "PASS"
			if !chk.Pass {
				status = "FAIL"
			}
			fmt.Printf("%s  %s  %s：%s\n", chk.ID, status, chk.Name, chk.Detail)
		}
		fmt.Printf("\n通过 %d/%d，人工介入率 %.1f%%\n",
			rep.Passed, len(rep.Checks), rep.ManualRate*100)
	}
	if !rep.OK() {
		return fmt.Errorf("eval flow: %d 项未通过", rep.Failed)
	}
	return nil
}
