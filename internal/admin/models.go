package admin

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge"
	"freetier2api-plugin/internal/logger"
)

// consoleModel 是控制台页展示用的模型条目。
//
// 刻意自定义而不直接回传 pluginapi.ModelInfo：后者没有 json tag，
// 字段名会变成 Go 字段名，页面按小写键读会全部拿到 undefined。
type consoleModel struct {
	ID string `json:"id"`
	// PublicID 是客户端在 /v1/models 里看到的模型 ID（官方 ID + 供应商后缀 /
	// 手动别名）。控制台展示它，用户照着页面填就不会 404。
	PublicID        string   `json:"public_id,omitempty"`
	ScopeID         string   `json:"scope_id"`
	VendorID        string   `json:"vendor_id"`
	VendorName      string   `json:"vendor_name"`
	Realm           string   `json:"realm"`
	Name            string   `json:"name,omitempty"`
	Alias           string   `json:"alias,omitempty"`
	Description     string   `json:"description,omitempty"`
	ContextLength   int64    `json:"context_length,omitempty"`
	MaxOutputTokens int64    `json:"max_output_tokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"`
	DefaultEffort   string   `json:"default_effort,omitempty"`
	SupportsImages  bool     `json:"supports_images,omitempty"`
	SupportsTools   bool     `json:"supports_tools,omitempty"`
	Credits         string   `json:"credits,omitempty"`
	Disabled        bool     `json:"disabled,omitempty"`
}

func (s *Service) handleModelsList(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	ctx, cancel := hostbridge.ManagementContext(req, requestTimeout)
	defer cancel()
	callbackID := hostbridge.RequestCallbackID(req)
	cfg := s.deps.Config()
	manager := s.deps.Model()
	models := make([]consoleModel, 0, 128)

	active := s.hostActiveVendors(ctx, callbackID)

	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		// 缓存为空的供应商先发起补探测（不等待），收集完再统一等一次，
		// 这样 N 家缺模型时总等待仍是单次上游延迟，而不是 N 倍。
		manager.EnsureModels(vendor)
	}
	manager.WaitProbes(modelProbeWait)

	for _, vendor := range s.registry.Vendors() {
		if !core.RealmEnabled(cfg.EnabledRealms, vendor.Region()) || !active[vendor.ID()] {
			continue
		}
		models = append(models, s.consoleModelsForVendor(manager, vendor)...)
	}

	for _, extra := range manager.ExtraModels() {
		vendorID := extra.OwnedBy
		if vendorID != "" {
			if _, okVendor := s.registry.ByID(vendorID); !okVendor || !active[vendorID] {
				continue
			}
		}
		models = append(models, consoleModel{
			ID:            extra.ID,
			ScopeID:       s.registry.ModelKeyFor(vendorID, extra.ID),
			VendorID:      vendorID,
			VendorName:    s.vendorNameFor(vendorID),
			Realm:         s.vendorRegionFor(vendorID),
			Name:          core.FirstNonEmpty(extra.DisplayName, extra.Name),
			ContextLength: extra.ContextLength,
		})
	}

	for index := range models {
		models[index].Disabled = manager.IsDisabled(models[index].VendorID, models[index].ID)
		models[index].Alias = manager.AliasFor(models[index].VendorID, models[index].ID)
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].VendorName != models[j].VendorName {
			return models[i].VendorName < models[j].VendorName
		}
		return models[i].ID < models[j].ID
	})

	payload := map[string]any{"ok": true, "models": models, "count": len(models)}
	if len(models) == 0 {
		payload["error"] = "本地暂无模型缓存，正在从上游获取；也可点上方「读取模型清单」从上游拉取。"
	} else {
		// 有供应商仍在后台补探测时告知前端，让它稍后再拉一次；
		// 否则用户会以为缺的那几家就是没有模型。
		if missing := manager.ProbesRunning(); missing > 0 {
			payload["pending_probes"] = missing
		}
	}
	return JSONResponse(http.StatusOK, payload)
}

func (s *Service) consoleModelsForVendor(manager *Catalog, vendor core.Vendor) []consoleModel {
	out := consoleModelsFromRegistered(manager, s.registry, vendor)
	// 供应商能给出更详细的展示信息时优先用它；但返回空快照（冷启动尚未
	// 探测、无可用凭证）不能把整家供应商从控制台抹掉，因此只在非空时替换。
	if provider, okProvider := core.As[core.ConsoleModelProvider](vendor); okProvider {
		if models := provider.ConsoleModels(); len(models) > 0 {
			out = s.consoleModelsFromProvider(vendor, models)
		}
	}
	// 倍率随模型缓存落盘，重启后从这里回填；provider 已带倍率时不覆盖。
	for index := range out {
		if out[index].Credits == "" {
			out[index].Credits = manager.CreditsFor(vendor.ID(), out[index].ID)
		}
	}
	// 对外模型 ID 必须和 /v1/models 完全一致：用户在客户端里看到的、能调通的
	// 名字，就是这里展示的名字。复用同一套路由规则（手动别名 > 标识 > 官方 ID），
	// 不给控制台单独实现一份，避免两边漂移。
	if len(out) > 0 {
		probe := make([]pluginapi.ModelInfo, len(out))
		for index := range out {
			probe[index] = pluginapi.ModelInfo{ID: out[index].ID}
		}
		for index, info := range manager.ApplyAliases(vendor.ID(), probe) {
			out[index].PublicID = info.ID
		}
	}
	return out
}

// consoleModelsFromRegistered 把目录缓存的官方清单转成控制台条目。
func consoleModelsFromRegistered(manager *Catalog, registry *core.Registry, vendor core.Vendor) []consoleModel {
	infos := manager.Registered(vendor.ID())
	out := make([]consoleModel, 0, len(infos))
	for _, info := range infos {
		entry := consoleModel{
			ID:              info.ID,
			ScopeID:         registry.ModelKeyFor(vendor.ID(), info.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            core.FirstNonEmpty(info.DisplayName, info.Name),
			Description:     info.Description,
			ContextLength:   info.ContextLength,
			MaxOutputTokens: info.MaxCompletionTokens,
			SupportsImages:  core.ModelSupportsImages(info),
			SupportsTools:   core.ModelSupportsTools(info),
		}
		if info.Thinking != nil {
			entry.Efforts = info.Thinking.Levels
		}
		out = append(out, entry)
	}
	return out
}

func (s *Service) consoleModelsFromProvider(vendor core.Vendor, models []core.ConsoleModel) []consoleModel {
	out := make([]consoleModel, 0, len(models))
	for _, model := range models {
		out = append(out, consoleModel{
			ID:              model.ID,
			ScopeID:         s.registry.ModelKeyFor(vendor.ID(), model.ID),
			VendorID:        vendor.ID(),
			VendorName:      vendor.Name(),
			Realm:           vendor.Region(),
			Name:            model.Name,
			Description:     model.Description,
			ContextLength:   model.ContextLength,
			MaxOutputTokens: model.MaxOutputTokens,
			Efforts:         model.Efforts,
			DefaultEffort:   model.DefaultEffort,
			SupportsImages:  model.SupportsImages,
			SupportsTools:   model.SupportsTools,
			Credits:         model.Credits,
		})
	}
	return out
}

func (s *Service) vendorNameFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Name()
	}
	return ""
}

func (s *Service) vendorRegionFor(vendorID string) string {
	if vendor, okVendor := s.registry.ByID(vendorID); okVendor {
		return vendor.Region()
	}
	return ""
}

// ModelToggle 批量禁用/启用模型（导出给根层兼容包装）。
func (s *Service) ModelToggle(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelToggleHandler(req)
}

// ModelAlias 设置/清除别名（导出给根层兼容包装）。
func (s *Service) ModelAlias(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return s.modelAliasHandler(req)
}

// modelToggleRequest 是批量禁用/启用模型的请求体。
type modelToggleRequest struct {
	Models   []string `json:"models"`
	Disabled bool     `json:"disabled"`
}

// modelToggleHandler 批量禁用/启用模型。
func (s *Service) modelToggleHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[modelToggleRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	targets := make([]string, 0, len(body.Models))
	for _, item := range body.Models {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			targets = append(targets, trimmed)
		}
	}
	if len(targets) == 0 {
		return JSONResponse(http.StatusBadRequest, map[string]any{"ok": false, "error": "请先选择要操作的模型"})
	}
	s.deps.Model().SetDisabled(targets, body.Disabled)
	verb := "启用"
	if body.Disabled {
		verb = "禁用"
	}
	logger.Info("models %s: %d 个（%s）", verb, len(targets), strings.Join(targets, ", "))
	return JSONResponse(http.StatusOK, map[string]any{
		"ok": true, "changed": len(targets), "disabled": body.Disabled,
		"disabled_models": s.deps.State().DisabledModels,
		"message":         "已" + verb + " " + strconv.Itoa(len(targets)) + " 个模型（调用即时生效）",
	})
}

// modelAliasRequest 是设置/清除模型别名的请求体。
type modelAliasRequest struct {
	Model string `json:"model"`
	Alias string `json:"alias"`
}

// modelAliasHandler 设置或清除某个模型的别名。
func (s *Service) modelAliasHandler(req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	body, errDecode := decodeRequest[modelAliasRequest](req.Body)
	if errDecode != nil {
		return BadJSONRequestResponse()
	}
	vendorID, bare := s.deps.Model().VendorForAliasScope(body.Model)
	if vendorID == "" || strings.TrimSpace(bare) == "" {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "model 必须是 <vendor>:<模型 ID> 形式",
		})
	}
	alias := strings.TrimSpace(body.Alias)
	if strings.ContainsAny(alias, " \t\r\n") {
		return JSONResponse(http.StatusBadRequest, map[string]any{
			"ok": false, "error": "别名不能包含空白字符",
		})
	}
	s.deps.Model().SetAlias(vendorID, bare, alias)
	return JSONResponse(http.StatusOK, map[string]any{"ok": true})
}
