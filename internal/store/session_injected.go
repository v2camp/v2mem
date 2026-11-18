package store

import "strings"

// 本文件解决「notes 去重把 cap 外的规则永久饿死」这一缺陷的数据面。
//
// 背景：读侧曾在每轮检索时把**全库** kind=rule 都当成「已由开场小字条覆盖」而去重。
// 但开场注入受条数上限（--limit，生产默认 3）与字符预算约束，实际只注入了其中一部分。
// 于是超过上限的那部分规则：开场不注入、每轮又被判 notes_duplicate 跳过 ——
// 在整个库的生命周期里一次都进不来上下文，而审计看起来只是「正常去重」。
//
// 判据必须落在「这个会话**实际**注入了哪些规则」上，而不是「库里有哪些规则」。
// read_traces / read_trace_items 正是这件事的既有记录（开场注入也写轨迹），
// 因此不再引入新表、也不新增写路径。

// openingGateReasons 是「开场类」事件的 gate_reason。
//
// 取值来源是写入侧（cmd/mem/hook.go 的 sessionStartContext），不是猜的：
// SessionStart 写 "session_start"，PreCompact 写 "pre_compact"。
// 这里刻意用白名单而非「不等于检索类」，新增开场类事件时必须同步登记，
// 否则它注入过的规则会被当成未注入、在每轮检索里重复注入（多注入是啰嗦，
// 但把「覆盖集合」算错成全集才是本文件要修的那个 bug）。
var openingGateReasons = []string{"session_start", "pre_compact"}

// SessionInjectedRuleIDs 返回某会话在开场类事件里实际注入过的 rule 记忆 id 集合。
//
// 口径三件套（缺一不可）：
//   - read_traces.session_id = 本会话：跨会话不共享，别的会话注入过不代表本会话注入过；
//   - gate_reason 属于开场类事件：prompt-submit 的注入**不能**反过来证明开场覆盖过；
//   - injection_rank > 0 + memories.kind = 'rule'：只算真正进了上下文的那几条，
//     被 limit/budget 丢弃的条目（injection_rank = 0）不算覆盖。
//
// sessionID 为空串时直接返回空集合与 nil：没有会话维度就谈不上「本会话注入过」。
// 调用方据此退化为不去重（失败开放）—— 空集合与查询失败在语义上是同一件事：
// 「无法证明它被覆盖过」。
func (s *Store) SessionInjectedRuleIDs(sessionID string) (map[string]bool, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return map[string]bool{}, nil
	}
	ph := make([]string, len(openingGateReasons))
	args := make([]any, 0, len(openingGateReasons)+1)
	args = append(args, sessionID)
	for i, r := range openingGateReasons {
		ph[i] = "?"
		args = append(args, r)
	}
	rows, err := s.db.Query(
		`SELECT DISTINCT m.id
		   FROM read_traces rt
		   JOIN read_trace_items rti ON rti.trace_id = rt.id
		   JOIN memories m ON m.id = rti.memory_id
		  WHERE rt.session_id = ?
		    AND rt.gate_reason IN (`+strings.Join(ph, ",")+`)
		    AND rti.injection_rank > 0
		    AND m.kind = 'rule'`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
