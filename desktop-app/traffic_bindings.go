package main

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
	"moonbridge/internal/config"
	bridgeapp "moonbridge/internal/service/app"
	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/deepseek"
	"moonbridge/internal/service/gateway"
	"moonbridge/internal/service/recovery"
	"moonbridge/internal/service/routingswitch"
	"moonbridge/internal/service/trafficanalysis"
	"moonbridge/internal/service/traffictransaction"
)

const trafficCommandTimeout = 30 * time.Second

type exitState string

const (
	exitIdle      exitState = "idle"
	exitRequested exitState = "requested"
	exitConfirmed exitState = "confirmed"
	exitCancelled exitState = "cancelled"
)

type ConfirmExitInput struct {
	Confirm        bool `json:"confirm"`
	DiscardUnsaved bool `json:"discardUnsaved"`
}

type ExitConfirmationPayload struct {
	Reason              string `json:"reason"`
	TrafficActive       bool   `json:"trafficActive"`
	GatewayActive       bool   `json:"gatewayActive"`
	UnsavedObservations bool   `json:"unsavedObservations"`
	RecoveryRequired    bool   `json:"recoveryRequired"`
}

// FinishTrafficRelayRequest contains only the explicit user confirmation
// needed when auto-log data may still be unsaved.
type FinishTrafficRelayRequest struct {
	DiscardUnsaved bool `json:"discardUnsaved"`
}

// FileFilter and SaveFileDialogOptions mirror the Wails runtime dialog types so
// the frontend can pass them as a plain JSON struct.
type FileFilter struct {
	DisplayName string `json:"displayName"`
	Pattern     string `json:"pattern"`
}

type SaveFileDialogOptions struct {
	Title            string       `json:"title"`
	DefaultDirectory string       `json:"defaultDirectory"`
	DefaultFilename  string       `json:"defaultFilename"`
	Filters          []FileFilter `json:"filters"`
}

type TrafficExportRequest struct {
	Destination string `json:"destination"`
}

type TrafficRevealRequest struct {
	Destination string `json:"destination"`
}

type appLifecycle uint8

const (
	lifecycleNew appLifecycle = iota
	lifecycleStarting
	lifecycleStarted
	lifecycleReady
	lifecycleClosing
	lifecycleClosed
)

func (a *App) domReady(ctx context.Context) {
	if a.ctx == nil {
		a.ctx = ctx
	}
	a.lifecycleMu.Lock()
	a.domReadySeen = true
	if a.lifecycle == lifecycleStarted {
		a.lifecycle = lifecycleReady
	}
	a.lifecycleMu.Unlock()
	// A frontend listener is optional; the event sink itself is panic-safe in
	// the binding boundary and no call is made when Wails is not attached.
	a.safeEmit("desktop-status", map[string]any{"ready": true})
}

// beforeClose is synchronous in Wails. It blocks only when the frontend must
// obtain explicit confirmation; all cleanup remains in OnShutdown.
func (a *App) beforeClose(ctx context.Context) bool {
	a.exitMu.Lock()
	if a.exitState == exitConfirmed {
		a.exitMu.Unlock()
		return false
	}
	if a.exitState == exitRequested {
		a.exitMu.Unlock()
		return true
	}
	a.exitMu.Unlock()
	traffic := a.traffic.Status()
	gatewayRunning := a.svc.Status().Status == gateway.StatusRunning
	unsaved := false
	recoveryRequired := false
	recoveryPhase := ""
	reconciliationStatus := ""
	integrationActive := false
	relayActive := false
	discardConfirmed := false
	if a.recovery != nil {
		if state, err := a.recovery.Load(ctx); err == nil && state != nil {
			unsaved = state.UnsavedObservationsMayRemain && !state.UnsavedDiscardConfirmed
			recoveryRequired = !recovery.IsKnownPhase(state.Phase) || state.Phase == recovery.PhaseReconciliationReq || state.Phase == recovery.PhaseReconciliationConf
			recoveryPhase = string(state.Phase)
			integrationActive = state.IntegrationActive
			relayActive = state.RelayActiveLastKnown
			discardConfirmed = state.UnsavedDiscardConfirmed
			if state.ReconciliationStatus != nil {
				reconciliationStatus = *state.ReconciliationStatus
			}
		}
	}
	active := traffic.Mode == trafficanalysis.ModeDesktop || traffic.Mode == trafficanalysis.ModeRecovery ||
		traffic.Mode == trafficanalysis.ModeCaptureOnly && traffic.CaptureState == "passthrough"
	decision := "close"
	reason := ""
	switch {
	case unsaved:
		decision = "unsaved_observations"
		reason = "unsaved_observations"
	case recoveryRequired:
		decision = "recovery_required"
		reason = "recovery_required"
	case active:
		decision = "traffic_active"
		reason = "traffic_active"
	case gatewayRunning:
		decision = "gateway_active"
		reason = "gateway_active"
	}
	log.Printf("desktop close decision: traffic_active=%t gateway_running=%t integration_active=%t relay_active=%t capture_state=%q recovery_phase=%q reconciliation_status=%q unsaved_observations=%t discard_confirmed=%t decision=%q",
		active, gatewayRunning, integrationActive, relayActive, traffic.CaptureState, recoveryPhase, reconciliationStatus, unsaved, discardConfirmed, decision)
	if decision == "close" {
		return false
	}
	a.exitMu.Lock()
	if a.exitState == exitConfirmed || a.exitState == exitRequested {
		a.exitMu.Unlock()
		return true
	}
	a.exitState = exitRequested
	a.exitMu.Unlock()
	a.safeEmit("desktop-exit-confirmation-requested", ExitConfirmationPayload{
		Reason: reason, TrafficActive: active, GatewayActive: gatewayRunning, UnsavedObservations: unsaved, RecoveryRequired: recoveryRequired,
	})
	return true
}

func (a *App) ConfirmExit(input ConfirmExitInput) DesktopCommandResult {
	a.exitMu.Lock()
	if !input.Confirm {
		a.exitState = exitCancelled
		a.exitState = exitIdle
		a.exitMu.Unlock()
		return a.safeSnapshotResult("ConfirmExit")
	}
	if a.exitState != exitRequested && a.exitState != exitConfirmed {
		a.exitMu.Unlock()
		return errDesktop("ConfirmExit", "exit", "exit_confirmation_required", "exit confirmation is required", false)
	}
	a.exitState = exitConfirmed
	a.exitMu.Unlock()
	if input.DiscardUnsaved {
		if err := a.persistExitDiscardEvidence(context.Background()); err != nil {
			a.exitMu.Lock()
			a.exitState = exitRequested
			a.exitMu.Unlock()
			result := errDesktop("ConfirmExit", "recovery", "recovery_checkpoint_failed", "exit confirmation could not be saved", true)
			result.Error.RecoveryRequired = true
			return result
		}
	}
	if a.ctx != nil && !a.safeQuit(a.ctx) {
		a.exitMu.Lock()
		a.exitState = exitRequested
		a.exitMu.Unlock()
		return errDesktop("ConfirmExit", "shutdown", "desktop_quit_failed", "desktop could not begin shutdown", true)
	}
	return a.safeSnapshotResult("ConfirmExit")
}

func (a *App) safeQuit(ctx context.Context) (ok bool) {
	if a.quitDesktop == nil {
		return false
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	a.quitDesktop(ctx)
	return true
}

func (a *App) CancelExit() DesktopCommandResult {
	a.exitMu.Lock()
	a.exitState = exitCancelled
	a.exitState = exitIdle
	a.exitMu.Unlock()
	return a.safeSnapshotResult("CancelExit")
}

func (a *App) safeSnapshotResult(operation string) DesktopCommandResult {
	value, err := a.desktopSnapshot(context.Background())
	if err != nil {
		return errDesktop(operation, "snapshot", "desktop_snapshot_unavailable", "desktop snapshot is unavailable", true)
	}
	return okDesktop(value)
}

func (a *App) persistExitDiscardEvidence(ctx context.Context) error {
	if a.recovery == nil {
		return nil
	}
	return a.recovery.Update(ctx, func(state *recovery.State) error {
		if state == nil {
			return nil
		}
		if state.UnsavedObservationsMayRemain {
			state.UnsavedDiscardConfirmed = true
		}
		return nil
	})
}

func (a *App) safeEmit(name string, payload any) {
	defer func() { _ = recover() }()
	a.emitEvents(name, payload)
}

// trafficGatewayProvider adapts the already-running Gateway service. It never
// exposes the Gateway identity to a binding; the transaction service keeps it
// in its private ownership checks.
type trafficGatewayProvider struct{ app *App }

func (p trafficGatewayProvider) Snapshot(ctx context.Context) (traffictransaction.GatewaySnapshot, error) {
	session, ok := p.app.copySession()
	if !ok {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway session identity is unavailable")
	}
	st := p.app.svc.Status()
	if st.Status != gateway.StatusRunning {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway is not running")
	}
	if st.InstanceID == "" || st.Addr == "" {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway identity is incomplete")
	}
	if session.InstanceID != st.InstanceID || session.ControlToken == "" {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway session identity is unavailable")
	}
	if session.Config.Mode != config.ModeTransform {
		return traffictransaction.GatewaySnapshot{Running: true, InstanceID: st.InstanceID, Address: st.Addr}, nil
	}
	effective, err := deepseek.NewHTTPClient("http://"+st.Addr, session.ControlToken).Effective(ctx)
	if err != nil {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway effective config is unavailable")
	}
	resolved, err := config.FromFileConfigWithOptions(effective, config.LoadOptions{ExtensionSpecs: bridgeapp.BuiltinExtensions().ConfigSpecs()})
	if err != nil {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway effective config is invalid")
	}
	alias := resolved.DefaultModelAlias()
	if alias == "" {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway default route is unavailable")
	}
	route, ok := resolved.Routes[alias]
	if !ok || route.Provider == "" || route.Model == "" {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway default route is incomplete")
	}
	providerDef, ok := resolved.ProviderDefs[route.Provider]
	if !ok || providerDef.BaseURL == "" {
		return traffictransaction.GatewaySnapshot{}, errors.New("gateway default provider is unavailable")
	}
	return traffictransaction.GatewaySnapshot{Running: true, InstanceID: st.InstanceID, Address: st.Addr, DefaultModelAlias: alias, RoutingAvailable: true}, nil
}

type trafficBackupManager struct {
	configPath string
	backupDir  string
}

func (b trafficBackupManager) Create(ctx context.Context) (traffictransaction.BackupRef, error) {
	return b.create(ctx, nil)
}

func (b trafficBackupManager) CreateProtected(ctx context.Context, protected []string) (traffictransaction.BackupRef, error) {
	return b.create(ctx, protected)
}

func (b trafficBackupManager) create(ctx context.Context, protected []string) (traffictransaction.BackupRef, error) {
	if err := ctx.Err(); err != nil {
		return traffictransaction.BackupRef{}, err
	}
	data, err := os.ReadFile(b.configPath)
	if err != nil {
		return traffictransaction.BackupRef{}, err
	}
	path, err := codexconfig.CreateBackupWithProtected(b.backupDir, data, protected)
	if err != nil {
		return traffictransaction.BackupRef{}, err
	}
	return traffictransaction.BackupRef{ID: filepath.Base(path)}, nil
}

func (b trafficBackupManager) Remove(ctx context.Context, ref traffictransaction.BackupRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := codexconfig.ResolveBackupPath(b.backupDir, ref.ID)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

// trafficRecoveryWriter bridges the transaction checkpoint model to the
// existing Recovery v2 store. Internal IDs and URLs remain on disk only where
// the Recovery v2 contract already permits them; they never enter Wails DTOs.
type trafficRecoveryWriter struct {
	store      *recovery.Store
	configHome string
	backupDir  string
}

func (r trafficRecoveryWriter) HasUnresolved(ctx context.Context) (bool, error) {
	st, err := r.store.Load(ctx)
	if err != nil || st == nil {
		if err != nil {
			log.Printf("traffic recovery status load failed: cause=%q", classifyCheckpointFailure(err).Cause)
		}
		return false, err
	}
	// An active integration is the normal journal while this process owns the
	// Desktop transaction; Disable/Finish must be allowed to consume it. Only
	// reconciliation/error phases are unresolved blockers. Startup profile
	// switching separately checks IntegrationActive before rebinding a store.
	return st.Phase == recovery.PhaseReconciliationReq ||
		st.Phase == recovery.PhaseReconciliationConf || st.Phase == recovery.PhaseRestartFailed, nil
}

func (r trafficRecoveryWriter) Current(ctx context.Context) (traffictransaction.Checkpoint, error) {
	st, err := r.store.Load(ctx)
	if err != nil {
		return traffictransaction.Checkpoint{}, err
	}
	if st == nil {
		return traffictransaction.Checkpoint{}, errors.New("recovery state is absent")
	}
	return checkpointFromRecovery(st), nil
}

func (r trafficRecoveryWriter) SetCleanupPending(ctx context.Context, pending traffictransaction.CleanupPending) error {
	return r.store.UpdateOrCreate(ctx, func(current *recovery.State) error {
		current.CleanupPending = &recovery.CleanupPending{
			TransactionID:       pending.TransactionID,
			BackupID:            pending.BackupID,
			RouteMutationResult: pending.RouteMutationResult,
			Status:              pending.Status,
		}
		return nil
	})
}

func (r trafficRecoveryWriter) GetCleanupPending(ctx context.Context) (*traffictransaction.CleanupPending, error) {
	st, err := r.store.Load(ctx)
	if err != nil || st == nil || st.CleanupPending == nil {
		return nil, err
	}
	return &traffictransaction.CleanupPending{
		TransactionID:       st.CleanupPending.TransactionID,
		BackupID:            st.CleanupPending.BackupID,
		RouteMutationResult: st.CleanupPending.RouteMutationResult,
		Status:              st.CleanupPending.Status,
	}, nil
}

func (r trafficRecoveryWriter) ClearCleanupPending(ctx context.Context, transactionID, backupID string) error {
	_, err := r.store.ClearCleanupPending(ctx, transactionID, backupID)
	return err
}

func (r trafficRecoveryWriter) Checkpoint(ctx context.Context, cp traffictransaction.Checkpoint) error {
	err := r.store.UpdateOrCreate(ctx, func(current *recovery.State) error {
		fp, err := recovery.CodexHomeFingerprint(r.configHome)
		if err != nil {
			return err
		}
		phase := recoveryPhaseForCheckpoint(cp)
		if shouldClearStaleAutoLog(current, phase) {
			current.AutoLog = nil
			current.AutoLogStatus = nil
		}
		current.SchemaVersion = recovery.SchemaVersion
		current.Phase = phase
		current.IntegrationTarget = recovery.IntegrationTarget(cp.IntegrationTarget)
		switch cp.IntegrationTarget {
		case traffictransaction.TargetGateway, traffictransaction.TargetAnalysis:
			current.IntegrationActive = true
		case traffictransaction.TargetOriginal:
			current.IntegrationActive = false
		default:
			current.IntegrationActive = cp.IntegrationActive
		}
		current.OperationID = cp.OperationID
		current.ConfigPath = "config.toml"
		current.CodexHomeFingerprint = fp
		current.PreviousOpenaiBaseURLPresent = cp.PreviousPresent
		if cp.PreviousPresent {
			value := cp.PreviousValue
			current.PreviousOpenaiBaseURL = &value
		} else {
			current.PreviousOpenaiBaseURL = nil
		}
		current.AppliedOpenaiBaseURL = cp.AppliedValue
		current.ConfigHashBeforeApply = cp.BeforeHash
		current.ConfigHashAfterApply = cp.AfterHash
		current.BackupPath = nil
		if cp.BackupID != "" {
			path := filepath.Join(r.backupDir, cp.BackupID)
			current.BackupPath = &path
		}
		if current.StartedAt == "" {
			current.StartedAt = time.Now().UTC().Format(time.RFC3339)
		}
		current.CaptureStateLastKnown = cp.CaptureState
		current.RelayActiveLastKnown = cp.RelayActive
		current.UnsavedObservationsMayRemain = cp.UnsavedObservationsMayRemain
		current.UnsavedDiscardConfirmed = cp.UnsavedDiscardConfirmed
		if cp.AutoLogFinalized {
			status := "finalized"
			current.AutoLogStatus = &status
		}
		if cp.ReconciliationStatus != "" {
			value := cp.ReconciliationStatus
			current.ReconciliationStatus = &value
		}
		return nil
	})
	if err != nil {
		f := classifyCheckpointFailure(err)
		log.Printf("traffic checkpoint failed: stage=%q checkpoint_phase=%q recovery_phase=%q cause=%q field=%q",
			"recovery_checkpoint", cp.Phase, recoveryPhaseForCheckpoint(cp), f.Cause, f.Field)
		return err
	}
	if cp.Phase == traffictransaction.PhaseDisableCompleted {
		// S2→S1 demote: Codex is back on the gateway URL, so the recovery record
		// must land on gateway. Logging the actual target here makes the demote
		// (always gateway after the Plan 9-21 fix) observable in the desktop log
		// without exposing the URL or any secret.
		log.Printf("traffic demote: recovery_target=%q original_present=%t", cp.IntegrationTarget, cp.OriginalPresent)
	}
	return nil
}

// checkpointFailureFields is the secret-free subset of a recovery error that is
// safe to log: only the error kind and a field name, never the message or any
// path it may carry.
type checkpointFailureFields struct {
	Cause string
	Field string
}

// classifyCheckpointFailure extracts safe diagnostic fields from a recovery
// write/load error. The Kind is an enum string; the Field is derived only from
// the fixed "invalid recovery <field>" messages produced by normalizeForWrite.
// Anything else yields empty fields so raw error text is never logged.
func classifyCheckpointFailure(err error) checkpointFailureFields {
	var recErr *recovery.Error
	if !errors.As(err, &recErr) {
		return checkpointFailureFields{}
	}
	fields := checkpointFailureFields{Cause: string(recErr.Kind)}
	if msg := recErr.Message; strings.HasPrefix(msg, "invalid recovery ") {
		fields.Field = strings.TrimPrefix(msg, "invalid recovery ")
	}
	return fields
}

// shouldClearStaleAutoLog reports whether a checkpoint that activates a new
// Enable journal may drop the previous session's autoLog evidence. It is
// allowed only when the stored state is resolved (absent, inactive, or
// recovered) and no unsaved observations may remain, so a recovery-required or
// unsaved state is never silently cleared.
func shouldClearStaleAutoLog(current *recovery.State, nextPhase recovery.Phase) bool {
	if current == nil || current.UnsavedObservationsMayRemain {
		return false
	}
	activating := nextPhase == recovery.PhasePrepared ||
		nextPhase == recovery.PhaseCaptureStarted ||
		nextPhase == recovery.PhaseIntegrationApplied
	if !activating {
		return false
	}
	switch current.Phase {
	case "", recovery.PhaseInactive, recovery.PhaseRecovered:
		return true
	default:
		return false
	}
}

func recoveryPhaseForCheckpoint(cp traffictransaction.Checkpoint) recovery.Phase {
	switch cp.DurablePhase {
	case traffictransaction.DurableIntegrationApplied:
		return recovery.PhaseIntegrationApplied
	case traffictransaction.DurableRecovered:
		return recovery.PhaseRecovered
	case traffictransaction.DurableInactive:
		return recovery.PhaseInactive
	case traffictransaction.DurableReconciliationRequired:
		return recovery.PhaseReconciliationReq
	}
	switch cp.Phase {
	case traffictransaction.PhasePrepared:
		return recovery.PhasePrepared
	case traffictransaction.PhaseCaptureStarted, traffictransaction.PhaseCaptureAdopted:
		return recovery.PhaseCaptureStarted
	case traffictransaction.PhaseRecoveryRequired, traffictransaction.PhaseBackoutPending, traffictransaction.PhaseCheckpointUncertain:
		return recovery.PhaseReconciliationReq
	case traffictransaction.PhaseAborted:
		return recovery.PhaseAborted
	default:
		if cp.IntegrationActive {
			return recovery.PhaseIntegrationApplied
		}
		return recovery.PhaseInactive
	}
}

func checkpointFromRecovery(st *recovery.State) traffictransaction.Checkpoint {
	previous := ""
	if st.PreviousOpenaiBaseURL != nil {
		previous = *st.PreviousOpenaiBaseURL
	}
	backup := ""
	if st.BackupPath != nil {
		backup = filepath.Base(*st.BackupPath)
	}
	reconciliationStatus := ""
	if st.ReconciliationStatus != nil {
		reconciliationStatus = *st.ReconciliationStatus
	}
	// The traffic (analysis) layer is active only when the persisted target is
	// analysis. A gateway-only redirect (S1) leaves State.IntegrationActive true
	// for recovery purposes but is NOT active for the traffic transaction's
	// ownership preconditions, so it is derived from the target, not the raw flag.
	trafficActive := st.Target() == recovery.TargetAnalysis
	durable := traffictransaction.DurableInactive
	phase := traffictransaction.Phase(st.Phase)
	switch st.Phase {
	case recovery.PhaseIntegrationApplied:
		durable = traffictransaction.DurableIntegrationApplied
		// Recovery v2 names the durable state integration_applied, while the
		// in-process transaction uses config_committed for the same checkpoint.
		// Preserve that distinction when validating the final Enable state.
		if trafficActive {
			phase = traffictransaction.PhaseConfigCommitted
		}
	case recovery.PhaseRecovered:
		durable = traffictransaction.DurableRecovered
	case recovery.PhaseReconciliationReq, recovery.PhaseReconciliationConf:
		durable = traffictransaction.DurableReconciliationRequired
	}
	return traffictransaction.Checkpoint{
		OperationID: st.OperationID, DurablePhase: durable, Phase: phase,
		BeforeHash: st.ConfigHashBeforeApply, AfterHash: st.ConfigHashAfterApply,
		PreviousPresent: st.PreviousOpenaiBaseURLPresent, PreviousValue: previous,
		AppliedValue: st.AppliedOpenaiBaseURL, BackupID: backup,
		// Recovery v2 intentionally does not persist the in-process Capture
		// generation. A reloaded record is evidence-only and never used to
		// guess ownership, so generation remains zero here.
		CaptureGeneration: 0,
		IntegrationActive: trafficActive, RelayActive: st.RelayActiveLastKnown,
		IntegrationTarget: traffictransaction.IntegrationTarget(st.Target()),
		OriginalPresent:   st.OriginalOpenaiBaseURLPresent,
		CaptureState:                 st.CaptureStateLastKnown,
		UnsavedObservationsMayRemain: st.UnsavedObservationsMayRemain,
		UnsavedDiscardConfirmed:      st.UnsavedDiscardConfirmed,
		AutoLogFinalized:             st.AutoLogStatus != nil && *st.AutoLogStatus == "finalized",
		ReconciliationStatus:         reconciliationStatus,
	}
}

func (a *App) ensureTrafficTransaction() (*traffictransaction.Service, error) {
	configPath, err := a.resolveCodexConfigPath(context.Background())
	if err != nil {
		return nil, err
	}
	configHome := filepath.Dir(configPath)
	if a.trafficTx != nil {
		if a.trafficConfigPath == configPath {
			return a.trafficTx, nil
		}
		return nil, errors.New("traffic transaction is bound to another codex profile")
	}
	if err := a.ensureRecoveryStore(configHome); err != nil {
		return nil, err
	}
	if err := a.ensureTrafficBackupDir(); err != nil {
		return nil, err
	}
	backupDir := a.trafficBackupDir
	store := a.recovery
	configEditor := codexconfig.New(codexconfig.Options{Home: configHome, BackupDir: backupDir})
	a.trafficTx = traffictransaction.New(traffictransaction.Dependencies{
		Gateway:  trafficGatewayProvider{app: a},
		Traffic:  a.traffic,
		Config:   configEditor,
		Backup:   trafficBackupManager{configPath: configPath, backupDir: backupDir},
		Recovery: trafficRecoveryWriter{store: store, configHome: configHome, backupDir: backupDir},
		Events: func(event traffictransaction.Event) {
			a.safeEmit(trafficEvent, event)
		},
	})
	a.trafficConfigPath = configPath
	return a.trafficTx, nil
}

// ensureTrafficBackupDir lazily resolves and caches the transaction backup
// directory. The normal Wails path leaves AppOptions.BackupDir empty and uses
// %LOCALAPPDATA%\Moon Bridge\backups\codex-config; a restore must be able to
// resolve it even when ensureTrafficTransaction has not run.
func (a *App) ensureTrafficBackupDir() error {
	if a.trafficBackupDir != "" {
		return nil
	}
	base, err := recovery.DefaultDir(os.Getenv)
	if err != nil {
		return err
	}
	a.trafficBackupDir = filepath.Join(base, "backups", "codex-config")
	return nil
}

// resolveCodexConfigPath deliberately does not use activeConfigPath. That
// field identifies the Moon Bridge gateway YAML used for the current run;
// Traffic transaction integration edits the user's Codex TOML instead. The
// existing Codex config controller is the single source of truth for profile
// resolution, including CODEX_HOME overrides.
func (a *App) resolveCodexConfigPath(ctx context.Context) (string, error) {
	if a.codexConfig != nil {
		snapshot, err := a.codexConfig.Load(ctx)
		if err == nil && snapshot.Path != "" {
			return snapshot.Path, nil
		}
	}
	return codexconfig.New(codexconfig.Options{}).ResolvePath()
}

func (a *App) ensureRecoveryStore(configHome string) error {
	if a.recovery != nil && a.recoveryHome == "" {
		// An explicitly injected store is already scoped by its owner. Bind it
		// to the first resolved Codex profile, but never replace it implicitly.
		a.recoveryHome = configHome
		return nil
	}
	if a.recovery != nil && a.recoveryHome == configHome {
		return nil
	}
	if a.recovery != nil {
		st, err := a.recovery.Load(context.Background())
		if err != nil {
			return err
		}
		if st != nil && (st.IntegrationActive || st.Phase == recovery.PhaseReconciliationReq || st.Phase == recovery.PhaseReconciliationConf) {
			return errors.New("unresolved recovery belongs to another codex profile")
		}
	}
	base, err := recovery.DefaultDir(os.Getenv)
	if err != nil {
		return err
	}
	a.trafficLogDir = filepath.Join(base, "logs", "traffic-analysis")
	backupDir := filepath.Join(base, "backups", "codex-config")
	store, err := recovery.NewStore(&recovery.Paths{
		RecoveryDir:   filepath.Join(base, "recovery"),
		CodexHome:     configHome,
		BackupDir:     backupDir,
		TrafficLogDir: filepath.Join(base, "logs", "traffic-analysis"),
		AppDataRoot:   os.Getenv("APPDATA"),
	}, "")
	if err != nil {
		return err
	}
	a.recovery = store
	a.recoveryHome = configHome
	return nil
}

func (a *App) trafficSnapshot() *TrafficAnalysisSnapshot {
	st := a.traffic.Status()
	return &TrafficAnalysisSnapshot{
		Mode: string(st.Mode), CaptureState: st.CaptureState, Operation: string(st.Operation),
		Generation: st.Generation, GatewayMatches: st.GatewayInstanceID != "" && st.GatewayAddress != "",
		RelayActive:          st.Mode != trafficanalysis.ModeIdle && st.CaptureState != "stopped",
		IntegrationActive:    st.Mode == trafficanalysis.ModeDesktop || st.Mode == trafficanalysis.ModeRecovery,
		Listening:            st.ListeningAddress != "",
		HTTPRequests:         st.HTTPRequests,
		SSEStreams:           st.SSEStreams,
		WebSocketConnections: st.WebSocketConnections,
		ObservationCount:     st.ObservationCount,
		ObservationCapacity:  st.ObservationCapacity,
		DroppedObservations:  st.DroppedObservations,
		AutoSaveStatus:       a.autoSaveStatus(),
	}
}

func (a *App) recoverySnapshot(ctx context.Context) (*RecoverySnapshot, error) {
	if a.recovery == nil {
		return &RecoverySnapshot{}, nil
	}
	st, err := a.recovery.Load(ctx)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return &RecoverySnapshot{}, nil
	}
	status := ""
	if st.ReconciliationStatus != nil {
		status = *st.ReconciliationStatus
	}
	return &RecoverySnapshot{
		Exists: true, Phase: string(st.Phase), ReconciliationStatus: status,
		IntegrationActive:    st.IntegrationActive,
		RestoreRequired:      status == string(recovery.StatusPendingRestore) || st.IntegrationActive,
		RecoveryRequired:     !recovery.IsKnownPhase(st.Phase) || status == string(recovery.StatusPendingRestore) || status == string(recovery.StatusConfigConflict),
		Conflict:             status == string(recovery.StatusConfigConflict) || st.Phase == recovery.PhaseReconciliationConf,
		ConfirmationRequired: st.UnsavedObservationsMayRemain && !st.UnsavedDiscardConfirmed,
		RestartAttempted:     st.RestartAttempted, UnsavedObservations: st.UnsavedObservationsMayRemain,
	}, nil
}

func (a *App) desktopSnapshot(ctx context.Context) (*DesktopSnapshot, error) {
	rec, err := a.recoverySnapshot(ctx)
	if err != nil {
		return nil, err
	}
	a.lifecycleMu.Lock()
	appState := &AppLifecycleSnapshot{
		Started: a.lifecycle >= lifecycleStarted, Ready: a.lifecycle == lifecycleReady,
		Closing: a.lifecycle == lifecycleClosing, Closed: a.lifecycle == lifecycleClosed,
	}
	a.lifecycleMu.Unlock()
	gatewayState := a.svc.Status()
	return &DesktopSnapshot{
		Gateway:         &SafeGatewaySnapshot{State: string(gatewayState.Status), Listening: gatewayState.Status == gateway.StatusRunning},
		TrafficAnalysis: a.trafficSnapshot(), Recovery: rec, App: appState,
	}, nil
}

func trafficBindingError(operation string, err error) DesktopCommandResult {
	var te *traffictransaction.Error
	if !errors.As(err, &te) {
		return errDesktop(operation, "traffic", "traffic_transaction_failed", "traffic operation failed", true)
	}
	log.Printf("traffic binding error: operation=%q kind=%q retryable=%t stage=%q", operation, te.Kind, te.Retryable, te.Stage)
	code := string(te.Kind)
	message := "traffic operation was rejected"
	retryable := te.Retryable
	confirmation := te.ConfirmationRequired
	recoveryRequired := false
	if strings.Contains(code, "recovery") || strings.Contains(code, "backout") || strings.Contains(code, "final") {
		recoveryRequired = true
	}
	switch te.Kind {
	case traffictransaction.KindGatewayNotRunning:
		message = "gateway is not running"
	case traffictransaction.KindTransactionInProgress:
		message = "another traffic operation is in progress"
	case traffictransaction.KindConfigConflict, traffictransaction.KindRestoreConflict:
		message = "codex configuration changed; confirmation is required"
		confirmation = true
	case traffictransaction.KindFinishConfirmation:
		message = "explicit confirmation is required for unsaved observations"
		confirmation = true
	case traffictransaction.KindRecoveryRequired:
		message = "recovery confirmation is required"
		recoveryRequired = true
	case traffictransaction.KindFinishCloseFailed, traffictransaction.KindFinishFinalValidation:
		message = "traffic relay finish requires recovery"
		recoveryRequired = true
	}
	result := errDesktop(operation, "traffic", code, message, retryable)
	result.Error.ConfirmationRequired = confirmation
	result.Error.RecoveryRequired = recoveryRequired
	return result
}

func routeMutationResult(snap traffictransaction.Snapshot) string {
	if snap.CleanupPending != nil && snap.CleanupPending.RouteMutationResult != "" {
		return snap.CleanupPending.RouteMutationResult
	}
	if snap.IntegrationActive || snap.Phase == traffictransaction.PhaseCompleted {
		return "applied"
	}
	return "unchanged"
}

func (a *App) executeTrafficCommand(operation string, fn func(context.Context, *traffictransaction.Service) (traffictransaction.Snapshot, error)) (result DesktopCommandResult) {
	if !a.trafficMutationAllowed(operation) {
		return errDesktop(operation, "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	token, err := a.operationGate().Begin(routingswitch.OperationTraffic)
	if err != nil {
		return errDesktop(operation, "coordination", "route_operation_busy", "another route operation is in progress", true)
	}
	defer func() { _ = token.Release() }()
	defer func() {
		if recover() != nil {
			result = errDesktop(operation, "binding", "desktop_command_failed", "desktop command failed", true)
		}
	}()
	ctx, cancel := context.WithTimeout(a.appCtx, trafficCommandTimeout)
	defer cancel()
	a.trafficMu.Lock()
	defer a.trafficMu.Unlock()
	tx, err := a.ensureTrafficTransaction()
	if err != nil {
		return errDesktop(operation, "initialization", "traffic_service_unavailable", "traffic service is unavailable", true)
	}
	snap, err := fn(ctx, tx)
	if err != nil {
		if snap.IntegrationActive || snap.Phase == traffictransaction.PhaseCompleted || snap.CleanupPending != nil {
			result = trafficBindingError(operation, err)
			value, snapshotErr := a.desktopSnapshot(ctx)
			if snapshotErr != nil {
				return errDesktop(operation, "snapshot", "desktop_snapshot_unavailable", "desktop snapshot is unavailable", true)
			}
			value.RouteMutationResult = routeMutationResult(snap)
			value.CleanupPending = snap.CleanupPending != nil
			if snap.CleanupPending != nil {
				value.CleanupStatus = snap.CleanupPending.Status
			}
			result.Value = value
			if result.Error != nil {
				result.Error.Details = map[string]any{"routeMutationResult": value.RouteMutationResult, "cleanupStatus": value.CleanupStatus, "cleanupPending": value.CleanupPending}
			}
			return result
		}
		return trafficBindingError(operation, err)
	}
	value, err := a.desktopSnapshot(ctx)
	if err != nil {
		return errDesktop(operation, "snapshot", "desktop_snapshot_unavailable", "desktop snapshot is unavailable", true)
	}
	value.TrafficAnalysis = a.trafficSnapshot()
	_ = snap // Snapshot fields are represented by the live safe service state.
	return okDesktop(value)
}

func (a *App) trafficMutationAllowed(operation string) bool {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.lifecycle == lifecycleNew {
		// Unit tests and programmatic callers may use the existing App before
		// Wails OnStartup. The Wails path sets lifecycle before exposing calls.
		return a.ctx == nil
	}
	return a.lifecycle == lifecycleReady || a.lifecycle == lifecycleStarted
}

func (a *App) trafficReadAllowed() bool {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.lifecycle == lifecycleNew {
		return true
	}
	return a.lifecycle != lifecycleStarting
}

func (a *App) StartTrafficAnalysis() DesktopCommandResult {
	return a.executeTrafficCommand("StartTrafficAnalysis", func(ctx context.Context, tx *traffictransaction.Service) (traffictransaction.Snapshot, error) {
		snap, err := tx.Enable(ctx)
		if err == nil {
			// Autosave starts as a side effect of a successful Enable. Writer
			// failures are soft (surfaced via AutoSaveStatus); the capture
			// itself already succeeded and must not be torn down.
			a.startTrafficAutosaveLocked()
		}
		return snap, err
	})
}

func (a *App) StopTrafficAnalysis() DesktopCommandResult {
	return a.executeTrafficCommand("StopTrafficAnalysis", func(ctx context.Context, tx *traffictransaction.Service) (traffictransaction.Snapshot, error) {
		snap, err := tx.Disable(ctx)
		if err == nil {
			a.closeTrafficAutosaveLocked(true)
		}
		return snap, err
	})
}

func (a *App) FinishTrafficRelay(req FinishTrafficRelayRequest) DesktopCommandResult {
	return a.executeTrafficCommand("FinishTrafficRelay", func(ctx context.Context, tx *traffictransaction.Service) (traffictransaction.Snapshot, error) {
		snap, err := tx.Finish(ctx, req.DiscardUnsaved)
		if err == nil {
			a.closeTrafficAutosaveLocked(true)
		}
		return snap, err
	})
}

func (a *App) TrafficAnalysisStatus() DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisStatus", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	ctx, cancel := context.WithTimeout(a.appCtx, 5*time.Second)
	defer cancel()
	value, err := a.desktopSnapshot(ctx)
	if err != nil {
		return errDesktop("TrafficAnalysisStatus", "snapshot", "desktop_snapshot_unavailable", "desktop snapshot is unavailable", true)
	}
	return okDesktop(value)
}

// TrafficAnalysisObservations returns the recorded observations as a
// secret-free Desktop summary DTO. Internal Observation fields that could
// carry prompts, bodies, responses, headers, URL paths/query, API keys, or
// model/provider names are dropped by desktopObservations at this boundary.
func (a *App) TrafficAnalysisObservations() DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisObservations", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	items, _ := a.traffic.Observations(0)
	return okDesktop(&DesktopSnapshot{TrafficObservations: desktopObservations(items)})
}

// SaveFileDialog opens the Wails native Save File Dialog. Cancellation is not
// an error: it returns OK=true with Canceled=true and an empty path, because
// the frontend command() envelope requires a defined value on success.
func (a *App) SaveFileDialog(options SaveFileDialogOptions) DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("SaveFileDialog", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	if a.ctx == nil {
		return errDesktop("SaveFileDialog", "platform", "desktop_context_unavailable", "desktop runtime is not ready", true)
	}
	dialog := runtime.SaveDialogOptions{
		Title:            options.Title,
		DefaultDirectory: options.DefaultDirectory,
		DefaultFilename:  options.DefaultFilename,
		Filters:          make([]runtime.FileFilter, 0, len(options.Filters)),
	}
	for _, f := range options.Filters {
		dialog.Filters = append(dialog.Filters, runtime.FileFilter{DisplayName: f.DisplayName, Pattern: f.Pattern})
	}
	path, err := a.saveDialogFunc(a.ctx, dialog)
	if err != nil {
		return errDesktop("SaveFileDialog", "platform", "save_dialog_failed", "save dialog failed", true)
	}
	return okDesktop(&DesktopSnapshot{SaveDialog: &SaveDialogSnapshot{Path: path, Canceled: path == ""}})
}

// TrafficAnalysisExport copies the current autosave log (preferred) or renders
// the in-memory observations to the destination chosen via the native Save
// Dialog. The copy is flush-consistent (temp+rename) so the destination never
// holds a partially written file.
func (a *App) TrafficAnalysisExport(req TrafficExportRequest) DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisExport", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	if err := validateExportDestination(req.Destination); err != nil {
		return errDesktop("TrafficAnalysisExport", "validation", "invalid_export_destination", err.Error(), false)
	}
	canonical, err := filepath.Abs(req.Destination)
	if err != nil {
		return errDesktop("TrafficAnalysisExport", "validation", "invalid_export_destination", "unable to resolve the destination", false)
	}
	if w := a.trafficLog.Load(); w != nil {
		if err := w.copyTo(canonical); err != nil {
			return errDesktop("TrafficAnalysisExport", "export", "export_failed", "unable to write the export log", true)
		}
		a.recordExport(canonical)
		return okDesktop(&DesktopSnapshot{Export: &TrafficExportSnapshot{
			Destination:      canonical,
			ObservationCount: w.observationCount(),
		}})
	}
	rendered, count := a.renderObservationsLog()
	if count == 0 {
		return errDesktop("TrafficAnalysisExport", "export", "no_autosave_log", "no observations to export", false)
	}
	if err := writeFileAtomic(canonical, []byte(rendered)); err != nil {
		return errDesktop("TrafficAnalysisExport", "export", "export_failed", "unable to write the export log", true)
	}
	a.recordExport(canonical)
	return okDesktop(&DesktopSnapshot{Export: &TrafficExportSnapshot{
		Destination:      canonical,
		ObservationCount: count,
	}})
}

// TrafficAnalysisRevealExport reveals the last export in the file explorer.
// It only reveals destinations this session exported (ownership guard).
func (a *App) TrafficAnalysisRevealExport(req TrafficRevealRequest) DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisRevealExport", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	if req.Destination == "" {
		return errDesktop("TrafficAnalysisRevealExport", "validation", "invalid_export_destination", "destination is required", false)
	}
	canonical, err := filepath.Abs(req.Destination)
	if err != nil {
		return errDesktop("TrafficAnalysisRevealExport", "validation", "invalid_export_destination", "unable to resolve the destination", false)
	}
	a.exportMu.Lock()
	owned := strings.EqualFold(canonical, a.lastTrafficExport)
	a.exportMu.Unlock()
	if !owned {
		return errDesktop("TrafficAnalysisRevealExport", "export", "reveal_ownership_mismatch", "export destination is not owned by this session", false)
	}
	if err := a.explorerFunc("/select," + canonical); err != nil {
		return errDesktop("TrafficAnalysisRevealExport", "export", "reveal_unsupported", "unable to open the export folder", false)
	}
	return okDesktop(&DesktopSnapshot{RevealExport: &TrafficRevealSnapshot{Destination: canonical}})
}

// TrafficAnalysisOpenLogFolder opens the traffic-analysis log folder in the
// file explorer, creating it if needed.
func (a *App) TrafficAnalysisOpenLogFolder() DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisOpenLogFolder", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	dir := a.trafficLogDirPath()
	if dir == "" {
		return errDesktop("TrafficAnalysisOpenLogFolder", "log_folder", "log_folder_unavailable", "traffic log folder is unavailable", false)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errDesktop("TrafficAnalysisOpenLogFolder", "log_folder", "log_folder_unavailable", "unable to ensure the traffic log folder", false)
	}
	if err := a.explorerFunc(dir); err != nil {
		return errDesktop("TrafficAnalysisOpenLogFolder", "log_folder", "open_folder_unsupported", "unable to open the traffic log folder", false)
	}
	return okDesktop(&DesktopSnapshot{})
}

// TrafficAnalysisOpenLogFile opens the current traffic-analysis log file with
// the default application. It prefers the active autosave writer and falls back
// to the most recent retained log so the button stays useful after capture.
func (a *App) TrafficAnalysisOpenLogFile() DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("TrafficAnalysisOpenLogFile", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	path := ""
	if w := a.trafficLog.Load(); w != nil && w.path != "" {
		path = w.path
	} else {
		path = a.latestTrafficLogPath()
	}
	if path == "" {
		return errDesktop("TrafficAnalysisOpenLogFile", "log_file", "log_file_unavailable", "no traffic log file is available", false)
	}
	if err := a.explorerFunc(path); err != nil {
		return errDesktop("TrafficAnalysisOpenLogFile", "log_file", "open_log_unsupported", "unable to open the traffic log file", false)
	}
	return okDesktop(&DesktopSnapshot{})
}

func validateExportDestination(dst string) error {
	if dst == "" {
		return errors.New("destination is empty")
	}
	if !filepath.IsAbs(dst) {
		return errors.New("destination must be an absolute path")
	}
	base := filepath.Base(dst)
	if base == "." || base == string(filepath.Separator) {
		return errors.New("destination must be a file path")
	}
	if strings.ToLower(filepath.Ext(base)) != ".log" {
		return errors.New("destination must end with .log")
	}
	return nil
}

// recordExport stores the canonical destination of the latest export, which
// gates the reveal ownership guard.
func (a *App) recordExport(canonical string) {
	a.exportMu.Lock()
	a.lastTrafficExport = canonical
	a.exportMu.Unlock()
}

func (a *App) RecoveryStatus() DesktopCommandResult {
	return a.TrafficAnalysisStatus()
}

func (a *App) RefreshRecoveryStatus() DesktopCommandResult {
	if !a.trafficReadAllowed() {
		return errDesktop("RefreshRecoveryStatus", "lifecycle", "desktop_app_not_ready", "desktop app is not ready", true)
	}
	if a.recovery == nil {
		return errDesktop("RefreshRecoveryStatus", "recovery", "recovery_state_unavailable", "recovery state is unavailable", true)
	}
	ctx, cancel := context.WithTimeout(a.appCtx, 10*time.Second)
	defer cancel()
	_, err := a.recovery.ReconcileStartup(ctx, func(path string) ([]byte, error) { return os.ReadFile(path) })
	if err != nil {
		return errDesktop("RefreshRecoveryStatus", "recovery", "recovery_reconcile_failed", "recovery status refresh failed", true)
	}
	return a.TrafficAnalysisStatus()
}

// RestoreRecovery and DiscardRecovery are implemented in
// recovery_transaction_bindings.go. They use explicit confirmation inputs,
// the shared App operation locks, and atomic Recovery Store operations.
