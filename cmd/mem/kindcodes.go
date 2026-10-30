package main

import (
	"fmt"
	"strings"
	"unicode"
)

// kindCode 是类码表的一行。
//
// 真理源是库里的 kind 字段。这里只是 L1 / notes 用的短别名，
// 不新增表、不新增列、不改 schema。常数 4 行，不随记忆条数增长。
// 不列举 fact：它是入库默认值，写不出「什么时候该查」。
type kindCode struct {
	Code    string `json:"code"`
	Kind    string `json:"kind"`
	Trigger string `json:"trigger"`
}

// kindCodes 是类码表的单一真理源。文案照抄 VALUE §1.2，不得增删行。
var kindCodes = []kindCode{
	{"P", "preference", "涉及用户 / 项目偏好、默认选择、禁用做法时"},
	{"D", "decision", "要复述「为什么这么定」时"},
	{"X", "pitfall", "遇到报错、反常现象、曾被坑过的路径时"},
	{"T", "task", "要确认「上次做到哪 / 还剩什么」时"},
}

// renderKindCodes 渲染类码表。notes、instructionBlock、SessionStart 都消费它，
// 禁止在别处再写一份触发条件。
func renderKindCodes() string {
	var sb strings.Builder
	for _, k := range kindCodes {
		fmt.Fprintf(&sb, "%s %s %s\n", k.Code, k.Kind, k.Trigger)
	}
	return sb.String()
}

func hanLen(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}
