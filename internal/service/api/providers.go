package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/protocol/anthropic"
	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/store"
)

// ---- Providers ----

// GET /providers
func (r *Router) handleListProviders(w http.ResponseWriter, req *http.Request) {
	p := parsePagination(req)

	cfg := r.runtime.Current()
	providerKeys := make([]string, 0, len(cfg.Config.ProviderDefs))
	for key := range cfg.Config.ProviderDefs {
		providerKeys = append(providerKeys, key)
	}
	sortStrings(providerKeys)

	total := len(providerKeys)

	sliceEnd := p.Offset + p.Limit
	if p.Offset > len(providerKeys) {
		p.Offset = len(providerKeys)
	}
	if sliceEnd > len(providerKeys) {
		sliceEnd = len(providerKeys)
	}
	page := providerKeys[p.Offset:sliceEnd]

	type providerItem struct {
		Key          string `json:"key"`
		Protocol     string `json:"protocol"`
		OfferCount   int    `json:"offer_count"`
		BaseURL      string `json:"base_url"`
		HealthStatus string `json:"health_status"`
	}

	items := make([]providerItem, 0, len(page))
	for _, key := range page {
		def := cfg.Config.ProviderDefs[key]
		items = append(items, providerItem{
			Key:          key,
			Protocol:     def.Protocol,
			OfferCount:   len(def.Offers),
			BaseURL:      def.BaseURL,
			HealthStatus: "unknown",
		})
	}

	respondJSON(w, http.StatusOK, paginatedResponse{
		Data:   items,
		Total:  total,
		Limit:  p.Limit,
		Offset: p.Offset,
	})
}

// GET /providers/{key}
func (r *Router) handleGetProvider(w http.ResponseWriter, req *http.Request) {
	key := req.PathValue("key")
	if key == "" {
		respondError(w, http.StatusBadRequest, "invalid_key", "无效的 provider key")
		return
	}

	cfg := r.runtime.Current()
	def, ok := cfg.Config.ProviderDefs[key]
	if !ok {
		respondError(w, http.StatusNotFound, "not_found", fmt.Sprintf("provider %q 不存在", key))
		return
	}

	type offerItem struct {
		Model        string  `json:"model"`
		UpstreamName string  `json:"upstream_name,omitempty"`
		Priority     int     `json:"priority"`
		InputPrice   float64 `json:"input_price"`
		OutputPrice  float64 `json:"output_price"`
		CacheWrite   float64 `json:"cache_write"`
		CacheRead    float64 `json:"cache_read"`
	}

	offers := make([]offerItem, 0, len(def.Offers))
	for _, offer := range def.Offers {
		offers = append(offers, offerItem{
			Model:        offer.Model,
			UpstreamName: offer.UpstreamName,
			Priority:     offer.Priority,
			InputPrice:   offer.Pricing.InputPrice,
			OutputPrice:  offer.Pricing.OutputPrice,
			CacheWrite:   offer.Pricing.CacheWritePrice,
			CacheRead:    offer.Pricing.CacheReadPrice,
		})
	}

	resp := map[string]any{
		"key":                 key,
		"base_url":            def.BaseURL,
		"api_key":             maskAPIKey(def.APIKey),
		"version":             def.Version,
		"protocol":            def.Protocol,
		"user_agent":          def.UserAgent,
		"offers":              offers,
		"offer_count":         len(offers),
		"web_search":          string(def.WebSearchSupport),
		"web_search_max_uses": def.WebSearchMaxUses,
	}

	respondJSON(w, http.StatusOK, resp)
}

// PUT /providers/{key}
func (r *Router) handlePutProvider(w http.ResponseWriter, req *http.Request) {
	key := req.PathValue("key")
	if key == "" {
		respondError(w, http.StatusBadRequest, "invalid_key", "无效的 provider key")
		return
	}

	var body struct {
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		Version   string `json:"version"`
		Protocol  string `json:"protocol"`
		UserAgent string `json:"user_agent"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}
	if body.BaseURL == "" {
		respondError(w, http.StatusBadRequest, "validation_error", "base_url 不能为空")
		return
	}
	if body.APIKey == "" {
		respondError(w, http.StatusBadRequest, "validation_error", "api_key 不能为空")
		return
	}
	if body.Protocol == "" {
		body.Protocol = "anthropic"
	}

	afterJSON, _ := json.Marshal(map[string]any{
		"base_url":   body.BaseURL,
		"api_key":    body.APIKey,
		"version":    body.Version,
		"protocol":   body.Protocol,
		"user_agent": body.UserAgent,
	})

	chID, err := r.store.StageChange(store.ChangeRow{
		Action:    "create",
		Resource:  "provider",
		TargetKey: key,
		After:     string(afterJSON),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "stage_error", fmt.Sprintf("暂存变更失败: %v", err))
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"change_id": chID,
		"status":    "pending",
		"message":   "变更已暂存，请调用 POST /changes/apply 使其生效",
	})
}

// PATCH /providers/{key}
func (r *Router) handlePatchProvider(w http.ResponseWriter, req *http.Request) {
	key := req.PathValue("key")
	if key == "" {
		respondError(w, http.StatusBadRequest, "invalid_key", "无效的 provider key")
		return
	}

	var body struct {
		BaseURL   string `json:"base_url"`
		APIKey    string `json:"api_key"`
		Version   string `json:"version"`
		Protocol  string `json:"protocol"`
		UserAgent string `json:"user_agent"`
	}

	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		respondError(w, http.StatusBadRequest, "invalid_json", "无效的 JSON 请求体")
		return
	}

	cfg := r.runtime.Current()
	def, exists := cfg.Config.ProviderDefs[key]

	apiKey := body.APIKey
	if apiKey == "******" && exists {
		apiKey = def.APIKey
	}

	baseURL := body.BaseURL
	if baseURL == "" && exists {
		baseURL = def.BaseURL
	}

	version := body.Version
	if version == "" && exists {
		version = def.Version
	}

	protocol := body.Protocol
	if protocol == "" && exists {
		protocol = def.Protocol
	}

	userAgent := body.UserAgent
	if userAgent == "" && exists {
		userAgent = def.UserAgent
	}

	action := "update"
	if !exists {
		action = "create"
	}

	afterJSON, _ := json.Marshal(map[string]any{
		"base_url":   baseURL,
		"api_key":    apiKey,
		"version":    version,
		"protocol":   protocol,
		"user_agent": userAgent,
	})

	chID, err := r.store.StageChange(store.ChangeRow{
		Action:    action,
		Resource:  "provider",
		TargetKey: key,
		After:     string(afterJSON),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "stage_error", fmt.Sprintf("暂存变更失败: %v", err))
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"change_id": chID,
		"status":    "pending",
		"message":   "变更已暂存，请调用 POST /changes/apply 使其生效",
	})
}

// DELETE /providers/{key}
func (r *Router) handleDeleteProvider(w http.ResponseWriter, req *http.Request) {
	key := req.PathValue("key")
	if key == "" {
		respondError(w, http.StatusBadRequest, "invalid_key", "无效的 provider key")
		return
	}

	cfg := r.runtime.Current()
	if _, ok := cfg.Config.ProviderDefs[key]; !ok {
		respondError(w, http.StatusNotFound, "not_found", fmt.Sprintf("provider %q 不存在", key))
		return
	}

	chID, err := r.store.StageChange(store.ChangeRow{
		Action:    "delete",
		Resource:  "provider",
		TargetKey: key,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "stage_error", fmt.Sprintf("暂存删除失败: %v", err))
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]any{
		"change_id": chID,
		"status":    "pending",
		"message":   "删除已暂存，请调用 POST /changes/apply 使其生效",
	})
}

// probeTimeout bounds a provider connection probe. The old 5s was too short for
// a cold request to a real upstream; the seam lets tests shorten it.
var probeTimeout = 12 * time.Second

// probeResult is the structured connection-test response. Code is allowlisted
// and Message is authored by Moon Bridge, never the upstream error body.
type probeResult struct {
	Success  bool   `json:"success"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	Model    string `json:"model"`
	Duration string `json:"duration,omitempty"`
}

// POST /providers/{key}/test
func (r *Router) handleTestProvider(w http.ResponseWriter, req *http.Request) {
	key := req.PathValue("key")
	if key == "" {
		respondError(w, http.StatusBadRequest, "invalid_key", "无效的 provider key")
		return
	}

	cfg := r.runtime.Current()
	def, ok := cfg.Config.ProviderDefs[key]
	if !ok {
		respondError(w, http.StatusNotFound, "not_found", fmt.Sprintf("provider %q 不存在", key))
		return
	}

	var migrationIssue *provider.CredentialInfo
	if pm := r.runtime.Current().ProviderMgr; pm != nil {
		if status, blocked := pm.MigrationIssue(key); blocked {
			copy := status
			migrationIssue = &copy
		}
	}
	if migrationIssue == nil {
		if resolver := r.runtime.Resolver(); resolver != nil && resolver.Registry != nil {
			if status, blocked := resolver.Registry.Get(key); blocked && status.State == provider.StateUnavailable {
				copy := status
				migrationIssue = &copy
			}
		}
	}
	apiKey := resolveProbeKey(r.runtime.Resolver(), key, def.APIKey, def.APIKeyEnv, migrationIssue)
	if apiKey == "" {
		respondProbeResult(w, probeResult{
			Success: false,
			Code:    "credential_unavailable",
			Message: "no usable API key: check the stored key or environment variable",
		})
		return
	}

	activeModel := r.activeProfileModelForProvider(req.Context(), key)
	probeModel, ok := pickProbeModel(cfg.Config, key, def, activeModel)
	if !ok {
		respondProbeResult(w, probeResult{
			Success: false,
			Code:    "model_unavailable",
			Message: "no model available to probe",
		})
		return
	}

	probe := anthropic.MessageRequest{
		Model:     probeModel,
		MaxTokens: 1,
		Messages: []anthropic.Message{
			{Role: "user", Content: []anthropic.ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}

	client := anthropic.NewClient(anthropic.ClientConfig{
		BaseURL:   def.BaseURL,
		APIKey:    apiKey,
		Version:   def.Version,
		UserAgent: def.UserAgent,
	})

	ctx, cancel := context.WithTimeout(req.Context(), probeTimeout)
	defer cancel()

	start := time.Now()
	_, err := client.CreateMessage(ctx, probe)
	duration := time.Since(start)

	code, message := classifyProbeError(err)
	respondProbeResult(w, probeResult{
		Success:  err == nil,
		Code:     code,
		Message:  message,
		Model:    probeModel,
		Duration: duration.String(),
	})
}

// respondProbeResult always responds 200 with the structured probe result; the
// connection outcome is carried by success/code, not the HTTP status.
func respondProbeResult(w http.ResponseWriter, result probeResult) {
	respondJSON(w, http.StatusOK, result)
}

// resolveProbeKey resolves the effective probe key through the shared resolver
// when one is injected (recording the outcome in the credential registry),
// falling back to a stored/env lookup otherwise. The returned key is used only
// to build the probe client and never leaves the handler.
func resolveProbeKey(resolver *provider.CredentialResolver, providerID, stored, envName string, issue *provider.CredentialInfo) string {
	if resolver == nil {
		return ""
	}
	return resolver.ResolveWithIssue(providerID, stored, envName, issue)
}

// pickProbeModel chooses the model used for the connection probe with a fixed
// precedence: ① the provider's explicit default route model, ② the model in use
// by the active routing profile, ③ the first model ID when offers are sorted,
// ④ no usable model (ok=false). It never falls back to a hardcoded model.
func pickProbeModel(cfg config.Config, key string, def config.ProviderDef, activeModel string) (string, bool) {
	if model := defaultRouteModelForProvider(cfg, key); model != "" {
		return model, true
	}
	if activeModel != "" {
		return activeModel, true
	}
	if model := firstSortedOfferModel(def); model != "" {
		return model, true
	}
	return "", false
}

// defaultRouteModelForProvider returns the model of the provider's explicit
// default route (defaults.model → routes.<alias>), or the route alias itself
// when the route model is empty.
func defaultRouteModelForProvider(cfg config.Config, key string) string {
	alias := cfg.DefaultModelAlias()
	if alias == "" {
		return ""
	}
	route, ok := cfg.Routes[alias]
	if !ok {
		return ""
	}
	if route.Provider != "" && route.Provider != key {
		return ""
	}
	if route.Model != "" {
		return route.Model
	}
	return alias
}

// firstSortedOfferModel returns the first model ID after sorting the provider's
// offer models (falling back to its catalog model keys), or "" when there are
// none. Deterministic so the probe target does not depend on map iteration.
func firstSortedOfferModel(def config.ProviderDef) string {
	seen := make(map[string]struct{})
	var models []string
	for _, offer := range def.Offers {
		model := offer.Model
		if model == "" {
			model = offer.UpstreamName
		}
		if model == "" {
			continue
		}
		if _, dup := seen[model]; dup {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	if len(models) == 0 {
		for model := range def.Models {
			if _, dup := seen[model]; dup {
				continue
			}
			seen[model] = struct{}{}
			models = append(models, model)
		}
	}
	if len(models) == 0 {
		return ""
	}
	sortStrings(models)
	return models[0]
}

// activeProfileSlotModel returns the first non-empty upstream model of the
// active routing profile for the given provider, or "" when none matches.
func activeProfileSlotModel(graph configgraph.Graph, key string) string {
	snap := routingprofile.SnapshotFromGraph(graph, true)
	for _, profile := range snap.Profiles {
		if !profile.Active {
			continue
		}
		for _, slot := range profile.Slots {
			if slot.ProviderID == key && slot.UpstreamModel != "" {
				return slot.UpstreamModel
			}
		}
	}
	return ""
}

// activeProfileModelForProvider reads the active routing profile slot model for
// the provider through the live config graph. A graph read error yields "" so
// the probe falls back to the next precedence.
func (r *Router) activeProfileModelForProvider(ctx context.Context, key string) string {
	graph, err := r.configGraphService().Graph(ctx)
	if err != nil {
		return ""
	}
	return activeProfileSlotModel(graph, key)
}

// classifyProbeError maps an upstream probe error to an allowlisted code and a
// Moon Bridge-authored message. The upstream error body/string never leaks: only
// the mapped code and wording leave this function.
func classifyProbeError(err error) (code, message string) {
	if err == nil {
		return "ok", "connection succeeded"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "connection timed out"
	}
	providerErr, ok := anthropic.IsProviderError(err)
	if !ok {
		return "network_error", "network connection failed"
	}
	switch {
	case providerErr.StatusCode == http.StatusUnauthorized || providerErr.StatusCode == http.StatusForbidden:
		return "auth_failed", "authentication failed: check the API key"
	case providerErr.StatusCode == http.StatusTooManyRequests:
		return "rate_limited", "rate limited: try again later"
	case providerErr.StatusCode == http.StatusNotFound:
		return "model_unavailable", "probe model unavailable"
	case providerErr.StatusCode == http.StatusRequestTimeout || providerErr.StatusCode == http.StatusGatewayTimeout:
		return "timeout", "connection timed out"
	case providerErr.StatusCode >= 500:
		return "general", "upstream provider error"
	default:
		return "general", "provider request rejected"
	}
}
