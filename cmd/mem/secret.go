package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// promptSecretLine 读一行密钥：真终端临时关 ECHO，管道/重定向按普通行读。
//
// 为什么要碰终端属性：密钥回显会留在屏幕、录屏和终端回滚里，与「只写 0600 文件」
// 的边界冲突。不引入第三方终端库（项目纪律：直接依赖只有一个），OS 差异放到
// 同包的 secret_echo_*.go 里；拿不到终端属性时直接报错，不静默回显密钥。
func promptSecretLine(in *os.File, out io.Writer, prompt string) (string, error) {
	restore, hidden, err := silenceEcho(in.Fd())
	if err != nil {
		return "", err
	}
	if restore != nil {
		defer restore()
	}
	fmt.Fprint(out, prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if hidden {
		fmt.Fprintln(out) // 无回显时看不到用户按了回车，自己补一个换行
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
