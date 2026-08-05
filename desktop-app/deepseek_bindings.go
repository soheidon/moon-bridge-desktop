package main

import (
	"moonbridge/internal/service/deepseek"
)

// LoadDeepSeekSettings returns the current provider configuration through the
// live gateway session's control token.
func (a *App) LoadDeepSeekSettings() DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("LoadDeepSeekSettings")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return deepSeekGatewayNotRunning("LoadDeepSeekSettings")
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Load(a.appCtx)
	if err != nil {
		return deepSeekError("LoadDeepSeekSettings", "load", "deepseek_load_failed", err)
	}
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
}

// ValidateDeepSeekSettings validates the input in isolation: it needs no
// gateway session, so it works while the gateway is stopped. The result is a
// masked preview of the normalized input, never a plaintext key.
func (a *App) ValidateDeepSeekSettings(input deepseek.Input) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("ValidateDeepSeekSettings")
	}
	if err := input.Validate(); err != nil {
		return deepSeekError("ValidateDeepSeekSettings", "validation", "deepseek_validate_failed", err)
	}
	snap := deepSeekInputSnapshot(input)
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(&snap)})
}

// SaveDeepSeekSettings reconciles the provider graph, then refreshes the
// session config so a subsequent codex launch uses the new settings.
func (a *App) SaveDeepSeekSettings(input deepseek.Input) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("SaveDeepSeekSettings")
	}
	if err := input.Validate(); err != nil {
		return deepSeekError("SaveDeepSeekSettings", "validation", "deepseek_validate_failed", err)
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return deepSeekGatewayNotRunning("SaveDeepSeekSettings")
	}
	ctrl := a.newDeepSeek("http://"+session.Address, session.ControlToken)
	snap, err := ctrl.Save(a.appCtx, input)
	if err != nil {
		return deepSeekError("SaveDeepSeekSettings", "save", "deepseek_save_failed", err)
	}
	if cfg, derr := a.deriveConfigCodex(session); derr != nil {
		// Saved, but the session could not pick up the new config from the live
		// gateway effective store: refuse codex launches until the gateway
		// restarts. Non-secret partial-success info rides in Details; Value stays
		// nil per the envelope contract.
		session.ConfigValid = false
		return DesktopCommandResult{
			OK: false,
			Error: &CommandError{
				Operation:       "SaveDeepSeekSettings",
				Stage:           "refresh_session_config",
				Code:            "deepseek_saved_session_refresh_failed",
				Message:         "settings saved but session config refresh failed",
				Retryable:       true,
				MutationStarted: true,
				Details: map[string]any{
					"saved":                  true,
					"sessionConfigRefreshed": false,
					"requiresGatewayRestart": true,
				},
			},
		}
	} else {
		session.Config = cfg
		session.ConfigValid = true
	}
	return okDesktop(&DesktopSnapshot{DeepSeek: desktopDeepSeek(snap)})
}

// deepSeekInputSnapshot builds the masked preview snapshot for a validated
// input without consulting the gateway. A provided api key collapses to the
// same irreversible "configured" mask form used elsewhere; plaintext never
// appears in the result. Graph-derived flags are left false — nothing was
// read from or written to the gateway.
func deepSeekInputSnapshot(input deepseek.Input) deepseek.Snapshot {
	in := input.Normalized()
	selected := in.SelectedModel()
	reasoning := in.ProReasoning
	allowed := deepseek.AllowedReasoningEfforts(deepseek.ModelPro)
	if selected == deepseek.ModelFlash {
		reasoning = in.FlashReasoning
		allowed = deepseek.AllowedReasoningEfforts(deepseek.ModelFlash)
	}
	keySet := in.APIKey != ""
	masked := ""
	if keySet {
		masked = "configured"
	}
	return deepseek.Snapshot{
		GatewayRunning:                false,
		ProviderExists:                false,
		APIKeySet:                     keySet,
		APIKeyMasked:                  masked,
		Configured:                    false,
		Active:                        false,
		SelectedModel:                 selected,
		DefaultModel:                  in.DefaultModel,
		ReasoningEffort:               reasoning,
		ReasoningExplicitlyConfigured: true,
		AllowedReasoningEfforts:       allowed,
		RouteAlias:                    deepseek.RouteID,
		Pro: deepseek.ModelConfig{
			ModelID:   deepseek.ModelPro,
			Reasoning: in.ProReasoning,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelPro),
		},
		Flash: deepseek.ModelConfig{
			ModelID:   deepseek.ModelFlash,
			Reasoning: in.FlashReasoning,
			Supported: deepseek.AllowedReasoningEfforts(deepseek.ModelFlash),
		},
	}
}
