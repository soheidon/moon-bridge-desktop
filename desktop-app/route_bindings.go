package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/routingswitch"
	"moonbridge/internal/service/trafficanalysis"
)

// RouteStatusCommandResult keeps the read-only route DTO separate from the
// broad DesktopSnapshot contract used by existing bindings.
type RouteStatusCommandResult struct {
	OK    bool                       `json:"ok"`
	Value *routingswitch.RouteStatus `json:"value,omitempty"`
	Error *CommandError              `json:"error,omitempty"`
}

// RouteStatus is read-only. It performs only safe source reads and returns a
// dedicated DTO; it never starts a process, edits config, or mutates Recovery.
func (a *App) RouteStatus() RouteStatusCommandResult {
	if !a.trafficReadAllowed() {
		return routeStatusError("lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	ctx, cancel := context.WithTimeout(a.appCtx, 5*time.Second)
	defer cancel()
	status, err := a.routeStatus(ctx)
	if err != nil {
		return routeStatusError("snapshot", "route_status_unavailable", "route status is unavailable", true)
	}
	return RouteStatusCommandResult{OK: true, Value: status}
}

func routeStatusError(stage, code, message string, retryable bool) RouteStatusCommandResult {
	result := errDesktop("RouteStatus", stage, code, message, retryable)
	return RouteStatusCommandResult{OK: false, Error: result.Error}
}

func (a *App) routeStatus(ctx context.Context) (*routingswitch.RouteStatus, error) {
	if a.recovery == nil || a.codexConfig == nil || a.traffic == nil || a.svc == nil {
		return nil, errors.New("route status source unavailable")
	}
	rootEditor, ok := a.codexConfig.(recoveryRootEditor)
	if !ok {
		return nil, errors.New("route status config source unavailable")
	}
	current, err := rootEditor.ReadRootURL(ctx)
	if err != nil {
		return nil, errors.New("route status config read failed")
	}
	recoveryState, err := a.recovery.Load(ctx)
	if err != nil {
		return nil, errors.New("route status recovery read failed")
	}
	trafficState := a.traffic.Status()
	gatewayState := a.svc.Status()
	configRoute := classifyConfigRoute(current, recoveryState)
	inputs := routingswitch.Inputs{
		ConfigRoute:     configRoute,
		GatewayState:    safeGatewayState(gatewayState.Status),
		RelayState:      safeRelayState(trafficState.CaptureState),
		RecordingActive: trafficState.Mode == trafficanalysis.ModeDesktop && trafficState.CaptureState == "capturing",
		AutoSaveStatus:  safeAutoSaveStatus(a.autoSaveStatus()),
	}
	if recoveryState != nil {
		inputs = applyRecoveryRouteInputs(inputs, recoveryState)
	}
	if recoveryState != nil && recoveryState.IntegrationActive && configRoute == routingswitch.ConfigRouteOriginal {
		inputs.Contradiction = true
	}
	status := routingswitch.Derive(inputs)
	return &status, nil
}

func classifyConfigRoute(current codexconfig.RootURLSnapshot, state *recovery.State) routingswitch.ConfigRoute {
	if state == nil {
		return routingswitch.ConfigRouteOriginal
	}
	if current.ConfigHash != "" && current.ConfigHash == state.ConfigHashAfterApply && state.ConfigHashAfterApply != "" {
		return routingswitch.ConfigRouteCapture
	}
	if current.ConfigHash != "" && current.ConfigHash == state.ConfigHashBeforeApply && state.ConfigHashBeforeApply != "" {
		return routingswitch.ConfigRouteOriginal
	}
	if !state.IntegrationActive && state.ConfigHashBeforeApply == "" && state.ConfigHashAfterApply == "" {
		return routingswitch.ConfigRouteOriginal
	}
	return routingswitch.ConfigRouteUnknown
}

func applyRecoveryRouteInputs(in routingswitch.Inputs, state *recovery.State) routingswitch.Inputs {
	in.RecoveryRequired = recoveryNeedsRouteHandling(state)
	in.IntegrationActive = state.IntegrationActive
	if state.DesiredRoute == string(routingswitch.DesiredRouteDeepSeek) {
		in.DesiredRoute = routingswitch.DesiredRouteDeepSeek
	} else if state.DesiredRoute == string(routingswitch.DesiredRouteNative) || state.DesiredRoute == "" {
		in.DesiredRoute = routingswitch.DesiredRouteNative
	} else {
		in.DesiredRoute = routingswitch.DesiredRouteUnknown
	}
	if in.DesiredRoute == routingswitch.DesiredRouteNative && in.ConfigRoute == routingswitch.ConfigRouteCapture {
		in.DesiredRoute = routingswitch.DesiredRouteDeepSeek
	}
	switch state.RouteEvidence {
	case string(routingswitch.RouteEvidenceDeepSeekObserved):
		in.RouteEvidence = routingswitch.RouteEvidenceDeepSeekObserved
	case string(routingswitch.RouteEvidenceNativeUserUnverified):
		in.RouteEvidence = routingswitch.RouteEvidenceNativeUserUnverified
	}
	if state.TransitionID != "" || state.RoutePhase != "" {
		in.TransitionActive = true
		in.TransitionID = routingswitch.TransitionID(state.TransitionID)
		in.TransitionPhase = routingswitch.Phase(state.RoutePhase)
		if !in.TransitionID.Valid() || in.TransitionPhase == "" {
			in.RecoveryRequired = true
		}
	}
	return in
}

func recoveryNeedsRouteHandling(state *recovery.State) bool {
	if state == nil {
		return false
	}
	if !recovery.IsKnownPhase(state.Phase) || state.CleanupPending != nil {
		return true
	}
	if state.ReconciliationStatus != nil {
		switch *state.ReconciliationStatus {
		case string(recovery.StatusPendingRestore), string(recovery.StatusConfigConflict), string(recovery.StatusConfigUnreadable), string(recovery.StatusConfigPathInvalid):
			return true
		}
	}
	// A legacy active record has no route epoch and cannot be safely correlated
	// with the current capture/gateway run.
	if state.IntegrationActive && state.TransitionID == "" {
		return true
	}
	if (state.TransitionID == "") != (state.RoutePhase == "") {
		return true
	}
	return false
}

func safeGatewayState(status gateway.Status) routingswitch.GatewayState {
	switch status {
	case gateway.StatusStopped:
		return routingswitch.GatewayStopped
	case gateway.StatusStarting:
		return routingswitch.GatewayStarting
	case gateway.StatusRunning:
		return routingswitch.GatewayRunning
	case gateway.StatusStopping:
		return routingswitch.GatewayStopping
	case gateway.StatusFailed:
		return routingswitch.GatewayError
	default:
		return routingswitch.GatewayUnknown
	}
}

func safeRelayState(value string) routingswitch.RelayState {
	switch strings.ToLower(value) {
	case "capturing":
		return routingswitch.RelayCapturing
	case "passthrough":
		return routingswitch.RelayPassthrough
	case "draining":
		return routingswitch.RelayDraining
	case "stopped", "ready", "":
		return routingswitch.RelayStopped
	default:
		return routingswitch.RelayFailed
	}
}

func safeAutoSaveStatus(value string) *routingswitch.AutoSaveStatus {
	var status routingswitch.AutoSaveStatus
	switch value {
	case string(routingswitch.AutoSaveActive):
		status = routingswitch.AutoSaveActive
	case string(routingswitch.AutoSaveFinalized):
		status = routingswitch.AutoSaveFinalized
	case string(routingswitch.AutoSaveFailed):
		status = routingswitch.AutoSaveFailed
	default:
		return nil
	}
	return &status
}
