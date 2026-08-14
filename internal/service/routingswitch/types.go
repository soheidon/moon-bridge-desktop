// Package routingswitch contains the side-effect-free state contract used by
// the desktop route status adapter and the operation gate shared by route,
// traffic, and recovery mutations.
package routingswitch

// Phase is the safe, user-facing classification of the route lifecycle.
type Phase string

const (
	PhaseNative                  Phase = "native"
	PhaseActivatingDeepSeek      Phase = "activating_deepseek"
	PhaseDeepSeekRestartRequired Phase = "deepseek_restart_required"
	PhaseDeepSeekActive          Phase = "deepseek_active"
	PhaseDeactivatingDeepSeek    Phase = "deactivating_deepseek"
	PhaseNativeRestartRequired   Phase = "native_restart_required"
	PhaseRecoveryRequired        Phase = "recovery_required"
	PhaseError                   Phase = "error"
)

// DesiredRoute is the selected destination of a future route transition.
type DesiredRoute string

const (
	DesiredRouteNative   DesiredRoute = "native"
	DesiredRouteDeepSeek DesiredRoute = "deepseek"
	DesiredRouteUnknown  DesiredRoute = "unknown"
)

// ConfigRoute is the redacted classification of the on-disk Codex route.
type ConfigRoute string

const (
	ConfigRouteOriginal ConfigRoute = "original"
	ConfigRouteCapture  ConfigRoute = "capture"
	ConfigRouteUnknown  ConfigRoute = "unknown"
)

// GatewayState is deliberately limited to process/listener state.
type GatewayState string

const (
	GatewayStopped  GatewayState = "stopped"
	GatewayStarting GatewayState = "starting"
	GatewayRunning  GatewayState = "running"
	GatewayStopping GatewayState = "stopping"
	GatewayError    GatewayState = "error"
	GatewayUnknown  GatewayState = "unknown"
)

// RelayState is the safe wire-state summary of the capture relay.
type RelayState string

const (
	RelayStopped     RelayState = "stopped"
	RelayCapturing   RelayState = "capturing"
	RelayPassthrough RelayState = "passthrough"
	RelayDraining    RelayState = "draining"
	RelayFailed      RelayState = "failed"
)

// AutoSaveStatus is the safe persistence status of the observation log.
type AutoSaveStatus string

const (
	AutoSaveActive    AutoSaveStatus = "active"
	AutoSaveFinalized AutoSaveStatus = "finalized"
	AutoSaveFailed    AutoSaveStatus = "failed"
)

// RouteEvidence is the only evidence classification exposed by RouteStatus.
type RouteEvidence string

const (
	RouteEvidenceNone                 RouteEvidence = "none"
	RouteEvidenceDeepSeekObserved     RouteEvidence = "deepseek_observed"
	RouteEvidenceNativeUserUnverified RouteEvidence = "native_user_confirmed_unverified"
)

// RouteStatus is a dedicated, secret-safe DTO. It intentionally contains no
// URL, path, hash, backup identifier, gateway identity, generation, or error.
type RouteStatus struct {
	Phase            Phase           `json:"phase"`
	TransitionID     TransitionID    `json:"transitionId,omitempty"`
	DesiredRoute     DesiredRoute    `json:"desiredRoute"`
	ConfigRoute      ConfigRoute     `json:"configRoute"`
	GatewayState     GatewayState    `json:"gatewayState"`
	RelayState       RelayState      `json:"relayState"`
	RecordingActive  bool            `json:"recordingActive"`
	AutoSaveStatus   *AutoSaveStatus `json:"autoSaveStatus,omitempty"`
	RestartRequired  bool            `json:"restartRequired"`
	RouteEvidence    RouteEvidence   `json:"routeEvidence"`
	RecoveryRequired bool            `json:"recoveryRequired"`
}

// Inputs are already-redacted observations assembled by an adapter. No raw
// filesystem, config, URL, or recovery values belong in this type.
type Inputs struct {
	TransitionID      TransitionID
	TransitionPhase   Phase
	TransitionActive  bool
	DesiredRoute      DesiredRoute
	ConfigRoute       ConfigRoute
	GatewayState      GatewayState
	RelayState        RelayState
	RecordingActive   bool
	AutoSaveStatus    *AutoSaveStatus
	RestartRequired   bool
	RouteEvidence     RouteEvidence
	RecoveryRequired  bool
	ConfigConflict    bool
	IntegrationActive bool
	Contradiction     bool
}

// Derive classifies redacted observations without I/O, locks, mutation,
// timestamps, or ID generation.
func Derive(in Inputs) RouteStatus {
	status := RouteStatus{
		Phase:            PhaseNative,
		TransitionID:     in.TransitionID,
		DesiredRoute:     normalizeDesired(in.DesiredRoute),
		ConfigRoute:      normalizeConfigRoute(in.ConfigRoute),
		GatewayState:     normalizeGateway(in.GatewayState),
		RelayState:       in.RelayState,
		RecordingActive:  in.RecordingActive,
		AutoSaveStatus:   in.AutoSaveStatus,
		RestartRequired:  in.RestartRequired,
		RouteEvidence:    normalizeEvidence(in.RouteEvidence),
		RecoveryRequired: in.RecoveryRequired,
	}

	if in.RecoveryRequired || in.ConfigConflict || in.Contradiction {
		status.Phase = PhaseRecoveryRequired
		status.RecoveryRequired = true
		return status
	}
	if in.TransitionActive {
		if in.TransitionID == "" || !isTransitionPhase(in.TransitionPhase) {
			status.Phase = PhaseRecoveryRequired
			status.RecoveryRequired = true
			return status
		}
		status.Phase = in.TransitionPhase
		return status
	}
	if status.DesiredRoute == DesiredRouteDeepSeek &&
		status.ConfigRoute == ConfigRouteCapture &&
		status.GatewayState == GatewayRunning &&
		status.RelayState == RelayCapturing &&
		status.RecordingActive &&
		status.RouteEvidence == RouteEvidenceDeepSeekObserved {
		status.Phase = PhaseDeepSeekActive
		return status
	}
	if in.IntegrationActive || status.ConfigRoute == ConfigRouteCapture || in.RestartRequired {
		status.Phase = PhaseDeepSeekRestartRequired
		status.RestartRequired = true
		return status
	}
	if status.ConfigRoute == ConfigRouteOriginal &&
		(status.RelayState == RelayPassthrough || status.RelayState == RelayDraining) {
		status.Phase = PhaseNativeRestartRequired
		status.RestartRequired = true
		return status
	}
	if status.ConfigRoute == ConfigRouteUnknown || status.DesiredRoute == DesiredRouteUnknown {
		status.Phase = PhaseError
		return status
	}
	return status
}

func isTransitionPhase(phase Phase) bool {
	switch phase {
	case PhaseActivatingDeepSeek, PhaseDeactivatingDeepSeek,
		PhaseDeepSeekRestartRequired, PhaseNativeRestartRequired:
		return true
	default:
		return false
	}
}

func normalizeDesired(value DesiredRoute) DesiredRoute {
	switch value {
	case DesiredRouteNative, DesiredRouteDeepSeek:
		return value
	default:
		return DesiredRouteUnknown
	}
}

func normalizeConfigRoute(value ConfigRoute) ConfigRoute {
	switch value {
	case ConfigRouteOriginal, ConfigRouteCapture:
		return value
	default:
		return ConfigRouteUnknown
	}
}

func normalizeGateway(value GatewayState) GatewayState {
	switch value {
	case GatewayStopped, GatewayStarting, GatewayRunning, GatewayStopping, GatewayError:
		return value
	default:
		return GatewayUnknown
	}
}

func normalizeEvidence(value RouteEvidence) RouteEvidence {
	switch value {
	case RouteEvidenceDeepSeekObserved, RouteEvidenceNativeUserUnverified:
		return value
	default:
		return RouteEvidenceNone
	}
}
