package distill

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// cliTemplate 是内置白名单里的一条 CLI 调用模板。
//
// 配置只存 Name，绝不存命令串：可执行的名字与 flag 都由代码决定，
// 换 flag 必须同时升 Ver，让历史 judge_model 自动分段。
type cliTemplate struct {
	Name        string
	Args        []string
	PromptArg   bool // true：用 Args 里的 {prompt} 占位；false：prompt 走 stdin
	VersionArgs []string
	Ver         string
}

const promptPlaceholder = "{prompt}"

// argv 未在本机实测；dry-run 失败时该模板不得写盘，换 flag 必须同时升 Ver。
//
// 工具策略：claude/codebuddy 用 --tools "" 全禁；copilot 用空 --available-tools；
// codex/trae 用只读沙箱；gemini 用 plan 只读；qoder 用非交互默认拒绝 + max-turns 1；
// cursor 官方没有硬禁工具开关，--mode ask + --sandbox enabled 只是只读倾向，标为受限支持。
//
// prompt 传输：官方明确支持 stdin 的走 stdin；cursor/codebuddy 官方示例是 `-p "prompt"`，
// 管道只作上下文，因此把 prompt 放进 Args 的 {prompt} 占位位置。
var cliTemplates = []cliTemplate{
	{Name: "claude", Args: []string{"-p", "--output-format", "text", "--tools", "", "--permission-mode", "dontAsk"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "codex", Args: []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "--ephemeral", "-"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "cursor-agent", Args: []string{"--output-format", "text", "--mode", "ask", "--sandbox", "enabled", "--trust", "-p", promptPlaceholder}, PromptArg: true, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "gemini", Args: []string{"--output-format", "text", "--approval-mode", "plan", "--extensions", "none", "--skip-trust"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "copilot", Args: []string{"-s", "--no-ask-user", "--trust-folder", "--available-tools=", "--deny-tool=shell,write,read,url,memory"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "qoder", Args: []string{"-p", "--output-format", "text", "--max-turns", "1"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "codebuddy", Args: []string{"-y", "-p", promptPlaceholder, "--output-format", "text", "--tools", "", "--append-system-prompt", "Only output the JSON array requested; no prose."}, PromptArg: true, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "traecli", Args: []string{"exec", "--sandbox", "read-only", "--ask-for-approval", "never", "--skip-git-repo-check", "--ephemeral"}, VersionArgs: []string{"--version"}, Ver: "1"},
}

// CLIWhitelist 返回白名单顺序的名字，供 mem setup 的错误文案复用，避免第二出处。
func CLIWhitelist() []string {
	names := make([]string, len(cliTemplates))
	for i, t := range cliTemplates {
		names[i] = t.Name
	}
	return names
}

// ProbeAll 按白名单顺序返回本机所有可用的 CLI 名。
//
// 为什么需要遍历而不是只取第一个：模板 argv 未实测，某个候选可能探测得到却
// 在 dry-run 里失败；此时应当换下一个，而不是让用户手工排查。
func ProbeAll() []string {
	var names []string
	for _, t := range cliTemplates {
		if _, err := exec.LookPath(t.Name); err == nil {
			names = append(names, t.Name)
		}
	}
	return names
}

// Probe 按白名单顺序返回第一个可用的 CLI 名字。不提问、不列编号。
// 它等价于 ProbeAll()[0]：保留旧签名，调用方不必都改成切片。
func Probe() (name string, ok bool) {
	if all := ProbeAll(); len(all) > 0 {
		return all[0], true
	}
	return "", false
}

// VersionTag 返回形如 claude@<版本首 token>+tpl<Ver> 的归因标签。
// 探测失败返回 <name>@unknown+tpl<Ver>：宁可标注未知，也不要编造版本。
func VersionTag(name string) string {
	for _, t := range cliTemplates {
		if t.Name != name {
			continue
		}
		// 10s：并行包测试/冷启动下 3s 会偶发超时，版本探测不是热路径。
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, t.Name, t.VersionArgs...).Output()
		if err != nil {
			return name + "@unknown+tpl" + t.Ver
		}
		// 只取第一个空白分隔的 token：claude --version 可能打印
		// "2.1.34 (Claude Code)"，整行进标签会让 judge_model 带空格和括号，难按版本分段。
		ver := ""
		if f := strings.Fields(firstLine(string(out))); len(f) > 0 {
			ver = f[0]
		}
		if ver == "" {
			return name + "@unknown+tpl" + t.Ver
		}
		return name + "@" + ver + "+tpl" + t.Ver
	}
	return name + "@unknown"
}

// runCLI 用白名单模板跑一次非交互调用：prompt 走 stdin，纯文本走 stdout。
// 超时与非零退出统一归 ErrLLMUnavailable，调用方据此降级或重试。
func runCLI(ctx context.Context, name, prompt string) (string, error) {
	var tpl *cliTemplate
	for i := range cliTemplates {
		if cliTemplates[i].Name == name {
			tpl = &cliTemplates[i]
			break
		}
	}
	if tpl == nil {
		return "", fmt.Errorf("%w: CLI 不在白名单: %s", ErrLLMUnavailable, name)
	}
	args := append([]string(nil), tpl.Args...)
	if tpl.PromptArg {
		replaced := false
		for i, a := range args {
			if a == promptPlaceholder {
				args[i] = prompt
				replaced = true
			}
		}
		if !replaced {
			args = append(args, prompt)
		}
	}
	cmd := exec.CommandContext(ctx, tpl.Name, args...)
	if !tpl.PromptArg {
		cmd.Stdin = strings.NewReader(prompt)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := firstLine(stderr.String())
		if msg == "" {
			// 超时时 stderr 通常是空的；ctx.Err() 比 "signal: killed" 更能说明原因。
			if ctx.Err() != nil {
				msg = ctx.Err().Error()
			} else {
				msg = err.Error()
			}
		}
		return "", fmt.Errorf("%w: %s: %s", ErrLLMUnavailable, name, msg)
	}
	return stdout.String(), nil
}

// firstLine 截首行并限 200 字节，避免把整段 stderr 灌进日志。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
