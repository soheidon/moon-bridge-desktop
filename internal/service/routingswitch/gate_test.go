package routingswitch

import (
	"errors"
	"testing"
)

func TestRouteGateCompetitionAndRelease(t *testing.T) {
	gate := NewGate()
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		token, err := gate.Begin(OperationRoute)
		if err != nil {
			done <- err
			return
		}
		close(started)
		<-release
		done <- token.Release()
	}()
	<-started
	if _, err := gate.Begin(OperationRoute); !errors.Is(err, ErrRouteOperationBusy) {
		t.Fatalf("second route begin error = %v, want %v", err, ErrRouteOperationBusy)
	}
	if _, err := gate.Begin(OperationTraffic); !errors.Is(err, ErrRouteOperationBusy) {
		t.Fatalf("traffic begin error = %v, want %v", err, ErrRouteOperationBusy)
	}
	if _, err := gate.Begin(OperationRecovery); !errors.Is(err, ErrRouteOperationBusy) {
		t.Fatalf("recovery begin error = %v, want %v", err, ErrRouteOperationBusy)
	}
	if !gate.Active() {
		t.Fatal("gate is inactive while route token is held")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if gate.Active() {
		t.Fatal("gate remains active after release")
	}
	token, err := gate.Begin(OperationTraffic)
	if err != nil {
		t.Fatal(err)
	}
	if err := token.Release(); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(token.Release(), ErrTokenReleased) {
		t.Fatal("second release was not rejected")
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic path did not panic")
			}
		}()
		panicWithRelease(gate)
	}()
	if gate.Active() {
		t.Fatal("panic path did not release gate")
	}
}

func panicWithRelease(gate *Gate) {
	token, err := gate.Begin(OperationRoute)
	if err != nil {
		panic(err)
	}
	defer func() { _ = token.Release() }()
	panic("controlled test panic")
}
