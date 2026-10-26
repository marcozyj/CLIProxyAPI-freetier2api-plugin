package core

import "strings"

// vendorTags 是「模型名渠道标识」用的供应商短标识。
//
// 之所以不直接用 vendorID：workbuddycn / minimaxcodeglobal 这类标识放进模型 ID
// 后偏长，而模型 ID 会出现在客户端配置、日志与错误信息里。缩写只影响对外名字，
// 不改变凭证文件名、禁用键等任何以 vendorID 为键的状态。
//
// 区域变体的缩写保持「品牌 + 区域首字母」，避免 ac / wb 这类短名在同一品牌的
// 国内外实例之间再次撞车。
var vendorTags = map[string]string{
	"autoclawcn":        "ac",
	"autoclawglobal":    "acg",
	"cline":             "cl",
	"codearts":          "ca",
	"minimaxcodecn":     "mm",
	"minimaxcodeglobal": "mmg",
	"opencodezen":       "zen",
	"qodercn":           "qd",
	"qoderglobal":       "qdg",
	"raccoon":           "rc",
	"traecn":            "tr",
	"traeglobal":        "trg",
	"traesolo":          "trs",
	"workbuddycn":       "wb",
	"workbuddyglobal":   "wbg",
	"zcode":             "zc",
}

// VendorTag 返回某供应商实例在模型名里使用的短标识。
//
// 未登记的标识原样返回（小写、去空白）：新增供应商时即使忘了登记，模型名也
// 只会退化成「品牌 ID 后缀」，不会退化成空后缀——空后缀会让不同供应商的同名
// 模型在宿主侧重新变得无法区分。
func VendorTag(vendorID string) string {
	trimmed := strings.ToLower(strings.TrimSpace(vendorID))
	if tag, ok := vendorTags[trimmed]; ok {
		return tag
	}
	return trimmed
}
