package codexlauncher

import (
	"errors"
	"fmt"
)

// ErrorKind matches the CommandError codes the App binding maps to.
type ErrorKind string

const (
	KindNotFound               ErrorKind = "codex_not_found"
	KindInvalidExecutable      ErrorKind = "codex_invalid_executable"
	KindVersionProbeFailed     ErrorKind = "codex_version_probe_failed"
	KindRouteNotFound          ErrorKind = "codex_route_not_found"
	KindConfigGenerationFailed ErrorKind = "codex_config_generation_failed"
	KindConfigPublishFailed    ErrorKind = "codex_config_publish_failed"
	KindAlreadyRunning         ErrorKind = "codex_already_running"
	KindStartFailed            ErrorKind = "codex_start_failed"
	KindProjectInvalid         ErrorKind = "codex_project_invalid"
	KindProjectNotFound        ErrorKind = "codex_project_not_found"
	KindProjectNotDirectory    ErrorKind = "codex_project_not_directory"
	KindStopFailed             ErrorKind = "codex_stop_failed"
)

// Error is a typed failure the App binding maps to a CommandError code. Details
// never carries secrets.
type Error struct {
	Kind    ErrorKind
	Message string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func newError(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

// ErrUnsupportedPlatform is returned by every launcher operation on platforms
// without the Windows process-management implementation.
var ErrUnsupportedPlatform = errors.New("codex launcher is unsupported on this platform")

// asErrorKind wraps err as a typed *Error of kind when it is not already one.
// Typed launcher errors pass through unchanged.
func asErrorKind(err error, kind ErrorKind) error {
	var le *Error
	if errors.As(err, &le) {
		return err
	}
	return &Error{Kind: kind, Message: err.Error()}
}
