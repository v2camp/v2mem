package evidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// spool 契约：
//
//	<db-dir>/spool/<session>/seg-<ts>-<rand>.jsonl
//
// 每条失败事件独占一个 segment，写入走「同目录临时文件 → fsync → 原子 rename」。
// 文件永远只追加/整段删除，禁止「读全文件再原地重写」：
// drain 用原子 rename 抢占为 *.processing，成功删除，失败 rename 回去（恢复原状），
// 逐行流式读取，已处理行不落回文件 —— 重放由 event_uid / (source,gen,seq) 幂等兜住。

// errSpoolBudget 是内部信号：本次 drain 的预算用完了，未处理的行必须保留。
var errSpoolBudget = errors.New("spool drain 预算已用尽")

// SpoolDrainResult 是一次 drain 的计数。
type SpoolDrainResult struct {
	Files    int
	Drained  int
	Dropped  int
	Restored int
}

// writeSpool 把一个事件落成独立 spool segment，返回文件路径。
func (e *Engine) writeSpool(ev RawEvent) (string, error) {
	dir := filepath.Join(e.spoolRoot, spoolSession(ev.SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	body, err := json.Marshal(recordOf(ev))
	if err != nil {
		return "", err
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(dir, "seg-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	// spool 的全部意义是宿主崩溃后还能补写，必须 fsync 而不是只写页缓存。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	final := strings.TrimSuffix(tmpName, ".tmp") + ".jsonl"
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return "", err
	}
	if err := syncDir(dir); err != nil {
		return "", err
	}
	return final, nil
}

func spoolSession(session string) string {
	name := strings.TrimSpace(session)
	if name == "" {
		return "unknown"
	}
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/', r == '\\', r == ':', r == 0:
			return '_'
		case r < 0x20:
			return '_'
		default:
			return r
		}
	}, name)
	name = strings.Trim(name, ".")
	if name == "" {
		return "unknown"
	}
	if len(name) > 100 {
		name = name[:100]
	}
	return name
}

func isSpoolSegment(name string) bool {
	if !strings.HasPrefix(name, "seg-") {
		return false
	}
	return strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".jsonl.processing")
}

// DrainSpool 补写 spool 里的事件，最多处理 max 条。
//
// 任一条 capture 失败就恢复整个 segment（已成功的行会重放，幂等保证不重复），
// 并把错误返回给调用方；其余文件保持原样，下次继续。
func (e *Engine) DrainSpool(max int) (SpoolDrainResult, error) {
	if max <= 0 {
		max = 200
	}
	var res SpoolDrainResult
	dirs, err := os.ReadDir(e.spoolRoot)
	if errors.Is(err, os.ErrNotExist) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	left := max
	var firstErr error
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(e.spoolRoot, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			return res, err
		}
		for _, f := range files {
			if left <= 0 {
				return res, firstErr
			}
			if f.IsDir() || !isSpoolSegment(f.Name()) {
				continue
			}
			path := filepath.Join(dir, f.Name())
			claimed := path
			if !strings.HasSuffix(f.Name(), ".processing") {
				claimed = path + ".processing"
				if err := os.Rename(path, claimed); err != nil {
					continue // 另一个 drain 已认领
				}
			}
			n, dropped, derr := e.drainOneSpool(claimed, &left)
			res.Drained += n
			res.Dropped += dropped
			switch {
			case derr == nil:
				if err := os.Remove(claimed); err != nil {
					return res, err
				}
				res.Files++
			case errors.Is(derr, errSpoolBudget):
				restoreSpool(claimed)
				res.Restored++
			default:
				restoreSpool(claimed)
				res.Restored++
				if firstErr == nil {
					firstErr = derr
				}
			}
		}
		if firstErr != nil {
			return res, firstErr
		}
	}
	return res, firstErr
}

func (e *Engine) drainOneSpool(path string, left *int) (drained, dropped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		if *left <= 0 {
			return drained, dropped, errSpoolBudget
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec segmentRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			dropped++
			continue
		}
		ev := rec.event()
		if ev.EventUID == "" || ev.SourceID == "" {
			dropped++
			continue
		}
		if _, err := e.capture(ev, false); err != nil {
			return drained, dropped, err
		}
		drained++
		*left--
	}
	if err := sc.Err(); err != nil {
		return drained, dropped, err
	}
	return drained, dropped, nil
}

// restoreSpool 把认领文件 rename 回原名；原名被占用时保留 .processing，
// 下次 drain 会把它当作已认领文件继续处理。
func restoreSpool(claimed string) {
	target := strings.TrimSuffix(claimed, ".processing")
	if target == claimed {
		return
	}
	if _, err := os.Stat(target); err == nil {
		return
	}
	_ = os.Rename(claimed, target)
}
