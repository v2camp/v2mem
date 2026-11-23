//go:build linux

package main

import (
	"syscall"
	"unsafe"
)

// silenceEcho 只关 ECHO，不动 ICANON / ISIG：行编辑和 Ctrl-C 保持原样。
// 非终端（管道）返回 (nil,false,nil)，由调用方按普通行读取。
func silenceEcho(fd uintptr) (func(), bool, error) {
	var t syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		if errno == syscall.ENOTTY {
			return nil, false, nil
		}
		return nil, false, errno
	}
	old := t
	t.Lflag &^= syscall.ECHO
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&t))); errno != 0 {
		return nil, false, errno
	}
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCSETS, uintptr(unsafe.Pointer(&old)))
	}, true, nil
}
