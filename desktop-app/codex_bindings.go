package main

import (
	"os"
	"path/filepath"

	"moonbridge/internal/config"
	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/traffictransaction"
)

const (
	codexStatusEvent            = "codex-status"
	codexOperationProgressEvent = "codex-operation-progress"
)

type LaunchCodexRequest struct {
	ProjectDirectory string `json:"projectDirectory"`
}

type StopCodexRequest struct{}

// LaunchCodex launches a visible PowerShell terminal running codex. It
// requires a running gateway with a fresh session config; the derived
// launch options come entirely from the session (never from the file).
func (a *App) LaunchCodex(req LaunchCodexRequest) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("LaunchCodex")
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return errDesktop("LaunchCodex", "gateway_check", "codex_gateway_not_running", "gateway is not running", false)
	}
	cfg, cerr := a.deriveConfigCodex(session)
	if cerr != nil || !session.ConfigValid {
		return errDesktop("LaunchCodex", "config", "codex_gateway_session_stale", "gateway session config is stale; restart the gateway", false)
	}
	opts := a.codexLaunchOptions(&cfg, session, req)

	a.codexMu.Lock()
	defer a.codexMu.Unlock()
	a.codexOp = "launch"
	st, err := a.codex.Launch(a.appCtx, opts)
	if err != nil {
		return codexError("LaunchCodex", "launch", err)
	}
	a.emitCodexStatus(st)
	return okDesktop(&DesktopSnapshot{Codex: desktopCodexState(st)})
}

// StopCodex stops the terminal session gracefully. It does not need the
// gateway running.
func (a *App) StopCodex(StopCodexRequest) DesktopCommandResult {
	a.codexMu.Lock()
	defer a.codexMu.Unlock()
	if a.closed.Load() {
		return hostClosed("StopCodex")
	}
	st, err := a.codex.Stop(a.appCtx, codexlauncher.StopReasonGraceful)
	if err != nil {
		res := codexError("StopCodex", "stop", err)
		if res.Error != nil && res.Error.Stage == "stop" {
			// Non-secret partial state: the terminal's current status even when
			// both stop paths failed.
			res.Error.Details = map[string]any{"codexState": desktopCodexState(st)}
		}
		return res
	}
	a.emitCodexStatus(st)
	return okDesktop(&DesktopSnapshot{Codex: desktopCodexState(st)})
}

// RestartCodex stops the current session (always possible, gateway
// independent) and launches a new one; only the new-launch part requires the
// gateway.
func (a *App) RestartCodex(req LaunchCodexRequest) DesktopCommandResult {
	a.operationMu.Lock()
	defer a.operationMu.Unlock()
	if a.closed.Load() {
		return hostClosed("RestartCodex")
	}

	a.codexMu.Lock()
	defer a.codexMu.Unlock()

	if _, err := a.codex.Stop(a.appCtx, codexlauncher.StopReasonGraceful); err != nil {
		return codexError("RestartCodex", "stop", err)
	}
	session, ok := a.ensureActiveSession()
	if !ok {
		return errDesktop("RestartCodex", "gateway_check", "codex_gateway_not_running", "gateway is not running", false)
	}
	cfg, cerr := a.deriveConfigCodex(session)
	if cerr != nil || !session.ConfigValid {
		return errDesktop("RestartCodex", "config", "codex_gateway_session_stale", "gateway session config is stale; restart the gateway", false)
	}
	opts := a.codexLaunchOptions(&cfg, session, req)

	a.codexOp = "restart"
	st, err := a.codex.Launch(a.appCtx, opts)
	if err != nil {
		return codexError("RestartCodex", "launch", err)
	}
	a.emitCodexStatus(st)
	return okDesktop(&DesktopSnapshot{Codex: desktopCodexState(st)})
}

// CodexStatus returns the current terminal-session state. It does not need the
// gateway running.
func (a *App) CodexStatus() DesktopCommandResult {
	a.codexMu.Lock()
	defer a.codexMu.Unlock()
	if a.closed.Load() {
		return hostClosed("CodexStatus")
	}
	st := a.codex.Status()
	return okDesktop(&DesktopSnapshot{Codex: desktopCodexState(st)})
}

// codexLaunchOptions derives the launcher options from a freshly derived
// effective config. Route validation happens inside the launcher
// (checking_route), so this derivation cannot fail; the base URL and auth token
// always come from the live session. Only the live server token is used for
// codex auth.json — masked provider api_keys from the effective payload are
// never copied into the launch options.
func (a *App) codexLaunchOptions(cfg *config.Config, session *gatewaySession, req LaunchCodexRequest) codexlauncher.LaunchOptions {
	serverCfg := config.ServerFromGlobalConfig(cfg)
	serverCfg.AuthToken = session.ServerToken // auth.json must match the actual run
	return codexlauncher.LaunchOptions{
		CodexHome:        resolveCodexHome(),
		ProjectDirectory: req.ProjectDirectory,
		ModelAlias:       deepseek.RouteID,
		// Codex must point at the stable front door (:38440), not the gateway
		// backend (:38442) — otherwise a running Codex would hit a dead listener
		// the moment the gateway backend stops.
		BaseURL:     "http://" + traffictransaction.FrontDoorAddress + "/v1",
		AuthToken:   session.ServerToken,
		ProviderCfg: config.ProviderFromGlobalConfig(cfg),
		PluginCfg:   config.PluginFromGlobalConfig(cfg),
		ServerCfg:   serverCfg,
	}
}

func (a *App) emitCodexStatus(st codexlauncher.State) {
	a.emitEvents(codexStatusEvent, desktopCodexState(st))
}

// emitCodexProgress is the launcher's progress callback. It is invoked
// synchronously inside Launch/Restart, which the bindings call under codexMu,
// so reading a.codexOp is lock-guarded.
func (a *App) emitCodexProgress(operation, stage, detail string) {
	payload := map[string]any{"operation": operation, "stage": stage}
	if detail != "" {
		payload["detail"] = detail
	}
	a.emitEvents(codexOperationProgressEvent, payload)
}

// resolveCodexHome returns the Desktop-owned codex home the launcher publishes
// into: %APPDATA%\Moon Bridge Desktop\codex-home (old paths.rs behavior).
func resolveCodexHome() string {
	return filepath.Join(os.Getenv("APPDATA"), "Moon Bridge Desktop", "codex-home")
}
