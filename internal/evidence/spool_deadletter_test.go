package evidence

// S6c：spool 坏件不再「随段删除」——坏行进 dead-letter 保留原文，确定性坏的
// 整段移入 dead-letter 后继续处理后续段/后续会话；预算与瞬时故障仍 restore。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deadLetterDir(root string) string { return filepath.Join(root, "dead-letter") }

func TestSpoolBadLineKeptInDeadLetterAndGoodLineDrained(t *testing.T) {
	e, _ := newTestEngine(t)
	dir := filepath.Join(e.SpoolRoot(), "sess-1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ev := rawEvent("uid-good")
	ev.Generation, ev.Seq = 1, 1
	good, _ := json.Marshal(recordOf(ev))
	original := "这不是 JSON\n"
	identity := `{"event_uid":"","source_id":""}` + "\n"
	seg := filepath.Join(dir, "seg-bad.jsonl")
	body := append([]byte(original), []byte(identity)...)
	body = append(body, append(good, '\n')...)
	if err := os.WriteFile(seg, body, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := e.DrainSpool(10)
	if err != nil {
		t.Fatalf("坏行不应让整轮 drain 失败: %v", err)
	}
	if res.Drained != 1 || res.Dropped != 2 || res.Files != 1 {
		t.Fatalf("好行入库、两类坏行都计数: %+v", res)
	}
	if _, err := os.Stat(seg); !os.IsNotExist(err) {
		t.Fatalf("drain 成功后原段应删除: %v", err)
	}
	badFile := filepath.Join(deadLetterDir(e.SpoolRoot()), "sess-1-seg-bad-badlines.jsonl")
	body, err = os.ReadFile(badFile)
	if err != nil {
		t.Fatalf("坏行应保留在 dead-letter: %v", err)
	}
	if !strings.Contains(string(body), "这不是 JSON") || !strings.Contains(string(body), `"reason"`) {
		t.Fatalf("dead-letter 必须同时保留原文与原因: %s", body)
	}
	for _, reason := range []string{"bad_json", "missing_identity"} {
		if !strings.Contains(string(body), reason) {
			t.Fatalf("dead-letter 应标注原因 %q: %s", reason, body)
		}
	}
	// 好行确实入库。
	if _, ok, err := e.st.LookupEvidence("uid-good", "src-a", 0, 0); err != nil || !ok {
		t.Fatalf("同段好行应入库: ok=%v err=%v", ok, err)
	}
	// dead-letter 目录不是会话目录：下一轮不应把它当段重复处理。
	again, err := e.DrainSpool(10)
	if err != nil || again.Drained != 0 || again.Files != 0 {
		t.Fatalf("dead-letter 不应被当作正常段: %+v err=%v", again, err)
	}
}

func TestSpoolPermanentSegmentFailureDeadLettersAndContinues(t *testing.T) {
	e, _ := newTestEngine(t)
	// sess-a：seq=5 对空水位是确定性序号洞，重试永远不会成功。
	bad := rawEvent("uid-gap")
	bad.SessionID = "sess-a"
	bad.Generation, bad.Seq = 1, 5
	badPath, err := e.writeSpool(bad)
	if err != nil {
		t.Fatal(err)
	}
	// sess-b：健康段，按字典序排在坏段之后，必须被同一轮处理。
	good := rawEvent("uid-ok")
	good.SessionID = "sess-b"
	if _, err := e.writeSpool(good); err != nil {
		t.Fatal(err)
	}

	res, err := e.DrainSpool(10)
	if err != nil {
		t.Fatalf("确定性坏段不应 restore-block 整轮: %v", err)
	}
	if res.DeadLettered != 1 || res.Drained != 1 || res.Files != 1 || res.Restored != 0 {
		t.Fatalf("坏段移出、健康段继续: %+v", res)
	}
	if _, err := os.Stat(badPath); !os.IsNotExist(err) {
		t.Fatalf("坏段原文件应被移走: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(deadLetterDir(e.SpoolRoot()), "sess-a-*.bad"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("坏段应保留段名 + .bad: %v err=%v", matches, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil || !strings.Contains(string(body), "uid-gap") {
		t.Fatalf("坏段原文必须可人工修复: %s err=%v", body, err)
	}
	if _, ok, err := e.st.LookupEvidence("uid-ok", "src-a", 0, 0); err != nil || !ok {
		t.Fatalf("后续会话段应入库: ok=%v err=%v", ok, err)
	}
	// 再跑一轮：坏段已在 dead-letter，不应被重复计数。
	again, err := e.DrainSpool(10)
	if err != nil || again.DeadLettered != 0 || again.Drained != 0 {
		t.Fatalf("dead-letter 段不应重复处理: %+v err=%v", again, err)
	}

	// 代际不符同属确定性失败，也必须进 dead-letter 而不是无限重试。
	mismatch := rawEvent("uid-gen")
	mismatch.SessionID = "sess-c"
	mismatch.Generation, mismatch.Seq = 2, 1
	if _, err := e.writeSpool(mismatch); err != nil {
		t.Fatal(err)
	}
	res, err = e.DrainSpool(10)
	if err != nil || res.DeadLettered != 1 {
		t.Fatalf("代际不符也应 dead-letter: %+v err=%v", res, err)
	}
}

func TestSpoolDrainCursorCorruptFallsBackToFirstSession(t *testing.T) {
	e, _ := newTestEngine(t)
	a := rawEvent("uid-a")
	a.SessionID = "sess-a"
	if _, err := e.writeSpool(a); err != nil {
		t.Fatal(err)
	}
	cursorFile := filepath.Join(e.SpoolRoot(), ".drain-cursor")
	if err := os.WriteFile(cursorFile, []byte("gone-session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := e.DrainSpool(10)
	if err != nil || res.Drained != 1 {
		t.Fatalf("游标指向已删目录时应从头开始: %+v err=%v", res, err)
	}
	if body, err := os.ReadFile(cursorFile); err != nil || strings.TrimSpace(string(body)) != "sess-a" {
		t.Fatalf("本轮结束后应刷新游标: %q err=%v", body, err)
	}
}

func TestSpoolDrainRotatesSessionsWhenBudgetExhausted(t *testing.T) {
	e, _ := newTestEngine(t)
	a := rawEvent("uid-a")
	a.SessionID = "sess-a"
	b := rawEvent("uid-b")
	b.SessionID = "sess-b"
	if _, err := e.writeSpool(a); err != nil {
		t.Fatal(err)
	}
	if _, err := e.writeSpool(b); err != nil {
		t.Fatal(err)
	}

	res, err := e.DrainSpool(1)
	if err != nil || res.Drained != 1 || res.Files != 1 {
		t.Fatalf("第一轮应该只处理一个会话: %+v err=%v", res, err)
	}
	cursorFile := filepath.Join(e.SpoolRoot(), ".drain-cursor")
	body, err := os.ReadFile(cursorFile)
	if err != nil || strings.TrimSpace(string(body)) != "sess-a" {
		t.Fatalf("第一轮后应记下 sess-a: %q err=%v", body, err)
	}
	if _, ok, _ := e.st.LookupEvidence("uid-a", "src-a", 0, 0); !ok {
		t.Fatal("第一轮应轮到 sess-a（字典序）")
	}
	if _, ok, _ := e.st.LookupEvidence("uid-b", "src-a", 0, 0); ok {
		t.Fatal("预算耗尽时 sess-b 应留到下一轮")
	}

	// 第二轮从 sess-a 之后开始，轮到 sess-b——两个会话都不会饿死。
	res, err = e.DrainSpool(1)
	if err != nil || res.Drained != 1 || res.Files != 1 {
		t.Fatalf("第二轮应轮到下一个会话: %+v err=%v", res, err)
	}
	if body, err := os.ReadFile(cursorFile); err != nil || strings.TrimSpace(string(body)) != "sess-b" {
		t.Fatalf("第二轮后应记下 sess-b: %q err=%v", body, err)
	}
	if _, ok, _ := e.st.LookupEvidence("uid-b", "src-a", 0, 0); !ok {
		t.Fatal("第二轮应处理 sess-b")
	}

	// 第三轮：新会话从尾回绕到第一个目录。
	c := rawEvent("uid-c")
	c.SessionID = "sess-a"
	if _, err := e.writeSpool(c); err != nil {
		t.Fatal(err)
	}
	res, err = e.DrainSpool(1)
	if err != nil || res.Drained != 1 || res.Files != 1 {
		t.Fatalf("回绕后应重新轮到 sess-a: %+v err=%v", res, err)
	}
	if _, ok, _ := e.st.LookupEvidence("uid-c", "src-a", 0, 0); !ok {
		t.Fatal("回绕应把新段补上")
	}
}
