// Package audit 记录每一次钩子激活的检索行为，为「用户会话 → mem 查询」提供真实样本。
//
// 为什么必须有埋点：评测检索质量需要**真实 query**。凭记忆构造的样本只能测出
// 「索引能不能用」，测不出「用户会怎么问、系统会不会答对」。审计日志把每次
// 真实会话的查询与命中固化下来，才可能做事后标注与回归。
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Record 是一次钩子激活的记录。
type Record struct {
	TS      int64    `json:"ts"`
	Event   string   `json:"event"` // session-start | prompt-submit | manual
	Harness string   `json:"harness,omitempty"`
	Project string   `json:"project,omitempty"`
	Query   string   `json:"query,omitempty"`  // prompt-submit 才有
	Hashes  []string `json:"hashes,omitempty"` // 实际注入的记忆
	Kinds   []string `json:"kinds,omitempty"`
	Empty   bool     `json:"empty"` // 注入内容为空
	MS      int64    `json:"ms"`
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
// 返回错误是为了可测；**调用方必须忽略它** —— 这条路径在宿主会话的关键链上，
// 审计写失败绝不能影响记忆注入，更不能让会话收到错误。
func Append(path string, r Record) error {
	if path == "" {
		path = Path()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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

// Read 读最近 limit 条（limit<=0 表示全部）。坏行跳过而不是整体失败 ——
// 一行坏数据不该让整个审计不可读。
func Read(path string, limit int) ([]Record, error) {
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

// Summary 是审计日志的汇总。
type Summary struct {
	Total     int            `json:"total"`
	Empty     int            `json:"empty"`
	EmptyRate float64        `json:"empty_rate"` // 注入为空的次数占比
	AvgMS     float64        `json:"avg_ms"`
	ByEvent   map[string]int `json:"by_event"`
	ByHarness map[string]int `json:"by_harness"`
	WithQuery int            `json:"with_query"` // 可用于评测的样本数
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
	}
	if s.Total > 0 {
		s.EmptyRate = float64(s.Empty) / float64(s.Total)
		s.AvgMS = float64(msTotal) / float64(s.Total)
	}
	return s
}
