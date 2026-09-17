// Package audit 记录每一次钩子激活的检索行为，为「用户会话 → mem 查询」提供真实样本。
//
// 为什么必须有埋点：评测检索质量需要**真实 query**。凭记忆构造的样本只能测出
// 「索引能不能用」，测不出「用户会怎么问、系统会不会答对」。审计日志把每次
// 真实会话的查询与命中固化下来，才可能做事后标注与回归。
package audit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxArchiveBytes 是触发自动轮转的日志体量上限。审计日志会随使用无限增长，
// 若不设防最终会把 ~/.v2mem 撑爆；达到上限后，下一次追加前把当前日志归档。
var MaxArchiveBytes int64 = 5 << 20 // 5MB

// MaxArchives 是保留的归档份数（audit.jsonl.1 .. .N），更早的自动删除。
const MaxArchives = 3

// Record 是一次钩子激活的记录。
type Record struct {
	TS      int64  `json:"ts"`
	Event   string `json:"event"` // session-start | prompt-submit | manual-search | manual-add
	Harness string `json:"harness,omitempty"`
	Project string `json:"project,omitempty"`
	// SessionID 由钩子传入（stdin 的 session_id）。有了它才能按「本次会话」切分，
	// 而不只按时间窗粗略估计。
	SessionID string   `json:"session_id,omitempty"`
	Query     string   `json:"query,omitempty"`  // 读侧才有（prompt-submit 的用户提问）
	Hashes    []string `json:"hashes,omitempty"` // 读侧＝实际注入的记忆 id；写侧＝写入的 content_hash
	Kinds     []string `json:"kinds,omitempty"`
	Empty     bool     `json:"empty"` // 注入内容为空
	// Mode 仅写侧使用：create=新增，overwrite=命中「相同知识覆盖」。
	// 用它可算「模型重复记录率」——去重是否在起作用。
	Mode string `json:"mode,omitempty"`
	// Suppressed=true 表示这次触发被幂等去重抑制了（同一事件被多处配置并行触发）。
	// 它本身是度量：能看出「重复触发」在真实环境里是否真的发生。
	Suppressed bool  `json:"suppressed,omitempty"`
	MS         int64 `json:"ms"`
	// Source 仅写侧使用：标记写入来源（human|llm|ingest|harness-summary|bench|test）。
	// 用量视图默认排除 bench/test，避免程序化灌库污染统计。
	Source string `json:"source,omitempty"`
}

// Path 返回默认审计日志路径（与库同目录，便于一起备份/清理）。
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "audit.jsonl"
	}
	return filepath.Join(home, ".v2mem", "audit.jsonl")
}

// Append 追加一条记录。
//
// 在写之前做体量轮转（超过 MaxArchiveBytes 即归档当前日志），避免文件无限膨胀。
// 轮转失败会被吞掉：审计写失败绝不能影响记忆注入或宿主会话。
//
// 返回错误是为了可测；**调用方必须忽略它** —— 这条路径在宿主会话的关键链上，
// 审计写失败绝不能影响记忆注入，更不能让会话收到错误。
func Append(path string, r Record) error {
	if path == "" {
		path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = MaybeRotate(path, MaxArchiveBytes)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// MaybeRotate 若日志超过 maxBytes 则归档为 .1，并把既有归档依次后移、
// 删除超过 MaxArchives 份的旧档。未超限则不做任何事。文件不存在返回 nil。
func MaybeRotate(path string, maxBytes int64) error {
	if path == "" {
		path = Path()
	}
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Size() <= maxBytes {
		return nil
	}
	return rotate(path)
}

// rotate 把当前日志归档为 .1，其余归档依次后移，删除超出保留数的旧档。
func rotate(path string) error {
	// 先删最旧，腾出 .N 位置，避免覆盖
	oldest := fmt.Sprintf("%s.%d", path, MaxArchives)
	_ = os.Remove(oldest)
	// 从旧往新移位（先 .2→.3，再 .1→.2，最后 .0→.1）
	for i := MaxArchives - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, fmt.Sprintf("%s.%d", path, i+1)); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	// 重建空当前日志，维持「当前日志恒存在」的不变量
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// Read 读最近 limit 条（limit<=0 表示全部）。坏行跳过而不是整体失败 ——
// 一行坏数据不该让整个审计不可读。
func Read(path string, limit int) ([]Record, error) {
	// 与 Append 保持对称：空路径取默认位置。
	// 此前不兜底 ⇒ os.Open("") 报 ENOENT ⇒ 被 isNotExist 分支吞成「空日志」，
	// 调用方会把「没读到」误当成「没有数据」（实测：mem report 因此报出
	// 「未使用记忆库」这个错误结论）。
	if path == "" {
		path = Path()
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var all []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			continue // 坏行跳过
		}
		all = append(all, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

// Exists 判断审计日志是否已存在。
//
// 用途：把「读不到日志」与「日志里没有匹配记录」分开 ——
// 前者是环境/路径问题，后者是「真的没用过」。两者混为一谈会给出错误结论。
func Exists(path string) bool {
	if path == "" {
		path = Path()
	}
	_, err := os.Stat(path)
	return err == nil
}

// Summary 是审计日志的汇总。
type Summary struct {
	Total     int            `json:"total"`
	Empty     int            `json:"empty"`
	EmptyRate float64        `json:"empty_rate"` // 注入为空的次数占比
	AvgMS     float64        `json:"avg_ms"`
	ByEvent   map[string]int `json:"by_event"`
	ByHarness map[string]int `json:"by_harness"`
	WithQuery int            `json:"with_query"` // 可用于检索评测的样本数（读侧，带 query）

	// 读/写两侧分别计数 —— 回答「这套东西到底用起来了没有」要看这两个数，
	// 而不是看总次数：只有写侧增长说明只记不查，只有读侧增长说明只查不记。
	Retrievals int `json:"retrievals"` // 读侧：session-start / prompt-submit / manual-search
	Suppressed int `json:"suppressed"` // 被幂等去重抑制的重复触发次数（>0 说明配置存在重复）
	Writes     int `json:"writes"`     // 写侧：manual-add / manual-forget
	Creates    int `json:"creates"`    // 其中新增
	Overwrites int `json:"overwrites"` // 其中命中「相同知识覆盖」→ 可算重复记录率
}

// isWriteEvent 判定事件属于写侧。写侧没有 query，「空注入率」不适用于它。
func isWriteEvent(e string) bool {
	return e == "manual-add" || e == "manual-forget"
}

// Summarize 汇总。WithQuery 是「能拿来评测的样本数」——
// 这是判断「现在能不能做检索评测」的关键数字。
func Summarize(rs []Record) Summary {
	s := Summary{ByEvent: map[string]int{}, ByHarness: map[string]int{}}
	var msTotal int64
	for _, r := range rs {
		s.Total++
		if r.Empty {
			s.Empty++
		}
		msTotal += r.MS
		s.ByEvent[r.Event]++
		if r.Harness != "" {
			s.ByHarness[r.Harness]++
		}
		if strings.TrimSpace(r.Query) != "" {
			s.WithQuery++
		}
		if r.Suppressed {
			s.Suppressed++
			// 被抑制的触发没有注入任何内容，不计入读侧活动
			continue
		}
		if isWriteEvent(r.Event) {
			s.Writes++
			switch r.Mode {
			case "create":
				s.Creates++
			case "overwrite":
				s.Overwrites++
			}
		} else {
			s.Retrievals++
		}
	}
	if s.Total > 0 {
		s.EmptyRate = float64(s.Empty) / float64(s.Total)
		s.AvgMS = float64(msTotal) / float64(s.Total)
	}
	return s
}

// AsyncAppender 是异步写的审计通道。
//
// 为什么存在：同步 Append 在每次调用时打开文件 + 轮转检查 + 写盘，虽然便宜，
// 但在批量灌库 / 高频钩子场景仍会把磁盘 IO 摊到调用链上。异步化把写入交给
// 后台 goroutine，调用方只投递记录就返回 —— 审计只是观测，不该拖慢宿主。
//
// 通道有界：生产端写满即drop（宁可丢审计也不阻塞调用方），与「审计失败静默」
// 的总原则一致。Close 会排空队列再退出，保证进程退出前不留脏数据。
type AsyncAppender struct {
	path string
	ch   chan Record
	done chan struct{}
}

// NewAsyncAppender 启动一条后台写协程，pending 是通道容量。
func NewAsyncAppender(path string, pending int) *AsyncAppender {
	if pending < 1 {
		pending = 64
	}
	a := &AsyncAppender{
		path: path,
		ch:   make(chan Record, pending),
		done: make(chan struct{}),
	}
	go a.run()
	return a
}

func (a *AsyncAppender) run() {
	defer close(a.done)
	for r := range a.ch {
		_ = Append(a.path, r) // 写失败静默：观测从不超过功能
	}
}

// Append 投递一条记录，非阻塞。队列满时丢弃并返回 false。
func (a *AsyncAppender) Append(r Record) bool {
	select {
	case a.ch <- r:
		return true
	default:
		return false
	}
}

// Close 停发并等待后台写协程排空队列后退出。
func (a *AsyncAppender) Close() {
	close(a.ch)
	<-a.done
}
