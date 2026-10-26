//go:build live

package live

// Trae 实况探针：TRAE_LIVE=1 且设置 TRAE_LIVE_CREDS 时运行。
// 依次验证 token 刷新、模型目录、非流式对话、流式对话、签到、额度。
// 只打印截断后的响应，不打印凭证内容。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
	"freetier2api-plugin/internal/vendors/trae"
)

// configureTraeHost 装配直连宿主桥。
//
// 同时支持 host.http.do（非流式）与 host.http.do_stream（SSE）两条路径：
// 流式路径把上游响应整体缓冲后经 stream_read 一次性交付——本地探针验证的是
// 协议正确性，增量投递由宿主负责。
func configureTraeHost(t *testing.T) {
	t.Helper()
	var (
		mu      sync.Mutex
		streams = map[string]*probeStream{}
		nextID  int
	)
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method   string              `json:"method"`
			URL      string              `json:"url"`
			Headers  map[string][]string `json:"headers"`
			Body     []byte              `json:"body"`
			StreamID string              `json:"stream_id"`
		}
		if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
			return nil, errUnmarshal
		}

		switch method {
		case pluginabi.MethodHostHTTPDo:
			status, header, body, errDo := doUpstream(req.Method, req.URL, req.Headers, req.Body)
			if errDo != nil {
				return nil, errDo
			}
			return json.Marshal(map[string]any{"StatusCode": status, "Headers": header, "Body": body})
		case pluginabi.MethodHostHTTPDoStream:
			// 流式请求必须**边读边投**：上游是 SSE，很多端点在客户端读完之前
			// 不会关闭连接，整体 ReadAll 会一直挂住（实测 Qoder 国际版就是）。
			httpReq, errNew := http.NewRequest(req.Method, req.URL, bytes.NewReader(req.Body))
			if errNew != nil {
				return nil, errNew
			}
			for key, values := range req.Headers {
				for _, value := range values {
					httpReq.Header.Add(key, value)
				}
			}
			resp, errDo := (&http.Client{Timeout: 180 * time.Second}).Do(httpReq)
			if errDo != nil {
				return nil, errDo
			}
			mu.Lock()
			nextID++
			streamID := fmt.Sprintf("probe-%d", nextID)
			stream := &probeStream{}
			streams[streamID] = stream
			mu.Unlock()
			go stream.pump(resp.Body)
			return json.Marshal(map[string]any{"status_code": resp.StatusCode, "headers": resp.Header, "stream_id": streamID})
		case pluginabi.MethodHostHTTPStreamRead:
			mu.Lock()
			stream := streams[req.StreamID]
			mu.Unlock()
			var payload []byte
			done := true
			if stream != nil {
				payload, done = stream.pop()
			}
			response := map[string]any{"done": done}
			if len(payload) > 0 {
				response["payload"] = payload
			}
			if stream != nil && stream.err != nil && done {
				response["error"] = stream.err.Error()
			}
			return json.Marshal(response)
		case pluginabi.MethodHostHTTPStreamClose:
			return json.Marshal(map[string]any{})
		default:
			return nil, fmt.Errorf("trae probe host: unsupported method %s", method)
		}
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// probeStream 是探针侧的上游 SSE 缓冲：读协程写入、宿主回调取走。
type probeStream struct {
	mu   sync.Mutex
	buf  []byte
	done bool
	err  error
}

func (s *probeStream) pump(body io.ReadCloser) {
	defer func() { _ = body.Close() }()
	chunk := make([]byte, 32*1024)
	for {
		n, errRead := body.Read(chunk)
		if n > 0 {
			s.mu.Lock()
			s.buf = append(s.buf, chunk[:n]...)
			s.mu.Unlock()
		}
		if errRead != nil {
			s.mu.Lock()
			if errRead != io.EOF {
				s.err = errRead
			}
			s.done = true
			s.mu.Unlock()
			return
		}
	}
}

func (s *probeStream) pop() ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) == 0 {
		return nil, s.done
	}
	out := append([]byte(nil), s.buf...)
	s.buf = nil
	return out, false
}

func doUpstream(method, url string, headers map[string][]string, body []byte) (int, http.Header, []byte, error) {
	var reader io.Reader
	if len(body) > 0 {
		reader = strings.NewReader(string(body))
	}
	httpReq, errNew := http.NewRequest(method, url, reader)
	if errNew != nil {
		return 0, nil, nil, errNew
	}
	for key, values := range headers {
		for _, value := range values {
			httpReq.Header.Add(key, value)
		}
	}
	client := &http.Client{Timeout: 180 * time.Second}
	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return 0, nil, nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		return 0, nil, nil, errRead
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// traeLiveCredential 读取凭证并按 vendor 选择 profile。
//
// 文件里的 access_token 可能已过期为空，先跑一次 RefreshCredential，
// 与插件运行时的刷新链路保持一致。
func traeLiveCredential(t *testing.T) (*trae.Credential, trae.Config) {
	t.Helper()
	if os.Getenv("TRAE_LIVE") == "" {
		t.Skip("TRAE_LIVE 未设置，跳过实况探针")
	}
	path := os.Getenv("TRAE_LIVE_CREDS")
	if path == "" {
		t.Skip("TRAE_LIVE_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}

	cfg := trae.ConfigFor(trae.RegionCN)
	if strings.Contains(strings.ToLower(path), "traesolo") {
		cfg = trae.SoloConfig()
	} else if strings.Contains(strings.ToLower(path), "traeglobal") {
		cfg = trae.ConfigFor(trae.RegionGlobal)
	}
	cred, errParse := trae.ParseCredentialFor(raw, cfg)
	if errParse != nil {
		t.Fatalf("parse creds: %v", errParse)
	}
	configureTraeHost(t)

	if cred.RefreshToken != "" {
		refreshed, changed, errRefresh := trae.RefreshCredential(context.Background(), cred, cfg)
		if errRefresh != nil {
			t.Fatalf("RefreshCredential: %v", errRefresh)
		}
		if changed {
			t.Logf("token 已刷新，expires_at=%d", refreshed.ExpiresAt)
			cred = refreshed
		}
	}
	if cred.AccessToken == "" {
		t.Fatalf("凭证没有可用的 access_token")
	}
	return cred, cfg
}

func traeChatPayload(model string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"model": model,
		"messages": []any{
			map[string]any{"role": "user", "content": "用一句话介绍你自己"},
		},
		"stream": false,
	})
	return payload
}

func TestTraeLiveRefreshAndQuota(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	quota, errQuota := trae.FetchQuota(context.Background(), cred, cfg)
	if errQuota != nil {
		t.Fatalf("FetchQuota: %v", errQuota)
	}
	t.Logf("额度: %+v", quota.Summary)
}

func TestTraeLiveCheckin(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	result, errCheckin := trae.Checkin(context.Background(), cred, cfg)
	if errCheckin != nil {
		t.Fatalf("Checkin: %v", errCheckin)
	}
	t.Logf("签到: already=%v credit=%d msg=%s", result.Already, result.Credit, result.Message)
}

func TestTraeLiveChat(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	body, errChat := trae.Chat(context.Background(), "", trae.ChatRequest{
		Credential: cred,
		Profile:    cfg,
		Body:       traeChatPayload("glm-5.2"),
		Stream:     false,
	})
	if errChat != nil {
		t.Fatalf("Chat: %v", errChat)
	}
	t.Logf("非流式响应: %s", truncateProbe(string(body), 1500))
}

func TestTraeLiveChatStream(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	var (
		builder strings.Builder
		chunks  int
	)
	started := time.Now()
	errStream := trae.ChatStream(context.Background(), "", trae.ChatRequest{
		Credential: cred,
		Profile:    cfg,
		Body:       traeChatPayload("glm-5.2"),
		Stream:     true,
	}, func(chunk []byte) error {
		chunks++
		builder.Write(chunk)
		return nil
	})
	if errStream != nil {
		t.Fatalf("ChatStream: %v", errStream)
	}
	t.Logf("耗时 %s, chunks=%d", time.Since(started), chunks)
	t.Logf("流式响应: %s", truncateProbe(builder.String(), 1500))
	if chunks == 0 {
		t.Fatalf("流式没有产出任何 chunk")
	}
}

func TestTraeLiveModels(t *testing.T) {
	cred, cfg := traeLiveCredential(t)

	models, errModels := trae.FetchModels(context.Background(), cred, cfg)
	if errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	names := make([]string, 0, len(models))
	for _, model := range models {
		names = append(names, model.ID)
	}
	t.Logf("目录模型数=%d: %s", len(models), truncateProbe(strings.Join(names, ","), 800))
}

// TestTraeLiveCatalogRates 打印目录里的倍率覆盖情况。
//
// 没有倍率的条目会触发 models.go 的「键名探针」（只打键名、不打值），
// 用于确认上游是不是把 display_contact_config 搬了家。
func TestTraeLiveCatalogRates(t *testing.T) {
	cred, cfg := traeLiveCredential(t)
	logger.SetLevel("debug")

	models, errModels := trae.FetchModels(context.Background(), cred, cfg)
	if errModels != nil {
		t.Fatalf("FetchModels: %v", errModels)
	}
	missing := 0
	for _, model := range models {
		if model.Credits == "" {
			missing++
		}
	}
	t.Logf("目录: %d 个模型，%d 个无倍率", len(models), missing)
	shown := 0
	for _, model := range models {
		if shown >= 5 {
			break
		}
		shown++
		t.Logf("样例: id=%s credits=%q context=%d output=%d tools=%v",
			model.ID, model.Credits, model.ContextWindow, model.MaxTokens, model.SupportsTools)
	}
}
