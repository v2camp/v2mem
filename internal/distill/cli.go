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
	VersionArgs []string
	Ver         string
}

// argv 未在本机实测；dry-run 失败时该模板不得写盘，换 flag 必须同时升 Ver。
var cliTemplates = []cliTemplate{
	{Name: "claude", Args: []string{"-p", "--output-format", "text", "--tools", "", "--permission-mode", "dontAsk"}, VersionArgs: []string{"--version"}, Ver: "1"},
	{Name: "codex", Args: []string{"exec", "--sandbox", "read-only", "--skip-git-repo-check", "-"}, VersionArgs: []string{"--version"}, Ver: "1"},
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

// VersionTag 返回形如 claude@<版本首行>+tpl<Ver> 的归因标签。
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
		line := firstLine(string(out))
		if line == "" {
			return name + "@unknown+tpl" + t.Ver
		}
		return name + "@" + line + "+tpl" + t.Ver
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
	cmd := exec.CommandContext(ctx, tpl.Name, tpl.Args...)
	cmd.Stdin = strings.NewReader(prompt)
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
