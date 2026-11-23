package main

import (
	"fmt"
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

func TestSetupModeAPIUsesEnv(t *testing.T) {
	p := setupHome(t)
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("DEEPSEEK_API_KEY", "sk-test")

	if err := cmdSetup([]string{"--mode", "api"}); err != nil {
		t.Fatalf("cmdSetup --mode api: %v", err)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Mode != "api" || c.Provider != "deepseek" || c.APIKey != "sk-test" {
		t.Errorf("应从环境变量构造 deepseek 配置，got %+v", c)
	}
	if c.Model != distill.DefaultDeepSeekModel {
		t.Errorf("deepseek 默认模型应为 %q，got %q", distill.DefaultDeepSeekModel, c.Model)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
}

func TestSetupModeAPIWithoutKeyErrors(t *testing.T) {
	p := setupHome(t)
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	err := cmdSetup([]string{"--mode", "api"})
	if err == nil {
		t.Fatal("没有环境变量密钥时应报错")
	}
	if !strings.Contains(err.Error(), "DEEPSEEK_API_KEY") || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("错误应提示两种 export，got: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("报错时不得写盘，got err=%v", err)
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

// spec §4：mode=cli 时 api 字段保留不删，--mode api 切回时没有环境变量也应沿用文件里的 key。
func TestSetupModeAPIReusesStoredKey(t *testing.T) {
	p := setupHome(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"provider":"deepseek","model":"deepseek-flash","api_key":"sk-stored","base_url":"https://api.deepseek.com/v1"}` + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEEPSEEK_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	if _, err := captureStdout(t, func() error { return cmdSetup([]string{"--mode", "api"}) }); err != nil {
		t.Fatalf("cmdSetup --mode api: %v", err)
	}
	c, err := distill.LoadModelConfig(p)
	if err != nil {
		t.Fatalf("LoadModelConfig: %v", err)
	}
	if c.Mode != "api" || c.APIKey != "sk-stored" || c.Provider != "deepseek" {
		t.Errorf("应沿用 model.json 里已有的 api_key 与 provider，got %+v", c)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
}
