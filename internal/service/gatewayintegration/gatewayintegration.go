// Package gatewayintegration owns the outer routing layer of the three-state
// model: the Gateway integration redirects Codex's openai_base_url to the
// gateway address (S0 → S1) and restores the true original upstream on teardown
// (S1 → S0). It is distinct from the inner Traffic Analysis layer (S1 ↔ S2),
// which lives in traffictransaction and reuses the same recovery.Store.
//
// The service is crash-safe via a single recovery record: Enable persists the
// original upstream and the gateway target before touching the Codex config,
// and a failed post-write step attempts a rollback to the original. If the
// rollback also fails, Enable returns ErrFailClosed so the caller keeps the
// gateway running (Codex must never point at a dead listener) and surfaces
// recovery-required.
package gatewayintegration

import (
	"context"
	"errors"
	"fmt"

	"moonbridge/internal/service/codexconfig"
)

// IntegrationTarget mirrors recovery.IntegrationTarget at this boundary. The
// service only ever writes TargetGateway or TargetOriginal; TargetAnalysis is
// read back from a shared record so Enable can reject an unresolved inner
// (Traffic Analysis) integration.
type IntegrationTarget string

const (
	TargetOriginal IntegrationTarget = "original"
	TargetGateway  IntegrationTarget = "gateway"
	TargetAnalysis IntegrationTarget = "analysis"
)

// Phase is the durable recovery phase this service writes.
type Phase string

const (
	PhaseIdle               Phase = "idle"
	PhaseIntegrationApplied Phase = "integration_applied"
	PhaseRecoveryRequired   Phase = "recovery_required"
)

// Checkpoint is the recovery evidence written/read by this service. Original
// fields are owned exclusively by the Gateway layer and are never written by
// the inner Traffic Analysis layer.
type Checkpoint struct {
	Target                IntegrationTarget
	OriginalPresent       bool
	OriginalValue         *string
	AppliedValue          string
	Active                bool
	Phase                 Phase
	ConfigHashBeforeApply string
	ConfigHashAfterApply  string
}

// ConfigEditor is the Codex openai_base_url compare-and-swap surface needed by
// the outer layer. It is satisfied by codexconfig.Service.
type ConfigEditor interface {
	ReadRootURL(context.Context) (codexconfig.RootURLSnapshot, error)
	PrepareRootURLChange(context.Context, *string, string) (*codexconfig.PreparedRootURLChange, error)
	CommitPreparedRootURLChange(context.Context, *codexconfig.PreparedRootURLChange) error
}

// RecoveryWriter bridges the checkpoint to the shared recovery.Store.
type RecoveryWriter interface {
	Current(context.Context) (*Checkpoint, error)
	Checkpoint(context.Context, Checkpoint) error
}

// Sentinel errors consumed by the caller (desktop-app) to decide the gateway
// process lifecycle.
var (
	// ErrAlreadyIntegrated reports that a prior integration (gateway or
	// analysis) is still recorded, or the config already points at the gateway
	// URL. Enable must not clobber an existing redirect, which would overwrite
	// the true original upstream with a managed URL.
	ErrAlreadyIntegrated = errors.New("gateway integration is already active")
	// ErrFailClosed reports that Enable failed after touching the Codex config
	// AND the rollback to the original upstream also failed. The caller must
	// keep the gateway running so Codex never points at a stopped listener, and
	// surface recovery-required.
	ErrFailClosed = errors.New("gateway integration failed closed")
	// ErrDisableConflict reports that the Codex config moved away from the
	// gateway URL during Disable; a blind restore would be unsafe.
	ErrDisableConflict = errors.New("codex configuration changed during gateway disable")
)

// CurrentTarget is a safe classification of the current Codex openai_base_url
// value, used only for diagnostic logging. It never carries the URL itself:
// "original" here means the key is absent, which is distinct from the persisted
// true original upstream that only the Gateway layer owns (OriginalOpenaiBaseURL).
// A user's own pre-existing openai_base_url classifies as "other".
type CurrentTarget string

const (
	CurrentTargetUnknown  CurrentTarget = "unknown"
	CurrentTargetOriginal CurrentTarget = "original"
	CurrentTargetGateway  CurrentTarget = "gateway"
	CurrentTargetAnalysis CurrentTarget = "analysis"
	CurrentTargetOther    CurrentTarget = "other"
)

// Fixed stage enum values for Enable/Disable diagnostic errors.
const (
	stageResetStaleIntegration = "reset_stale_integration"
	stageReadRecovery          = "read_recovery"
	stageReadCurrent           = "read_current"
	stageGuardExistingTarget   = "guard_existing_target"
	stageGuardRestoreTarget    = "guard_restore_target"
	stageSaveRecovery          = "save_recovery"
	stagePrepareChange         = "prepare_change"
	stageCommitChange          = "commit_change"
	stageRollback              = "rollback"
)

// Fixed rollback-outcome enum values for the Enable diagnostic error.
const (
	rollbackNone      = ""
	rollbackSucceeded = "succeeded"
	rollbackFailed    = "failed"
)

// Error is a diagnostic error returned by Enable/Disable. It carries only fixed
// enum strings (operation, stage, current-target classification, rollback
// outcome); URLs, config bodies, and secrets are never included. Error() returns
// only the stage; Unwrap preserves the underlying cause so errors.Is/As keep
// working across the diagnostic wrapper.
type Error struct {
	Operation     string
	Stage         string
	CurrentTarget CurrentTarget
	Rollback      string
	Err           error
}

func (e *Error) Error() string { return e.Stage }
func (e *Error) Unwrap() error { return e.Err }

// Service drives the outer gateway integration layer.
type Service struct {
	config     ConfigEditor
	recovery   RecoveryWriter
	captureURL string
	// gatewayURL is the last gateway target passed to Enable, retained so a
	// later Disable can classify the config even when the recovery record has
	// already been demoted to original (the orphaned-config case). It is
	// process-local and diagnostic-only; its value is never logged.
	gatewayURL string
}

// New returns a Service bound to a Codex config editor, the shared recovery
// writer, and the fixed capture listener URL (http://127.0.0.1:38441). The
// capture URL is used only to classify the current value for diagnostics; it is
// never logged.
func New(config ConfigEditor, recovery RecoveryWriter, captureURL string) *Service {
	return &Service{config: config, recovery: recovery, captureURL: captureURL}
}

func (s *Service) enableError(stage string, current CurrentTarget, rollback string, cause error) error {
	return &Error{Operation: "enable", Stage: stage, CurrentTarget: current, Rollback: rollback, Err: cause}
}

func (s *Service) disableError(stage string, current CurrentTarget, cause error) error {
	return &Error{Operation: "disable", Stage: stage, CurrentTarget: current, Err: cause}
}

// Enable redirects Codex to targetURL (the gateway address, e.g.
// http://127.0.0.1:38440) after recording the current value as the original
// upstream. The recovery record is written before the config commit so a crash
// mid-write still has the original on disk. Failures are classified so the
// caller can decide whether to stop the gateway.
func (s *Service) Enable(ctx context.Context, targetURL string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := codexconfig.ValidateTrafficURL(targetURL); err != nil {
		return err
	}
	s.gatewayURL = targetURL
	if cur, err := s.recovery.Current(ctx); err == nil && cur != nil && cur.Target != TargetOriginal {
		// A previous integration (gateway or analysis) is still recorded but the
		// listener it redirected to is no longer running (clean external stop or a
		// crash). Reset to S0 first so the recorded true original upstream is
		// preserved instead of being overwritten with a managed URL, then
		// integrate fresh.
		if err := s.Disable(ctx); err != nil {
			return s.enableError(stageResetStaleIntegration, CurrentTargetUnknown, rollbackNone, err)
		}
	}
	current, err := s.config.ReadRootURL(ctx)
	if err != nil {
		return s.enableError(stageReadCurrent, CurrentTargetUnknown, rollbackNone, err)
	}
	curTarget := classifyCurrentTarget(current, targetURL, s.captureURL)
	if current.Present && current.Value == targetURL {
		return s.enableError(stageGuardExistingTarget, curTarget, rollbackNone, ErrAlreadyIntegrated)
	}
	var originalValue *string
	if current.Present {
		v := current.Value
		originalValue = &v
	}
	prepared, err := s.config.PrepareRootURLChange(ctx, stringPtr(targetURL), current.ConfigHash)
	if err != nil {
		return s.enableError(stagePrepareChange, curTarget, rollbackNone, err)
	}
	cp := Checkpoint{
		Target:                TargetGateway,
		OriginalPresent:       current.Present,
		OriginalValue:         originalValue,
		AppliedValue:          targetURL,
		Active:                true,
		Phase:                 PhaseIntegrationApplied,
		ConfigHashBeforeApply: prepared.BeforeHash,
		ConfigHashAfterApply:  prepared.AfterHash,
	}
	if err := s.recovery.Checkpoint(ctx, cp); err != nil {
		return s.enableError(stageSaveRecovery, curTarget, rollbackNone, err)
	}
	if err := s.config.CommitPreparedRootURLChange(ctx, prepared); err != nil {
		if rbErr := s.restoreOriginal(ctx, originalValue); rbErr != nil {
			// Codex was (or may have been) redirected and could not be rolled
			// back. Keep the gateway alive and mark recovery-required.
			cp.Phase = PhaseRecoveryRequired
			_ = s.recovery.Checkpoint(ctx, cp)
			return s.enableError(stageRollback, curTarget, rollbackFailed, fmt.Errorf("%w: commit=%v rollback=%v", ErrFailClosed, err, rbErr))
		}
		// Rolled back cleanly: clear the record so the caller may stop the
		// gateway safely.
		_ = s.recovery.Checkpoint(ctx, Checkpoint{
			Target:                TargetOriginal,
			OriginalPresent:       false,
			OriginalValue:         nil,
			AppliedValue:          "",
			Active:                false,
			Phase:                 PhaseIdle,
			ConfigHashBeforeApply: "",
			ConfigHashAfterApply:  "",
		})
		return s.enableError(stageCommitChange, curTarget, rollbackSucceeded, err)
	}
	return nil
}

// DisableReport captures a safe, enum-only diagnostic summary of a Disable
// call. It never carries URLs, config bodies, or secrets: every field is a
// fixed enum or a boolean. The binding layer uses it to log why a no-op
// happened (e.g. an orphaned gateway config whose recovery is already original).
type DisableReport struct {
	RecoveryTarget  IntegrationTarget // recovery target read at Disable entry
	OriginalPresent bool              // recorded true original presence at entry
	Before          CurrentTarget     // config openai_base_url classification before restore
	Restored        bool              // true if a restore commit was actually performed
	After           CurrentTarget     // config classification after restore (== Before when !Restored)
}

// Disable restores the original upstream recorded by Enable and clears the
// recovery record back to S0. The caller must have already demoted the inner
// Traffic Analysis layer (S2 → S1) so the config currently points at the
// gateway URL. It returns only the error; DisableWithReport also exposes the
// safe diagnostic summary for the caller to log.
func (s *Service) Disable(ctx context.Context) error {
	_, err := s.disable(ctx)
	return err
}

// DisableWithReport is Disable plus a safe diagnostic report. It never returns
// URLs or secrets.
func (s *Service) DisableWithReport(ctx context.Context) (DisableReport, error) {
	return s.disable(ctx)
}

func (s *Service) disable(ctx context.Context) (DisableReport, error) {
	report := DisableReport{}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	cur, err := s.recovery.Current(ctx)
	if err != nil {
		return report, s.disableError(stageReadRecovery, CurrentTargetUnknown, err)
	}
	if cur != nil {
		report.RecoveryTarget = cur.Target
		report.OriginalPresent = cur.OriginalPresent
	} else {
		report.RecoveryTarget = TargetOriginal
	}
	// Read the current config up front so the report records the before
	// classification even on the no-op path (recovery already original/nil):
	// that is exactly the signature of an orphaned gateway config.
	current, err := s.config.ReadRootURL(ctx)
	if err != nil {
		return report, s.disableError(stageReadCurrent, CurrentTargetUnknown, err)
	}
	gatewayURL := ""
	if cur != nil && cur.Target == TargetGateway {
		gatewayURL = cur.AppliedValue
	} else if s.gatewayURL != "" {
		// Recovery is already original/nil, so the recorded applied value is
		// gone; fall back to the last Enable target so the orphaned config is
		// classified as gateway rather than "other".
		gatewayURL = s.gatewayURL
	}
	report.Before = classifyCurrentTarget(current, gatewayURL, s.captureURL)
	report.After = report.Before
	if cur == nil || cur.Target == TargetOriginal {
		// Nothing integrated; a gateway that was started without a successful
		// Enable (or a no-op) has no redirect to undo. The report still records
		// the before classification so an orphaned config is not silently hidden.
		return report, nil
	}
	if cur.Target == TargetGateway && cur.AppliedValue != "" {
		if !current.Present || current.Value != cur.AppliedValue {
			return report, s.disableError(stageGuardRestoreTarget, report.Before, ErrDisableConflict)
		}
	}
	var desired *string
	if cur.OriginalPresent {
		v := ""
		if cur.OriginalValue != nil {
			v = *cur.OriginalValue
		}
		desired = &v
	}
	prepared, err := s.config.PrepareRootURLChange(ctx, desired, current.ConfigHash)
	if err != nil {
		return report, s.disableError(stagePrepareChange, report.Before, err)
	}
	if err := s.config.CommitPreparedRootURLChange(ctx, prepared); err != nil {
		return report, s.disableError(stageCommitChange, report.Before, err)
	}
	report.Restored = true
	desiredValue := ""
	if desired != nil {
		desiredValue = *desired
	}
	report.After = classifyCurrentTarget(codexconfig.RootURLSnapshot{
		Present: desired != nil,
		Value:   desiredValue,
	}, gatewayURL, s.captureURL)
	if err := s.recovery.Checkpoint(ctx, Checkpoint{
		Target:                TargetOriginal,
		OriginalPresent:       false,
		OriginalValue:         nil,
		AppliedValue:          "",
		Active:                false,
		Phase:                 PhaseIdle,
		ConfigHashBeforeApply: "",
		ConfigHashAfterApply:  "",
	}); err != nil {
		return report, s.disableError(stageSaveRecovery, report.Before, err)
	}
	return report, nil
}

// restoreOriginal re-reads the current config and restores the original
// upstream from wherever the config currently points. It is a best-effort
// rollback used after a failed Enable commit.
func (s *Service) restoreOriginal(ctx context.Context, originalValue *string) error {
	current, err := s.config.ReadRootURL(ctx)
	if err != nil {
		return err
	}
	prepared, err := s.config.PrepareRootURLChange(ctx, originalValue, current.ConfigHash)
	if err != nil {
		return err
	}
	return s.config.CommitPreparedRootURLChange(ctx, prepared)
}

// classifyCurrentTarget maps the decoded openai_base_url value to a fixed enum
// for diagnostic logging. "original" means the key is absent (a user's own
// pre-existing upstream URL classifies as "other"). It compares against the two
// managed URLs and never returns the raw value.
func classifyCurrentTarget(current codexconfig.RootURLSnapshot, gatewayURL, captureURL string) CurrentTarget {
	if !current.Present {
		return CurrentTargetOriginal
	}
	switch current.Value {
	case gatewayURL:
		return CurrentTargetGateway
	case captureURL:
		return CurrentTargetAnalysis
	default:
		return CurrentTargetOther
	}
}

func stringPtr(s string) *string { return &s }
