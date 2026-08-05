//go:build !windows

package codexlauncher

import "context"

type unsupportedRunner struct{}

func newProcessRunner() processRunner { return unsupportedRunner{} }

func (unsupportedRunner) Start(ctx context.Context, opts startOptions) (ProcessHandle, error) {
	return nil, ErrUnsupportedPlatform
}

func isUncPath(string) bool { return false }
