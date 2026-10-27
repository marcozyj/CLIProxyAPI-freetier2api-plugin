package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
)

// TestExtractChatReplyShapes 验证结果展示能读懂上游的几种常见响应形状。
func TestExtractChatReplyShapes(t *testing.T) {
	cases := map[string]string{
		"字符串 content": `{"choices":[{"message":{"role":"assistant","content":"你好！"}}]}`,
		"分段 content":  `{"choices":[{"message":{"content":[{"type":"text","text":"你"},{"type":"text","text":"好"}]}}]}`,
		"纯文本补全":       `{"choices":[{"text":"你好"}]}`,
		"只有推理内容":      `{"choices":[{"message":{"reasoning_content":"先想一想"}}]}`,
		"错误塞进 200 响应": `{"error":{"message":"model is busy"}}`,
		"空 choices":   `{"choices":[]}`,
		"非 JSON":      `not-json`,
	}
	for name, payload := range cases {
		got := extractChatReply(&pluginapi.ExecutorResponse{Payload: []byte(payload)})
		switch name {
		case "字符串 content":
			if got != "你好！" {
				t.Fatalf("%s: got %q", name, got)
			}
		case "分段 content", "纯文本补全":
			if got != "你好" {
				t.Fatalf("%s: got %q", name, got)
			}
		case "只有推理内容":
			if got != "先想一想" {
				t.Fatalf("%s: got %q", name, got)
			}
		case "错误塞进 200 响应":
			if got != "model is busy" {
				t.Fatalf("%s: got %q", name, got)
			}
		default:
			if got != "" {
				t.Fatalf("%s: got %q, want empty", name, got)
			}
		}
	}
}

// TestTruncateRunes 验证按字符截断：中文不能被截成半个 UTF-8 序列。
func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes(" 你好世界 ", 2); got != "你好…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncateRunes("你好", 5); got != "你好" {
		t.Fatalf("short = %q", got)
	}
	if got := truncateRunes("  ", 3); got != "" {
		t.Fatalf("blank = %q", got)
	}
}

// modelTestResponse 是测试里解析管理响应用的形状。
type modelTestResponse struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error"`
	Data  modelTestResult `json:"-"`
}

// callModelTest 调一次测试接口并解析响应体。
func callModelTest(t *testing.T, service *Service, payload string) (int, modelTestResult, string) {
	t.Helper()
	response := service.modelTestHandler(pluginapi.ManagementRequest{
		Method: "POST",
		Path:   "/plugins/freetier2api-fork/models/test",
		Body:   []byte(payload),
	})
	var raw struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(response.Body, &raw)
	if raw.Error != "" {
		return response.StatusCode, modelTestResult{}, raw.Error
	}
	var result modelTestResult
	if errUnmarshal := json.Unmarshal(response.Body, &result); errUnmarshal != nil {
		t.Fatalf("decode result: %v (body=%s)", errUnmarshal, response.Body)
	}
	return response.StatusCode, result, ""
}

// TestModelTestHandlerValidatesInput 验证空参数/未知供应商直接 400，且给中文原因。
func TestModelTestHandlerValidatesInput(t *testing.T) {
	deps := newStubDeps(t)
	if errRegister := deps.registry.Register(&fakeVendor{Meta: core.Meta{
		VendorID: "zcode", VendorName: "ZCode", VendorRegion: "global",
	}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	service := newServiceWithDeps(t, deps)

	status, _, message := callModelTest(t, service, `{"vendor_id":"","model":"GLM-5.3"}`)
	if status != 400 || !strings.Contains(message, "不能为空") {
		t.Fatalf("empty ids: status=%d message=%q", status, message)
	}

	status, _, message = callModelTest(t, service, `{"vendor_id":"nope","model":"GLM-5.3"}`)
	if status != 400 || !strings.Contains(message, "未找到供应商") {
		t.Fatalf("unknown vendor: status=%d message=%q", status, message)
	}

	status, _, _ = callModelTest(t, service, `{`)
	if status != 400 {
		t.Fatalf("bad json: status=%d", status)
	}
}

// TestModelTestHandlerWithoutAccounts 验证没有凭证时返回可读原因而不是报错。
func TestModelTestHandlerWithoutAccounts(t *testing.T) {
	deps := newStubDeps(t)
	if errRegister := deps.registry.Register(&fakeVendor{Meta: core.Meta{
		VendorID: "zcode", VendorName: "ZCode", VendorRegion: "global",
	}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method == pluginabi.MethodHostAuthList {
			return json.RawMessage("[]"), nil
		}
		return json.RawMessage("{}"), nil
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
	service := newServiceWithDeps(t, deps)

	status, result, _ := callModelTest(t, service, `{"vendor_id":"zcode","model":"GLM-5.3"}`)
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if result.OK {
		t.Fatalf("must not report success without accounts: %+v", result)
	}
	if !strings.Contains(result.Message, "没有可用账号") {
		t.Fatalf("message = %q", result.Message)
	}
	if result.Resolved != "GLM-5.3" {
		t.Fatalf("resolved = %q", result.Resolved)
	}
}

// TestConsoleModelsExposeTaggedID 验证控制台展示的是客户端真正能调通的 ID：
// 默认 suffix 模式下必须带供应商缩写后缀，否则用户照着页面填会 404。
func TestConsoleModelsExposeTaggedID(t *testing.T) {
	deps := newStubDeps(t)
	if errRegister := deps.registry.Register(&fakeVendor{Meta: core.Meta{
		VendorID: "zcode", VendorName: "ZCode", VendorRegion: "global",
	}}); errRegister != nil {
		t.Fatal(errRegister)
	}
	deps.manager.Configure(ModelSettings{ModelNameTag: ModelNameTagSuffix})
	if ok := deps.manager.CacheProbedModels("zcode", []pluginapi.ModelInfo{
		{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"},
	}); !ok {
		t.Fatal("cache models")
	}
	service := newServiceWithDeps(t, deps)
	vendor, okVendor := deps.registry.ByID("zcode")
	if !okVendor {
		t.Fatal("vendor missing")
	}

	models := service.consoleModelsForVendor(deps.manager, vendor)
	if len(models) != 1 {
		t.Fatalf("models = %+v", models)
	}
	if models[0].ID != "GLM-5.3" {
		t.Fatalf("id must stay the official id, got %q", models[0].ID)
	}
	if models[0].PublicID != "GLM-5.3-zc" {
		t.Fatalf("public id = %q, want GLM-5.3-zc", models[0].PublicID)
	}
}
