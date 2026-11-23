//go:build !darwin && !linux

package main

// 尚未适配的平台不关回显：宁可按普通行读，也不为了无回显引入平台专属依赖。
// 提示文本由调用方保证（见 secret.go）。
func silenceEcho(fd uintptr) (func(), bool, error) {
	return nil, false, nil
}
