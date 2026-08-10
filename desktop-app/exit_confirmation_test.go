package main

import (
	"context"
	"testing"
)

func TestConfirmExitUsesInjectedRuntimeAfterConfirmation(t *testing.T) {
	quitCalls := 0
	a := NewApp(AppOptions{Quit: func(context.Context) { quitCalls++ }})
	a.ctx = context.Background()
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if !result.OK {
		t.Fatalf("ConfirmExit failed: %+v", result.Error)
	}
	if quitCalls != 1 {
		t.Fatalf("quit calls = %d, want 1", quitCalls)
	}
}

func TestConfirmExitRuntimePanicIsSafeAndRetryable(t *testing.T) {
	a := NewApp(AppOptions{Quit: func(context.Context) { panic("runtime sentinel") }})
	a.ctx = context.Background()
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.ConfirmExit(ConfirmExitInput{Confirm: true})
	if result.OK || result.Error == nil {
		t.Fatalf("panic must be returned as a safe error: %+v", result)
	}
	if result.Error.Code != "desktop_quit_failed" || !result.Error.Retryable {
		t.Fatalf("unexpected safe error: %+v", result.Error)
	}
	a.exitMu.Lock()
	state := a.exitState
	a.exitMu.Unlock()
	if state != exitRequested {
		t.Fatalf("exit state = %q, want requested for retry", state)
	}
}

func TestCancelExitDoesNotInvokeRuntime(t *testing.T) {
	quitCalls := 0
	a := NewApp(AppOptions{Quit: func(context.Context) { quitCalls++ }})
	a.exitMu.Lock()
	a.exitState = exitRequested
	a.exitMu.Unlock()

	result := a.CancelExit()
	if !result.OK {
		t.Fatalf("CancelExit failed: %+v", result.Error)
	}
	if quitCalls != 0 {
		t.Fatalf("cancel invoked quit %d times", quitCalls)
	}
	a.exitMu.Lock()
	state := a.exitState
	a.exitMu.Unlock()
	if state != exitIdle {
		t.Fatalf("exit state = %q, want idle", state)
	}
}
