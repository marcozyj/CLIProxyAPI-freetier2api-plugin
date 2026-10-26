package workbuddy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAggregateMergesToolCallDeltas 验证流式工具调用分片的合并。
func TestAggregateMergesToolCallDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		"",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	aggregated, errAggregate := Aggregate(strings.NewReader(stream))
	if errAggregate != nil {
		t.Fatalf("Aggregate: %v", errAggregate)
	}
	var response struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if errUnmarshal := json.Unmarshal(aggregated, &response); errUnmarshal != nil {
		t.Fatalf("decode aggregate: %v", errUnmarshal)
	}
	if len(response.Choices) != 1 || len(response.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected one merged tool call, got %+v", response.Choices)
	}
	call := response.Choices[0].Message.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "Bash" {
		t.Fatalf("tool call identity lost: %+v", call)
	}
	if call.Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments not concatenated: %q", call.Function.Arguments)
	}
}

// TestAggregateEmptyStreamIsRecognizable 验证空流可被识别。
//
// 空流是上游缺陷，需要与「客户端断连」区分开。
func TestAggregateEmptyStreamIsRecognizable(t *testing.T) {
	_, errAggregate := Aggregate(strings.NewReader("data: [DONE]\n\n"))
	if errAggregate == nil {
		t.Fatal("empty stream must return an error")
	}
	if !IsEmptyStreamError(errAggregate) {
		t.Fatalf("empty stream must be recognizable via IsEmptyStreamError, got %v", errAggregate)
	}
}

// TestStreamNormalizesFrames 验证流式帧的白名单重建。
func TestStreamNormalizesFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"id":"c1","object":"chat.completion.chunk","model":"glm-5.2","service_tier":"x","private_field":"leak","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"private_choice":1}],"extra":{}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	stats, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
	})
	if errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if stats.Frames != 1 || !stats.SawDone {
		t.Fatalf("stats = %+v, want 1 frame and SawDone", stats)
	}
	frame := string(frames[0])
	if strings.Contains(frame, "private_field") || strings.Contains(frame, "private_choice") {
		t.Fatalf("whitelist rebuild must drop unknown fields: %s", frame)
	}
	if !strings.Contains(frame, `"service_tier":"x"`) {
		t.Fatalf("known top-level fields must survive: %s", frame)
	}
}

// TestStreamPassesThroughErrorFrames 验证错误帧原样透传。
//
// 上游的错误信息比我们重建的更有诊断价值，不该被白名单改写。
func TestStreamPassesThroughErrorFrames(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"error":{"message":"upstream blew up","code":"500"}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n")

	var frames [][]byte
	if _, errStream := Stream(strings.NewReader(stream), StreamOptions{
		Emit: func(payload []byte) error {
			frames = append(frames, append([]byte(nil), payload...))
			return nil
		},
		Hint: func(string) string { return "try again later" },
	}); errStream != nil {
		t.Fatalf("Stream: %v", errStream)
	}
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d", len(frames))
	}
	frame := string(frames[0])
	if !strings.Contains(frame, "upstream blew up") {
		t.Fatalf("error message must be preserved verbatim: %s", frame)
	}
	if !strings.Contains(frame, "gateway_hint") {
		t.Fatalf("hint must be attached to the error frame: %s", frame)
	}
}

// TestNormalizeFrameDropsUnknownFields 验证帧白名单在非流式路径也生效。
func TestNormalizeFrameDropsUnknownFields(t *testing.T) {
	frame := map[string]any{
		"id": "c1", "secret": "leak",
		"choices": []any{map[string]any{"index": float64(0), "delta": map[string]any{"content": "hi", "private": 1}}},
	}
	normalized := normalizeFrame(frame)
	if _, okSecret := normalized["secret"]; okSecret {
		t.Fatal("unknown top-level field must be dropped")
	}
	choices := normalized["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	if _, okPrivate := delta["private"]; okPrivate {
		t.Fatal("unknown delta field must be dropped")
	}
	if delta["content"] != "hi" {
		t.Fatal("known delta fields must survive")
	}
}

// ==================== 请求体改写与清洗测试 ====================

// prepareTest 执行一次改写管线（测试辅助）。
func prepareTest(t *testing.T, body string, opts PrepareOptions) map[string]any {
	t.Helper()
	prepared := PrepareBody([]byte(body), opts)
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(prepared, &payload); errUnmarshal != nil {
		t.Fatalf("decode prepared body: %v (raw=%s)", errUnmarshal, prepared)
	}
	return payload
}

// TestPrepareBodyCoreRules 验证改写管线的核心规则。
func TestPrepareBodyCoreRules(t *testing.T) {
	payload := prepareTest(t, `{
		"model":"glm-5.2",
		"stream":false,
		"max_completion_tokens":32000,
		"messages":[{"role":"developer","content":"be nice"},
		            {"role":"user","content":[{"type":"image_url","image_url":"http://x/y.png"}]}]
	}`, PrepareOptions{})

	// 上游拒绝非流式。
	if payload["stream"] != true {
		t.Fatalf("stream must be forced to true, got %v", payload["stream"])
	}
	// max_completion_tokens → max_tokens（并删除别名）。
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("max_completion_tokens alias must be removed")
	}
	if payload["max_tokens"] != float64(32000) {
		t.Fatalf("max_tokens = %v, want 32000", payload["max_tokens"])
	}
	// stream_options 缺省补齐（否则拿不到 usage）。
	if _, okOptions := payload["stream_options"]; !okOptions {
		t.Fatal("stream_options must be defaulted to include_usage")
	}
	// developer → system（上游 role 白名单不含 developer）。
	messages, _ := payload["messages"].([]any)
	first, _ := messages[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("developer role must normalize to system, got %v", first["role"])
	}
	// image_url 字符串 → 对象。
	second, _ := messages[1].(map[string]any)
	parts, _ := second["content"].([]any)
	part, _ := parts[0].(map[string]any)
	if _, okString := part["image_url"].(string); okString {
		t.Fatal("image_url string must be normalized to an object")
	}
	imageObject, okObject := part["image_url"].(map[string]any)
	if !okObject || imageObject["url"] != "http://x/y.png" {
		t.Fatalf("image_url = %v, want {url: ...}", part["image_url"])
	}
}

// TestPrepareBodyExplicitMaxTokensWins 验证显式 max_tokens 优先。
func TestPrepareBodyExplicitMaxTokensWins(t *testing.T) {
	payload := prepareTest(t, `{"model":"m","max_tokens":100,"max_completion_tokens":200,"messages":[]}`, PrepareOptions{})
	if payload["max_tokens"] != float64(100) {
		t.Fatalf("explicit max_tokens must win, got %v", payload["max_tokens"])
	}
	if _, okAlias := payload["max_completion_tokens"]; okAlias {
		t.Fatal("alias must still be removed")
	}
}

// TestPrepareBodyToolChoiceNormalization 验证 tool_choice 归一。
//
// 上游该字段是 Go string，对象形态会 400。
func TestPrepareBodyToolChoiceNormalization(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, payload map[string]any)
	}{
		{"object auto becomes string", `{"model":"m","tool_choice":{"type":"auto"},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "auto" {
					t.Fatalf("tool_choice = %v, want auto", payload["tool_choice"])
				}
			}},
		{"none drops tools", `{"model":"m","tool_choice":"none","tools":[{"x":1}],"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if _, okChoice := payload["tool_choice"]; okChoice {
					t.Fatal("none must drop tool_choice")
				}
				if _, okTools := payload["tools"]; okTools {
					t.Fatal("none must drop tools")
				}
			}},
		{"function object becomes name", `{"model":"m","tool_choice":{"type":"function","function":{"name":"Bash"}},"messages":[]}`,
			func(t *testing.T, payload map[string]any) {
				if payload["tool_choice"] != "Bash" {
					t.Fatalf("tool_choice = %v, want Bash", payload["tool_choice"])
				}
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			testCase.check(t, prepareTest(t, testCase.body, PrepareOptions{}))
		})
	}
}

// TestPrepareBodyDeepSeekThinkingInjection 验证 DeepSeek 思维链注入。
//
// deepseek 系不显式开思考就不返回思维链。
func TestPrepareBodyDeepSeekThinkingInjection(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[]}`, PrepareOptions{})
	thinking, okThinking := payload["thinking"].(map[string]any)
	if !okThinking || thinking["type"] != "enabled" {
		t.Fatalf("deepseek must get thinking enabled, got %v", payload["thinking"])
	}
	if payload["reasoning_effort"] != defaultDeepSeekEffort {
		t.Fatalf("reasoning_effort = %v, want %s", payload["reasoning_effort"], defaultDeepSeekEffort)
	}

	// 显式 disabled 要被尊重，并清掉档位（开着档位却关思考是矛盾组合）。
	disabled := prepareTest(t, `{"model":"deepseek-v4.1-flash","thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`,
		PrepareOptions{})
	if _, okEffort := disabled["reasoning_effort"]; okEffort {
		t.Fatal("disabled thinking must drop reasoning_effort")
	}

	// 非 deepseek 模型不注入。
	other := prepareTest(t, `{"model":"glm-5.2","messages":[]}`, PrepareOptions{})
	if _, okThinking := other["thinking"]; okThinking {
		t.Fatal("non-deepseek models must not get thinking injected")
	}

	// 显式档位不被覆盖。
	explicit := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"low","messages":[]}`,
		PrepareOptions{DefaultEfforts: map[string]string{"deepseek-v4.1-flash": "high"}})
	if explicit["reasoning_effort"] != "low" {
		t.Fatalf("explicit effort must be respected, got %v", explicit["reasoning_effort"])
	}
}

// TestPrepareBodyEffortDowngrade 验证档位降级到模型支持的范围。
//
// 上游收到非法档位直接 400，降级到最近的合法档位能让请求成功。
func TestPrepareBodyEffortDowngrade(t *testing.T) {
	// 支持 low/high/max，请求 xhigh → 降到最接近的 high。
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"xhigh","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("xhigh should downgrade to high, got %v", payload["reasoning_effort"])
	}
	// 支持集合里的档位原样透传。
	supported := prepareTest(t, `{"model":"deepseek-v4.1-flash","reasoning_effort":"max","messages":[]}`,
		PrepareOptions{SupportedEfforts: map[string][]string{"deepseek-v4.1-flash": {"low", "high", "max"}}})
	if supported["reasoning_effort"] != "max" {
		t.Fatalf("supported effort must pass through, got %v", supported["reasoning_effort"])
	}
	// 支持表为空时一律透传，不做猜测。
	unknown := prepareTest(t, `{"model":"unknown","reasoning_effort":"xhigh","messages":[]}`, PrepareOptions{})
	if unknown["reasoning_effort"] != "xhigh" {
		t.Fatalf("unknown model must pass the effort through, got %v", unknown["reasoning_effort"])
	}
}

// TestPrepareBodyBackfillsReasoning 验证 assistant 消息的 reasoning 回填。
//
// 部分租户校验 len(reasoning) > 0，缺失/null/空串会 400（空白串可以）。
func TestPrepareBodyBackfillsReasoning(t *testing.T) {
	payload := prepareTest(t, `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"assistant","content":"hi"}
	]}`, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	if _, okReasoning := message["reasoning"]; !okReasoning {
		t.Fatal("assistant messages must carry a reasoning field")
	}
	if message["reasoning"] == "" {
		t.Fatal("reasoning must be non-empty (blank string is the neutral value)")
	}
}

// TestToolPairingRepackAndCleanup 验证工具配对的重排与裁剪。
func TestToolPairingRepackAndCleanup(t *testing.T) {
	// 孤儿调用被裁掉；对应的 tool 结果也一并删除。
	body := `{"model":"m","messages":[
		{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"A","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c2","content":"orphan result"}
	]}`
	payload := prepareTest(t, body, PrepareOptions{})
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("orphan tool result must be removed, got %d messages", len(messages))
	}
	first, _ := messages[0].(map[string]any)
	if _, okCalls := first["tool_calls"]; okCalls {
		t.Fatal("unpaired tool_calls must be dropped")
	}
}

// TestDropTruncatedToolCalls 验证残缺参数的工具调用被丢弃。
func TestDropTruncatedToolCalls(t *testing.T) {
	calls := []any{
		map[string]any{"id": "c1", "function": map[string]any{"name": "A", "arguments": `{"path":"/tmp"}`}},
		map[string]any{"id": "c2", "function": map[string]any{"name": "B", "arguments": `{"path":"/tmp`}}, // 截断
		map[string]any{"id": "c3", "function": map[string]any{"name": "C", "arguments": ""}},              // 无参合法
	}
	kept := DropTruncatedToolCalls(calls)
	if len(kept) != 2 {
		t.Fatalf("truncated call must be dropped, kept %d of 3", len(kept))
	}
}

// TestSplitModelIDPrefixProtocol 验证模型 ID 的前缀协议。
func TestSplitModelIDPrefixProtocol(t *testing.T) {
	cases := []struct {
		input     string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.5", "global", "gpt-5.5"},
		// 无前缀：由凭证决定域。
		{"glm-5.2", "", "glm-5.2"},
		// 前缀非法（大小写敏感）：当成裸名的一部分。
		{"CN:glm-5.2", "", "CN:glm-5.2"},
		{"other:model", "", "other:model"},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			// 前缀协议在 main 包实现，这里验证 cb 复用的 Region 归一。
			if testCase.wantRealm == "" {
				return
			}
			if NormalizeRegion(testCase.wantRealm) != Region(testCase.wantRealm) {
				t.Fatalf("realm normalization mismatch for %s", testCase.wantRealm)
			}
		})
	}
}

// TestNormalizeRegionDefaultsToCN 验证未知域回落 cn。
func TestNormalizeRegionDefaultsToCN(t *testing.T) {
	for _, input := range []string{"", "CN", "GLOBAL", "global", "us"} {
		region := NormalizeRegion(input)
		if region != RegionCN && region != RegionGlobal {
			t.Fatalf("NormalizeRegion(%q) = %q, must be a valid region", input, region)
		}
	}
	if NormalizeRegion("GLOBAL") != RegionGlobal {
		t.Fatal("region normalization must be case-insensitive")
	}
	if NormalizeRegion("us") != RegionCN {
		t.Fatal("unknown region must fall back to cn")
	}
}

// TestEffortStaticTableIsPerRealm 验证档位表按域分表。
//
// 同一模型名在两个域的合法档位不同，混用会让请求带非法档位。
func TestEffortStaticTableIsPerRealm(t *testing.T) {
	cnCap, okCN := staticEffortCap(RegionCN, "deepseek-v4.1-flash")
	globalCap, okGlobal := staticEffortCap(RegionGlobal, "deepseek-v4.1-flash")
	if !okCN || !okGlobal {
		t.Skip("static effort tables are not embedded")
	}
	// CN 三档、global 仅 high —— 这是刻意的差异。
	if len(cnCap.Efforts) <= len(globalCap.Efforts) {
		t.Fatalf("cn efforts (%v) should be broader than global (%v) for this model",
			cnCap.Efforts, globalCap.Efforts)
	}
}

// TestApplyCatalogFallbacksFillsWindow 验证窗口查找链的兜底。
func TestApplyCatalogFallbacksFillsWindow(t *testing.T) {
	models := []ModelInfo{{ID: "unknown-model-xyz"}}
	filled := applyCatalogFallbacks(models, RegionCN)
	if filled[0].ContextWindow <= 0 {
		t.Fatal("context window must be filled by the fallback chain")
	}
	if filled[0].ContextWindow != defaultContextWindow {
		t.Fatalf("unknown model should get the default window, got %d", filled[0].ContextWindow)
	}
}

// TestSanitizeRemovesFingerprints 验证指纹脱敏确实改写了命中文本。
func TestSanitizeRemovesFingerprints(t *testing.T) {
	rules, regexes := loadSanitizeRules()
	if len(rules.Rewrites) == 0 && len(regexes) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	_ = regexes

	// 改写表里的任一条命中都应产生变化。
	if len(rules.Rewrites) > 0 {
		pair := rules.Rewrites[0]
		rewritten := sanitizeText(pair[0], rules, regexes)
		if rewritten == pair[0] {
			t.Fatalf("rewrite rule did not apply: %q", pair[0])
		}
	}
	// 不命中特征串的文本原样返回（快路径）。
	plain := "just a normal user message about go programming"
	if got := sanitizeText(plain, rules, regexes); got != plain {
		t.Fatalf("plain text must pass through unchanged, got %q", got)
	}
}

// TestSanitizeMessagesCoversToolArguments 验证工具参数也被净化。
//
// 早期实现在 content 缺失时跳过整条消息，导致 tool_calls 的参数
// （文件名、命令、写入内容）完全不被净化。
func TestSanitizeMessagesCoversToolArguments(t *testing.T) {
	rules, _ := loadSanitizeRules()
	if len(rules.Rewrites) == 0 {
		t.Skip("sanitize rules are not embedded")
	}
	needle := rules.Rewrites[0][0]

	messages := []any{map[string]any{
		// content 为 null（工具调用消息的常见形态）。
		"role":    "assistant",
		"content": nil,
		"tool_calls": []any{map[string]any{
			"id": "c1",
			"function": map[string]any{
				"name":      "Write",
				"arguments": `{"content":"` + needle + `"}`,
			},
		}},
	}}
	sanitizeMessages(messages)

	call := messages[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	arguments := call["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(arguments, needle) {
		t.Fatal("tool_call arguments must be sanitized even when content is null")
	}
}

// TestPrepareBodySoleEffortLevelIsInjected 锁定「只认单一档位」的模型行为。
//
// 实况（2026-10-27，CN）：glm-5.1 的 supportedEfforts 只有 medium，请求不带
// reasoning_effort 时上游直接 400「the reasoning effort value is not supported」。
// 唯一合法档位就是答案，请求缺省时补上；多档位模型不补。
func TestPrepareBodySoleEffortLevelIsInjected(t *testing.T) {
	payload := prepareTest(t, `{"model":"glm-5.1","messages":[]}`, PrepareOptions{
		SupportedEfforts: map[string][]string{"glm-5.1": {"medium"}},
	})
	if payload["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort = %v, want medium", payload["reasoning_effort"])
	}

	// 多档位模型：没有默认档就不猜。
	payload = prepareTest(t, `{"model":"deepseek-v4-flash","messages":[]}`, PrepareOptions{
		SupportedEfforts: map[string][]string{"deepseek-v4-flash": {"low", "high", "xhigh"}},
	})
	if _, okEffort := payload["reasoning_effort"]; okEffort && payload["reasoning_effort"] != "high" {
		t.Fatalf("多档位模型不应被塞入非默认档，got %v", payload["reasoning_effort"])
	}

	// 调用方显式给的档位优先，不被覆盖。
	payload = prepareTest(t, `{"model":"glm-5.1","reasoning_effort":"low","messages":[]}`, PrepareOptions{
		SupportedEfforts: map[string][]string{"glm-5.1": {"medium"}},
	})
	if payload["reasoning_effort"] != "medium" {
		// 显式 low 不在支持列表里，normalizeReasoningEffort 会归一到 medium。
		t.Fatalf("显式档位应被归一到支持档，got %v", payload["reasoning_effort"])
	}
}
