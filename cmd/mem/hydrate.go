package main

// 外置 summary 的读侧回填。ref 为空是 legacy 行，保留库内值；ref 非空则按行级 hash 读回。
// 文件 IO 只在这里和 evidence.Engine 里发生，store 不反向依赖 evidence/cmd。

import "github.com/v2camp/v2mem/internal/store"

func hydrateHits(dbPath string, hits []store.Hit) error {
	root := store.SummaryContentRoot(dbPath)
	for i := range hits {
		h := &hits[i]
		if h.SummaryRef == "" {
			continue
		}
		line, err := store.ReadSummaryLine(root, h.SummaryRef, h.SummaryHash)
		if err != nil {
			return err
		}
		h.Content = line.Summary
	}
	return nil
}

func hydrateObservations(dbPath string, obs []store.Observation) error {
	root := store.SummaryContentRoot(dbPath)
	for i := range obs {
		o := &obs[i]
		if o.SummaryRef == "" {
			continue
		}
		line, err := store.ReadSummaryLine(root, o.SummaryRef, o.SummaryHash)
		if err != nil {
			return err
		}
		o.Summary = line.Summary
	}
	return nil
}
