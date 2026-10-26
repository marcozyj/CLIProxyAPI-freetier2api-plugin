package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	accountpkg "freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// modelTestTimeout 是单次模型连通性测试的上限。
//
// 与真实对话不同，这里只发一句「你好」、把 max_tokens 压到很小，正常情况下
// 秒级返回；给 90 秒是为了容纳排队严重的上游（Qoder 的 p3 队列实测会排几十秒），
// 而不是让一次测试把管理页卡死——页面本身是异步等结果的。
const modelTestTimeout = 90 * time.Second

// modelTestPrompt 是连通性测试的输入：最短的一句中文，既不触发内容策略，
// 也能让「模型其实没通」的上游原样回错误而不是回空。
const modelTestPrompt = "你好"

// modelTestRequest 是「测试模型连通性」的请求体。
type modelTestRequest struct {
	VendorID string `json:"vendor_id"`
	Model    string `json:"model"`
}

// modelTestResult 是单次测试结果。
//
// 失败也走 HTTP 200：页面用 ok 区分，错误信息直接展示在悬浮卡片里。
// 用 4xx/5xx 会让前端 api() 抛出异常，反而拿不到 vendor/model/耗时这些上下文。
type modelTestResult struct {
	OK        bool   `json:"ok"`
	VendorID  string `json:"vendor_id"`
	Vendor    string `json:"vendor_name,omitempty"`
	Model     string `json:"model"`
	Resolved  string `json:"resolved,omitempty"`
	Account   string `json:"account,omitempty"`
	Reply     string `json:"reply,omitempty"`
	Message   string `json:"message,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms"`
}

// modelTestHandler 用真实账号对单个模型发一次最小对话，验证「这个模型真的能用」。
//
// 它只读：不动配置、不写状态、不改模型清单。失败时也不记「死模型」——
// 测试按钮是人工触发的一次性动作，把一次临时失败记成 6 小时摘除太激进，
// 真实对话链路上的 noteModelGone 才是该负责的地方。
func (s *Service) modelTestHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[modelTestRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorID := strings.TrimSpace(body.VendorID)
	model := strings.TrimSpace(body.Model)
	if vendorID == "" || model == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "vendor_id 与 model 都不能为空",
		})
	}

	vendor, okVendor := s.registry.ByID(vendorID)
	if !okVendor {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "未找到供应商 " + vendorID,
		})
	}
	cfg := s.deps.Config()
	if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "供应商 " + vendor.Name() + " 所在区域已被配置关闭",
		})
	}

	// 模型名可能是官方 ID、带渠道标识的名字或手动别名：统一还原成官方 ID，
	// 出站请求才和上游目录对得上。
	_, bare := s.registry.SplitModelID(model)
	bare = s.deps.Model().ResolveOutboundModel(vendor.ID(), bare)
	resolved := strings.TrimSpace(bare)
	if resolved == "" {
		resolved = model
	}

	ctx, cancel := hostbridge.ManagementContext(req, modelTestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)

	loaded, failures := s.pool.Load(ctx, callbackID, accountpkg.Options{
		EnabledRealms: cfg.EnabledRealms,
		Filter: func(account accountpkg.Account) bool {
			return account.Vendor != nil && account.Vendor.ID() == vendor.ID()
		},
	})
	if len(loaded) == 0 {
		message := "该供应商下没有可用账号"
		if len(failures) > 0 && strings.TrimSpace(failures[0].Message) != "" {
			message = failures[0].Message
		}
		return JSONResponse(http.StatusOK, modelTestResult{
			VendorID: vendor.ID(), Vendor: vendor.Name(), Model: model, Resolved: resolved,
			Message: message,
		})
	}

	account := loaded[0]
	payload, errMarshal := json.Marshal(map[string]any{
		"model": resolved,
		"messages": []map[string]any{{
			"role": "user", "content": modelTestPrompt,
		}},
		"max_tokens": 64,
		"stream":     false,
	})
	if errMarshal != nil {
		return JSONResponse(http.StatusOK, modelTestResult{
			VendorID: vendor.ID(), Vendor: vendor.Name(), Model: model, Resolved: resolved,
			Message: "构造测试请求失败：" + errMarshal.Error(),
		})
	}

	started := time.Now()
	response, errExecute := account.Vendor.Execute(ctx, account.Credential, &core.ExecuteRequest{
		Model:   resolved,
		Payload: payload,
	})
	elapsed := time.Since(started)
	label := core.FirstNonEmpty(account.Credential.LabelValue(), account.Entry.Label, account.Entry.Name)
	result := modelTestResult{
		VendorID: vendor.ID(), Vendor: vendor.Name(), Model: model, Resolved: resolved,
		Account: label, ElapsedMS: elapsed.Milliseconds(),
	}
	if errExecute != nil {
		result.Message = errExecute.Error()
		return JSONResponse(http.StatusOK, result)
	}
	result.OK = true
	result.Reply = extractChatReply(response)
	if result.Reply == "" {
		result.Reply = "（上游返回了空内容）"
	}
	return JSONResponse(http.StatusOK, result)
}

// extractChatReply 从上游响应体里取第一段可见文本。
//
// 只做展示用途，因此宽松地兼容 chat-completions 的几种常见形状：
// choices[].message.content（字符串或分段数组）、choices[].text，以及
// 上游把错误塞进 200 响应体时的 error.message。
func extractChatReply(response *pluginapi.ExecutorResponse) string {
	if response == nil || len(response.Payload) == 0 {
		return ""
	}
	var decoded map[string]any
	if errDecode := json.Unmarshal(response.Payload, &decoded); errDecode != nil || decoded == nil {
		return ""
	}
	if reply := firstChoiceText(decoded); reply != "" {
		return truncateRunes(reply, 200)
	}
	if errObj, okErr := decoded["error"].(map[string]any); okErr {
		if message, okMsg := errObj["message"].(string); okMsg {
			return truncateRunes(strings.TrimSpace(message), 200)
		}
	}
	return ""
}

// firstChoiceText 遍历 choices 取第一段文本。
func firstChoiceText(decoded map[string]any) string {
	choices, okChoices := decoded["choices"].([]any)
	if !okChoices {
		return ""
	}
	for _, rawChoice := range choices {
		choice, okChoice := rawChoice.(map[string]any)
		if !okChoice {
			continue
		}
		if message, okMessage := choice["message"].(map[string]any); okMessage {
			if text := contentText(message["content"]); text != "" {
				return text
			}
			if text := contentText(message["reasoning_content"]); text != "" {
				return text
			}
		}
		if text := contentText(choice["text"]); text != "" {
			return text
		}
	}
	return ""
}

// contentText 把 content 字段（字符串或分段数组）压成纯文本。
func contentText(raw any) string {
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		var builder strings.Builder
		for _, item := range value {
			part, okPart := item.(map[string]any)
			if !okPart {
				continue
			}
			if text, okText := part["text"].(string); okText {
				builder.WriteString(text)
			}
		}
		return strings.TrimSpace(builder.String())
	}
	return ""
}

// truncateRunes 按字符截断，避免把多字节汉字截成半个。
func truncateRunes(value string, limit int) string {
	trimmed := strings.TrimSpace(value)
	if limit <= 0 {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) <= limit {
		return trimmed
	}
	return string(runes[:limit]) + "…"
}
