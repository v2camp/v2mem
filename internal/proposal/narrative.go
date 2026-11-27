package proposal

import "regexp"

// isProcessNarrative 判定正文是否仍是「过程叙述」而不是可复用规则。
//
// 为什么在解析层拦而不是只靠提示词：候选正文落库时会被 240 rune 预算截断，
// 过程叙述的结局是「既没有完整原文，也没有提炼后的规则」——2026-11-27 库里
// 唯一的记忆就是这种残句（断在 `- 本机在 /Users`）。提示词负责引导，解析层
// 负责不让它进库。
//
// 判据只取三类高置信特征，宁漏不误杀：
//   - 第一人称 + 动作（我无法 / 我们尝试）：过程叙述的主语形态；
//   - 会话内指代（这个链接 / 那个报错）：脱离本次会话就没有指称对象；
//   - 时点相对词（本次 / 刚才 / 目前还没有）：换会话即失效。
//
// 代价：像「这个报错说明缺凭证」这类缺场景但结论正确的句子也会被拒。它本该
// 改写成「遇到 X 报错时…」，让模型重写一次比入库一句无法执行的话更划算。
var (
	narrativeFirstPerson = regexp.MustCompile(`(^|[。；！？，、\s])(我|我们)(无法|不能|尝试|发现|准备|需要|已经|还没|没有|打不开|读不到)`)
	narrativeDeictic     = regexp.MustCompile(`(这个|那个)(链接|页面|文件|仓库|报错|问题|任务)`)
	narrativeTemporal    = regexp.MustCompile(`本次|刚才|截至目前|目前还(没有|没)`)
)

// 选择器在基础三类之外补齐的两类一次性信号：
//   - 完成回执：已修复/已合入/已上线/已完成/完成（/待办/短 sha 提交；
//   - 显式日期：20xx-xx-xx，存量记忆里常与回执一起出现。
//
// narrativeFirstPersonLax 是选择器专用的宽口径第一人称：正文里出现「我/我们」
// 就说明它还在讲「谁做了什么」。validateDraft 不走这条，避免把
// 「我方…」这类结论句误杀。
var (
	narrativeReceipt        = regexp.MustCompile(`已修复|已合入|已上线|已完成|完成（|待办|提交\s*[0-9a-f]{7}`)
	narrativeDate           = regexp.MustCompile(`20\d{2}-\d{2}-\d{2}`)
	narrativeFirstPersonLax = regexp.MustCompile(`我`)
)

func isProcessNarrative(s string) bool {
	return narrativeFirstPerson.MatchString(s) ||
		narrativeDeictic.MatchString(s) ||
		narrativeTemporal.MatchString(s)
}

// IsProcessNarrative 判断一段正文是否过程叙述/一次性回执，属于「该提炼后才能入库」的形态：
// 第一人称、会话指代、时点词、完成回执（已修复/已合入/已完成/完成（/待办/提交 <7位sha>）、
// 显式日期（20xx-xx-xx）。这是选择器，不是内容改写：命中只代表值得送去模型提炼。
func IsProcessNarrative(content string) bool {
	return isProcessNarrative(content) ||
		narrativeFirstPersonLax.MatchString(content) ||
		narrativeReceipt.MatchString(content) ||
		narrativeDate.MatchString(content)
}
