package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"moonbridge/internal/service/gateway"
)

// TestConfirmExitHandoffDetectionErrorStaysRunning: a detection failure happens
// before any mutation, so ConfirmExit must return handoff_unavailable, must not
// quit, and must leave the exit state at requested for retry.
func TestConfirmExitHandoffDetectionErrorStaysRunning(t *testing.T) {
	quitCalls := 0
	a := NewApp(AppOptions{
		Service: newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		DetectChatGPTCodexAppServer: func(context.Context) (uint32, bool, error) {
			return 0, false, errors.New("detection sentinel")
		},
		Quit: func(context.Context) { quitCalls++ },
	})
	a.ctx = context.Background()
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if result.OK || result.Error == nil {
		t.Fatalf("detection error must fail: %+v", result)
	}
	if result.Error.Code != "handoff_unavailable" {
		t.Fatalf("code = %q, want handoff_unavailable", result.Error.Code)
	}
	if quitCalls != 0 {
		t.Fatalf("quit calls = %d, want 0", quitCalls)
	}
	a.exitMu.Lock()
	state := a.exitState
	a.exitMu.Unlock()
	if state != exitRequested {
		t.Fatalf("exit state = %q, want requested", state)
	}
}

// TestConfirmExitHandoffNotFoundQuitsNormally: no app-server detected means the
// normal exit path runs unchanged (no handoff transaction, no extra delay).
func TestConfirmExitHandoffNotFoundQuitsNormally(t *testing.T) {
	quitCalls := 0
	a := NewApp(AppOptions{
		Service: newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		DetectChatGPTCodexAppServer: func(context.Context) (uint32, bool, error) {
			return 0, false, nil
		},
		Quit: func(context.Context) { quitCalls++ },
	})
	a.ctx = context.Background()
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if !result.OK {
		t.Fatalf("not-found must quit normally: %+v", result.Error)
	}
	if quitCalls != 1 {
		t.Fatalf("quit calls = %d, want 1", quitCalls)
	}
}

// TestConfirmExitHandoffSuccessQuits: a found app-server triggers the handoff
// transaction; a helper that reports READY lets ConfirmExit quit normally and
// passes the correct PID + original upstream to the helper.
func TestConfirmExitHandoffSuccessQuits(t *testing.T) {
	opts := scopedGatewayIntegration(t, AppOptions{
		Service: newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		DetectChatGPTCodexAppServer: func(context.Context) (uint32, bool, error) {
			return 4242, true, nil
		},
	})
	var gotPID uint32
	var gotUpstream string
	opts.SpawnHandoffHelper = func(_ context.Context, upstream string, pid uint32, readyFile string) error {
		gotPID = pid
		gotUpstream = upstream
		return os.WriteFile(readyFile, []byte("ready"), 0o600)
	}
	quitCalls := 0
	opts.Quit = func(context.Context) { quitCalls++ }

	a := NewApp(opts)
	a.ctx = context.Background()
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if !result.OK {
		t.Fatalf("handoff success must quit: %+v", result.Error)
	}
	if quitCalls != 1 {
		t.Fatalf("quit calls = %d, want 1", quitCalls)
	}
	if gotPID != 4242 {
		t.Fatalf("helper pid = %d, want 4242", gotPID)
	}
	if gotUpstream == "" {
		t.Fatal("helper upstream must be non-empty")
	}
}

// TestConfirmExitHandoffReadyTimeoutRollsBack: a helper that never reports READY
// fails the handoff; ConfirmExit must roll back (re-bind :38440 → gateway), must
// not quit, and must leave the exit state at requested.
func TestConfirmExitHandoffReadyTimeoutRollsBack(t *testing.T) {
	oldWait := handoffReadyWait
	handoffReadyWait = 50 * time.Millisecond
	defer func() { handoffReadyWait = oldWait }()

	opts := scopedGatewayIntegration(t, AppOptions{
		Service: newScriptedController(gateway.State{Status: gateway.StatusRunning}),
		DetectChatGPTCodexAppServer: func(context.Context) (uint32, bool, error) {
			return 4242, true, nil
		},
	})
	spawnCalls := 0
	opts.SpawnHandoffHelper = func(_ context.Context, _ string, _ uint32, _ string) error {
		spawnCalls++
		return nil // never writes READY → bounded wait times out
	}
	quitCalls := 0
	opts.Quit = func(context.Context) { quitCalls++ }

	a := NewApp(opts)
	a.ctx = context.Background()
	defer a.stopFrontDoor() // release :38440 that the rollback re-binds

	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if result.OK || result.Error == nil {
		t.Fatalf("ready timeout must fail: %+v", result)
	}
	if result.Error.Code != "handoff_failed" {
		t.Fatalf("code = %q, want handoff_failed", result.Error.Code)
	}
	if quitCalls != 0 {
		t.Fatalf("quit calls = %d, want 0", quitCalls)
	}
	if spawnCalls != 1 {
		t.Fatalf("spawn calls = %d, want 1", spawnCalls)
	}
	a.exitMu.Lock()
	state := a.exitState
	a.exitMu.Unlock()
	if state != exitRequested {
		t.Fatalf("exit state = %q, want requested", state)
	}
}
