package traffictransaction

type ErrorKind string

const (
	KindGatewayNotRunning     ErrorKind = "traffic_gateway_not_running"
	KindGatewayMismatch       ErrorKind = "traffic_gateway_mismatch"
	KindCaptureNotActive      ErrorKind = "traffic_capture_not_active"
	KindTransactionInProgress ErrorKind = "traffic_transaction_in_progress"
	KindPrepareFailed         ErrorKind = "traffic_transaction_prepare_failed"
	KindCheckpointFailed      ErrorKind = "traffic_transaction_checkpoint_failed"
	KindCheckpointUncertain   ErrorKind = "traffic_transaction_checkpoint_uncertain"
	KindConfigReadFailed      ErrorKind = "traffic_config_read_failed"
	KindConfigEditFailed      ErrorKind = "traffic_config_edit_failed"
	KindBackupFailed          ErrorKind = "traffic_config_backup_failed"
	KindConfigSaveFailed      ErrorKind = "traffic_config_save_failed"
	KindConfigVerifyFailed    ErrorKind = "traffic_config_verify_failed"
	KindConfigConflict        ErrorKind = "traffic_config_conflict"
	KindCaptureStartFailed    ErrorKind = "traffic_capture_start_failed"
	KindOwnershipClaimFailed  ErrorKind = "traffic_ownership_claim_failed"
	KindBackoutFailed         ErrorKind = "traffic_transaction_backout_failed"
	KindTransactionFailed     ErrorKind = "traffic_transaction_failed"
	KindRecoveryRequired      ErrorKind = "traffic_transaction_recovery_required"
	KindPauseFailed           ErrorKind = "traffic_pause_failed"
	KindRestoreConflict       ErrorKind = "traffic_config_restore_conflict"
	KindRestoreFailed         ErrorKind = "traffic_config_restore_failed"
	KindReleaseFailed         ErrorKind = "traffic_desktop_release_failed"
	KindFinalValidationFailed ErrorKind = "traffic_disable_final_validation_failed"
	KindFinishPrecondition    ErrorKind = "traffic_finish_precondition_failed"
	KindFinishConfirmation    ErrorKind = "traffic_finish_confirmation_required"
	KindFinishCloseFailed     ErrorKind = "traffic_finish_close_failed"
	KindFinishFinalValidation ErrorKind = "traffic_finish_final_validation_failed"
)

// Error is safe for UI/public mapping. Lower-level error text is intentionally
// not retained because it may contain paths, URLs, or secret material.
type Error struct {
	Kind                 ErrorKind `json:"kind"`
	Message              string    `json:"message"`
	Retryable            bool      `json:"retryable"`
	ConfirmationRequired bool      `json:"confirmationRequired,omitempty"`
}

func (e *Error) Error() string { return e.Message }

func safeError(kind ErrorKind, message string, retryable bool) *Error {
	return &Error{Kind: kind, Message: message, Retryable: retryable}
}

func finishConfirmationError() *Error {
	return &Error{Kind: KindFinishConfirmation, Message: "finishing requires confirmation for unsaved observations", Retryable: false, ConfirmationRequired: true}
}
