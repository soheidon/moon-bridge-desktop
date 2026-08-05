package publishrecovery

import "errors"

// ErrorKind classifies publish-recovery failures for the App binding layer.
type ErrorKind string

const (
	KindJournalParseFailed   ErrorKind = "publish_journal_parse_failed"
	KindJournalWriteFailed   ErrorKind = "publish_journal_write_failed"
	KindTransactionActive    ErrorKind = "publish_transaction_active"
	KindRollbackRequired     ErrorKind = "publish_rollback_required"
	KindRollbackFailed       ErrorKind = "publish_rollback_failed"
	KindBackoutFailed        ErrorKind = "publish_backout_failed"
	KindExternalModification ErrorKind = "publish_external_modification"
	KindTargetHomeChanged    ErrorKind = "publish_target_home_changed"
	KindConfigPathInvalid    ErrorKind = "publish_config_path_invalid"
	KindTransactionInvalid   ErrorKind = "publish_transaction_invalid"
)

// Error is a typed publish-recovery failure. Details never carries raw secrets,
// content hashes, absolute paths, or file contents: only non-sensitive logical
// state may be included.
type Error struct {
	Kind    ErrorKind
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func newError(kind ErrorKind, msg string) *Error {
	return &Error{Kind: kind, Message: msg}
}

// asErrorKind unwraps err to its ErrorKind, or "" when it is not a typed Error.
func asErrorKind(err error) ErrorKind {
	var te *Error
	if errors.As(err, &te) {
		return te.Kind
	}
	return ""
}
