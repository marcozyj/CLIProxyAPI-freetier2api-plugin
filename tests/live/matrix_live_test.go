//go:build live

package live

// 通用实况矩阵探针：MATRIX_LIVE=1 时，对单个供应商跑「解析凭证 → 拉目录 →
// 逐个模型发一条最小对话」，把可用性、倍率、上下文一次打出来。
//
// 用法：
//
//	MATRIX_LIVE=1 MATRIX_VENDOR=autoclawcn MATRIX_CREDS=/tmp/creds/x.json \
//	  go test -tags live ./tests/live -run TestVendorMatrix -v
//
// 可选：MATRIX_MODELS=a,b,c 指定只打这几个模型；MATRIX_MAX=0 表示只拉目录不对话。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"freetier2api-plugin/internal/app"
	"freetier2api-plugin/internal/core"
)

// probeHost 给供应商提供一个「宿主里有这份凭证」的视角。
//
// 有些供应商的模型目录是探测出来的：它不信任调用方传进来的凭证，而是从宿主
// 的 auth 列表里挑一条可用的去打上游（workbuddy 就是这样）。实况探针里没有
// 真宿主，用这个最小实现把待测凭证喂进去。
type probeHost struct {
	name string
	raw  []byte
}

func (h probeHost) ListAuths(context.Context) ([]core.HostAuth, error) {
	return []core.HostAuth{{
		ID:        "probe",
		AuthIndex: "probe",
		Name:      h.name,
		Provider:  core.ProviderKey,
	}}, nil
}

func (h probeHost) AuthJSON(context.Context, string) ([]byte, bool) { return h.raw, true }

func (h probeHost) Settings() core.Settings { return core.Settings{} }

func matrixInstances(t *testing.T, fileName string, raw []byte) ([]core.Vendor, core.Env) {
	t.Helper()
	registry := core.NewRegistry()
	env := core.Env{Host: probeHost{name: fileName, raw: raw}, Registry: registry}
	var out []core.Vendor
	for _, factory := range app.BuiltinFactories() {
		for _, vendor := range factory.Instances(env) {
			if errRegister := registry.Register(vendor); errRegister != nil {
				t.Logf("注册 %s 失败: %v", vendor.ID(), errRegister)
				continue
			}
			out = append(out, vendor)
		}
	}
	if len(out) == 0 {
		t.Fatal("没有内置供应商实例")
	}
	return out, env
}

func TestVendorMatrix(t *testing.T) {
	if os.Getenv("MATRIX_LIVE") == "" {
		t.Skip("MATRIX_LIVE 未设置，跳过矩阵探针")
	}
	vendorID := os.Getenv("MATRIX_VENDOR")
	path := os.Getenv("MATRIX_CREDS")
	if vendorID == "" || path == "" {
		t.Skip("MATRIX_VENDOR / MATRIX_CREDS 未设置")
	}
	raw, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatalf("read creds: %v", errRead)
	}

	// 复用 trae 探针里的直连宿主：它同时实现了 do 与 do_stream，
	// 大多数供应商的上游都是 SSE，只能走流式回调。
	configureTraeHost(t)

	instances, _ := matrixInstances(t, filepath.Base(path), raw)

	var vendor core.Vendor
	for _, candidate := range instances {
		if candidate.ID() == vendorID {
			vendor = candidate
			break
		}
	}
	if vendor == nil {
		t.Fatalf("不认识供应商 %q", vendorID)
	}
	t.Logf("供应商: %s (%s / %s)", vendor.ID(), vendor.Name(), vendor.Region())

	cred, errParse := vendor.Parse(raw, path)
	if errParse != nil {
		t.Fatalf("Parse: %v", errParse)
	}

	if refreshed, changed, errRefresh := core.RefreshVendor(vendor, context.Background(), cred); errRefresh != nil {
		t.Logf("  凭证续期失败（继续用原凭证）: %v", errRefresh)
	} else if changed {
		cred = refreshed
		t.Logf("  凭证已续期")
	}

	models, errModels := core.ModelsFor(vendor, context.Background(), cred)
	if errModels != nil {
		t.Fatalf("ModelsFor: %v", errModels)
	}
	var credits map[string]string
	if provider, okProvider := core.As[core.CreditsProvider](vendor); okProvider {
		credits = provider.ModelCredits()
	}
	t.Logf("目录: %d 个模型，%d 个有倍率", len(models), len(credits))

	prefix := vendor.ModelPrefix()
	var missing []string
	for _, model := range models {
		id := strings.TrimPrefix(model.ID, prefix)
		if strings.TrimSpace(credits[id]) == "" {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		if len(missing) > 12 {
			missing = append(missing[:12], fmt.Sprintf("...共 %d 个", len(missing)))
		}
		t.Logf("  无倍率: %s", strings.Join(missing, ", "))
	}

	wanted := map[string]bool{}
	for _, id := range strings.Split(os.Getenv("MATRIX_MODELS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = true
		}
	}
	limit := 2
	if rawLimit := strings.TrimSpace(os.Getenv("MATRIX_MAX")); rawLimit != "" {
		if parsed, errAtoi := strconv.Atoi(rawLimit); errAtoi == nil && parsed >= 0 {
			limit = parsed
		}
	}

	sample := 0
	passed, failed := 0, 0
	var failures []string
	for _, model := range models {
		id := strings.TrimPrefix(model.ID, prefix)
		if len(wanted) > 0 && !wanted[id] {
			continue
		}
		if sample < 3 || len(wanted) > 0 {
			levels := ""
			if model.Thinking != nil {
				levels = strings.Join(model.Thinking.Levels, "/")
			}
			t.Logf("  - %-42s credits=%-8q context=%d output=%d tools=%v images=%v efforts=%q",
				id, credits[id], model.ContextLength, model.MaxCompletionTokens,
				core.ModelSupportsTools(model), core.ModelSupportsImages(model), levels)
			sample++
		}
		if limit == 0 {
			continue
		}
		if len(wanted) == 0 && passed+failed >= limit {
			continue
		}
		body := map[string]any{
			"model":      id,
			"messages":   []any{map[string]any{"role": "user", "content": "只回复两个字：收到"}},
			"max_tokens": 16,
			"stream":     false,
		}
		if extra := strings.TrimSpace(os.Getenv("MATRIX_EXTRA")); extra != "" {
			var patch map[string]any
			if errPatch := json.Unmarshal([]byte(extra), &patch); errPatch != nil {
				t.Fatalf("MATRIX_EXTRA 不是合法 JSON: %v", errPatch)
			}
			for key, value := range patch {
				body[key] = value
			}
		}
		payload, _ := json.Marshal(body)
		resp, errExec := vendor.Execute(context.Background(), cred, &core.ExecuteRequest{
			Model:   id,
			Payload: payload,
			Stream:  false,
		})
		if errExec != nil {
			failed++
			msg := errExec.Error()
			if len(msg) > 220 {
				msg = msg[:220]
			}
			t.Logf("  ✗ %s: %s", id, msg)
			failures = append(failures, fmt.Sprintf("%s: %s", id, msg))
			continue
		}
		passed++
		if resp != nil && len(resp.Payload) > 0 {
			t.Logf("  ✓ %s: %d 字节", id, len(resp.Payload))
		} else {
			t.Logf("  ✓ %s", id)
		}
	}
	t.Logf("结果: %d 通 / %d 不通", passed, failed)
	for _, item := range failures {
		t.Logf("  失败明细: %s", item)
	}
}
