package traffictransaction

import (
	"context"
	"errors"
	"io/fs"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
	"moonbridge/internal/service/trafficanalysis"
)

type defaultIDGenerator struct{}

func (defaultIDGenerator) New() string { return strings.ReplaceAll(uuid.NewString(), "-", "") }

type activeTransaction struct {
	id        string
	operation Operation
}

type Service struct {
	mu      sync.Mutex
	active  *activeTransaction
	ownerID string
	// Recovery v2 intentionally does not persist the in-process Gateway
	// identity or Capture generation. Keep those as private same-process
	// evidence so Disable/Finish can validate an Enable completed in this
	// Service; after a process restart the evidence is absent and operations
	// fail closed instead of guessing ownership.
	lastGateway    GatewaySnapshot
	lastGeneration uint64
	deps           Dependencies
	ids            IDGenerator
}

func New(deps Dependencies) *Service {
	ids := deps.IDs
	if ids == nil {
		ids = defaultIDGenerator{}
	}
	return &Service{deps: deps, ids: ids}
}

// NewService is the conventional constructor name used by the other Go
// services; New remains as a concise alias for tests and adapters.
func NewService(deps Dependencies) *Service { return New(deps) }

// RetryBackupCleanup removes only the pending backup artifact. It never reruns
// a route mutation, and stale transaction IDs are safe no-ops.
func validateBackupID(id string) error {
	if id == "" || id == "." || id == ".." || filepath.IsAbs(id) ||
		strings.ContainsAny(id, `/\\`) || id != filepath.Base(id) {
		return errors.New("invalid backup id")
	}
	for _, r := range id {
		if r < 0x20 || r == 0x7f {
			return errors.New("invalid backup id")
		}
	}
	return nil
}

func (s *Service) RetryBackupCleanup(ctx context.Context, transactionID string) (bool, error) {
	s.mu.Lock()
	if s.active != nil {
		s.mu.Unlock()
		return false, safeError(KindTransactionInProgress, "another traffic operation is in progress", true)
	}
	s.active = &activeTransaction{id: transactionID, operation: OperationCleanup}
	s.mu.Unlock()
	defer s.releaseOperation(transactionID)

	if transactionID == "" {
		return false, nil
	}
	writer := s.deps.Recovery
	pending, err := writer.GetCleanupPending(ctx)
	if err != nil || pending == nil || pending.TransactionID != transactionID || pending.BackupID == "" {
		return false, err
	}
	if err := validateBackupID(pending.BackupID); err != nil {
		return false, err
	}
	if err := s.deps.Backup.Remove(ctx, BackupRef{ID: pending.BackupID}); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.emitEvent(EventCleanupPending, EventSeverityWarning)
		return false, err
	}
	if err := writer.ClearCleanupPending(ctx, transactionID, pending.BackupID); err != nil {
		s.emitEvent(EventCleanupPending, EventSeverityWarning)
		return false, err
	}
	return true, nil
}

// OwnerID returns the transaction identity that currently claims Desktop
// capture ownership in this process, or "" when none is held. Enable stores the
// same identity in s.ownerID and the capture's desktopOwnerID, and a failed
// Disable keeps both, so a live restore can prove same-process ownership. A
// restarted process has a fresh Service with no owner and fails closed.
func (s *Service) OwnerID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerID
}

// Enable executes only the Desktop enable transaction. In the front-door model
// it starts the capture relay on :38441 (upstream = the running gateway backend
// :38442), claims desktop ownership, and switches the stable front door to
// :38441. Codex config is never touched here: it stays at the front door
// :38440 for the whole time Moon Bridge runs.
func (s *Service) Enable(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	txID := s.ids.New()
	if txID == "" {
		return Snapshot{}, safeError(KindTransactionFailed, "transaction identity generation failed", false)
	}
	if err := s.reserveOperation(txID, OperationEnable); err != nil {
		return Snapshot{}, err
	}
	defer s.releaseOperation(txID)

	gw, err := s.deps.Gateway.Snapshot(ctx)
	if err != nil {
		return Snapshot{}, safeError(KindGatewayNotRunning, "gateway status is unavailable", true)
	}
	if !gw.Running {
		return Snapshot{}, safeError(KindGatewayNotRunning, "gateway is not running", true)
	}
	if gw.InstanceID == "" || gw.Address == "" {
		return Snapshot{}, safeError(KindGatewayMismatch, "gateway identity is incomplete", false)
	}

	unresolved, err := s.deps.Recovery.HasUnresolved(ctx)
	if err != nil {
		return Snapshot{}, safeError(KindCheckpointFailed, "recovery status is unavailable", true)
	}
	if unresolved {
		return Snapshot{}, safeError(KindRecoveryRequired, "recovery confirmation is required", false)
	}
	// An integration journal from a previous process is not an enableable
	// starting point: its private owner/generation evidence is unavailable.
	// The only active journal that can coexist with Enable is one owned by this
	// in-process Service, and the traffic-mode guard below will reject it as
	// already desktop-managed.
	if journal, journalErr := s.deps.Recovery.Current(ctx); journalErr == nil && journal.IntegrationActive {
		s.mu.Lock()
		ownedHere := s.ownerID != ""
		s.mu.Unlock()
		if !ownedHere {
			return Snapshot{}, safeError(KindRecoveryRequired, "recovery confirmation is required", false)
		}
	}

	traffic := s.deps.Traffic.Status()
	started := false
	adopted := false
	switch {
	case traffic.Mode == trafficanalysis.ModeIdle && traffic.CaptureState == "stopped" && traffic.ListeningAddress == "":
		started = true
	case traffic.Mode == trafficanalysis.ModeCaptureOnly && traffic.CaptureState == "capturing" && traffic.Operation == trafficanalysis.OperationNone &&
		traffic.GatewayInstanceID == gw.InstanceID && traffic.GatewayAddress == gw.Address &&
		traffic.ListeningAddress == CaptureListenAddress:
		adopted = true
	case traffic.Mode == trafficanalysis.ModeRecovery:
		return Snapshot{}, safeError(KindRecoveryRequired, "capture requires recovery confirmation", false)
	case traffic.Mode == trafficanalysis.ModeDesktop:
		return Snapshot{}, safeError(KindTransactionInProgress, "traffic integration is already desktop-managed", false)
	default:
		return Snapshot{}, safeError(KindCaptureNotActive, "capture is not in an enableable state", false)
	}

	state := FailureState{Phase: PhasePrepared, AdoptedCapture: adopted}
	if err := s.deps.Recovery.Checkpoint(ctx, checkpointFor(txID, PhasePrepared, gw, traffic.Generation, false)); err != nil {
		return Snapshot{}, safeError(KindCheckpointFailed, "prepared recovery checkpoint failed", true)
	}
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, traffic, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed after preparation", true))
	}

	if started {
		upstream := gw.Address
		if !strings.HasPrefix(upstream, "http://") && !strings.HasPrefix(upstream, "https://") {
			upstream = "http://" + upstream
		}
		if _, err := s.deps.Traffic.BindGatewayRun(gw.InstanceID, gw.Address); err != nil {
			primary := safeError(KindCaptureStartFailed, "capture start failed", true)
			primary.Stage = captureStartFailureStage(err)
			return s.backout(ctx, txID, gw, traffic, state, CauseCaptureStart, primary)
		}
		traffic, err = s.deps.Traffic.StartCapture(trafficanalysis.StartOptions{
			UpstreamBase: upstream,
			ListenAddr:   CaptureListenAddress,
		})
		if err != nil {
			primary := safeError(KindCaptureStartFailed, "capture start failed", true)
			primary.Stage = captureStartFailureStage(err)
			return s.backout(ctx, txID, gw, traffic, state, CauseCaptureStart, primary)
		}
		state.StartedCapture = true
		state.Phase = PhaseCaptureStarted
		if !captureMatches(traffic, gw) {
			return s.backout(ctx, txID, gw, traffic, state, CauseValidation, safeError(KindGatewayMismatch, "capture identity or listener changed during start", false))
		}
	} else {
		validated, validationErr := s.deps.Traffic.ValidateCaptureExpected(traffic.Generation, gw.InstanceID, gw.Address)
		if validationErr != nil {
			return s.backout(ctx, txID, gw, traffic, state, CauseAdoption, safeError(KindCaptureNotActive, "existing capture is not adoptable", false))
		}
		traffic = validated
		if !captureMatches(traffic, gw) {
			return s.backout(ctx, txID, gw, traffic, state, CauseValidation, safeError(KindGatewayMismatch, "adopted capture identity or listener changed", false))
		}
		state.Phase = PhaseCaptureAdopted
	}
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, traffic, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed after capture start", true))
	}

	if traffic.Generation == 0 {
		return s.backout(ctx, txID, gw, traffic, state, CauseValidation, safeError(KindCaptureNotActive, "capture generation is invalid", false))
	}
	claimed, err := s.deps.Traffic.ClaimDesktopExpected(traffic.Generation, gw.InstanceID, gw.Address, txID)
	if err != nil {
		return s.backout(ctx, txID, gw, traffic, state, CauseOwnershipClaim, safeError(KindOwnershipClaimFailed, "desktop capture ownership claim failed", true))
	}
	state.OwnershipClaimed = true
	state.Phase = PhaseOwnershipClaimed
	if claimed.Mode != trafficanalysis.ModeDesktop || claimed.Generation != traffic.Generation || !captureIdentityMatches(claimed, gw) ||
		claimed.ObservationCount != traffic.ObservationCount {
		return s.backout(ctx, txID, gw, claimed, state, CauseOwnershipClaim, safeError(KindGatewayMismatch, "capture changed during ownership claim", false))
	}
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed after ownership claim", true))
	}

	state.Phase = PhaseCaptureStarted
	if err := s.deps.Recovery.Checkpoint(ctx, checkpointFor(txID, PhaseCaptureStarted, gw, claimed.Generation, false)); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseCheckpoint, safeError(KindCheckpointFailed, "capture recovery checkpoint failed", true))
	}
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed before the front-door switch", true))
	}
	if gw.RoutingAvailable {
		if gw.DefaultModelAlias == "" {
			return s.backout(ctx, txID, gw, claimed, state, CauseValidation, safeError(KindConfigReadFailed, "gateway default route is unavailable", true))
		}
		if err := s.deps.Traffic.SetDesktopModelMappingExpected(claimed.Generation, gw.InstanceID, gw.Address, txID, "", gw.DefaultModelAlias); err != nil {
			return s.backout(ctx, txID, gw, claimed, state, CauseOwnershipClaim, safeError(KindOwnershipClaimFailed, "desktop model mapping registration failed", true))
		}
		state.ModelMappingClaimed = true
	}

	// The front-door switch is the transaction boundary. Capture is started and
	// owned, so switching the stable endpoint from :38442 to :38441 is safe. On
	// failure the current upstream (:38442) is preserved and the just-claimed
	// capture is torn down by backout (fail-closed).
	if err := s.switchFrontDoor(ctx, captureURL); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseFrontDoorSwitch, safeError(KindFrontDoorSwitch, "front door switch failed", true))
	}
	state.ConfigCommitted = true
	state.Phase = PhaseConfigCommitted
	s.emitEvent(EventRouteApplied, EventSeverityInfo)
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed after the front-door switch", true))
	}

	if err := s.deps.Recovery.Checkpoint(ctx, checkpointFor(txID, PhaseConfigCommitted, gw, claimed.Generation, true)); err != nil {
		return s.backout(ctx, txID, gw, claimed, state, CauseCheckpoint, safeError(KindCheckpointFailed, "integration checkpoint failed", true))
	}

	final := s.deps.Traffic.Status()
	if err := s.verifyGateway(ctx, gw); err != nil {
		return s.backout(ctx, txID, gw, final, state, CauseGatewayLost, safeError(KindGatewayNotRunning, "gateway changed after integration checkpoint", true))
	}
	journal, journalErr := s.deps.Recovery.Current(ctx)
	if journalErr != nil || journal.Phase != PhaseConfigCommitted || !journal.IntegrationActive || journal.OperationID != txID {
		return s.backout(ctx, txID, gw, final, state, CauseFinalValidation, safeError(KindRecoveryRequired, "durable integration state could not be verified", true))
	}
	validatedFinal, trafficValidationErr := s.deps.Traffic.ValidateDesktopIntegrationExpected(claimed.Generation, gw.InstanceID, gw.Address, txID, CaptureListenAddress)
	if trafficValidationErr != nil {
		return s.backout(ctx, txID, gw, final, state, CauseFinalValidation, safeError(KindRecoveryRequired, "final traffic ownership could not be verified", true))
	}
	s.mu.Lock()
	s.ownerID = txID
	s.lastGateway = gw
	s.lastGeneration = validatedFinal.Generation
	s.mu.Unlock()
	s.emitEvent(EventAnalysisStarted, EventSeveritySuccess)
	return snapshotFrom(validatedFinal, gw, PhaseCompleted, true), nil
}

// Disable demotes Desktop ownership back to capture-only passthrough (S2 → S1).
// In the front-door model it switches the stable front door away from the
// capture relay (:38441) back to the gateway backend (:38442) before pausing
// capture, so the front door never points at a paused capture. Codex config is
// never touched here. It deliberately leaves the Capture relay alive; Finish
// belongs to a later boundary.
func (s *Service) Disable(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	txID := s.ids.New()
	if txID == "" {
		return Snapshot{}, safeError(KindTransactionFailed, "transaction identity generation failed", false)
	}
	if err := s.reserveOperation(txID, OperationDisable); err != nil {
		return Snapshot{}, err
	}
	defer s.releaseOperation(txID)

	unresolved, err := s.deps.Recovery.HasUnresolved(ctx)
	if err != nil || unresolved {
		return Snapshot{}, safeError(KindRecoveryRequired, "recovery confirmation is required", true)
	}
	journal, err := s.deps.Recovery.Current(ctx)
	if err != nil || !journal.IntegrationActive || journal.DurablePhase != DurableIntegrationApplied || journal.OperationID == "" {
		return Snapshot{}, safeError(KindRecoveryRequired, "active integration evidence is unavailable", false)
	}

	s.mu.Lock()
	ownerID := s.ownerID
	lastGateway := s.lastGateway
	lastGeneration := s.lastGeneration
	s.mu.Unlock()
	if journal.GatewayInstance == "" {
		journal.GatewayInstance = lastGateway.InstanceID
	}
	if journal.GatewayAddress == "" {
		journal.GatewayAddress = lastGateway.Address
	}
	if journal.CaptureGeneration == 0 {
		journal.CaptureGeneration = lastGeneration
	}
	if ownerID == "" || (journal.OwnerID != "" && journal.OwnerID != ownerID) {
		return Snapshot{}, safeError(KindRecoveryRequired, "desktop ownership cannot be confirmed in this process", false)
	}

	traffic := s.deps.Traffic.Status()
	if traffic.Mode != trafficanalysis.ModeDesktop || traffic.Generation != journal.CaptureGeneration ||
		traffic.GatewayInstanceID != journal.GatewayInstance || traffic.GatewayAddress != journal.GatewayAddress {
		return Snapshot{}, safeError(KindGatewayMismatch, "desktop traffic ownership does not match recovery evidence", false)
	}
	if traffic.CaptureState == "capturing" {
		if _, err := s.deps.Traffic.ValidateDesktopIntegrationExpected(traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, ownerID, CaptureListenAddress); err != nil {
			return Snapshot{}, safeError(KindCaptureNotActive, "desktop traffic is not ready to disable", true)
		}
	} else if traffic.CaptureState == "passthrough" {
		if _, err := s.deps.Traffic.ValidateDesktopPassthroughExpected(traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, ownerID, CaptureListenAddress); err != nil {
			return Snapshot{}, safeError(KindCaptureNotActive, "desktop traffic is not ready to disable", true)
		}
	} else {
		return Snapshot{}, safeError(KindCaptureNotActive, "desktop traffic is not ready to disable", true)
	}

	if err := s.deps.Recovery.Checkpoint(ctx, checkpointForDisable(txID, ownerID, PhaseDisableStarted, DurableIntegrationApplied, journal, traffic.Generation, true)); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, true)
	}

	// Switch the front door away from the capture relay (:38441) back to the
	// gateway backend (:38442) BEFORE pausing capture. On failure the front door
	// stays on :38441 and the current upstream is preserved (fail-closed).
	if err := s.switchFrontDoor(ctx, gatewayBackendURL); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, true)
	}

	paused, err := s.deps.Traffic.PauseDesktopExpected(ctx, traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, ownerID)
	if err != nil || paused.Mode != trafficanalysis.ModeDesktop || paused.CaptureState != "passthrough" || paused.Generation != traffic.Generation ||
		paused.ListeningAddress != CaptureListenAddress || paused.ObservationCount != traffic.ObservationCount {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, true)
	}
	if _, err := s.deps.Traffic.ValidateDesktopPassthroughExpected(traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, ownerID, CaptureListenAddress); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, true)
	}

	// This checkpoint previously recorded the Codex config restore; that restore
	// is gone in the front-door model, so it now records the completed
	// front-door switch + capture pause before ownership release.
	if err := s.deps.Recovery.Checkpoint(ctx, checkpointForDisable(txID, ownerID, PhaseConfigRestored, DurableRecovered, journal, traffic.Generation, false)); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}
	if _, err := s.deps.Traffic.ValidateDesktopPassthroughExpected(traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, ownerID, CaptureListenAddress); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}

	released, err := s.deps.Traffic.ReleaseDesktopExpected(traffic.Generation, ownerID)
	if err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}
	s.mu.Lock()
	if s.ownerID == ownerID {
		s.ownerID = ""
	}
	s.mu.Unlock()

	final, err := s.deps.Traffic.ValidateCaptureOnlyExpected(released.Generation, journal.GatewayInstance, journal.GatewayAddress, CaptureListenAddress)
	if err != nil || final.Mode != trafficanalysis.ModeCaptureOnly || final.CaptureState != "passthrough" || final.Generation != traffic.Generation {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}

	if err := s.deps.Recovery.Checkpoint(ctx, checkpointForDisableDemote(txID, journal, final.Generation)); err != nil {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}
	finalJournal, err := s.deps.Recovery.Current(ctx)
	if err != nil || finalJournal.DurablePhase != DurableInactive || finalJournal.IntegrationActive || finalJournal.OperationID != txID {
		return s.disableRecovery(ctx, txID, ownerID, journal, traffic.Generation, false)
	}
	s.emitEvent(EventAnalysisStopped, EventSeveritySuccess)
	return Snapshot{Operation: OperationDisable, Phase: PhaseDisableCompleted, CaptureState: final.CaptureState, TrafficMode: final.Mode, CaptureGeneration: final.Generation, GatewayMatches: true, IntegrationActive: false}, nil
}

// Finish closes an inactive passthrough relay. It never changes Codex config
// and never reclaims Desktop ownership. Auto-log finalization is not part of
// this boundary; an unsaved marker therefore requires explicit discard.
func (s *Service) Finish(ctx context.Context, discardUnsaved bool) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	txID := s.ids.New()
	if txID == "" {
		return Snapshot{}, safeError(KindTransactionFailed, "transaction identity generation failed", false)
	}
	if err := s.reserveOperation(txID, OperationFinish); err != nil {
		return Snapshot{}, err
	}
	defer s.releaseOperation(txID)

	unresolved, err := s.deps.Recovery.HasUnresolved(ctx)
	if err != nil || unresolved {
		return Snapshot{}, safeError(KindRecoveryRequired, "recovery confirmation is required", true)
	}
	journal, err := s.deps.Recovery.Current(ctx)
	if err != nil || journal.IntegrationActive || journal.DurablePhase != DurableInactive {
		return Snapshot{}, safeError(KindFinishPrecondition, "traffic integration is not inactive", false)
	}
	s.mu.Lock()
	lastGateway := s.lastGateway
	lastGeneration := s.lastGeneration
	s.mu.Unlock()
	if journal.GatewayInstance == "" {
		journal.GatewayInstance = lastGateway.InstanceID
	}
	if journal.GatewayAddress == "" {
		journal.GatewayAddress = lastGateway.Address
	}
	if journal.CaptureGeneration == 0 {
		journal.CaptureGeneration = lastGeneration
	}
	traffic := s.deps.Traffic.Status()
	if traffic.Mode != trafficanalysis.ModeCaptureOnly || traffic.CaptureState != "passthrough" || traffic.Generation == 0 ||
		traffic.GatewayInstanceID != journal.GatewayInstance || traffic.GatewayAddress != journal.GatewayAddress {
		return Snapshot{}, safeError(KindFinishPrecondition, "capture relay is not finishable", false)
	}
	if _, err := s.deps.Traffic.ValidateCaptureOnlyExpected(traffic.Generation, journal.GatewayInstance, journal.GatewayAddress, CaptureListenAddress); err != nil {
		return Snapshot{}, safeError(KindFinishPrecondition, "capture relay is not finishable", true)
	}
	if journal.UnsavedObservationsMayRemain && !discardUnsaved {
		return Snapshot{}, finishConfirmationError()
	}

	started := finishCheckpoint(txID, PhaseInactive, journal, traffic.Generation, discardUnsaved)
	if err := s.deps.Recovery.Checkpoint(ctx, started); err != nil {
		return s.finishRecovery(ctx, txID, journal, traffic, FinishFailureFinalize)
	}
	// The pre-close checkpoint is now the durable source of truth for Finish.
	// In particular, an explicit unsaved-discard decision must survive any
	// later close or final-checkpoint failure; do not rebuild recovery evidence
	// from the pre-checkpoint journal and accidentally erase it.
	finishJournal := started

	closed, err := s.deps.Traffic.CloseCapture(ctx)
	if err != nil {
		return s.finishRecovery(ctx, txID, finishJournal, closed, FinishFailureClose)
	}
	if _, err := s.deps.Traffic.ValidateIdleExpected(traffic.Generation); err != nil {
		return s.finishRecovery(ctx, txID, finishJournal, closed, FinishFailureFinalValidation)
	}

	finished := finishCheckpoint(txID, PhaseInactive, finishJournal, traffic.Generation, discardUnsaved)
	finished.RelayActive = false
	finished.CaptureState = "stopped"
	if err := s.deps.Recovery.Checkpoint(ctx, finished); err != nil {
		return s.finishRecovery(ctx, txID, finishJournal, closed, FinishFailureFinalCheckpoint)
	}
	finalJournal, err := s.deps.Recovery.Current(ctx)
	if err != nil || finalJournal.OperationID != txID || finalJournal.Phase != PhaseInactive || finalJournal.DurablePhase != DurableInactive ||
		finalJournal.IntegrationActive || finalJournal.RelayActive || finalJournal.CaptureState != "stopped" {
		return s.finishRecovery(ctx, txID, finishJournal, closed, FinishFailureFinalValidation)
	}
	final, err := s.deps.Traffic.ValidateIdleExpected(traffic.Generation)
	if err != nil {
		return s.finishRecovery(ctx, txID, finishJournal, final, FinishFailureFinalValidation)
	}
	s.mu.Lock()
	s.lastGateway = GatewaySnapshot{}
	s.lastGeneration = 0
	s.mu.Unlock()
	return Snapshot{Operation: OperationFinish, Phase: PhaseInactive, CaptureState: final.CaptureState, TrafficMode: final.Mode, CaptureGeneration: final.Generation, GatewayMatches: false, IntegrationActive: false}, nil
}

func (s *Service) finishRecovery(ctx context.Context, txID string, journal Checkpoint, traffic trafficanalysis.State, failure FinishFailure) (Snapshot, error) {
	cp := finishCheckpoint(txID, PhaseRecoveryRequired, journal, traffic.Generation, journal.UnsavedDiscardConfirmed)
	cp.DurablePhase = DurableReconciliationRequired
	cp.IntegrationActive = false
	cp.RelayActive = traffic.Mode != trafficanalysis.ModeIdle || traffic.CaptureState != "stopped"
	cp.CaptureState = traffic.CaptureState
	_ = s.deps.Recovery.Checkpoint(ctx, cp)
	planned := ClassifyFinishFailure(failure)
	return Snapshot{}, safeError(planned.ErrorKind, "traffic relay finish requires recovery", planned.Retryable)
}

func finishCheckpoint(id string, phase Phase, source Checkpoint, generation uint64, discardUnsaved bool) Checkpoint {
	return Checkpoint{
		OperationID: id, DurablePhase: DurableInactive, Phase: phase,
		BeforeHash: source.BeforeHash, AfterHash: source.AfterHash,
		PreviousPresent: source.PreviousPresent, PreviousValue: source.PreviousValue,
		AppliedValue: source.AppliedValue, BackupID: source.BackupID,
		GatewayInstance: source.GatewayInstance, GatewayAddress: source.GatewayAddress,
		CaptureGeneration: generation, IntegrationActive: false,
		IntegrationTarget: source.IntegrationTarget, OriginalPresent: source.OriginalPresent,
		RelayActive: true, CaptureState: "passthrough",
		UnsavedObservationsMayRemain: source.UnsavedObservationsMayRemain,
		UnsavedDiscardConfirmed:      discardUnsaved && source.UnsavedObservationsMayRemain,
		AutoLogFinalized:             !source.UnsavedObservationsMayRemain || discardUnsaved,
	}
}

func (s *Service) disableRecovery(ctx context.Context, txID, ownerID string, journal Checkpoint, generation uint64, integrationActive bool) (Snapshot, error) {
	_ = s.deps.Recovery.Checkpoint(ctx, checkpointForDisable(txID, ownerID, PhaseRecoveryRequired, DurableReconciliationRequired, journal, generation, integrationActive))
	s.emitEvent(EventRecoveryRequired, EventSeverityError)
	return Snapshot{}, safeError(KindRecoveryRequired, "traffic disable requires recovery", true)
}

func (s *Service) backout(ctx context.Context, txID string, gw GatewaySnapshot, traffic trafficanalysis.State, state FailureState, cause FailureCause, primary *Error) (Snapshot, error) {
	plan := ClassifyFailure(state, cause)
	if plan.RecoveryRequired {
		return s.requireRecovery(ctx, txID, gw, traffic.Generation)
	}
	if plan.RestoreConfig {
		// The front door was switched to the capture relay (:38441) before the
		// failure; switch it back to the gateway backend (:38442) so Codex never
		// keeps pointing at a capture that is about to be torn down.
		if err := s.switchFrontDoor(ctx, gatewayBackendURL); err != nil {
			return s.requireRecovery(ctx, txID, gw, traffic.Generation)
		}
	}
	if state.ModelMappingClaimed {
		if err := s.deps.Traffic.ClearDesktopModelMappingExpected(traffic.Generation, gw.InstanceID, gw.Address, txID); err != nil {
			return s.requireRecovery(ctx, txID, gw, traffic.Generation)
		}
	}
	if plan.ReleaseOwnership {
		if _, err := s.deps.Traffic.ReleaseDesktopExpected(traffic.Generation, txID); err != nil {
			return s.requireRecovery(ctx, txID, gw, traffic.Generation)
		}
	}
	if plan.CloseNewCapture {
		if _, err := s.deps.Traffic.CloseCapture(ctx); err != nil {
			return s.requireRecovery(ctx, txID, gw, traffic.Generation)
		}
	}
	if err := s.deps.Recovery.Checkpoint(ctx, checkpointFor(txID, PhaseAborted, gw, traffic.Generation, false)); err != nil {
		return s.requireRecovery(ctx, txID, gw, traffic.Generation)
	}
	return Snapshot{}, primary
}

func (s *Service) requireRecovery(ctx context.Context, txID string, gw GatewaySnapshot, generation uint64) (Snapshot, error) {
	_ = s.deps.Recovery.Checkpoint(ctx, checkpointFor(txID, PhaseRecoveryRequired, gw, generation, true))
	s.emitEvent(EventRecoveryRequired, EventSeverityError)
	return Snapshot{}, safeError(KindRecoveryRequired, "transaction backout is uncertain; recovery is required", true)
}

func (s *Service) reserveOperation(id string, operation Operation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return safeError(KindTransactionInProgress, "traffic transaction is already in progress", true)
	}
	s.active = &activeTransaction{id: id, operation: operation}
	return nil
}

func (s *Service) releaseOperation(id string) {
	s.mu.Lock()
	if s.active != nil && s.active.id == id {
		s.active = nil
	}
	s.mu.Unlock()
}

func (s *Service) verifyGateway(ctx context.Context, expected GatewaySnapshot) error {
	current, err := s.deps.Gateway.Snapshot(ctx)
	if err != nil || !current.Running || current.InstanceID != expected.InstanceID || current.Address != expected.Address {
		return errors.New("gateway identity changed")
	}
	return nil
}

// switchFrontDoor atomically swaps the stable front-door relay's forwarding
// target. It is the transaction boundary for the S1↔S2 switch: SetUpstream
// validates the target and swaps only on success, preserving the current
// upstream on failure. A nil SetFrontDoorUpstream means the binding did not
// wire the front-door relay, which is a hard precondition failure.
func (s *Service) switchFrontDoor(_ context.Context, base string) error {
	if s.deps.SetFrontDoorUpstream == nil {
		return safeError(KindFrontDoorSwitch, "front door relay is unavailable", true)
	}
	return s.deps.SetFrontDoorUpstream(base)
}

// captureStartFailureStage returns a fixed, secret-free classification of a
// trafficanalysis.StartCapture failure. For a proxy.Start failure it returns the
// concrete stage (bind/analyzer/loopback/relay_active); for any other rejection
// it returns the lower-level ErrorKind. It never surfaces error text.
func captureStartFailureStage(err error) string {
	var te *trafficanalysis.Error
	if errors.As(err, &te) {
		if te.Stage != "" {
			return te.Stage
		}
		return string(te.Kind)
	}
	return ""
}

func snapshotFrom(st trafficanalysis.State, gw GatewaySnapshot, phase Phase, integration bool) Snapshot {
	return Snapshot{
		Operation:         OperationEnable,
		Phase:             phase,
		CaptureState:      st.CaptureState,
		TrafficMode:       st.Mode,
		CaptureGeneration: st.Generation,
		GatewayMatches:    st.GatewayInstanceID == gw.InstanceID && st.GatewayAddress == gw.Address,
		IntegrationActive: integration,
	}
}

// checkpointFor builds a traffic-layer checkpoint. In the front-door model the
// Codex config is always at the stable front door (:38440), so AppliedValue is
// the front-door URL and the config hash/previous fields are empty (they belong
// to the outer Gateway layer, which owns the config). IntegrationTarget stays
// TargetAnalysis as the marker that the traffic (analysis) layer owns the
// front-door relay (S2); the config value itself never becomes :38441.
func checkpointFor(id string, phase Phase, gw GatewaySnapshot, generation uint64, active bool) Checkpoint {
	return Checkpoint{
		OperationID:       id,
		OwnerID:           id,
		DurablePhase:      durablePhaseFor(phase),
		Phase:             phase,
		AppliedValue:      frontDoorURL,
		GatewayInstance:   gw.InstanceID,
		GatewayAddress:    gw.Address,
		CaptureGeneration: generation,
		IntegrationActive: active,
		IntegrationTarget: TargetAnalysis,
	}
}

func durablePhaseFor(phase Phase) DurablePhase {
	if phase == PhaseConfigCommitted || phase == PhaseCompleted {
		return DurableIntegrationApplied
	}
	return ""
}

func checkpointForDisable(id, owner string, phase Phase, durable DurablePhase, source Checkpoint, generation uint64, active bool) Checkpoint {
	return Checkpoint{
		OperationID:       id,
		OwnerID:           owner,
		DurablePhase:      durable,
		Phase:             phase,
		BeforeHash:        source.BeforeHash,
		AfterHash:         source.AfterHash,
		PreviousPresent:   source.PreviousPresent,
		PreviousValue:     source.PreviousValue,
		AppliedValue:      source.AppliedValue,
		BackupID:          source.BackupID,
		GatewayInstance:   source.GatewayInstance,
		GatewayAddress:    source.GatewayAddress,
		CaptureGeneration: generation,
		IntegrationActive: active,
		IntegrationTarget: source.IntegrationTarget,
		OriginalPresent:   source.OriginalPresent,
	}
}

// checkpointForDisableDemote builds the final Disable checkpoint. The traffic
// layer has switched the front door back to the gateway backend and paused the
// capture, so the recovery record demotes from analysis back to gateway while
// preserving the Gateway layer's original-upstream evidence. The applied value
// stays the front-door URL (the Codex config never moves), and the hash fields
// stay empty so the writer preserves the outer Gateway layer's hashes.
func checkpointForDisableDemote(id string, source Checkpoint, generation uint64) Checkpoint {
	return Checkpoint{
		OperationID:       id,
		OwnerID:           id,
		DurablePhase:      DurableInactive,
		Phase:             PhaseDisableCompleted,
		AppliedValue:      frontDoorURL,
		BackupID:          source.BackupID,
		GatewayInstance:   source.GatewayInstance,
		GatewayAddress:    source.GatewayAddress,
		CaptureGeneration: generation,
		IntegrationActive: false,
		IntegrationTarget: TargetGateway,
		OriginalPresent:   source.OriginalPresent,
	}
}

func captureMatches(st trafficanalysis.State, gw GatewaySnapshot) bool {
	return st.Mode == trafficanalysis.ModeCaptureOnly && st.CaptureState == "capturing" && captureIdentityMatches(st, gw)
}

func captureIdentityMatches(st trafficanalysis.State, gw GatewaySnapshot) bool {
	return st.CaptureState == "capturing" &&
		st.GatewayInstanceID == gw.InstanceID && st.GatewayAddress == gw.Address &&
		st.ListeningAddress == CaptureListenAddress
}

func validateCaptureURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "http" && u.Host == CaptureListenAddress && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func init() {
	if !validateCaptureURL(captureURL) {
		panic("invalid fixed capture URL")
	}
}
