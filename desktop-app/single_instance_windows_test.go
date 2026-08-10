//go:build windows

package main

import (
	"fmt"
	"os"
	"testing"
)

func TestSingleInstanceRejectsSecondOwnerAndAllowsReacquire(t *testing.T) {
	name := fmt.Sprintf("%stest-%d", singleInstanceNamePrefix, os.Getpid())
	release, err := acquireNamedSingleInstance(name)
	if err != nil {
		t.Fatalf("first acquisition: %v", err)
	}
	released := false
	defer func() {
		if !released {
			release()
		}
	}()

	secondRelease, err := acquireNamedSingleInstance(name)
	if err != errSingleInstanceAlreadyRunning {
		if secondRelease != nil {
			secondRelease()
		}
		t.Fatalf("second acquisition error = %v, want already-running", err)
	}

	release()
	released = true
	thirdRelease, err := acquireNamedSingleInstance(name)
	if err != nil {
		t.Fatalf("reacquisition after release: %v", err)
	}
	thirdRelease()
}
