package recovery

// ErrorKind classifies recovery failures for the App binding layer.
type ErrorKind string

const (
	KindStateNotFound           ErrorKind = "recovery_state_not_found"
	KindStateParseFailed        ErrorKind = "recovery_state_parse_failed"
	KindStateUnsupportedVersion ErrorKind = "recovery_state_unsupported_version"
	KindMigrationFailed         ErrorKind = "recovery_migration_failed"
	KindReconcileFailed         ErrorKind = "recovery_reconcile_failed"
	KindConfirmationRequired    ErrorKind = "recovery_confirmation_required"
	KindUnsavedConfirmationReq  ErrorKind = "recovery_unsaved_confirmation_required"
	KindRestoreFailed           ErrorKind = "recovery_restore_failed"
	KindRollbackFailed          ErrorKind = "recovery_rollback_failed"
	KindPublishIncidentDetected ErrorKind = "recovery_publish_incident_detected"
	KindPublishRollbackFailed   ErrorKind = "recovery_publish_rollback_failed"
	KindConfigPathChangedError  ErrorKind = "recovery_config_path_changed"
	KindConfigPathInvalid       ErrorKind = "recovery_config_path_invalid"
)

// Error is a typed recovery failure. Details never carries secrets.
type Error struct {
	Kind    ErrorKind
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func newError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Message: msg}
}
