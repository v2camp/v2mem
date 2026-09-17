package store

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// SynonymDict 是同义词展开词典。设计见 docs/design-synonym-library.md §2。
//
// 只做「别名 → 标准词」的单向映射：等号左边是标准词（归一目标），右边是候选别名。
// 查询期把命中查询的别名对应的标准词补进查询，不动索引、不改 content_hash。
// 作用域分全局段与 [project] 段，工程词仅在该工程生效。
type SynonymDict struct {
	global    map[string]string            // alias -> standard
	byProject map[string]map[string]string // project -> alias -> standard
}

// SynonymPath 返回默认词库路径：与库同目录 <dbdir>/synonyms.txt。
// 环境变量 MEM_SYNONYMS 可覆盖；MEM_SYN_DICT=0 可禁用。
func SynonymPath(dbPath string) (path string, enabled bool) {
	path = filepath.Join(filepath.Dir(dbPath), "synonyms.txt")
	if p := os.Getenv("MEM_SYNONYMS"); p != "" {
		path = p
	}
	return path, os.Getenv("MEM_SYN_DICT") != "0"
}

// ParseSynonymFile 解析词库文件。格式：
//
//	# 注释；! 开头 = 显式停用某行；[project] = 工程作用域段；a = b, c 为「标准词=别名」。
//
// 坏行跳过不致命（词库是辅助配置，一行坏数据不该让检索崩）。
func ParseSynonymFile(path string) (*SynonymDict, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	d := &SynonymDict{global: map[string]string{}, byProject: map[string]map[string]string{}}
	var cur *map[string]string = &d.global
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(line[1 : len(line)-1])
			if _, ok := d.byProject[name]; !ok {
				d.byProject[name] = map[string]string{}
			}
			m := d.byProject[name]
			cur = &m
			continue
		}
		disabled := strings.HasPrefix(line, "!")
		if disabled {
			line = strings.TrimSpace(line[1:])
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		std := strings.TrimSpace(line[:eq])
		if std == "" || disabled {
			continue
		}
		aliases := strings.Split(line[eq+1:], ",")
		for _, a := range aliases {
			if a = strings.TrimSpace(a); a != "" && a != std {
				(*cur)[a] = std
			}
		}
	}
	return d, sc.Err()
}

// LoadSynonym 打开（若启用且存在文件）并返回词典；文件缺失返回 nil。
func LoadSynonym(dbPath string) (*SynonymDict, error) {
	path, enabled := SynonymPath(dbPath)
	if !enabled {
		return nil, nil
	}
	return ParseSynonymFile(path)
}

// reloadSynonyms 把 <dbdir>/synonyms.txt 载入 Store；出错时保持旧词典、返回错。
func (s *Store) reloadSynonyms() error {
	syn, err := LoadSynonym(s.path)
	if err == nil {
		s.syn = syn
	}
	return err
}

// ExpandQuery 返回对查询做同义词展开后的结果（词典 nil 时原样返回）。
// 供 Search 内部使用，也供命令行的 --explain 展示诊断。
func (s *Store) ExpandQuery(q, project string) string {
	if s.syn == nil {
		return q
	}
	return s.syn.Expand(q, project)
}

// Expand 返回加入标准词后的查询串。命中查询子串的别名会追加其标准词，
// 「保留原词 + 补标准词」，经 queryExpr 后两者以 OR 参与（召回优先）。
// 无命中别名或词典为 nil 时原样返回。
func (d *SynonymDict) Expand(q, project string) string {
	if d == nil || strings.TrimSpace(q) == "" || len(d.global)+len(d.byProject) == 0 {
		return q
	}
	var sb strings.Builder
	sb.WriteString(q)
	added := false
	add := func(a, std string) {
		if std == "" || a == "" || a == std {
			return
		}
		if strings.Contains(q, a) {
			sb.WriteByte(' ')
			sb.WriteString(std)
			added = true
		}
	}
	for a, std := range d.global {
		add(a, std)
	}
	if p := d.byProject[project]; p != nil {
		for a, std := range p {
			add(a, std)
		}
	}
	if !added {
		return q
	}
	return sb.String()
}

// Count 返回活跃别名总数（含全局与各工程段），供诊断与测试断言。
func (d *SynonymDict) Count() int {
	if d == nil {
		return 0
	}
	n := len(d.global)
	for _, m := range d.byProject {
		n += len(m)
	}
	return n
}