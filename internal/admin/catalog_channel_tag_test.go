package admin

import (
	"testing"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// newTagTestCatalog 构造一个带 zcode 供应商与已注册模型的目录，用于渠道标识测试。
func newTagTestCatalog(t *testing.T, tagMode string) *Catalog {
	t.Helper()
	store := state.NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	registry := core.NewRegistry()
	if err := registry.Register(&fakeVendor{Meta: core.Meta{VendorID: "zcode", VendorName: "ZCode", VendorRegion: "global"}}); err != nil {
		t.Fatal(err)
	}
	c := NewCatalog(store, registry)
	c.Configure(ModelSettings{ModelNameTag: tagMode})
	models := []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}}
	if !c.CacheProbedModels("zcode", models) {
		t.Fatal("register probe models")
	}
	return c
}

// TestChannelTagPrefixRenamesAndResolves 验证 prefix 模式：对外 ID 带供应商
// 缩写前缀（zc = zcode），请求侧剥前缀还原官方 ID；不带前缀的原名、以及
// 加缩写之前的旧前缀（裸 vendorID）都仍可解析。
func TestChannelTagPrefixRenamesAndResolves(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "zc-GLM-5.3" {
		t.Fatalf("tagged id = %+v", out)
	}
	if got := c.ResolveOutboundModel("zcode", "zc-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("untag resolve = %q", got)
	}
	// 旧形态（裸 vendorID 前缀）也要能还原：客户端可能还缓存着它。
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("legacy prefix resolve = %q", got)
	}
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("bare passthrough = %q", got)
	}
	// 其他供应商的请求不受影响（前缀只剥自己的 vendor 名）。
	if got := c.ResolveOutboundModel("workbuddycn", "zc-GLM-5.3"); got != "zc-GLM-5.3" {
		t.Fatalf("other vendor passthrough = %q", got)
	}
}

// TestChannelTagSuffixRenames 验证 suffix 模式：官方 ID + 供应商缩写。
func TestChannelTagSuffixRenames(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagSuffix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "GLM-5.3-zc" {
		t.Fatalf("tagged id = %+v", out)
	}
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3-zc"); got != "GLM-5.3" {
		t.Fatalf("untag resolve = %q", got)
	}
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3-zcode"); got != "GLM-5.3" {
		t.Fatalf("legacy suffix resolve = %q", got)
	}
}

// TestChannelTagOffKeepsBareID 验证 off 模式（默认）不改名。
func TestChannelTagOffKeepsBareID(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagOff)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{{ID: "GLM-5.3", Name: "GLM-5.3", DisplayName: "GLM-5.3"}})
	if len(out) != 1 || out[0].ID != "GLM-5.3" {
		t.Fatalf("off mode must keep bare id, got %+v", out)
	}
}

// TestChannelTagDoesNotShadowOtherModel 验证防呆：tag 后的名字撞上**同供应商
// 本次清单里的其他模型**时放弃标识，避免路由歧义。撞车判定用入参清单而非
// registeredLocked——后者对缓存为空的供应商会误判「已存在」导致标识被整体放弃。
func TestChannelTagDoesNotShadowOtherModel(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	out := c.ApplyAliases("zcode", []pluginapi.ModelInfo{
		{ID: "GLM-5.3", Name: "GLM-5.3"},
		{ID: "zc-GLM-5.3", Name: "zc-GLM-5.3"},
	})
	if len(out) != 2 || out[0].ID != "GLM-5.3" {
		t.Fatalf("collision must skip tagging, got %+v", out)
	}
}

// TestChannelTagResolveAfterModeSwitch 验证模式切换后旧标识请求仍能还原：
// prefix 开过又切回 off 时，客户端手里的 zcode-GLM-5.3 不能直通上游。
func TestChannelTagResolveAfterModeSwitch(t *testing.T) {
	c := newTagTestCatalog(t, ModelNameTagPrefix)
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("prefix resolve = %q", got)
	}
	// 切到 off：还原不依赖当前模式（两种形态都试）。
	c.Configure(ModelSettings{ModelNameTag: ModelNameTagOff})
	if got := c.ResolveOutboundModel("zcode", "zcode-GLM-5.3"); got != "GLM-5.3" {
		t.Fatalf("off-mode legacy prefix resolve = %q", got)
	}
	// suffix 形态同样可还原（从未开启过 suffix）。
	if got := c.ResolveOutboundModel("zcode", "GLM-5.3-zcode"); got != "GLM-5.3" {
		t.Fatalf("legacy suffix resolve = %q", got)
	}
}
