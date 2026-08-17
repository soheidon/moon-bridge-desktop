//go:build !windows

package main

import (
	"context"
	"errors"
)

// runHandoffRelay is a non-Windows stub; the relay helper is never spawned here.
func runHandoffRelay() int {
	return 1
}

// spawnHandoffHelper is a non-Windows stub; the handoff relay is unsupported.
func spawnHandoffHelper(ctx context.Context, upstream string, pid uint32, readyFile string) error {
	return errors.New("handoff relay is unsupported on this platform")
}
