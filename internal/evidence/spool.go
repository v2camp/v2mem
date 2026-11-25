package evidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/v2camp/v2mem/internal/store"
)

// spool 契约：
//
//	<db-dir>/spool/<session>/seg-<ts>-<rand>.jsonl
//
// 每条失败事件独占一个 segment，写入走「同目录临时文件 → fsync → 原子 rename」。
// 文件永远只追加/整段删除，禁止「读全文件再原地重写」：
// drain 用原子 rename 抢占为 *.processing，成功删除，瞬时失败 rename 回去（恢复原状），
// 逐行流式读取，已处理行不落回文件 —— 重放由 event_uid / (source,gen,seq) 幂等兜住。
//
// S6c 出口：坏行与确定性坏段不再随文件删除，而是落到
// <db-dir>/spool/dead-letter/（保留原文可人工修复）；dead-letter 不是会话目录，
// 也不匹配 seg-*.jsonl 口径，因此不进入 R7 / S4 的 spool 段计数。

// errSpoolBudget 是内部信号：本次 drain 的预算用完了，未处理的行必须保留。
var errSpoolBudget = errors.New("spool drain 预算已用尽")

const (
	spoolDeadLetterDir = "dead-letter"
	spoolDrainCursor   = ".drain-cursor"
)

// SpoolDrainResult 是一次 drain 的计数。
type SpoolDrainResult struct {
	Files        int
	Drained      int
	Dropped      int
	Restored     int
	DeadLettered int
}

// writeSpool 把一个事件落成独立 spool segment，返回文件路径。
func (e *Engine) writeSpool(ev RawEvent) (string, error) {
	return e.writeSpoolBatch([]RawEvent{ev})
}

// writeSpoolBatch 把一批事件写进同一个多行 spool segment（只做一次 fsync）。
// drainOneSpool 逐行读取并逐条幂等回放，整批失败后不会丢数据。
func (e *Engine) writeSpoolBatch(evs []RawEvent) (string, error) {
	if len(evs) == 0 {
		return "", errors.New("spool 批次不能为空")
	}
	dir := filepath.Join(e.spoolRoot, spoolSession(evs[0].SessionID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, "seg-*.tmp")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	for _, ev := range evs {
		body, err := json.Marshal(recordOf(ev))
		if err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", err
		}
		body = append(body, '\n')
		if _, err := tmp.Write(body); err != nil {
			tmp.Close()
			os.Remove(tmpName)
			return "", err
		}
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
// 预算耗尽或瞬时故障（库关闭、磁盘/权限）恢复 segment 并把错误返回给调用方；
// 确定性坏段（序号洞/代际不符/约束冲突）整段移入 dead-letter 后继续处理后续段
// 与后续会话目录，不再成为「每次第一个失败点」而永久挡住后面的数据。
//
// 公平：<spoolRoot>/.drain-cursor 记录上一轮最后处理的会话目录名，下一轮从它
// 之后开始并回绕。预算只够一轮时，多个会话也能轮流被处理。
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
	var sessions []string
	for _, d := range dirs {
		if !d.IsDir() || d.Name() == spoolDeadLetterDir {
			continue
		}
		sessions = append(sessions, d.Name())
	}
	left := max
	var firstErr error
	start := drainStartIndex(sessions, readDrainCursor(e.spoolRoot))
	for i := 0; i < len(sessions); i++ {
		name := sessions[(start+i)%len(sessions)]
		dir := filepath.Join(e.spoolRoot, name)
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
			n, dropped, derr := e.drainOneSpool(claimed, name, &left)
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
			case isPermanentSpoolError(derr):
				if err := moveToDeadLetter(e.spoolRoot, name, claimed); err != nil {
					restoreSpool(claimed)
					return res, err
				}
				res.DeadLettered++
			default:
				restoreSpool(claimed)
				res.Restored++
				if firstErr == nil {
					firstErr = derr
				}
			}
		}
		// 无论本轮是否在目录内耗尽预算，都记下「最后处理到的会话」：
		// 下一轮从它的后继开始，让被 restore 的段等一轮而不是饿死其它会话。
		writeDrainCursor(e.spoolRoot, name)
		if firstErr != nil {
			return res, firstErr
		}
		if left <= 0 {
			return res, nil
		}
	}
	return res, firstErr
}

// readDrainCursor 读上一轮的会话名；缺失/损坏一律从头开始（游标只是公平提示，
// 不是正确性水位，坏了不能挡住 drain）。
func readDrainCursor(root string) string {
	b, err := os.ReadFile(filepath.Join(root, spoolDrainCursor))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func writeDrainCursor(root, session string) {
	// 写失败只影响下一轮的起点，不影响本轮已完成的补写；静默忽略。
	_ = os.WriteFile(filepath.Join(root, spoolDrainCursor), []byte(session+"\n"), 0o600)
}

// drainStartIndex 返回游标后继的下标；游标不存在或已被删除时从 0 开始（回绕）。
func drainStartIndex(sessions []string, cursor string) int {
	if cursor == "" || len(sessions) == 0 {
		return 0
	}
	for i, name := range sessions {
		if name == cursor {
			return (i + 1) % len(sessions)
		}
	}
	return 0
}

func (e *Engine) drainOneSpool(path, session string, left *int) (drained, dropped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	segName := strings.TrimSuffix(filepath.Base(path), ".processing")
	var badFile *os.File
	defer func() {
		if badFile != nil {
			_ = badFile.Close()
		}
	}()

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
			if badFile == nil {
				badFile, err = openDeadLetterLines(path, session, segName)
				if err != nil {
					return drained, dropped, err
				}
			}
			if err := appendDeadLetterLine(badFile, "bad_json", line); err != nil {
				return drained, dropped, err
			}
			dropped++
			continue
		}
		ev := rec.event()
		if ev.EventUID == "" || ev.SourceID == "" {
			if badFile == nil {
				badFile, err = openDeadLetterLines(path, session, segName)
				if err != nil {
					return drained, dropped, err
				}
			}
			if err := appendDeadLetterLine(badFile, "missing_identity", line); err != nil {
				return drained, dropped, err
			}
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

// isPermanentSpoolError 判定「重试也不会成功」的段级错误。
//
// 序号洞/代际不符是账本约束；SQLite 的约束冲突在 modernc 驱动里没有可
// errors.Is 的导出码，只能按稳定文案识别。库关闭、磁盘、权限等一律按瞬时故障
// 处理（restore + 返回），等下一轮重试。
func isPermanentSpoolError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, store.ErrEvidenceSeqGap) || errors.Is(err, store.ErrEvidenceGenerationMismatch) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "constraint failed") || strings.Contains(msg, "UNIQUE constraint")
}

// deadLetterLinesPath 是坏行的追加目标：<session>-<seg>-badlines.jsonl。
// 后缀刻意不是 .jsonl 的段命名（seg- 前缀），不会被 drain 或 R7 计数。
func deadLetterLinesPath(root, session, segName string) string {
	base := strings.TrimSuffix(segName, ".jsonl")
	return filepath.Join(root, spoolDeadLetterDir, session+"-"+base+"-badlines.jsonl")
}

func openDeadLetterLines(claimed, session, segName string) (*os.File, error) {
	root := filepath.Dir(filepath.Dir(claimed))
	dir := filepath.Join(root, spoolDeadLetterDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(deadLetterLinesPath(root, session, segName),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// appendDeadLetterLine 一行坏数据写成一条 JSON 信封：reason 供程序分类，
// raw 原样保留原始行，人工修复后可直接放回 spool。
func appendDeadLetterLine(f *os.File, reason string, raw []byte) error {
	line, err := json.Marshal(struct {
		Reason string `json:"reason"`
		Raw    string `json:"raw"`
	}{Reason: reason, Raw: string(raw)})
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// moveToDeadLetter 把整段移到 dead-letter：<session>-<seg>.bad 保留段名。
func moveToDeadLetter(root, session, claimed string) error {
	dir := filepath.Join(root, spoolDeadLetterDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	segName := strings.TrimSuffix(filepath.Base(claimed), ".processing")
	return os.Rename(claimed, filepath.Join(dir, session+"-"+segName+".bad"))
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
