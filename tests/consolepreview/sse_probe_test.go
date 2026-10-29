//go:build preview

package main

import (
	"context"
	"strings"
	"testing"

	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/vendors/opencodezen"
	"freetier2api-plugin/tests/testhost"
)

// go test -tags preview ./tests/consolepreview -run TestZenSSEStub
func TestZenSSEStub(t *testing.T) {
	host := testhost.New()
	hostbridge.Configure(host.Call)
	defer hostbridge.Configure(nil)
	host.SetUpstream(stubUpstream)
	ctx := context.Background()
	body := `{"model":"ds4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`
	out, err := opencodezen.Chat(ctx, "https://opencode.ai/inference/openai", opencodezen.ChatRequest{
		APIKey: "sk-preview", Body: []byte(body), Catalog: nil,
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if !strings.Contains(string(out), "预览应答") {
		t.Fatalf("no content in %s", out)
	}
}
