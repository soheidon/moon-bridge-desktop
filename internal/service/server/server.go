package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"moonbridge/internal/config"
	"moonbridge/internal/extension/plugin"
	"moonbridge/internal/format"
	"moonbridge/internal/logger"
	"moonbridge/internal/protocol/chat"
	"moonbridge/internal/protocol/google"
	"moonbridge/internal/protocol/openai"
	"moonbridge/internal/service/api"
	"moonbridge/internal/service/egressobservation"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/routingprofile"
	"moonbridge/internal/service/runtime"
	"moonbridge/internal/service/stats"
	"moonbridge/internal/service/store"
	"moonbridge/internal/service/trafficanalysis"
	"moonbridge/internal/service/webui"

	"moonbridge/internal/service/server/session"
	"moonbridge/internal/service/server/trace"
	"moonbridge/internal/service/server/usage"

	mbtrace "moonbridge/internal/service/trace"
)

// ChatClient is the interface for OpenAI-chat protocol clients.
// It uses any parameters to avoid importing protocol-specific packages.
type Config struct {
	// ServerCfg is the scoped domain config for the server layer.
	// Used alongside AppConfig for the full config.
	ServerCfg              config.ServerConfig
	AdapterRegistry        *format.Registry        // adapter dispatch path (format registry)
	Provider               provider.ProviderClient // fallback provider for non-adapter path
	ProviderMgr            *provider.ProviderManager
	TrafficRouting         TrafficRouting
	RoutingObservationSink RoutingObservationSink
	RoutingProfileResolver RoutingProfileResolver
	RoutingProfileState    routingprofile.SafeResolverState
	RoutingConfigSource    string
	OpenAIHTTPClient       *http.Client
	ProxyHTTPClient        *http.Client
	ChatClients            map[string]any
	GoogleClients          map[string]any
	Tracer                 *mbtrace.Tracer
	TraceErrors            io.Writer
	Stats                  *stats.SessionStats
	PluginRegistry         *plugin.Registry
	AppConfig              config.ServerConfig
	Runtime                *runtime.Runtime
	Store                  store.ConfigStore
	SessionManager         session.Manager
	UsageTracker           usage.Tracker
	TraceWriter            trace.Writer
}

// RelayMarkerHeader names the origin-proof header the Capture relay stamps on
// forwarded requests. dispatch consumes and removes it before any tracing, and
// the Gateway validates it before a source model may be lazily bound.
const RelayMarkerHeader = "X-Moonbridge-Relay"

// RequestCorrelationHeader is stamped by the in-process Capture relay and
// consumed before tracing or provider dispatch. Its value is a short-lived,
// process-local correlation key and is never exposed in the public DTO.
const RequestCorrelationHeader = "X-Moonbridge-Request"

// TrafficRouting is the narrow, read-only bridge from the active Traffic
// relay to model resolution. Implementations lazily bind and exact-match a
// process-local source mapping for requests that provably traversed the relay.
type TrafficRouting interface {
	ObservedModelFor(sourceModel, relayMarker string) (targetRoute string, ok bool)
}

type RoutingObservationSink interface {
	RecordGatewayEvent(trafficanalysis.GatewayEventInput)
}

// RoutingProfileSlotResult is the output of a routing profile slot resolution.
type RoutingProfileSlotResult struct {
	SlotID          string
	ActiveProfileID string
	ProviderKey     string
	UpstreamModel   string
	Mode            string
	Reasoning       *string
}

// RoutingProfileResolver is the read-only bridge from the active routing
// profile to slot resolution at request time. It maps a Codex request model
// identifier to the profile's slot configuration without mutating the graph.
type RoutingProfileResolver interface {
	// ResolveSlot returns the slot configuration for a Codex request model.
	// Returns ok=false when no active profile or no exact match.
	ResolveSlot(requestModel string) (RoutingProfileSlotResult, bool)
}

type routingProfileResolverStateProvider interface {
	SafeState() routingprofile.SafeResolverState
}

// RoutingResolverStatus is a reduced, process-local diagnostic snapshot. It
// never contains raw profile/provider/model identifiers or connection data.
type RoutingResolverStatus struct {
	ServerInstance       string                           `json:"server_instance"`
	Generation           uint64                           `json:"generation"`
	InstallSource        string                           `json:"install_source"`
	ConfigSource         string                           `json:"config_source"`
	ResolverPresent      bool                             `json:"resolver_present"`
	RoutingProfileState  routingprofile.SafeResolverState `json:"routing_profile_state"`
	LastLoadedGeneration uint64                           `json:"last_loaded_generation"`
	LastResolutionStage  string                           `json:"last_resolution_stage"`
}

// RuntimeConfigurationStatus is the bounded, secret-safe runtime snapshot
// consumed by the Desktop Dashboard. It is deliberately derived from one
// resolver holder and the current ProviderManager; it never contains profile
// IDs, provider keys, URLs, config paths, or credentials.
type RuntimeConfigurationStatus struct {
	State                 string               `json:"state"`
	ServerInstance        string               `json:"server_instance"`
	ResolverGeneration    uint64               `json:"resolver_generation"`
	InstallSource         string               `json:"install_source"`
	ConfigSource          string               `json:"config_source"`
	ResolverPresent       bool                 `json:"resolver_present"`
	RoutingExtensionState string               `json:"routing_extension_state"`
	ActiveProfileState    string               `json:"active_profile_state"`
	ReadySlotCount        int                  `json:"ready_slot_count"`
	CredentialState       string               `json:"credential_state"`
	Slots                 RuntimeSlotStatusSet `json:"slots"`
}

type RuntimeSlotStatusSet struct {
	Sol   RuntimeSlotStatus `json:"sol"`
	Terra RuntimeSlotStatus `json:"terra"`
	Luna  RuntimeSlotStatus `json:"luna"`
}

type RuntimeSlotStatus struct {
	State            string `json:"state"`
	Provider         string `json:"provider,omitempty"`
	UpstreamModel    string `json:"upstream_model,omitempty"`
	Mode             string `json:"mode,omitempty"`
	ConfiguredEffort string `json:"configured_effort,omitempty"`
	CredentialState  string `json:"credential_state,omitempty"`
}

// routingProfileResolverHolder wraps a RoutingProfileResolver so atomic.Pointer
// can swap it without the concrete-type restriction of atomic.Value.
type routingProfileResolverHolder struct {
	resolver      RoutingProfileResolver
	state         routingprofile.SafeResolverState
	generation    uint64
	installSource string
}

var serverInstanceCounter atomic.Uint64

type Server struct {
	serverInstance         string
	adapterRegistry        *format.Registry
	provider               provider.ProviderClient
	providerMgr            *provider.ProviderManager
	trafficRouting         TrafficRouting
	routingObservationSink RoutingObservationSink
	routingProfileResolver atomic.Pointer[routingProfileResolverHolder]
	resolverGeneration     atomic.Uint64
	lastLoadedGeneration   atomic.Uint64
	lastResolutionStage    atomic.Value
	routingConfigSource    string
	openAIHTTP             *http.Client
	proxyHTTP              *http.Client
	chatClients            map[string]any
	googleClients          map[string]any
	tracer                 *mbtrace.Tracer
	traceErrors            io.Writer
	stats                  *stats.SessionStats
	pluginRegistry         *plugin.Registry
	mux                    *http.ServeMux
	onceClose              sync.Once
	appConfig              config.ServerConfig
	serverCfg              config.ServerConfig
	runtime                *runtime.Runtime
	store                  store.ConfigStore
	sessionManager         session.Manager
	usageTracker           usage.Tracker
	traceWriter            trace.Writer

	// clientCaches holds lazily-created HTTP clients for runtime-reloaded providers.
	// Keyed by provider key, invalidated when Runtime reloads.
	clientCache   map[string]*chat.Client
	googleCache   map[string]*google.Client
	clientCacheMu sync.RWMutex
	googleCacheMu sync.RWMutex
}

// SwapRoutingProfileResolver atomically replaces the routing profile slot
// resolver. Call this after a profile mutation so the next request uses the
// updated profile configuration. Pass nil to clear.
func (s *Server) SwapRoutingProfileResolver(r RoutingProfileResolver) {
	generation := s.resolverGeneration.Add(1)
	if r == nil {
		s.routingProfileResolver.Store(nil)
		s.logRoutingResolverState("profile_refresh")
		return
	}
	state := routingprofile.SafeResolverState{}
	if provider, ok := r.(routingProfileResolverStateProvider); ok {
		state = provider.SafeState()
	}
	state = normalizeSafeResolverState(state)
	s.routingProfileResolver.Store(&routingProfileResolverHolder{resolver: r, state: state, generation: generation, installSource: "profile_refresh"})
	s.logRoutingResolverState("profile_refresh")
}

// RoutingResolverStatus returns a reduced diagnostic snapshot with no raw
// identifiers or connection details.
func (s *Server) RoutingResolverStatus() RoutingResolverStatus {
	status := RoutingResolverStatus{
		ServerInstance:       s.serverInstance,
		ConfigSource:         s.routingConfigSource,
		InstallSource:        "none",
		LastLoadedGeneration: s.lastLoadedGeneration.Load(),
		LastResolutionStage:  "none",
	}
	if value := s.lastResolutionStage.Load(); value != nil {
		if stage, ok := value.(string); ok {
			status.LastResolutionStage = stage
		}
	}
	if holder := s.routingProfileResolver.Load(); holder != nil && holder.resolver != nil {
		status.ResolverPresent = true
		status.Generation = holder.generation
		status.InstallSource = holder.installSource
		status.RoutingProfileState = holder.state
	}
	return status
}

// RuntimeConfigurationStatus returns the effective configuration used by the
// current request path. The resolver holder is loaded once so all slot facts
// in the result describe the same installed generation.
func (s *Server) RuntimeConfigurationStatus() RuntimeConfigurationStatus {
	status := RuntimeConfigurationStatus{
		State:                 "invalid",
		ServerInstance:        safeServerInstanceForRuntime(s.serverInstance),
		InstallSource:         "none",
		ConfigSource:          s.routingConfigSource,
		RoutingExtensionState: "unknown",
		ActiveProfileState:    "unknown",
		CredentialState:       "unknown",
		Slots: RuntimeSlotStatusSet{
			Sol:   RuntimeSlotStatus{State: "unknown"},
			Terra: RuntimeSlotStatus{State: "unknown"},
			Luna:  RuntimeSlotStatus{State: "unknown"},
		},
	}
	if status.ConfigSource == "" {
		status.ConfigSource = "unknown"
	}
	pm := s.activeProviderManager()
	status.CredentialState = aggregateCredentialState(pm)
	holder := s.routingProfileResolver.Load()
	if holder == nil || holder.resolver == nil {
		status.State = "invalid"
		status.RoutingExtensionState = "absent"
		return status
	}
	status.ResolverPresent = true
	status.ResolverGeneration = holder.generation
	status.InstallSource = safeRuntimeEnum(holder.installSource, "none", "startup", "profile_refresh")
	state := holder.state
	status.RoutingExtensionState = safeRuntimeEnum(state.ExtensionState, "unknown", "absent", "valid", "invalid")
	status.ActiveProfileState = safeRuntimeEnum(state.ActiveProfileState, "unknown", "present_valid", "missing", "invalid", "unknown")
	status.Slots.Sol = s.runtimeSlotStatus(holder, pm, "gpt-5.6-sol", state.SolState)
	status.Slots.Terra = s.runtimeSlotStatus(holder, pm, "gpt-5.6-terra", state.TerraState)
	status.Slots.Luna = s.runtimeSlotStatus(holder, pm, "gpt-5.6-luna", state.LunaState)
	for _, slot := range []RuntimeSlotStatus{status.Slots.Sol, status.Slots.Terra, status.Slots.Luna} {
		if slot.State == "ready" {
			status.ReadySlotCount++
		}
	}
	if status.RoutingExtensionState == "invalid" || status.ActiveProfileState != "present_valid" {
		status.State = "invalid"
	} else if status.ReadySlotCount == 3 {
		status.State = "ready"
	} else {
		status.State = "degraded"
	}
	return status
}

func (s *Server) runtimeSlotStatus(holder *routingProfileResolverHolder, pm *provider.ProviderManager, modelName, safeState string) RuntimeSlotStatus {
	result := RuntimeSlotStatus{State: safeRuntimeSlotState(safeState)}
	if holder == nil || holder.resolver == nil {
		return result
	}
	slot, ok := holder.resolver.ResolveSlot(modelName)
	if !ok {
		return result
	}
	if pm == nil {
		result.State = "reference_unresolved"
		return result
	}
	target, err := pm.ResolveModel(slot.ProviderKey + "/" + slot.UpstreamModel)
	if err != nil || target == nil || len(target.Candidates) == 0 {
		result.State = "reference_unresolved"
		return result
	}
	result.State = "ready"
	result.Provider = safeRuntimeProvider(slot.ProviderKey)
	result.UpstreamModel = safeRuntimeModel(slot.UpstreamModel)
	result.Mode = safeRuntimeMode(slot.Mode)
	result.ConfiguredEffort = safeRuntimeEffort(slot.Reasoning)
	result.CredentialState = credentialStateFor(pm, slot.ProviderKey)
	return result
}

func safeRuntimeSlotState(value string) string {
	switch value {
	case "ready", "missing", "invalid", "reference_unresolved":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeEnum(value, fallback string, allowed ...string) string {
	for _, item := range allowed {
		if value == item {
			return value
		}
	}
	return fallback
}

func safeServerInstanceForRuntime(value string) string {
	if strings.HasPrefix(value, "server#") && runtimeDigitsOnly(value[len("server#"):]) {
		return value
	}
	return "unknown"
}

func runtimeDigitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func safeRuntimeProvider(value string) string {
	switch value {
	case "deepseek", "minimax", "kimi", "mimo", "openrouter", "anthropic", "openai":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeModel(value string) string {
	switch value {
	case "deepseek-v4-flash", "deepseek-v4-pro":
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeMode(value string) string {
	switch value {
	case string(routingprofile.ModeNormal), string(routingprofile.ModeThinking):
		return value
	default:
		return "unknown"
	}
}

func safeRuntimeEffort(value *string) string {
	if value == nil || *value == "" {
		return "none"
	}
	switch *value {
	case "low", "high", "max":
		return *value
	default:
		return "unknown"
	}
}

func credentialStateFor(pm *provider.ProviderManager, providerKey string) string {
	if pm == nil {
		return "unknown"
	}
	for _, info := range pm.CredentialStatus() {
		if info.ProviderID == providerKey {
			return safeCredentialState(info.State)
		}
	}
	return "unknown"
}

func aggregateCredentialState(pm *provider.ProviderManager) string {
	if pm == nil {
		return "unknown"
	}
	statuses := pm.CredentialStatus()
	if len(statuses) == 0 {
		return "unknown"
	}
	seenAvailable, seenUnverified, seenUnavailable, seenMissing := false, false, false, false
	for _, info := range statuses {
		switch info.State {
		case provider.StateAvailable:
			seenAvailable = true
		case provider.StateUnverified:
			seenUnverified = true
		case provider.StateUnavailable:
			seenUnavailable = true
		case provider.StateMissing:
			seenMissing = true
		}
	}
	switch {
	case seenUnavailable:
		return "unavailable"
	case seenAvailable:
		return "available"
	case seenUnverified:
		return "unverified"
	case seenMissing:
		return "missing"
	default:
		return "unknown"
	}
}

func safeCredentialState(value string) string {
	switch value {
	case provider.StateAvailable, provider.StateMissing, provider.StateUnavailable, provider.StateUnverified:
		return value
	default:
		return "unknown"
	}
}

func (s *Server) logRoutingResolverState(event string) {
	status := s.RoutingResolverStatus()
	logger.Info("routing resolver lifecycle",
		"event", event,
		"server_instance", status.ServerInstance,
		"generation", status.Generation,
		"install_source", status.InstallSource,
		"config_source", status.ConfigSource,
		"resolver_present", status.ResolverPresent,
		"extension_state", status.RoutingProfileState.ExtensionState,
		"active_profile_state", status.RoutingProfileState.ActiveProfileState,
		"slot_count", status.RoutingProfileState.SlotCount,
		"sol_state", status.RoutingProfileState.SolState,
		"terra_state", status.RoutingProfileState.TerraState,
		"luna_state", status.RoutingProfileState.LunaState)
}

func (s *Server) runtimeSnapshot() *runtime.ConfigSnapshot {
	if s.runtime == nil {
		return nil
	}
	return s.runtime.Current()
}

func (s *Server) activeProviderManager() *provider.ProviderManager {
	if snap := s.runtimeSnapshot(); snap != nil && snap.ProviderMgr != nil {
		return snap.ProviderMgr
	}
	return s.providerMgr
}

func (s *Server) activeProviderDefs() map[string]config.ProviderDef {
	if snap := s.runtimeSnapshot(); snap != nil {
		return snap.Config.ProviderDefs
	}
	return nil
}

func (s *Server) activeChatClient(providerKey string) any {
	// Check runtime-driven cache first.
	s.clientCacheMu.RLock()
	if cached, ok := s.clientCache[providerKey]; ok {
		s.clientCacheMu.RUnlock()
		return cached
	}
	s.clientCacheMu.RUnlock()

	if snap := s.runtimeSnapshot(); snap != nil {
		if def, ok := snap.Config.ProviderDefs[providerKey]; ok && def.Protocol == config.ProtocolOpenAIChat {
			client := chat.NewClient(chat.ClientConfig{
				BaseURL:   def.BaseURL,
				APIKey:    def.APIKey,
				UserAgent: def.UserAgent,
				Client:    s.proxyHTTP,
			})
			s.clientCacheMu.Lock()
			s.clientCache[providerKey] = client
			s.clientCacheMu.Unlock()
			return client
		}
	}
	return s.chatClients[providerKey]
}

func (s *Server) activeGoogleClient(providerKey string) any {
	// Check runtime-driven cache first.
	s.googleCacheMu.RLock()
	if cached, ok := s.googleCache[providerKey]; ok {
		s.googleCacheMu.RUnlock()
		return cached
	}
	s.googleCacheMu.RUnlock()

	if snap := s.runtimeSnapshot(); snap != nil {
		if def, ok := snap.Config.ProviderDefs[providerKey]; ok && def.Protocol == config.ProtocolGoogleGenAI {
			client := google.NewClient(google.ClientConfig{
				BaseURL:   def.BaseURL,
				APIKey:    def.APIKey,
				Project:   def.Project,
				Location:  def.Location,
				Version:   def.APIVersion,
				UserAgent: def.UserAgent,
				Client:    s.proxyHTTP,
			})
			s.googleCacheMu.Lock()
			s.googleCache[providerKey] = client
			s.googleCacheMu.Unlock()
			return client
		}
	}
	return s.googleClients[providerKey]
}

func New(cfg Config) *Server {
	if cfg.SessionManager == nil {
		cfg.SessionManager = newDefaultSessionManager(cfg)
	}
	s := &Server{
		serverInstance:         fmt.Sprintf("server#%d", serverInstanceCounter.Add(1)),
		adapterRegistry:        cfg.AdapterRegistry,
		provider:               cfg.Provider,
		providerMgr:            cfg.ProviderMgr,
		trafficRouting:         cfg.TrafficRouting,
		routingObservationSink: cfg.RoutingObservationSink,
		openAIHTTP:             egressobservation.WrapClient(cfg.OpenAIHTTPClient),
		proxyHTTP:              egressobservation.WrapClient(cfg.ProxyHTTPClient),
		tracer:                 cfg.Tracer,
		traceErrors:            cfg.TraceErrors,
		stats:                  cfg.Stats,
		pluginRegistry:         cfg.PluginRegistry,
		mux:                    http.NewServeMux(),
		appConfig:              cfg.AppConfig,
		serverCfg:              cfg.ServerCfg,
		chatClients:            cfg.ChatClients,
		googleClients:          cfg.GoogleClients,
		runtime:                cfg.Runtime,
		store:                  cfg.Store,
		sessionManager:         cfg.SessionManager,
		usageTracker:           cfg.UsageTracker,
		traceWriter:            cfg.TraceWriter,
		routingConfigSource:    safeConfigSource(cfg.RoutingConfigSource),
		clientCache:            make(map[string]*chat.Client),
		googleCache:            make(map[string]*google.Client),
	}
	s.lastResolutionStage.Store("none")
	if cfg.RoutingProfileResolver != nil {
		state := cfg.RoutingProfileState
		if provider, ok := cfg.RoutingProfileResolver.(routingProfileResolverStateProvider); ok {
			state = provider.SafeState()
		}
		state = normalizeSafeResolverState(state)
		generation := uint64(1)
		s.resolverGeneration.Store(generation)
		s.routingProfileResolver.Store(&routingProfileResolverHolder{resolver: cfg.RoutingProfileResolver, state: state, generation: generation, installSource: "startup"})
	}
	s.logRoutingResolverState("startup")
	s.mux.HandleFunc("/v1/responses", s.handleResponses)
	s.mux.HandleFunc("/responses", s.handleResponses)
	s.mux.HandleFunc("/v1/models", s.handleModels)
	s.mux.HandleFunc("/models", s.handleModels)
	s.mux.Handle("/console/", webui.Embedded())
	s.registerPluginRoutes()
	if cfg.Runtime != nil && cfg.Store != nil {
		apiRouter := api.NewRouter(s.store, s.runtime, s.stats, s.pluginRegistry, s)
		s.mux.Handle("/api/v1/", http.StripPrefix("/api/v1", apiRouter))
	}
	return s
}

func safeConfigSource(source string) string {
	if source == "persisted_store" {
		return source
	}
	return "file_seed"
}

func normalizeSafeResolverState(state routingprofile.SafeResolverState) routingprofile.SafeResolverState {
	if state.ExtensionState == "" {
		state.ExtensionState = "invalid"
	}
	if state.ActiveProfileState == "" {
		state.ActiveProfileState = "invalid"
	}
	if state.SolState == "" {
		state.SolState = "invalid"
	}
	if state.TerraState == "" {
		state.TerraState = "invalid"
	}
	if state.LunaState == "" {
		state.LunaState = "invalid"
	}
	return state
}

func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if token := s.currentConfig().AuthToken; token != "" && !isConsoleAssetPath(request.URL.Path) {
		if !checkAuth(request, token) {
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(writer).Encode(openai.ErrorResponse{Error: openai.ErrorObject{
				Message: "未提供有效的认证令牌，请在 Authorization header 中使用 Bearer 方案",
				Type:    "authentication_error",
				Code:    "invalid_auth",
			}})
			return
		}
	}
	s.mux.ServeHTTP(writer, request)
}

func isConsoleAssetPath(path string) bool {
	return path == "/console" || strings.HasPrefix(path, "/console/")
}

func (s *Server) handleModels(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writeOpenAIError(writer, http.StatusMethodNotAllowed, openai.ErrorResponse{Error: openai.ErrorObject{
			Message: "仅支持 GET 请求",
			Type:    "invalid_request_error",
			Code:    "method_not_allowed",
		}})
		return
	}
	models := s.listModels()
	resp := struct {
		Models []map[string]any `json:"models"`
	}{
		Models: models,
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(resp)
}

func (s *Server) listModels() []map[string]any {
	var models []map[string]any

	// Get provider data from runtime (full config snapshot).
	var providerDefs map[string]config.ProviderDef
	var routes map[string]config.RouteEntry
	if s.runtime != nil {
		fullCfg := s.runtime.Current().Config
		providerDefs = fullCfg.ProviderDefs
		routes = fullCfg.Routes
	}

	for key, def := range providerDefs {
		for modelName := range def.Models {
			models = append(models, map[string]any{
				"slug":     key + "/" + modelName,
				"name":     modelName,
				"provider": key,
			})
		}
	}

	for alias, route := range routes {
		displayName := route.DisplayName
		if displayName == "" {
			// When no explicit display_name is configured for this route,
			// derive from the alias slug (e.g. "gpt-5.4" -> "GPT 5.4").
			// This avoids inheriting the underlying model's DisplayName,
			// which would cause duplicates when multiple routes point to the same model.
			displayName = slugDisplayName(alias)
		}
		models = append(models, map[string]any{
			"slug":     alias,
			"name":     displayName,
			"provider": route.Provider,
			"model":    route.Model,
		})
	}
	return models
}

func (s *Server) currentConfig() config.ServerConfig {
	if snap := s.runtimeSnapshot(); snap != nil {
		return config.ServerFromGlobalConfig(&snap.Config)
	}
	return s.serverCfg
}

func (s *Server) CurrentConfig() api.ConfigAccessor {
	return s
}

func (s *Server) AuthToken() string {
	return s.currentConfig().AuthToken
}

func (s *Server) registerPluginRoutes() {
	if s.pluginRegistry == nil {
		return
	}
	s.pluginRegistry.RegisterRoutes(func(pattern string, handler http.Handler) {
		s.mux.Handle(pattern, handler)
	})
}

func InjectWebSearchTool(tools []openai.Tool) []openai.Tool {
	for _, t := range tools {
		if t.Type == "web_search" {
			return tools
		}
	}
	if tools == nil {
		tools = make([]openai.Tool, 0, 1)
	}
	return append(tools, openai.Tool{Type: "web_search"})
}

func (s *Server) Close() error {
	s.onceClose.Do(func() {
		if s.sessionManager != nil {
			s.sessionManager.Stop()
		}
	})
	return nil
}

func computeCostWithProviderPricing(pm *provider.ProviderManager, stats *stats.SessionStats, requestModel, actualModel, providerKey string, usage stats.BillingUsage) float64 {
	if stats == nil {
		return 0
	}
	if pm != nil {
		if meta, ok := pm.ModelMetaFor(actualModel, providerKey); ok {
			freshInput := float64(usage.FreshInputTokens)
			cacheWrite := float64(usage.CacheCreationInputTokens)
			cacheRead := float64(usage.CacheReadInputTokens)
			output := float64(usage.OutputTokens)
			cost := freshInput*meta.InputPrice/1000000 +
				cacheWrite*meta.CacheWritePrice/1000000 +
				cacheRead*meta.CacheReadPrice/1000000 +
				output*meta.OutputPrice/1000000
			if cost > 0 || meta.InputPrice > 0 || meta.OutputPrice > 0 {
				return cost
			}
		}
	}
	return stats.ComputeBillingCost(requestModel, usage)
}

// slugDisplayName converts a route alias slug to a human-readable display name.
// e.g. "gpt-5.4" -> "GPT 5.4", "codex-auto-review" -> "Codex Auto Review"
func slugDisplayName(slug string) string {
	slug = strings.ReplaceAll(slug, "-", " ")
	words := strings.Fields(slug)
	for i, w := range words {
		lower := strings.ToLower(w)
		if len(lower) >= 3 && lower[:3] == "gpt" {
			words[i] = "GPT" + w[3:]
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
	}
	return strings.Join(words, " ")
}

func checkAuth(r *http.Request, expectedToken string) bool {
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return false
	}
	return strings.TrimSpace(auth[7:]) == expectedToken
}

func (s *Server) resolveModelOrFallback(modelName, relayMarker string) (*provider.ResolvedRoute, string, error) {
	return s.resolveModelOrFallbackContext(context.Background(), modelName, relayMarker, false)
}

func (s *Server) resolveModelOrFallbackContext(ctx context.Context, modelName, relayMarker string, emitDiagnostic bool) (resolved *provider.ResolvedRoute, alias string, resolveErr error) {
	var holder *routingProfileResolverHolder
	holder = s.routingProfileResolver.Load()
	diagnostic := newResolverDiagnostic(modelName, s.serverInstance, s.routingConfigSource, holder)
	if emitDiagnostic {
		defer func() {
			s.recordResolverDiagnostic(ctx, diagnostic)
		}()
	}
	if pm := s.activeProviderManager(); pm != nil {
		resolved, err := pm.ResolveModel(modelName)
		if err == nil {
			diagnostic.NormalResult = "explicit_route"
			diagnostic.FinalStage = "explicit_route"
			s.lastLoadedGeneration.Store(0)
			s.lastResolutionStage.Store("explicit_route")
			return resolved, "", nil
		}
		if !isModelNotFound(err) {
			diagnostic.NormalResult = "slot_target_unresolved"
			diagnostic.FinalStage = "not_found"
			return nil, "", err
		}
		// Routing-profile slots are exact Codex model identities and must win
		// over the generic Capture relay fallback. Otherwise the first Desktop
		// request is mapped to the moonbridge route before its slot policy is
		// applied, which loses routing provenance and lets provider defaults
		// re-enable thinking for Luna.
		if holder == nil || holder.resolver == nil {
			diagnostic.NormalResult = "resolver_absent"
			s.lastLoadedGeneration.Store(0)
			s.lastResolutionStage.Store("fallback")
		} else {
			diagnostic.ResolverPresent = true
			diagnostic.ResolverGeneration = holder.generation
			s.lastLoadedGeneration.Store(holder.generation)
			slot, ok := holder.resolver.ResolveSlot(modelName)
			if ok {
				diagnostic.ResolvedSlot = slot.SlotID
				// Build a provider/model direct ref so the ProviderManager
				// resolves to the slot's specific provider, not an ambiguous
				// model name that could match multiple providers.
				targetRef := slot.ProviderKey + "/" + slot.UpstreamModel
				mapped, mappedErr := pm.ResolveModel(targetRef)
				if mappedErr == nil {
					diagnostic.NormalResult = "slot_hit"
					diagnostic.FinalStage = "exact_slot"
					s.lastResolutionStage.Store("exact_slot")
					mode, modeErr := routingprofile.NormalizeSlotMode(slot.Mode, slot.Reasoning)
					if modeErr != nil {
						return nil, "", modeErr
					}
					for i := range mapped.Candidates {
						mapped.Candidates[i].ReasoningMode = mode
						mapped.Candidates[i].RoutingSlot = slot.SlotID
						mapped.Candidates[i].RoutingProfileID = slot.ActiveProfileID
					}
					if mode == routingprofile.ModeThinking && slot.Reasoning != nil {
						for i := range mapped.Candidates {
							mapped.Candidates[i].ReasoningOverride = slot.Reasoning
						}
					}
					return mapped, slot.UpstreamModel, nil
				}
				diagnostic.NormalResult = "slot_target_unresolved"
				s.lastResolutionStage.Store("provider_reference")
			} else {
				diagnostic.NormalResult = "alias_miss"
			}
		}
		// Traffic relay fallback (existing)
		if s.trafficRouting != nil {
			resolveDiag := func(hit, attempted, success bool) {
				logger.Info("traffic model routing resolve",
					"primary_not_found", true,
					"mapping_lookup_hit", hit,
					"target_resolve_attempted", attempted,
					"target_resolve_success", success)
			}
			targetAlias, ok := s.trafficRouting.ObservedModelFor(modelName, relayMarker)
			if !ok || targetAlias == "" {
				diagnostic.FallbackResult = "miss"
				diagnostic.FinalStage = "not_found"
				s.lastResolutionStage.Store("not_found")
				resolveDiag(false, false, false)
			} else {
				mapped, mappedErr := pm.ResolveModel(targetAlias)
				if mappedErr == nil {
					diagnostic.FallbackResult = "hit"
					diagnostic.FinalStage = "fallback"
					s.lastResolutionStage.Store("fallback")
					resolveDiag(true, true, true)
					return mapped, targetAlias, nil
				}
				diagnostic.FallbackResult = "target_unresolved"
				diagnostic.FinalStage = "not_found"
				s.lastResolutionStage.Store("not_found")
				resolveDiag(true, true, false)
			}
		}
		s.lastResolutionStage.Store("not_found")
		diagnostic.FinalStage = "not_found"
		return nil, "", err
	}
	if s.provider != nil {
		diagnostic.NormalResult = "explicit_route"
		diagnostic.FinalStage = "explicit_route"
		return &provider.ResolvedRoute{
			Candidates: []provider.ProviderCandidate{{
				ProviderKey:   "default",
				UpstreamModel: modelName,
				Protocol:      "anthropic",
				Client:        s.provider,
			}},
		}, "", nil
	}
	diagnostic.NormalResult = "resolver_absent"
	diagnostic.FinalStage = "not_found"
	return nil, "", fmt.Errorf("no provider manager configured for model %q", modelName)
}

func newResolverDiagnostic(modelName, serverInstance, configSource string, holder *routingProfileResolverHolder) *trafficanalysis.ResolverDiagnosticInput {
	result := &trafficanalysis.ResolverDiagnosticInput{
		RequestedModel: modelName,
		ServerInstance: serverInstance,
		FallbackResult: "not_consulted",
		KnownAlias:     modelName == "gpt-5.6-sol" || modelName == "gpt-5.6-terra" || modelName == "gpt-5.6-luna",
	}
	if holder == nil || holder.resolver == nil {
		result.NormalResult = "resolver_absent"
		return result
	}
	result.ResolverPresent = true
	result.ResolverGeneration = holder.generation
	result.InstallSource = "startup"
	result.ConfigSource = configSource
	if result.ConfigSource == "" {
		result.ConfigSource = "unknown"
	}
	result.InstallSource = holder.installSource
	if result.InstallSource == "" {
		result.InstallSource = "none"
	}
	stateProvider, ok := holder.resolver.(routingProfileResolverStateProvider)
	if !ok {
		result.ExtensionState = "unknown"
		result.ActiveProfileState = "unknown"
		result.SolState = "unknown"
		result.TerraState = "unknown"
		result.LunaState = "unknown"
		return result
	}
	state := stateProvider.SafeState()
	result.ExtensionState = state.ExtensionState
	result.ActiveProfileState = state.ActiveProfileState
	result.SlotCount = state.SlotCount
	result.SolState = state.SolState
	result.TerraState = state.TerraState
	result.LunaState = state.LunaState
	return result
}

func (s *Server) recordResolverDiagnostic(ctx context.Context, diagnostic *trafficanalysis.ResolverDiagnosticInput) {
	if s.routingObservationSink == nil || diagnostic == nil {
		return
	}
	s.routingObservationSink.RecordGatewayEvent(trafficanalysis.GatewayEventInput{
		Kind:           trafficanalysis.ObservationRoutingResolutionDiagnosed,
		CorrelationKey: routingObservationKey(ctx),
		Resolver:       diagnostic,
	})
}

func isModelNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(err.Error(), "no route or provider found for model ")
}

func requestHasImage(input json.RawMessage) bool {
	if len(input) == 0 || string(input) == "null" {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(input, &items); err == nil {
		for _, it := range items {
			switch it.Type {
			case "input_image", "image", "image_url":
				return true
			}
		}
		return false
	}
	return false
}

func (s *Server) filterCandidatesByInput(candidates []provider.ProviderCandidate, input json.RawMessage) ([]provider.ProviderCandidate, string) {
	pm := s.activeProviderManager()
	if pm == nil {
		return candidates, ""
	}
	hasImage := requestHasImage(input)
	if !hasImage {
		return candidates, ""
	}
	filtered := make([]provider.ProviderCandidate, 0, len(candidates))
	removedCount := 0
	for _, c := range candidates {
		meta, ok := pm.ModelMetaFor(c.UpstreamModel, c.ProviderKey)
		if !ok || !hasModalityImage(meta.InputModalities) {
			removedCount++
			logger.L().Debug("过滤掉不支持图片的提供商候选", "provider", c.ProviderKey, "model", c.UpstreamModel)
			continue
		}
		filtered = append(filtered, c)
	}
	var reason string
	if removedCount > 0 {
		reason = fmt.Sprintf("请求包含图片输入，已过滤 %d 个不支持图片的提供商候选", removedCount)
	}
	return filtered, reason
}

func hasModalityImage(modalities []string) bool {
	for _, m := range modalities {
		if m == "image" {
			return true
		}
	}
	return false
}

func newDefaultSessionManager(cfg Config) session.Manager {
	return session.NewInMemoryManager(&sessionConfigAdapter{runtime: cfg.Runtime, fallback: cfg.AppConfig}, cfg.PluginRegistry)
}

type sessionConfigAdapter struct {
	runtime  *runtime.Runtime
	fallback config.ServerConfig
}

func (a *sessionConfigAdapter) currentServerConfig() config.ServerConfig {
	if a.runtime != nil {
		snap := a.runtime.Current()
		if snap != nil {
			return config.ServerFromGlobalConfig(&snap.Config)
		}
	}
	return a.fallback
}

func (a *sessionConfigAdapter) SessionTTL() time.Duration {
	raw := a.currentServerConfig().SessionTTL
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return 24 * time.Hour
}

func (a *sessionConfigAdapter) MaxSessions() int {
	return a.currentServerConfig().MaxSessions
}

func NewSessionConfigAdapter(cfg config.ServerConfig) session.ConfigAccessor {
	return &sessionConfigAdapter{fallback: cfg}
}

func NewSessionConfigAdapterFromRuntime(rt *runtime.Runtime, fallback config.ServerConfig) session.ConfigAccessor {
	return &sessionConfigAdapter{runtime: rt, fallback: fallback}
}
