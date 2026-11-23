package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/v2camp/v2mem/internal/distill"
)

// setupCLIStub 是「本机已登录 CLI」的假桩：--version 报一个版本，
// 其余调用从 stdin 读 prompt，按内容分别回 judge 的 verdict 数组与蒸馏的事实。
const setupCLIStub = `#!/bin/sh
case "$1" in --version) echo "1.2.3"; exit 0 ;; esac
in=$(/bin/cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *) echo '{"kind":"preference","content":"默认短句","salience":0.6,"obs_ids":["setup"]}' ;;
esac
`

// writeFakeCLI 在临时目录写一个名为 claude 的可执行脚本，返回该目录。
func writeFakeCLI(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(claude): %v", err)
	}
	return dir
}

// setupHome 把 HOME 指向临时目录，返回 ~/.v2mem/model.json 的路径。
func setupHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return filepath.Join(home, ".v2mem", "model.json")
}

func TestSetupWritesCLIAndNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in --version) echo "1.2.3"; exit 0 ;; esac
in=$(/bin/cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *) echo '{"kind":"preference","content":"默认短句","salience":0.6,"obs_ids":["setup"]}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := captureStdout(t, func() error { return cmdSetup(nil) })
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(home, ".v2mem", "model.json")
	c, err := distill.LoadModelConfig(p)
	if err != nil || c.Mode != "cli" || c.CLI != "claude" {
		t.Fatalf("config=%+v err=%v", c, err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
	// 成功写盘必须当场告知外发范围与回退命令，不能只在文档里写。
	for _, want := range []string{"会发往", "mem setup --undo"} {
		if !strings.Contains(out, want) {
			t.Errorf("告知应包含 %q，got:\n%s", want, out)
		}
	}
}

func TestSetupNoticeMentionsScope(t *testing.T) {
	notice := setupNotice("claude", "claude@1+tpl1")
	for _, want := range []string{"会发往", "mem setup --undo"} {
		if !strings.Contains(notice, want) {
			t.Errorf("setupNotice 应包含 %q，got:\n%s", want, notice)
		}
	}
}

// 全失败：不写盘，但不是错误（可选步骤不阻塞安装），退出码 0。
func TestSetupAllCandidatesFailLeavesNoConfig(t *testing.T) {
	p := setupHome(t)
	dir := writeFakeCLI(t, "#!/bin/sh\nexit 1\n")
	t.Setenv("PATH", dir)

	if err := cmdSetup(nil); err != nil {
		t.Fatalf("全失败不应返回错误，got %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("dry-run 未通过时不得写盘，got err=%v", err)
	}
}

// dry-run 只验证：通过也不落盘。
func TestSetupDryRunDoesNotWrite(t *testing.T) {
	p := setupHome(t)
	dir := writeFakeCLI(t, setupCLIStub)
	t.Setenv("PATH", dir)

	out, err := captureStdout(t, func() error { return cmdSetup([]string{"--dry-run"}) })
	if err != nil {
		t.Fatalf("cmdSetup --dry-run: %v", err)
	}
	if !strings.Contains(out, "dry-run") {
		t.Errorf("dry-run 应报告验证结果，got:\n%s", out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("--dry-run 不得写盘，got err=%v", err)
	}
}

// --status 只读本地文件，不得探测或调用 CLI（不联网、不花钱）。
func TestSetupStatusDoesNotProbe(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"mode":"cli","cli":"claude","model_tag":"claude@1+tpl1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "invoked")
	dir := writeFakeCLI(t, fmt.Sprintf("#!/bin/sh\n/bin/echo x >> %s\nexit 0\n", marker))
	t.Setenv("PATH", dir)

	out, err := captureStdout(t, func() error { return cmdSetup([]string{"--status"}) })
	if err != nil {
		t.Fatalf("cmdSetup --status: %v", err)
	}
	for _, want := range []string{"cli", "claude", "claude@1+tpl1", p, "0600"} {
		if !strings.Contains(out, want) {
			t.Errorf("status 应打印 %q，got:\n%s", want, out)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("--status 不得调用 CLI，但桩被调用了（marker err=%v）", err)
	}
}

// --status 要能回答「检查 model.json」这一步：密钥只露掩码，不能回显全量。
func TestSetupStatusShowsRedactedKey(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"mode":"api","provider":"deepseek","model":"deepseek-flash","api_key":"sk-secret-value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := captureStdout(t, func() error { return cmdSetup([]string{"--status"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sk-…lue") || !strings.Contains(out, "来自 model.json") {
		t.Errorf("应显示密钥掩码与来源，got:\n%s", out)
	}
	if strings.Contains(out, "sk-secret-value") {
		t.Errorf("不得回显完整密钥，got:\n%s", out)
	}
}

func TestSetupStatusReportsMissingConfig(t *testing.T) {
	p := setupHome(t)
	out, err := captureStdout(t, func() error { return cmdSetup([]string{"--status"}) })
	if err != nil {
		t.Fatalf("cmdSetup --status: %v", err)
	}
	if !strings.Contains(out, p) || !strings.Contains(out, "未配置") {
		t.Errorf("无配置时应说明未配置并给出路径，got:\n%s", out)
	}
}

// 已有配置默认不覆盖：跑一次默认流程必须逐字节不变。
func TestSetupDoesNotOverwriteExisting(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"provider":"deepseek","model":"deepseek-flash","api_key":"sk-old"}` + "\n"
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := writeFakeCLI(t, setupCLIStub)
	t.Setenv("PATH", dir)

	if _, err := captureStdout(t, func() error { return cmdSetup(nil) }); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != original {
		t.Errorf("已有配置不得被覆盖\nwant: %s\ngot:  %s", original, got)
	}
}

func TestSetupUndoRestoresBackup(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"provider":"deepseek","model":"m","api_key":"sk-old"}` + "\n"
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := cmdSetup([]string{"--mode", "off"}); err != nil {
		t.Fatalf("cmdSetup --mode off: %v", err)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil || c.Mode != "off" {
		t.Fatalf("应写入 mode=off，got %+v err=%v", c, err)
	}

	if _, err := captureStdout(t, func() error { return cmdSetup([]string{"--undo"}) }); err != nil {
		t.Fatalf("cmdSetup --undo: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != original {
		t.Errorf("--undo 应恢复备份内容\nwant: %s\ngot:  %s", original, got)
	}
	if _, err := os.Stat(p + ".v2mem.bak"); !os.IsNotExist(err) {
		t.Errorf("恢复后应删除备份，got err=%v", err)
	}
}

func TestSetupUndoWithoutBackupErrors(t *testing.T) {
	setupHome(t)
	err := cmdSetup([]string{"--undo"})
	if err == nil {
		t.Fatal("无备份时 --undo 应报错")
	}
	if !strings.Contains(err.Error(), "--mode off") {
		t.Errorf("错误应提示 --mode off，got: %v", err)
	}
}

// --undo 是真实且不可逆的写操作（写回配置并删除备份），不能被 --dry-run 当成预览执行。
func TestSetupUndoDryRunRejected(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	current := `{"mode":"off"}` + "\n"
	backup := `{"provider":"deepseek","model":"m","api_key":"sk-old"}` + "\n"
	if err := os.WriteFile(p, []byte(current), 0o600); err != nil {
		t.Fatal(err)
	}
	bak := p + ".v2mem.bak"
	if err := os.WriteFile(bak, []byte(backup), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cmdSetup([]string{"--undo", "--dry-run"})
	if err == nil {
		t.Fatal("--undo 与 --dry-run 同时使用应报错")
	}
	if !strings.Contains(err.Error(), "--undo") || !strings.Contains(err.Error(), "--dry-run") {
		t.Errorf("错误应点名两个 flag，got: %v", err)
	}

	got, rerr := os.ReadFile(p)
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(got) != current {
		t.Errorf("被拒绝的组合不得改动配置\nwant: %s\ngot:  %s", current, got)
	}
	gotBak, rerr := os.ReadFile(bak)
	if rerr != nil {
		t.Fatalf("备份不应被删除: %v", rerr)
	}
	if string(gotBak) != backup {
		t.Errorf("备份内容不得改动\nwant: %s\ngot:  %s", backup, gotBak)
	}
}

// 测试替身：不碰真实终端，也不发真实 HTTP。
func stubAPIKeyPrompt(t *testing.T, key string, calls *int) {
	t.Helper()
	old := readAPIKey
	readAPIKey = func(in *os.File, out io.Writer, prompt string) (string, error) {
		if calls != nil {
			*calls++
		}
		return key, nil
	}
	t.Cleanup(func() { readAPIKey = old })
}

func stubAPIDryRun(t *testing.T, fn func(distill.ModelConfig) error) {
	t.Helper()
	old := apiDryRun
	apiDryRun = fn
	t.Cleanup(func() { apiDryRun = old })
}

// --mode api 只从 model.json 和用户当场输入取密钥；即使环境里导出了 key 也不许用。
func TestSetupModeAPIPromptsAndIgnoresEnv(t *testing.T) {
	p := setupHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "sk-from-env")
	t.Setenv("OPENAI_API_KEY", "sk-from-env-openai")
	calls := 0
	stubAPIKeyPrompt(t, "sk-from-prompt", &calls)
	var seen distill.ModelConfig
	stubAPIDryRun(t, func(cfg distill.ModelConfig) error { seen = cfg; return nil })

	out, err := captureStdout(t, func() error { return cmdSetup([]string{"--mode", "api"}) })
	if err != nil {
		t.Fatalf("cmdSetup --mode api: %v", err)
	}
	if calls != 1 {
		t.Errorf("文件无 key 时应且只应提示一次，got %d", calls)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Mode != "api" || c.Provider != "deepseek" || c.APIKey != "sk-from-prompt" {
		t.Errorf("应写入用户输入的密钥，got %+v", c)
	}
	if c.Model != distill.DefaultDeepSeekModel || c.BaseURL != "https://api.deepseek.com/v1" {
		t.Errorf("应补 deepseek 默认值，got %+v", c)
	}
	if seen.APIKey != "sk-from-prompt" {
		t.Errorf("dry-run 应使用输入的密钥，got %+v", seen)
	}
	if strings.Contains(out, "sk-from-env") || strings.Contains(out, "sk-from-prompt") {
		t.Errorf("密钥不得回显，got:\n%s", out)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
}

// 流程第 1 步：文件不存在时先创建，再进入取 key / dry-run。
func TestSetupModeAPICreatesMissingModelJSON(t *testing.T) {
	p := setupHome(t)
	stubAPIKeyPrompt(t, "sk-new", nil)
	stubAPIDryRun(t, func(distill.ModelConfig) error { return nil })

	if _, err := captureStdout(t, func() error { return cmdSetup([]string{"--mode", "api"}) }); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("应创建 model.json: %v", err)
	}
	if !strings.Contains(string(b), `"api_key": "sk-new"`) {
		t.Errorf("文件应含密钥，got %q", b)
	}
}

func TestSetupModeAPIEmptyKeyErrors(t *testing.T) {
	p := setupHome(t)
	stubAPIKeyPrompt(t, "   ", nil)
	stubAPIDryRun(t, func(distill.ModelConfig) error {
		t.Error("空密钥不应进入 dry-run")
		return nil
	})

	err := cmdSetup([]string{"--mode", "api"})
	if err == nil || !strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空密钥应报错，got %v", err)
	}
	st, statErr := os.Stat(p)
	if statErr == nil && strings.Contains(string(mustRead(t, p)), "api_key") {
		t.Errorf("空密钥不得写入 config，got %v", st)
	}
}

func TestSetupModeAPIDryRunFailureDoesNotWriteKey(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	original := `{"mode":"off"}` + "\n"
	if err := os.WriteFile(p, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	stubAPIKeyPrompt(t, "sk-bad", nil)
	stubAPIDryRun(t, func(distill.ModelConfig) error { return fmt.Errorf("openai http 401") })

	err := cmdSetup([]string{"--mode", "api"})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("dry-run 失败应报出原因，got %v", err)
	}
	if got := string(mustRead(t, p)); got != original {
		t.Errorf("dry-run 失败不得写盘\nwant: %s\ngot:  %s", original, got)
	}
}

func TestSetupModeAPIDryRunFlagDoesNotWrite(t *testing.T) {
	p := setupHome(t)
	stubAPIKeyPrompt(t, "sk-dry", nil)
	stubAPIDryRun(t, func(distill.ModelConfig) error { return nil })

	out, err := captureStdout(t, func() error {
		return cmdSetup([]string{"--mode", "api", "--dry-run"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "dry-run 成功") {
		t.Errorf("应报告 dry-run 成功，got:\n%s", out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("--dry-run 不得写盘，got err=%v", err)
	}
}

func TestSetupModeAPIKeepsCustomModelAndBaseURL(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"mode":"api","provider":"deepseek","model":"my-model","base_url":"https://gw.internal/v1"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	stubAPIKeyPrompt(t, "sk-gw", nil)
	var seen distill.ModelConfig
	stubAPIDryRun(t, func(cfg distill.ModelConfig) error { seen = cfg; return nil })

	if err := cmdSetup([]string{"--mode", "api"}); err != nil {
		t.Fatal(err)
	}
	if seen.Model != "my-model" || seen.BaseURL != "https://gw.internal/v1" {
		t.Errorf("自建网关配置不得被默认值覆盖，got %+v", seen)
	}
}

func TestSetupModeAPIProviderFlag(t *testing.T) {
	setupHome(t)
	stubAPIKeyPrompt(t, "sk-openai", nil)
	var seen distill.ModelConfig
	stubAPIDryRun(t, func(cfg distill.ModelConfig) error { seen = cfg; return nil })

	if err := cmdSetup([]string{"--mode", "api", "--provider", "openai"}); err != nil {
		t.Fatal(err)
	}
	if seen.Provider != "openai" || seen.Model != "gpt-4o-mini" || !strings.HasPrefix(seen.BaseURL, "https://api.openai.com") {
		t.Errorf("--provider openai 应走默认端点，got %+v", seen)
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", p, err)
	}
	return string(b)
}

// 真实 dry-run 必须带着文件里的 key 打到 base_url，且能解析 judge 输出。
func TestDryRunAPIConfigUsesStoredKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"[{\"id\":\"aaaaaaaa\",\"verdict\":\"reject\",\"reason\":\"process_narrative\"}]"}}]}`)
	}))
	defer srv.Close()

	cfg := distill.ModelConfig{Provider: "deepseek", Model: "deepseek-flash", BaseURL: srv.URL, APIKey: "sk-live"}
	t.Setenv("DEEPSEEK_API_KEY", "sk-env-should-not-win")
	if err := dryRunAPIConfig(cfg); err != nil {
		t.Fatalf("dryRunAPIConfig: %v", err)
	}
	if gotAuth != "Bearer sk-live" {
		t.Errorf("应显式使用配置的 key，got %q", gotAuth)
	}
}

func TestDryRunAPIConfigRejectsBadResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"模型坏掉了"}}]}`)
	}))
	defer srv.Close()
	cfg := distill.ModelConfig{Provider: "deepseek", Model: "deepseek-flash", BaseURL: srv.URL, APIKey: "sk-live"}
	if err := dryRunAPIConfig(cfg); err == nil {
		t.Fatal("输出不可解析时应报错")
	}
}

// 非 TTY（管道 / 测试）时逐行读；真实终端的无回显由 term.ReadPassword 负责。
func TestReadSecretLineFromPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.WriteString(w, "sk-pipe\n"); err != nil {
		t.Fatal(err)
	}
	w.Close()
	var out strings.Builder
	key, err := promptSecretLine(r, &out, "输入密钥:")
	if err != nil {
		t.Fatal(err)
	}
	if key != "sk-pipe" {
		t.Errorf("got %q", key)
	}
	if !strings.Contains(out.String(), "输入密钥:") {
		t.Errorf("应先打印提示，got %q", out.String())
	}
}

// 显式 --mode cli 但没有可用 CLI 时必须报错（用户的要求无法满足，不能静默放过）。
func TestSetupModeCLIWithoutCLIErrors(t *testing.T) {
	setupHome(t)
	t.Setenv("PATH", t.TempDir())
	if err := cmdSetup([]string{"--mode", "cli"}); err == nil {
		t.Fatal("显式 --mode cli 且无候选时应报错")
	}
}

// 第一个候选失败时要按白名单顺序换下一个，而不是直接放弃（spec §7 step 4）。
func TestSetupFallsBackToSecondCLI(t *testing.T) {
	p := setupHome(t)
	dir := t.TempDir()
	// claude 能报版本，但 prompt 一律非零退出 → judge dry-run 失败。
	bad := "#!/bin/sh\ncase \"$1\" in --version) echo \"1.0.0\"; exit 0 ;; esac\nexit 1\n"
	// codex 同形脚本，prompt 按内容回合法 JSON。
	good := `#!/bin/sh
case "$1" in --version) echo "2.0.0"; exit 0 ;; esac
in=$(/bin/cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *) echo '{"kind":"preference","content":"默认短句","salience":0.6,"obs_ids":["setup"]}' ;;
esac
`
	for name, body := range map[string]string{"claude": bad, "codex": good} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)

	out, err := captureStdout(t, func() error { return cmdSetup(nil) })
	if err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Mode != "cli" || c.CLI != "codex" {
		t.Fatalf("claude 失败后应落到 codex，got %+v", c)
	}
	if !strings.HasPrefix(c.ModelTag, "codex@") {
		t.Errorf("ModelTag 应以 codex@ 开头，got %q", c.ModelTag)
	}
	if !strings.Contains(out, "会发往") {
		t.Errorf("换到第二个候选也要打印外发告知，got:\n%s", out)
	}
	if _, err := os.Stat(p + ".v2mem.bak"); !os.IsNotExist(err) {
		t.Errorf("首次写盘不应产生备份，got err=%v", err)
	}
}

// 显式 --mode cli 且 dry-run 失败时同样报错，不写盘。
func TestSetupModeCLIDryRunFailureErrors(t *testing.T) {
	p := setupHome(t)
	t.Setenv("PATH", writeFakeCLI(t, "#!/bin/sh\nexit 1\n"))
	if err := cmdSetup([]string{"--mode", "cli"}); err == nil {
		t.Fatal("显式 --mode cli 且验证失败时应报错")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("验证失败不得写盘，got err=%v", err)
	}
}

func TestSetupRejectsModeWithStatusOrUndo(t *testing.T) {
	setupHome(t)
	if err := cmdSetup([]string{"--status", "--mode", "off"}); err == nil {
		t.Error("--status 与 --mode 互斥，应报错")
	}
	if err := cmdSetup([]string{"--undo", "--mode", "off"}); err == nil {
		t.Error("--undo 与 --mode 互斥，应报错")
	}
	if err := cmdSetup([]string{"--mode", "bogus"}); err == nil {
		t.Error("未知 --mode 应报错")
	}
}

// spec §4：mode=cli 时 api 字段保留不删，--mode api 切回时直接沿用文件里的 key，不提示、不看环境。
func TestSetupModeAPIReusesStoredKey(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"provider":"deepseek","model":"deepseek-flash","api_key":"sk-stored","base_url":"https://api.deepseek.com/v1"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "sk-env-decoy")
	t.Setenv("OPENAI_API_KEY", "sk-env-decoy-2")
	calls := 0
	stubAPIKeyPrompt(t, "sk-should-not-be-used", &calls)
	var seen distill.ModelConfig
	stubAPIDryRun(t, func(cfg distill.ModelConfig) error { seen = cfg; return nil })

	if _, err := captureStdout(t, func() error { return cmdSetup([]string{"--mode", "api"}) }); err != nil {
		t.Fatalf("cmdSetup --mode api: %v", err)
	}
	if calls != 0 {
		t.Errorf("文件已有 key 时不得再提示，got %d", calls)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Mode != "api" || c.APIKey != "sk-stored" || c.Provider != "deepseek" {
		t.Errorf("应沿用 model.json 里已有的 api_key 与 provider，got %+v", c)
	}
	if seen.APIKey != "sk-stored" {
		t.Errorf("dry-run 应看到文件里的 key，got %+v", seen)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
}

// 本机只装了 freebuff 时，用户最容易怀疑「为什么探测不到」。必须点名解释，
// 而不是只丢一句「没有探测到可用的 CLI」。
func TestSetupMentionsNoHeadlessCLI(t *testing.T) {
	p := setupHome(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "freebuff"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	out, err := captureStdout(t, func() error { return cmdSetup(nil) })
	if err != nil {
		t.Fatalf("全不可用时默认流程不应返回错误，got %v", err)
	}
	if !strings.Contains(out, "freebuff") || !strings.Contains(out, "TUI-only") {
		t.Errorf("应点名 freebuff 无 headless 的原因，got:\n%s", out)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("识别到不支持的 CLI 时不得写盘，got err=%v", err)
	}
}

// dry-run 失败只报「响应不可解析」无法区分「CLI 未登录」和「模型输出格式错」
// （2026-11-23 codebuddy setup 失败正卡在这里）。失败信息必须带原始响应片段。
func TestSetupDryRunFailureShowsRawResponse(t *testing.T) {
	setupHome(t)
	dir := t.TempDir()
	// judge 返回合法数组；distill 返回一段典型的「模型说话了但不是 JSON」的散文。
	script := `#!/bin/sh
case "$1" in --version) echo "1.2.3"; exit 0 ;; esac
in=$(/bin/cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *) echo "Authentication required. Please use /login command to sign in to your account" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	out, err := captureStdout(t, func() error { return cmdSetup(nil) })
	if err != nil {
		t.Fatalf("默认流程全失败不应返回错误: %v", err)
	}
	if !strings.Contains(out, "Authentication required") {
		t.Errorf("失败信息应包含模型原始响应，got:\n%s", out)
	}
}

// 显式 --mode cli 失败时报错也要带原始响应，否则用户在终端同样无从排查。
func TestSetupModeCLIFailureShowsRawResponse(t *testing.T) {
	setupHome(t)
	dir := writeFakeCLI(t, `#!/bin/sh
case "$1" in --version) echo "1.2.3"; exit 0 ;; esac
echo "Authentication required. Please use /login command to sign in to your account"
`)
	t.Setenv("PATH", dir)

	err := cmdSetup([]string{"--mode", "cli"})
	if err == nil {
		t.Fatal("显式 --mode cli 验证失败时应报错")
	}
	if !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("错误信息应包含模型原始响应，got: %v", err)
	}
}

// dry-run 的观测必须明确到模型有事实可抽；过弱的观测会让正常模型也回散文。
func TestSetupDryRunUsesInformativeObservation(t *testing.T) {
	p := setupHome(t)
	dir := t.TempDir()
	// 只有 prompt 里出现「跑测试」才回合法事实，否则回散文 —— 直接验证 dry-run 观测内容。
	script := `#!/bin/sh
case "$1" in --version) echo "1.2.3"; exit 0 ;; esac
in=$(/bin/cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *"跑测试"*) echo '{"kind":"preference","content":"偏好短句","salience":0.6,"obs_ids":["setup"]}' ;;
  *) echo "这条观测没有可抽取的事实。" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	if _, err := captureStdout(t, func() error { return cmdSetup(nil) }); err != nil {
		t.Fatalf("cmdSetup: %v", err)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil || c.Mode != "cli" {
		t.Fatalf("dry-run 观测应可产出事实并写盘，got %+v err=%v", c, err)
	}
}
