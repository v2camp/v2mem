//go:build darwin

package main

import (
	"syscall"
	"unsafe"
)

// silenceEcho 只关 ECHO，不动 ICANON / ISIG：行编辑和 Ctrl-C 保持原样。
// 非终端（管道）返回 (nil,false,nil)，由调用方按普通行读取。
func silenceEcho(fd uintptr) (func(), bool, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)), 0, 0, 0); errno != 0 {
		if errno == syscall.ENOTTY {
			return nil, false, nil
		}
		return nil, false, errno
	}
	old := t
	t.Lflag &^= syscall.ECHO
	if _, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&t)), 0, 0, 0); errno != 0 {
		return nil, false, errno
	}
	return func() {
		syscall.Syscall6(syscall.SYS_IOCTL, fd, syscall.TIOCSETA, uintptr(unsafe.Pointer(&old)), 0, 0, 0)
	}, true, nil
}
