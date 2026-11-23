# 模型来源配置（api / cli）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 `mem setup` 零填写地把本机已登录的 harness CLI 配成 judge 与蒸馏的模型来源，同时保留 api 作为 canonical 路径。

**Architecture:** 在 `internal/distill` 的 `resolve()` 入口按 `Options.CLI` 分派 CLI 执行器，使 `Run` 与 `PromptCompleter` 两条路径同时生效；`ResolveOptions()` 是 `flag > model.json > env > auto` 的唯一实现点。`mem setup` 只做探测、dry-run、写 `model.json`（0600），不碰 hooks / MCP / 库。

**Tech Stack:** Go 标准库（`os/exec`、`encoding/json`、`flag`），现有 `internal/distill`、`cmd/mem`；无新依赖、无新包。

**Spec:** `docs/superpowers/specs/2026-11-23-model-provider-setup-design.md`

## Global Constraints

- 不新增包 → 不改 `coverage-policy.txt`；`internal/distill` 下限 86、`cmd/mem` 下限 81 不得下降。
- 配置路径固定 `~/.v2mem/model.json`，文件权限 0600；写前备份 `model.json.v2mem.bak`，原子替换。
- `mode=cli` 时不得读取 `api_key` / `base_url` 出网；`mode=off` 时 judge 报「判定器不可用」、蒸馏走规则。
- `mem setup` 不写 hooks、不写 MCP、不写 memories、不写 candidates、不启动常驻进程。
- CLI 只在写侧 detach 子进程里调用，绝不进 PromptSubmit / SessionStart 读路径。
- CLI 模板 argv 在本机未实测：模板错误必须由 dry-run 拦下（不写盘），不得放宽 `mem setup` 的验证条件。
- 所有提交在 worktree `.worktree/model-provider-setup/` 的分支 `feat/model-provider-setup` 上；合并提交正文按 AGENTS.md 写 `Base:` / `Gate:` / `Closes:`。

## Review Focus

1. 老 `model.json` 没有 `mode` 字段时，必须完全走原 api 路径，不能因新字段校验而报错。
2. `mode=cli` 下显式 `--provider none` 必须不调用 CLI（flag 优先），否则关不掉判定器。
3. CLI 输出夹带 Markdown / 解释时，现有 `parseJudgeVerdicts` 与 `ParseFacts` 仍能解析。
4. CLI 在无 TTY 环境等待 stdin 时，`runCLI` 必须被超时杀掉，不能挂住 Stop 钩子拉起的子进程。
5. `mem setup` 默认不得覆盖已有配置；`--undo` 无备份时不得删除或改写用户文件。

---

### Task 1: 配置模型、归一化与优先级

**Files:**
- Modify: `internal/distill/modelconfig.go`
- Test: `internal/distill/modelconfig_test.go`

**Interfaces:**
- Produces: `ModelConfig{Mode, CLI, ModelTag string}`；`LoadModelConfig(path string) (ModelConfig, error)`（Mode 归一化后只可能是 `api|cli|off`）；`SaveModelConfig(path string, c ModelConfig) error`；`ResolveOptions(cfg ModelConfig, flagProvider, flagModel string) Options`。

- [ ] **Step 1: 写失败测试**

在 `modelconfig_test.go` 追加：

```go
func TestLoadModelConfigLegacyDefaultsToAPI(t *testing.T) {
	p := writeJSON(t, `{"provider":"deepseek","model":"m","api_key":"k"}`)
	c, err := LoadModelConfig(p)
	if err != nil || c.Mode != "api" {
		t.Fatalf("老文件应归一化为 api，got %+v err=%v", c, err)
	}
}

func TestLoadModelConfigRejectsBadMode(t *testing.T) {
	for _, body := range []string{
		`{"mode":"bogus"}`,
		`{"mode":"cli","cli":"claude"}`,
		`{"mode":"cli","model_tag":"claude@1+tpl1"}`,
	} {
		if _, err := LoadModelConfig(writeJSON(t, body)); err == nil {
			t.Errorf("应报错: %s", body)
		}
	}
}

func TestSaveModelConfig0600AndBackup(t *testing.T) {
	p := filepath.Join(t.TempDir(), "model.json")
	if err := os.WriteFile(p, []byte(`{"provider":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveModelConfig(p, ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Errorf("权限应为 0600，got %v", st.Mode().Perm())
	}
	bak, err := os.ReadFile(p + ".v2mem.bak")
	if err != nil || !strings.Contains(string(bak), `"old"`) {
		t.Errorf("备份应保留原内容，got %q err=%v", bak, err)
	}
	c, _ := LoadModelConfig(p)
	if c.Mode != "cli" || c.CLI != "claude" {
		t.Errorf("写后读不一致: %+v", c)
	}
}

func TestResolveOptionsPrecedence(t *testing.T) {
	cli := ModelConfig{Mode: "cli", CLI: "claude", ModelTag: "claude@1+tpl1"}
	if opt := ResolveOptions(cli, "deepseek", ""); opt.CLI != "" || opt.Provider != "deepseek" {
		t.Errorf("显式 flag 应压过 cli，got %+v", opt)
	}
	if opt := ResolveOptions(cli, "", ""); opt.CLI != "claude" || opt.ModelTag != "claude@1+tpl1" {
		t.Errorf("无 flag 应用 cli，got %+v", opt)
	}
	if opt := ResolveOptions(ModelConfig{Mode: "off"}, "", ""); opt.Provider != "none" {
		t.Errorf("off 应映射 none，got %+v", opt)
	}
	if opt := ResolveOptions(ModelConfig{Mode: "api", Provider: "deepseek", Model: "m"}, "", ""); opt.Provider != "deepseek" || opt.CLI != "" {
		t.Errorf("api 应保持旧行为，got %+v", opt)
	}
}
```

新增 `writeJSON(t, body) string` helper：把 body 写入临时 `model.json`（0600）并返回路径。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/distill -run 'TestLoadModelConfigLegacy|TestLoadModelConfigRejectsBadMode|TestSaveModelConfig|TestResolveOptions' -v`
Expected: FAIL（`Mode` 字段不存在 / 函数未定义）。

- [ ] **Step 3: 实现**

`ModelConfig` 加字段（json tag 用 `mode` / `cli` / `model_tag`，均 `omitempty`）。
`LoadModelConfig` 解析后归一化：空 `Mode` → `api`；`api|cli|off` 之外报错；`cli` 缺 `CLI` 或 `ModelTag` 报错。
`SaveModelConfig`：`os.MkdirAll(dir, 0o755)`；若原文件存在且无备份，先写 `path+".v2mem.bak"`（原权限 0600）；`json.MarshalIndent` + 换行；写同目录临时文件 `0o600` 后 `os.Rename`。
`ResolveOptions` 按 spec §4 实现：`flagProvider != ""` → 只填 `Provider/Model/APIKey/BaseURL`（`Model` 为空时取 `cfg.Model`）；否则 `Mode=="cli"` → `CLI/ModelTag`；`Mode=="off"` → `Provider:"none"`；否则复制 api 字段。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/distill -run 'TestLoadModelConfig|TestSaveModelConfig|TestResolveOptions' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/distill/modelconfig.go internal/distill/modelconfig_test.go
git commit -m "feat(distill): model.json 增加 mode/cli/model_tag 与 ResolveOptions 优先级"
```

---

### Task 2: CLI 执行器与白名单模板

**Files:**
- Create: `internal/distill/cli.go`
- Create: `internal/distill/cli_test.go`
- Modify: `internal/distill/llm.go`（只加 `case "cli"`，留到 Task 3 也可；本任务先建执行器）

**Interfaces:**
- Produces: `Probe() (name string, ok bool)`；`VersionTag(name string) string`；`runCLI(ctx context.Context, name, prompt string) (string, error)`；模板表 `cliTemplates`（含 `Name/Args/VersionArgs/Ver`）。

- [ ] **Step 1: 写失败测试**

`cli_test.go`：

```go
func TestProbeOrderAndTemplate(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"codex", "claude"} {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("#!/bin/sh\necho "+n+"-v1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	if name, ok := Probe(); !ok || name != "claude" {
		t.Fatalf("应按白名单顺序取 claude，got %q ok=%v", name, ok)
	}
	if !strings.Contains(VersionTag("claude"), "claude@claude-v1+tpl") {
		t.Errorf("VersionTag 不对: %q", VersionTag("claude"))
	}
}

func TestRunCLIStdinStdoutAndError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\ncat\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := runCLI(context.Background(), "claude", "hello")
	if err != nil || out != "hello" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho boom >&2\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI(context.Background(), "claude", "x"); err == nil || !errors.Is(err, ErrLLMUnavailable) {
		t.Fatalf("非零退出应归 ErrLLMUnavailable，got %v", err)
	}
}

func TestRunCLITimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := runCLI(ctx, "claude", "x"); err == nil {
		t.Fatal("超时应报错")
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/distill -run 'TestProbe|TestRunCLI' -v`
Expected: FAIL（未定义）。

- [ ] **Step 3: 实现 `cli.go`**

模板表按 spec §5.1：

```go
var cliTemplates = []cliTemplate{
	{Name: "claude", Args: []string{"-p", "--output-format", "text", "--tools", "", "--permission-mode", "dontAsk"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "codex", Args: []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "-"}, VersionArgs: []string{"--version"}, Ver: "1"},
}
```

在表头写一行注释：`// argv 未在本机实测；dry-run 失败时该模板不得写盘，换 flag 必须同时升 Ver。`
`Probe` 用 `exec.LookPath` 按顺序取第一个命中。
`VersionTag` 用 `context.WithTimeout(3s)` 跑 `VersionArgs`，取 stdout 首行；失败返回 `<name>@unknown+tpl<Ver>`。
`runCLI`：查表；`exec.CommandContext`；`cmd.Stdin = strings.NewReader(prompt)`；stdout / stderr 各 `bytes.Buffer`；非零退出返回 `fmt.Errorf("%w: %s: %s", ErrLLMUnavailable, name, firstLine(stderr))`；成功返回 stdout。
`firstLine` 截首行、限 200 字节。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/distill -run 'TestProbe|TestRunCLI' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/distill/cli.go internal/distill/cli_test.go
git commit -m "feat(distill): CLI 白名单执行器与版本探测"
```

---

### Task 3: 接入 resolve / completeLLM 与两条调用链

**Files:**
- Modify: `internal/distill/distill.go`
- Modify: `internal/distill/llm.go`
- Modify: `cmd/mem/judge.go`
- Modify: `cmd/mem/distill.go`
- Test: `internal/distill/cli_test.go`、`internal/distill/completer_test.go`

**Interfaces:**
- Consumes: Task 1 的 `ResolveOptions`、Task 2 的 `runCLI`。
- Produces: `Options.CLI` / `Options.ModelTag`；`ModelSpec.Kind == "cli"`；`resolve()` 在 `CLI` 非空时返回 `name = "cli:" + ModelTag`、`ModelSpec{Name: CLI, Kind: "cli"}`。

- [ ] **Step 1: 写失败测试**

`cli_test.go` 追加：

```go
func TestResolveCLIWins(t *testing.T) {
	name, spec, err := resolve(Options{CLI: "claude", ModelTag: "claude@1+tpl1"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "cli:claude@1+tpl1" || spec.Kind != "cli" || spec.Name != "claude" {
		t.Fatalf("got %q %+v", name, spec)
	}
}

func TestCompleteLLMCLI(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	out, err := completeLLM(context.Background(), ModelSpec{Kind: "cli", Name: "claude"}, "[]")
	if err != nil || out != "[]" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}
```

`completer_test.go` 追加：

```go
func TestPromptCompleterCLI(t *testing.T) {
	fn, name, err := PromptCompleter(Options{CLI: "claude", ModelTag: "claude@1+tpl1"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "cli:claude@1+tpl1" || fn == nil {
		t.Fatalf("name=%q fn=%v", name, fn != nil)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/distill -run 'TestResolveCLIWins|TestCompleteLLMCLI|TestPromptCompleterCLI' -v`
Expected: FAIL（`Options.CLI` 未定义）。

- [ ] **Step 3: 实现**

`distill.go`：`Options` 加 `CLI`、`ModelTag`；`resolve()` 开头加：

```go
if strings.TrimSpace(opt.CLI) != "" {
	name := "cli:" + strings.TrimSpace(opt.ModelTag)
	if strings.TrimSpace(opt.ModelTag) == "" {
		name = "cli:" + strings.TrimSpace(opt.CLI)
	}
	return name, ModelSpec{Name: opt.CLI, Kind: "cli"}, nil
}
```

`llm.go`：`completeLLM` 加 `case "cli": return runCLI(ctx, spec.Name, prompt)`。

`judge.go`：把 `Options{Provider: *provider, ...}` 改为：

```go
opt := distill.ResolveOptions(cfg, *provider, *model)
opt.Timeout = *timeout
complete, providerName, err := distill.PromptCompleter(opt)
```

删除随后手工补 `provider = auto` / `model = deepseek-flash` 的分支（`resolve()` 已兜底）。

`distill.go`（cmd）：`--provider` 默认值从 `"auto"` 改为 `""`（空=读配置再退回 auto，语义不变但能区分显式 flag）；在 `model, timeout := distill.LoadConfig(...)` 后加 `cfg, _ := distill.LoadModelConfig("")`；line 68/84 的 `Options` 改为：

```go
localOpt := distill.Options{Provider: "ollama", Model: model, Timeout: timeout}
byokOpt := distill.Options{Provider: "openai", Model: model, Timeout: timeout}
if cfg.Mode == "cli" {
	localOpt = distill.ResolveOptions(cfg, "", "")
	localOpt.Timeout = timeout
	byokOpt = localOpt
}
```

**不要把 `config.toml` 的 `model` 传给 `flagModel`**：它不是显式 `--model` flag，传进去会把
`model_tag` 覆盖成 `cli:qwen2.5:7b`，破坏按 CLI 版本 + 模板版本归因。

line 446（在 `distillOne` 内）的 `Run` 改为：

```go
cfg, _ := distill.LoadModelConfig("")
opt := distill.ResolveOptions(cfg, job.Provider, "")
if opt.CLI == "" && opt.Model == "" {
	opt.Model = job.Model
}
opt.Timeout = job.Timeout
out := distill.Run(in, opt)
```

注意：`distillOne` 里原有的 `if provider == "" { provider = "auto" }` 只能用于 `report.Provider` 的初值，
不能把 `"auto"` 传给 `ResolveOptions`——否则 flag 会被当成显式值、CLI 永远不生效。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/distill ./cmd/mem -run 'TestResolveCLIWins|TestCompleteLLMCLI|TestPromptCompleter|TestLoadModelConfig|TestResolveOptions' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/distill cmd/mem/judge.go cmd/mem/distill.go
git commit -m "feat(distill): judge 与蒸馏共用 CLI 执行路径"
```

---

### Task 4: `mem setup` 命令

**Files:**
- Create: `cmd/mem/setup.go`
- Create: `cmd/mem/setup_test.go`
- Modify: `cmd/mem/main.go`（`knownSubcommands`、`switch`、`usageText`）
- Test: `cmd/mem/main_test.go`（护栏会断言已知子命令清单）

**Interfaces:**
- Consumes: `distill.Probe`、`distill.VersionTag`、`distill.LoadModelConfig`、`distill.SaveModelConfig`、`distill.DefaultModelConfigPath`、`distill.ResolveOptions`、`distill.Run`；本包 `parseJudgeVerdicts`、`judgePrompt`。
- Produces: `cmdSetup(args []string) error`；`setupNotice(name, tag string) string`；`apiConfigFromEnv(cfg ModelConfig) (ModelConfig, error)`。

- [ ] **Step 1: 写失败测试**

`setup_test.go` 用临时 HOME + 假 CLI 桩：

```go
func TestSetupWritesCLIAndNotice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	script := `#!/bin/sh
in=$(cat)
case "$in" in
  *"候选"*) echo '[{"id":"aaaaaaaa","verdict":"reject","reason":"process_narrative"}]' ;;
  *) echo '{"kind":"preference","content":"默认短句","salience":0.6,"obs_ids":["setup"]}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if err := cmdSetup(nil); err != nil {
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
}
```

再补五个用例（同文件）：
- `TestSetupNoticeMentionsScope`：`setupNotice("claude", "claude@1+tpl1")` 的返回串必须包含「会发往」与回退命令。
- `TestSetupAllCandidatesFailLeavesNoConfig`：桩 `exit 1`，断言 `model.json` 不存在、err 为 nil。
- `TestSetupDoesNotOverwriteExisting`：先写 api 配置，跑 `cmdSetup(nil)`，断言内容不变。
- `TestSetupUndoRestoresBackup`：先写旧配置，跑 `cmdSetup([]string{"--mode","off"})`，再 `cmdSetup([]string{"--undo"})`，断言恢复旧内容。
- `TestSetupModeAPIUsesEnv`：`t.Setenv("DEEPSEEK_API_KEY","sk-test")`，`cmdSetup([]string{"--mode","api"})`，断言 `Provider=="deepseek"`、`APIKey=="sk-test"`、0600。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/mem -run 'TestSetup' -v`
Expected: FAIL（`cmdSetup` 未定义）。

- [ ] **Step 3: 实现 `setup.go`**

flag：`--dry-run`、`--status`、`--mode api|cli|off`、`--undo`。
流程按 spec §7：读现状 → 已有配置且无显式 mode 时打印 status 返回 → `Probe()` → 逐候选 dry-run（judge prompt + `distill.Run` 一条合成观测）→ 全失败不写盘、退出 0 → 通过则 `SaveModelConfig` → 打印 `setupNotice`。
`--mode api` 只从环境变量取 key（`DEEPSEEK_API_KEY` 或 `OPENAI_API_KEY`），都没有则报错并提示两种 export；不读 stdin（避免回显与依赖）。
`--undo`：无 `model.json.v2mem.bak` 报错；有则读备份、写回 `model.json`（0600）、删除备份。
跨平台不用 `os.Rename` 覆盖目标（Windows 会失败）。
`--status`：只打印 `mode/cli/model_tag/path/perm`，不联网。
`main.go`：`knownSubcommands` 加 `"setup"`，switch 加 `case "setup"`，usageText 加一行。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/mem -run 'TestSetup|TestUsageText|TestGuidanceNames' -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add cmd/mem/setup.go cmd/mem/setup_test.go cmd/mem/main.go cmd/mem/main_test.go
git commit -m "feat(setup): 零填写探测 CLI 并写入 model.json"
```

---

### Task 5: 文档、登记与门禁

**Files:**
- Modify: `docs/install.md`（S6、§1 原则、删末尾「mem setup 尚未实现」）
- Modify: `docs/design-admission.md`（§5 增补 CLI 路径与降级）
- Modify: `README.md`（安装表加「模型配置（可选）」）

**Interfaces:**
- Consumes: Task 4 的 `mem setup` 命令行为与输出。

- [ ] **Step 1: 改 `docs/install.md`**

在 S5 之后加 `### S6 可选：配置模型来源（判定器 / 蒸馏）`，内容：
```bash
mem setup              # 探测本机 CLI，dry-run 通过才写 ~/.v2mem/model.json
mem setup --status     # 查看当前来源
mem setup --undo       # 回退
```
完成判据：`mem setup --status` 显示 `mode`；`mem judge --stats` 不再报「判定器不可用」。
明确一句：judge 候选正文与蒸馏观测摘要会发往该 CLI 所连模型；不配也能用，只是 judge / LLM 蒸馏不可用。
§1 原则加第 6 条「零填写但需告知」；删除末尾 `> mem setup 尚未实现，不要执行；实际按本文 S0–S5 走。`

- [ ] **Step 2: 改 `docs/design-admission.md` §5**

在现有表格后追加「CLI 路径」一段：`mode=cli` 的优先级、`cli` 只存白名单名、
`judge_model` 记 `cli:<name>@<version>+tpl<ver>@judge-v2`、这是 §4(a) 的降级（按 CLI + 模板版本而非模型版本），
影子期一致率须按该 tag 分段复核。

- [ ] **Step 3: 改 README 安装表**

在安装路径表加一行：`模型配置（可选） | judge / LLM 蒸馏 | mem setup | mem setup --undo`。

- [ ] **Step 4: 跑全量门禁**

Run:
```bash
make test
make cover-gate
make test-e2e
make test-install
make test-verify-install
```
Expected: 全部 PASS；`cover-gate` 的 `internal/distill` ≥86、`cmd/mem` ≥81。
若 `go test ./...` 因新增 CLI 测试依赖 `/bin/sh` 在 Windows 失败，给 CLI 测试加 `//go:build !windows`，不降覆盖率下限。

- [ ] **Step 5: 提交**

```bash
git add docs/install.md docs/design-admission.md README.md
git commit -m "docs(install): 增加 mem setup 可选项与 CLI 降级说明"
```

