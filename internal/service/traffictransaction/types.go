package traffictransaction

import (
	"context"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/trafficanalysis"
)

const CaptureListenAddress = "127.0.0.1:38441"
const captureURL = "http://" + CaptureListenAddress

type Operation string

const (
	OperationEnable  Operation = "enable"
	OperationFinish  Operation = "finish"
	OperationDisable Operation = "disable"
	OperationRestore Operation = "restore"
	OperationDiscard Operation = "discard"
	OperationCleanup Operation = "cleanup"
)

type Phase string

const (
	PhaseIdle                Phase = "idle"
	PhaseValidating          Phase = "validating"
	PhasePrepared            Phase = "prepared"
	PhaseCaptureStarted      Phase = "capture_started"
	PhaseCaptureAdopted      Phase = "capture_adopted"
	PhaseOwnershipClaimed    Phase = "ownership_claimed"
	PhaseConfigCommitted     Phase = "config_committed"
	PhaseCheckpointUncertain Phase = "checkpoint_uncertain"
	PhaseBackoutPending      Phase = "backout_pending"
	PhaseRecoveryRequired    Phase = "recovery_required"
	PhaseCompleted           Phase = "completed"
	PhaseAborted             Phase = "aborted"
	PhaseDisableStarted      Phase = "disable_started"
	PhaseConfigRestored      Phase = "config_restored"
	PhaseDisableCompleted    Phase = "disable_completed"
	PhaseInactive            Phase = "inactive"
)

type DurablePhase string

const (
	DurableIntegrationApplied     DurablePhase = "integration_applied"
	DurableRecovered              DurablePhase = "recovered"
	DurableInactive               DurablePhase = "inactive"
	DurableReconciliationRequired DurablePhase = "reconciliation_required"
)

// ReconciliationStatusConfigConflict marks a Disable restore-conflict
// checkpoint so the GUI can surface the live dead-end without a process
// restart. The value must equal recovery.StatusConfigConflict.
const ReconciliationStatusConfigConflict = "config_conflict"

type GatewaySnapshot struct {
	Running           bool
	InstanceID        string
	Address           string
	DefaultModelAlias string
	RoutingAvailable  bool
}

type GatewayProvider interface {
	Snapshot(context.Context) (GatewaySnapshot, error)
}

type TrafficProvider interface {
	Status() trafficanalysis.State
	ValidateCaptureExpected(uint64, string, string) (trafficanalysis.State, error)
	ValidateDesktopOwnershipExpected(uint64, string, string, string) bool
	ValidateDesktopIntegrationExpected(uint64, string, string, string, string) (trafficanalysis.State, error)
	ValidateDesktopPassthroughExpected(uint64, string, string, string, string) (trafficanalysis.State, error)
	ValidateCaptureOnlyExpected(uint64, string, string, string) (trafficanalysis.State, error)
	ValidateIdleExpected(uint64) (trafficanalysis.State, error)
	StartCapture(trafficanalysis.StartOptions) (trafficanalysis.State, error)
	ClaimDesktopExpected(uint64, string, string, string) (trafficanalysis.State, error)
	SetDesktopModelMappingExpected(uint64, string, string, string, string, string) error
	ClearDesktopModelMappingExpected(uint64, string, string, string) error
	PauseDesktopExpected(context.Context, uint64, string, string, string) (trafficanalysis.State, error)
	ReleaseDesktopExpected(uint64, string) (trafficanalysis.State, error)
	CloseCapture(context.Context) (trafficanalysis.State, error)
}

type ConfigEditor interface {
	ReadRootURL(context.Context) (codexconfig.RootURLSnapshot, error)
	ReadRoutingIdentity(context.Context) (codexconfig.RoutingIdentitySnapshot, error)
	PrepareRootURLChange(context.Context, *string, string) (*codexconfig.PreparedRootURLChange, error)
	CommitPreparedRootURLChange(context.Context, *codexconfig.PreparedRootURLChange) error
}

type BackupRef struct{ ID string }

type CleanupPending struct {
	TransactionID       string
	BackupID            string
	RouteMutationResult string
	Status              string
}

type BackupManager interface {
	Create(context.Context) (BackupRef, error)
	Remove(context.Context, BackupRef) error
}

type Checkpoint struct {
	OperationID string
	// OwnerID is process-local evidence for the Traffic Service adapter. It is
	// never serialized into Recovery JSON or exposed in public results.
	OwnerID                      string
	DurablePhase                 DurablePhase
	Phase                        Phase
	BeforeHash                   string
	AfterHash                    string
	PreviousPresent              bool
	PreviousValue                string
	AppliedValue                 string
	BackupID                     string
	GatewayInstance              string
	GatewayAddress               string
	CaptureGeneration            uint64
	IntegrationActive            bool
	RelayActive                  bool
	CaptureState                 string
	UnsavedObservationsMayRemain bool
	UnsavedDiscardConfirmed      bool
	AutoLogFinalized             bool
	// ReconciliationStatus records the Recovery reconciliation classification
	// (e.g. ReconciliationStatusConfigConflict) when Disable hits a restore
	// conflict, so the GUI can surface the live dead-end without a restart.
	ReconciliationStatus string
}

type RecoveryWriter interface {
	HasUnresolved(context.Context) (bool, error)
	Checkpoint(context.Context, Checkpoint) error
	Current(context.Context) (Checkpoint, error)
	SetCleanupPending(context.Context, CleanupPending) error
	GetCleanupPending(context.Context) (*CleanupPending, error)
	ClearCleanupPending(context.Context, string, string) error
}

type IDGenerator interface{ New() string }

type Dependencies struct {
	Gateway  GatewayProvider
	Traffic  TrafficProvider
	Config   ConfigEditor
	Backup   BackupManager
	Recovery RecoveryWriter
	IDs      IDGenerator
}

type Snapshot struct {
	Operation            Operation
	Phase                Phase
	CaptureState         string
	TrafficMode          trafficanalysis.ManagementMode
	CaptureGeneration    uint64
	GatewayMatches       bool
	IntegrationActive    bool
	Retryable            bool
	ConfirmationRequired bool
	CleanupPending       *CleanupPending
}

type FailureCause string

const (
	CauseValidation      FailureCause = "validation"
	CauseAdoption        FailureCause = "adoption"
	CauseGatewayLost     FailureCause = "gateway_lost"
	CausePrepare         FailureCause = "prepare"
	CauseBackup          FailureCause = "backup"
	CauseCheckpoint      FailureCause = "checkpoint"
	CauseCaptureStart    FailureCause = "capture_start"
	CauseOwnershipClaim  FailureCause = "ownership_claim"
	CauseConfigConflict  FailureCause = "config_conflict"
	CauseConfigSave      FailureCause = "config_save"
	CauseConfigVerify    FailureCause = "config_verify"
	CauseFinalValidation FailureCause = "final_validation"
	CauseStaleOwner      FailureCause = "stale_owner"
	CauseBackout         FailureCause = "backout"
)

type FailureState struct {
	Phase               Phase
	StartedCapture      bool
	AdoptedCapture      bool
	OwnershipClaimed    bool
	ModelMappingClaimed bool
	ConfigCommitted     bool
	CheckpointUncertain bool
}

type BackoutPlan struct {
	RestoreConfig        bool
	ReleaseOwnership     bool
	CloseNewCapture      bool
	RecoveryRequired     bool
	FinalPhase           Phase
	ErrorKind            ErrorKind
	Retryable            bool
	ConfirmationRequired bool
}

type FinishFailure string

const (
	FinishFailurePrecondition      FinishFailure = "precondition"
	FinishFailureIntegrationActive FinishFailure = "integration_active"
	FinishFailureUnsavedConfirm    FinishFailure = "unsaved_confirmation"
	FinishFailureFinalize          FinishFailure = "finalize"
	FinishFailureClose             FinishFailure = "close"
	FinishFailureCloseUnknown      FinishFailure = "close_unknown"
	FinishFailureStale             FinishFailure = "stale"
	FinishFailureFinalCheckpoint   FinishFailure = "final_checkpoint"
	FinishFailureFinalValidation   FinishFailure = "final_validation"
	FinishFailureConflict          FinishFailure = "operation_conflict"
)

type FinishFailurePlan struct {
	ErrorKind            ErrorKind
	Retryable            bool
	ConfirmationRequired bool
	RecoveryRequired     bool
}

func ClassifyFinishFailure(failure FinishFailure) FinishFailurePlan {
	switch failure {
	case FinishFailureUnsavedConfirm:
		return FinishFailurePlan{ErrorKind: KindFinishConfirmation, ConfirmationRequired: true}
	case FinishFailureClose:
		return FinishFailurePlan{ErrorKind: KindFinishCloseFailed, Retryable: true, RecoveryRequired: true}
	case FinishFailureCloseUnknown, FinishFailureFinalCheckpoint, FinishFailureFinalValidation, FinishFailureFinalize, FinishFailureStale:
		return FinishFailurePlan{ErrorKind: KindFinishFinalValidation, Retryable: true, RecoveryRequired: true}
	case FinishFailureConflict:
		return FinishFailurePlan{ErrorKind: KindTransactionInProgress, Retryable: true}
	default:
		return FinishFailurePlan{ErrorKind: KindFinishPrecondition, Retryable: true}
	}
}

func ClassifyFailure(state FailureState, cause FailureCause) BackoutPlan {
	if state.CheckpointUncertain {
		return BackoutPlan{RecoveryRequired: true, FinalPhase: PhaseRecoveryRequired, ErrorKind: KindCheckpointUncertain}
	}
	if cause == CauseStaleOwner {
		return BackoutPlan{RecoveryRequired: true, ConfirmationRequired: true, FinalPhase: PhaseRecoveryRequired, ErrorKind: KindRecoveryRequired, Retryable: true}
	}
	plan := BackoutPlan{FinalPhase: PhaseAborted, ErrorKind: errorKindForCause(cause), Retryable: true}
	plan.RestoreConfig = state.ConfigCommitted
	plan.ReleaseOwnership = state.OwnershipClaimed
	plan.CloseNewCapture = state.StartedCapture
	if state.ConfigCommitted || state.OwnershipClaimed || state.StartedCapture || state.AdoptedCapture {
		plan.FinalPhase = PhaseBackoutPending
	}
	return plan
}

func errorKindForCause(cause FailureCause) ErrorKind {
	switch cause {
	case CauseValidation:
		return KindTransactionFailed
	case CauseAdoption:
		return KindCaptureNotActive
	case CauseGatewayLost:
		return KindGatewayNotRunning
	case CausePrepare:
		return KindPrepareFailed
	case CauseBackup:
		return KindBackupFailed
	case CauseCheckpoint:
		return KindCheckpointFailed
	case CauseCaptureStart:
		return KindCaptureStartFailed
	case CauseOwnershipClaim:
		return KindOwnershipClaimFailed
	case CauseConfigConflict:
		return KindConfigConflict
	case CauseConfigSave:
		return KindConfigSaveFailed
	case CauseConfigVerify:
		return KindConfigVerifyFailed
	case CauseFinalValidation:
		return KindRecoveryRequired
	case CauseStaleOwner:
		return KindRecoveryRequired
	case CauseBackout:
		return KindBackoutFailed
	default:
		return KindTransactionFailed
	}
}
