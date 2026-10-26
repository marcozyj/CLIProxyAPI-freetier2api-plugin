package core

// 本文件判定「上游明确说这个模型不存在」。
//
// 供应商的 Classify 把上游错误统一成 *Failure，但错误文案与业务码各家不同。
// 这里只认最保守的一小组字面量：宁可漏判（模型继续留在列表里），也不要把
// 一次额度/风控/参数错误误判成「模型不存在」——后者会让清单永久少一个模型。

import "strings"

// modelGonePatterns 是各上游「模型不存在」的常见说法（小写比较）。
//
// 刻意不收录单独出现的 "does not exist" / "not found"：它们也可能来自文件、
// 会话、凭证错误，误判代价高于漏判。
var modelGonePatterns = []string{
	"model not found",
	"model_not_found",
	"invalid model",
	"unknown model",
	"no such model",
	"model does not exist",
	"model is not available",
	"unsupported model",
	"非法模型",
	"模型不存在",
	"模型未找到",
	"不存在的模型",
}

// ModelUnavailable 报告错误是否等价于「上游没有这个模型」。
//
// 只匹配错误文本：上游把这类错误放在 400/404 的 message 里，供应商的
// Failure.Msg 已经保留了原文，不需要再逐家解析结构化的 code。
func ModelUnavailable(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, pattern := range modelGonePatterns {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}
