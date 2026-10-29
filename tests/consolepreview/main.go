//go:build preview

// 本程序把插件的完整管理面（console.html + 管理接口）跑在一个本地 HTTP
// 服务器上，供浏览器（headless chromium）离线走查 UI：
//
//	go run -tags preview ./tests/consolepreview
//
// 路由：
//   - GET  /console                          控制台页面（admin.Page 注入 BASE）
//   - ANY  /v0/management/plugins/...        映射到插件的 management.handle
//   - GET  /seed                             写 sessionStorage 管理密钥后跳 /console
//
// 仅用于本地预览，绝不参与常规构建（build tag preview）与发布产物。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/vendors/codearts"
	"freetier2api-plugin/internal/vendors/trae"
	"freetier2api-plugin/internal/vendors/workbuddy"
	"freetier2api-plugin/tests/testhost"
)

const (
	listenAddr = "127.0.0.1:8791"
	pluginBase = "/plugins/freetier2api-fork"
	mgmtPrefix = "/v0/management" + pluginBase
	testKey    = "preview-key"
)

var testApp *app.App

func main() {
	host := testhost.New()
	hostbridge.Configure(host.Call)
	host.SetUpstream(stubUpstream)

	seedAccounts(host)

	cfg := app.DefaultConfig()
	cfg.StateDir = "/tmp/freetier2api-preview-state"
	cfg.LogLevel = "debug"
	cfg.LogToFile = false
	_ = os.MkdirAll(cfg.StateDir, 0o755)

	instance, errNew := app.New(app.Options{Version: "preview", Factories: app.BuiltinFactories()})
	if errNew != nil {
		log.Fatalf("new app: %v", errNew)
	}
	testApp = instance
	if errApply := instance.ApplyConfig(cfg); errApply != nil {
		log.Fatalf("apply config: %v", errApply)
	}
	if _, errState := instance.LoadState(cfg); errState != nil {
		log.Fatalf("load state: %v", errState)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/console", func(w http.ResponseWriter, r *http.Request) {
		// 与宿主一致：控制台页面是 /v0/resource 资源路由（GET .../console）。
		writeManagement(w, pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource" + pluginBase + "/console",
		})
	})
	mux.HandleFunc("/seed", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><script>
sessionStorage.setItem("cpa-management-key", %q);
location.replace("/console");
</script>`, testKey)
	})
	mux.HandleFunc("/", handleManagement)

	server := &http.Server{Addr: listenAddr, Handler: mux}
	go func() {
		log.Printf("console preview listening on http://%s", listenAddr)
		log.Printf("open http://%s/console (or /seed to prime the key)", listenAddr)
		if errListen := server.ListenAndServe(); errListen != nil && errListen != http.ErrServerClosed {
			log.Fatalf("listen: %v", errListen)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Printf("shutting down")
	instance.StopBackgroundWork()
}

// seedAccounts 预置三个账号：traecn（39 模型带倍率）、workbuddycn（嵌套凭证形态）、
// opencodezen（API key 形态）。足以覆盖控制台的账号表、筛选、批量与签到历史列。
func seedAccounts(host *testhost.Host) {
	host.AddAccount("1", "traecn-preview.json", `{"vendor":"traecn","region":"cn","access_token":"preview-trae-token","uid":"3577174322063482","label":"Trae-章鱼哥"}`)
	host.AddAccount("2", "opencodezen-preview.json", `{"api_key":"sk-preview-opencode","label":"zen-preview"}`)
	host.AddAccount("3", "workbuddycn-preview.json", workbuddyCredential("wb-cn-uid"))
}

func workbuddyCredential(uid string) string {
	return fmt.Sprintf(`{
  "auth": {"accessToken": "preview-at", "refreshToken": "preview-rt", "expiresAt": %d, "domain": "%s", "realm": "cn"},
  "account": {"uid": %q, "nickname": "预览账号"},
  "device_token": "preview-dt"
}`, time.Now().Add(24*time.Hour).Unix(), workbuddy.DefaultDomainFor("cn"), uid)
}

// traeCatalogStub 生成 Trae 目录应答：两个配置、四个 wire 模型、倍率齐全
// （display_contact_config 是 JSON 字符串包对象，与线上形态一致）。
func traeCatalogStub() string {
	rate := func(v string) string {
		inner := fmt.Sprintf(`{"consumption_rate":{"enable":true,"data":{"enable":true,"rate":%q}}}`, v)
		encoded, err := json.Marshal(inner) // 字符串包对象：外层必须是 JSON 字符串
		if err != nil {
			panic(err)
		}
		return string(encoded)
	}
	item := func(name, rateVal string, ctx int) string {
		return fmt.Sprintf(`{"config_name":%q,"config_switch":true,"usage":"chat_completion","context_window_tokens":{"dev":%d},`+
			`"display_config":{"display_name":%q,"model_capability":"agent"},"model_detail_list":[{"model_name":%q,"max_tokens":8192}],`+
			`"display_contact_config":%s}`, name, ctx, name, name+"__dev", rate(rateVal))
	}
	return fmt.Sprintf(`{"config_info_list":[%s,%s,%s,%s]}`,
		item("Kimi K3.1 Pro", "0.94", 116000),
		item("DeepSeek V4.1 Flash", "0.06", 116000),
		item("Doubao Seed 2.1 Pro", "1.20", 256000),
		item("GLM 5.3 Flash", "0.83", 116000))
}

// workbuddyCatalogStub 生成 WorkBuddy 目录应答：同时兼容 builtinModels（代码
// 内置目录探测）与 agents+models（企业目录 v3 形态）两种解析路径。
func workbuddyCatalogStub() string {
	// 注意：workbuddy 的 doJSON 会先解一层 {code,msg,data} 信封，这里给的是 data。
	inner := `{"agents":[{"name":"cli","models":["glm-5.1","ds4.1-flash"]}],` +
		`"models":[{"id":"glm-5.1","name":"GLM-5.1","credits":"1","maxInputTokens":128000,"maxOutputTokens":32768,"supportsImages":true},` +
		`{"id":"ds4.1-flash","name":"DS 4.1 Flash","credits":"0.5","maxInputTokens":128000,"maxOutputTokens":32768}]}`
	return `{"code":0,"msg":"ok","data":` + inner + `}`
}

// modelsDevStub 生成 models.dev 聚合目录（只含 ZEN provider 的两个模型）。
func modelsDevStub() string {
	// cost 单价必须为 0：freeOnly 只保留目录里已知免费的模型。
	return `{"opencode":{"models":{"glm-5.1":{"name":"GLM-5.1","tool_call":true,"cost":{"input":0,"output":0},` +
		`"limit":{"context":128000,"output":32768},"modalities":{"input":["text","image"]}},` +
		`"ds4.1-flash":{"name":"DS 4.1 Flash","reasoning":true,"tool_call":true,"cost":{"input":0,"output":0},` +
		`"limit":{"context":128000,"output":32768},"modalities":{"input":["text"]}}}}}`
}

// zenCatalogStub 生成 opencodezen 的 OpenAI 兼容 /models 应答。
func zenCatalogStub() string {
	return `{"data":[{"id":"glm-5.1","object":"model","owned_by":"zai"},{"id":"ds4.1-flash","object":"model","owned_by":"deepseek"}]}`
}

// debugStub 由 FREETIER_PREVIEW_STUB_DEBUG 打开：把每个出站请求打到日志。
var debugStub = os.Getenv("FREETIER_PREVIEW_STUB_DEBUG") != ""

// stubUpstream 给预览需要的上游端点返回最小成功应答；其余 404。
// wbChatSSE 是 WorkBuddy /v2/chat/completions 的最小 SSE 应答。
const wbChatSSE = "data: {\"id\":\"preview\",\"choices\":[{\"delta\":{\"content\":\"你好！我是预览应答。\"}}]}\n" +
	"\n" +
	"data: [DONE]\n\n"

func stubUpstream(method, url, body string) testhost.UpstreamResponse {
	if debugStub {
		log.Printf("[stub] %s %s", method, url)
	}
	switch {
	// opencodezen 必须先于通用 /chat/completions 判定：免费层强制流式，
	// 通用分支的普通 JSON 会被 SSE 聚合器整段跳过（表现为「上游返回了空内容」）。
	case strings.Contains(url, "opencode.ai/inference/v1/models"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: zenCatalogStub()}
	case strings.Contains(url, "opencode.ai"):
		// 免费层强制 stream=true，SSE 分片聚合回 JSON。
		return testhost.UpstreamResponse{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: "data: " + `{"id":"preview","choices":[{"delta":{"content":"你好！"}}]}` + "\n\n" +
				"data: " + `{"id":"preview","choices":[{"delta":{"content":"我是预览应答。"}}]}` + "\n\n" +
				"data: [DONE]\n\n",
		}
	// WorkBuddy /v2/chat/completions：上游强制流式（OpenAI SSE 形态），
	// 必须先于通用 JSON 分支（同 zen 的分支顺序问题）。
	case strings.Contains(url, "/v2/chat/completions"):
		return testhost.UpstreamResponse{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:   wbChatSSE,
		}
	// 模型测试按钮会对账号发一次最小对话：返回一段无 SSE 的普通 JSON。
	case strings.Contains(url, "/chat/completions"), strings.Contains(url, "/v1/chat"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: `{"id":"preview","choices":[{"message":{"role":"assistant","content":"你好！我是预览应答。"}}],"usage":{"total_tokens":8}}`}
	// WorkBuddy /v3/config 与企业模型目录（信封内 agents+models）。
	case strings.Contains(url, "/v3/config"), strings.Contains(url, "enterprises/personal/models"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: workbuddyCatalogStub()}
	// models.dev 聚合目录：map[provider]{models:{id:entry}}。
	case strings.Contains(url, "models.dev"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: modelsDevStub()}
	// WorkBuddy 登录态/徽章/额度探测：保证账号显示为健康。
	case strings.Contains(url, "badge"), strings.Contains(url, "resource-summary"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: `{"error_code":"0","result":{"remain_credit":"10.5","nickname":"预览账号"}}`}
	// Trae 目录（get_detail_param）。
	case strings.Contains(url, trae.EpModels):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: traeCatalogStub()}
	// Trae llm_raw_chat 是 event 流（event:output + data:{response}）。
	case strings.Contains(url, "llm_raw_chat"):
		return testhost.UpstreamResponse{
			Status: http.StatusOK,
			Header: http.Header{"Content-Type": []string{"text/event-stream"}},
			Body: "event: output\ndata: " + `{"response":"你好！我是预览应答。"}` + "\n\n" +
				"event: token_usage\ndata: " + `{"prompt_tokens":1,"completion_tokens":8}` + "\n\n",
		}
	// WorkBuddy 内置/企业模型目录（v3 目录形态：agents 白名单 + models 明细）。
	case strings.Contains(url, codearts.EpModelBuiltin), strings.Contains(url, "enterprises/personal/models"):
		return testhost.UpstreamResponse{Status: http.StatusOK, Body: workbuddyCatalogStub()}
	}
	return testhost.UpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404,"msg":"preview not stubbed: ` + url + `"}`}
}

func handleManagement(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if !strings.HasPrefix(path, "/v0/management/") {
		http.NotFound(w, r)
		return
	}
	// 宿主级管理路径（OAuth 回调等）预览环境一律 404。
	if !strings.HasPrefix(path, mgmtPrefix+"/") && path != mgmtPrefix {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"host route not available in preview"}`))
		return
	}

	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
	}
	query := r.URL.Query()
	headers := http.Header{}
	for key, values := range r.Header {
		headers[key] = values
	}
	writeManagement(w, pluginapi.ManagementRequest{
		Method:  r.Method,
		Path:    path,
		Headers: headers,
		Query:   query,
		Body:    body,
	})
}

// writeManagement 把一次 ManagementRequest 打进插件并回写 HTTP 响应。
func writeManagement(w http.ResponseWriter, request pluginapi.ManagementRequest) {
	raw, errHandle := testApp.HandleMethod(pluginabi.MethodManagementHandle, mustMarshal(request))
	if errHandle != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":%q}`, errHandle.Error())
		return
	}
	var env struct {
		OK     bool                         `json:"ok"`
		Result pluginapi.ManagementResponse `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil || !env.OK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"plugin envelope not ok: %s"}`, string(raw))
		return
	}
	status := env.Result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	for key, values := range env.Result.Headers {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(env.Result.Body)
}

func mustMarshal(value any) []byte {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		panic(errMarshal)
	}
	return raw
}
