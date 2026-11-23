package main

// 积压触发是自动链路里最容易失控的一处：判定跑多了就烧钱，跑少了积压还在。
// 所以两条判据（阈值、冷却）都单独测，且默认必须是关的。

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/v2camp/v2mem/internal/distill"
	"github.com/v2camp/v2mem/internal/harness"
	"github.com/v2camp/v2mem/internal/store"
)

func TestShouldSpawnJudge(t *testing.T) {
	const day = 24 * time.Hour
	now := int64(1_700_000_000)
	cases := []struct {
		name    string
		pending int
		last    int64
		want    bool
	}{
		{"低于阈值", 19, 0, false},
		{"刚到阈值且从未跑过", 20, 0, true},
		{"超阈值但在冷却内", 50, now - 3600, false},
		{"超阈值且过冷却", 50, now - int64(day.Seconds()), true},
		{"阈值内即便过了冷却也不发", 5, now - int64(day.Seconds())*7, false},
	}
	for _, c := range cases {
		if got := shouldSpawnJudge(c.pending, c.last, now, 20, day); got != c.want {
			t.Errorf("%s: shouldSpawnJudge(%d, %d) = %v, want %v", c.name, c.pending, c.last, got, c.want)
		}
	}
}

func TestJudgeStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "judge-state.json")
	if got := readJudgeState(p); got != 0 {
		t.Fatalf("缺失文件应返回 0，got %d", got)
	}
	if err := writeJudgeState(p, 12345); err != nil {
		t.Fatalf("writeJudgeState: %v", err)
	}
	if got := readJudgeState(p); got != 12345 {
		t.Fatalf("应读回 12345，got %d", got)
	}
}

func seedPending(t *testing.T, st *store.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := st.InsertCandidate(store.CandidateInput{
			SessionID: "s-trigger", Project: "v2mem", Kind: "decision",
			Content: "触发器测试候选 " + time.Now().Format("150405.000000000") + string(rune('a'+i%26)),
			Source:  "distill",
		}); err != nil {
			t.Fatalf("InsertCandidate: %v", err)
		}
	}
}

// 默认必须是关的：升级后自己开始烧 API 是最坏的一种「惊喜」。
func TestMaybeSpawnJudgeOffByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEM_JUDGE_SPAWN", "")
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedPending(t, st, 25)
	st.Close()

	called := 0
	startJudgeProc = func(string, int, bool) error { called++; return nil }
	t.Cleanup(func() { startJudgeProc = defaultStartJudge })

	maybeSpawnJudge(db, harness.EventStop)
	if called != 0 {
		t.Fatalf("未开 MEM_JUDGE_SPAWN 时不应拉起判定，called=%d", called)
	}
}

// 开了之后：够阈值发一次，冷却内不发第二次，且默认不 --apply。
func TestMaybeSpawnJudgeFiresOncePerCooldown(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEM_JUDGE_SPAWN", "1")
	t.Setenv("MEM_JUDGE_MIN_PENDING", "20")
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedPending(t, st, 21)
	st.Close()

	var calls int
	var gotLimit int
	var gotApply bool
	startJudgeProc = func(_ string, limit int, apply bool) error {
		calls++
		gotLimit, gotApply = limit, apply
		return nil
	}
	t.Cleanup(func() { startJudgeProc = defaultStartJudge })

	maybeSpawnJudge(db, harness.EventStop)
	maybeSpawnJudge(db, harness.EventStop) // 冷却内第二次
	if calls != 1 {
		t.Fatalf("冷却内应只发一次，calls=%d", calls)
	}
	if gotLimit != 20 || gotApply {
		t.Errorf("应以阈值条数、影子模式拉起，got limit=%d apply=%v", gotLimit, gotApply)
	}
}

// 读路径事件不得触发：判定走网络，绝不能挂在 PromptSubmit 上。
func TestMaybeSpawnJudgeSkipsReadEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MEM_JUDGE_SPAWN", "1")
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedPending(t, st, 25)
	st.Close()

	called := 0
	startJudgeProc = func(string, int, bool) error { called++; return nil }
	t.Cleanup(func() { startJudgeProc = defaultStartJudge })

	maybeSpawnJudge(db, harness.EventPromptSubmit)
	maybeSpawnJudge(db, harness.EventSessionStart)
	if called != 0 {
		t.Fatalf("读路径事件不应触发判定，called=%d", called)
	}
}

// 用临时 HOME 写一个开了 auto_spawn 的 model.json —— 钩子读的是文件，不是用户的 shell。
func setupAutoSpawnHome(t *testing.T, cfg distill.ModelConfig) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, ".v2mem", "model.json")
	if err := distill.SaveModelConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	return p
}

func seedPendingAndSpy(t *testing.T) (*int, string) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	seedPending(t, st, 25)
	st.Close()

	called := new(int)
	startJudgeProc = func(string, int, bool) error { *called++; return nil }
	t.Cleanup(func() { startJudgeProc = defaultStartJudge })
	return called, db
}

// setup 配好模型后写进 model.json 的 auto_spawn，应让钩子在没有环境变量时也拉起判定。
func TestMaybeSpawnJudgeFromModelConfig(t *testing.T) {
	setupAutoSpawnHome(t, distill.ModelConfig{Mode: "cli", CLI: "codebuddy", ModelTag: "codebuddy@1+tpl2", AutoSpawn: true})
	t.Setenv("MEM_JUDGE_SPAWN", "")
	called, db := seedPendingAndSpy(t)

	maybeSpawnJudge(db, harness.EventStop)
	if *called != 1 {
		t.Fatalf("model.json 开了 auto_spawn 应拉起一次，called=%d", *called)
	}
}

// 显式 0 是总闸：即使配置文件开了也要能一键关掉（GUI 宿主没法 export，这是主要关闭方式之一）。
func TestMaybeSpawnJudgeEnvZeroVetoesConfig(t *testing.T) {
	setupAutoSpawnHome(t, distill.ModelConfig{Mode: "cli", CLI: "codebuddy", ModelTag: "codebuddy@1+tpl2", AutoSpawn: true})
	t.Setenv("MEM_JUDGE_SPAWN", "0")
	called, db := seedPendingAndSpy(t)

	maybeSpawnJudge(db, harness.EventStop)
	if *called != 0 {
		t.Fatalf("MEM_JUDGE_SPAWN=0 应压过配置里的 auto_spawn，called=%d", *called)
	}
}

// mode=off 时即使残留 auto_spawn=true 也不得拉起：关掉模型来源就该停止一切外发。
func TestMaybeSpawnJudgeOffModeVetoesConfig(t *testing.T) {
	setupAutoSpawnHome(t, distill.ModelConfig{Mode: "off", AutoSpawn: true})
	t.Setenv("MEM_JUDGE_SPAWN", "")
	called, db := seedPendingAndSpy(t)

	maybeSpawnJudge(db, harness.EventStop)
	if *called != 0 {
		t.Fatalf("mode=off 不应拉起判定，called=%d", *called)
	}
}

// 配置里的 auto_spawn 不得成为读路径的额外 I/O：读路径在读文件之前就返回。
func TestMaybeSpawnJudgeConfigDoesNotTouchReadPath(t *testing.T) {
	p := setupAutoSpawnHome(t, distill.ModelConfig{Mode: "cli", CLI: "codebuddy", ModelTag: "codebuddy@1+tpl2", AutoSpawn: true})
	t.Setenv("MEM_JUDGE_SPAWN", "")
	called, db := seedPendingAndSpy(t)

	// 删掉配置后读路径仍必须零触发（它根本不该去读这个文件）。
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	maybeSpawnJudge(db, harness.EventPromptSubmit)
	if *called != 0 {
		t.Fatalf("读路径不应拉起判定，called=%d", *called)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("读路径不应尝试创建/读取 model.json，stat err=%v", err)
	}
}

// 钩子拉起时仍必须是影子模式：auto_spawn 只开「跑」，不开「改库」。
func TestAutoSpawnKeepsShadowDefault(t *testing.T) {
	setupAutoSpawnHome(t, distill.ModelConfig{Mode: "cli", CLI: "codebuddy", ModelTag: "codebuddy@1+tpl2", AutoSpawn: true})
	t.Setenv("MEM_JUDGE_SPAWN", "")
	t.Setenv("MEM_JUDGE_APPLY", "")
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	seedPending(t, st, 25)
	st.Close()

	var called int
	var gotApply bool
	startJudgeProc = func(_ string, _ int, apply bool) error { called++; gotApply = apply; return nil }
	t.Cleanup(func() { startJudgeProc = defaultStartJudge })

	maybeSpawnJudge(db, harness.EventStop)
	if called != 1 {
		t.Fatalf("应拉起一次，called=%d", called)
	}
	if gotApply {
		t.Error("auto_spawn 只开「跑」，不开「改库」：apply 必须保持 false")
	}
}
