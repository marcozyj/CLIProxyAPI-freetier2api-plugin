package workbuddy

import (
	"strings"
	"testing"
)

// 临时探针：确认 OpenAI SSE 形态（data: {choices:[{delta:{content}}]} + [DONE]）
// 能被 Aggregate 解析出内容；用于 consolepreview stub 的形状验证。
func TestAggregateOpenAISSEStub(t *testing.T) {
	body := "data: {\"id\":\"preview\",\"choices\":[{\"delta\":{\"content\":\"你好！我是预览应答。\"}}]}\n\ndata: [DONE]\n\n"
	out, err := Aggregate(strings.NewReader(body))
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if !strings.Contains(string(out), "预览应答") {
		t.Fatalf("no content: %s", out)
	}
}
