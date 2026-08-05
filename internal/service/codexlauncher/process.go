package codexlauncher

import (
	"context"
)

// processRunner starts a terminal process. It is a field on Launcher so unit
// tests can inject a fake.
type processRunner interface {
	// Start launches the process. On Windows the process is created suspended,
	// assigned to a kill-on-close job object, and only then resumed, so there is
	// no window where the child runs outside the job. Start returns after resume.
	Start(ctx context.Context, opts startOptions) (ProcessHandle, error)
}

// startOptions carries everything the platform adapter needs to start the
// terminal. The command line and working directory are absolute and the env is
// already merged (case-insensitive).
type startOptions struct {
	Executable  string
	CommandLine string
	WorkingDir  string
	Env         []string
}

// ProcessHandle is a running process owned by the launcher. Exactly one
// goroutine (the run's monitor) calls Wait; Stop only requests termination and
// waits on the run's done channel.
type ProcessHandle interface {
	PID() int
	// Wait blocks until the process exits and records its exit code.
	Wait(ctx context.Context) error
	// Terminate force-kills the process and its whole tree via the job object.
	Terminate() error
	// Close releases OS handles. It is idempotent.
	Close() error
	// ExitCode returns the recorded exit code once Wait has returned.
	ExitCode() (*int, error)
}

// platformSendCtrlBreak delivers CTRL_BREAK to a child's console via the
// short-lived helper process. The platform files override it with the real
// implementation; the default reports an unsupported platform.
var platformSendCtrlBreak = func(ctx context.Context, childPID int) error {
	return ErrUnsupportedPlatform
}
