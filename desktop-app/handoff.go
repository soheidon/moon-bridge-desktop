package main

import (
	"context"
	"errors"
	"log"
	"os"
	"time"

	"moonbridge/internal/service/traffictransaction"
)

// handoffReadyWait bounds how long MBD waits for the relay helper to report
// READY after :38440 is released. The helper's own bind timeout is a few
// seconds longer so it gives up promptly after MBD rolls back. It is a var (not
// const) so tests can shorten the bound; production always uses 5s.
var handoffReadyWait = 5 * time.Second

// handoffFrontDoor hands the :38440 → original passthrough over to a short-lived
// relay helper so a running Codex keeps working after MBD exits. It parks the
// front door, restores the config, spawns the helper, releases :38440, and waits
// for the helper's READY marker. On any failure it rolls back to Gateway ON and
// returns an error (fail-closed: MBD does not exit).
func (a *App) handoffFrontDoor(pid uint32) error {
	original := a.originalUpstream()

	if a.frontDoor != nil {
		_ = a.setFrontDoorUpstream(original)
		logFrontDoorMode("handoff_to_original_upstream")
	}

	restoreCtx, restoreCancel := context.WithTimeout(context.Background(), 10*time.Second)
	restoreErr := a.disableGatewayIntegration(restoreCtx)
	restoreCancel()
	if restoreErr != nil {
		log.Printf("handoff: config restore failed (rollback)")
		a.rollbackToGatewayOn()
		return restoreErr
	}

	readyFile, err := newHandoffReadyFile()
	if err != nil {
		log.Printf("handoff: ready file allocation failed (rollback)")
		a.rollbackToGatewayOn()
		return err
	}
	if err := a.spawnHandoffHelper(context.Background(), original, pid, readyFile); err != nil {
		log.Printf("handoff: helper spawn failed (rollback)")
		_ = os.Remove(readyFile)
		a.rollbackToGatewayOn()
		return err
	}
	a.stopFrontDoor()
	if err := waitHandoffReady(readyFile); err != nil {
		log.Printf("handoff: helper did not become ready (rollback)")
		_ = os.Remove(readyFile)
		a.rollbackToGatewayOn()
		return err
	}
	_ = os.Remove(readyFile)
	log.Printf("handoff: helper ready")
	return nil
}

// rollbackToGatewayOn restores the Gateway ON state after a failed handoff. It
// re-binds :38440, re-integrates the config, and re-points the front door at the
// gateway backend. The backend :38442 is still running (stopped later in
// shutdown), so its start is skipped. Each step is best-effort and secret-free.
func (a *App) rollbackToGatewayOn() {
	if err := a.startFrontDoor(); err != nil {
		log.Printf("handoff rollback: front door restart failed")
	}
	if err := a.integrateGateway("http://" + traffictransaction.FrontDoorAddress); err != nil {
		log.Printf("handoff rollback: config re-integration failed")
	}
	if err := a.setFrontDoorUpstream("http://" + traffictransaction.GatewayBackendAddress); err != nil {
		log.Printf("handoff rollback: front door switch failed")
	}
	logFrontDoorMode("handoff_rollback_to_gateway_backend")
}

// newHandoffReadyFile allocates a unique, currently-absent temp path for the
// helper's READY marker. The random suffix makes it unpredictable; the file is
// removed after allocation so the path is guaranteed absent before spawn.
func newHandoffReadyFile() (string, error) {
	f, err := os.CreateTemp("", "mbd_handoff_*.ready")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	return path, nil
}

// waitHandoffReady polls for the helper's READY marker until the bounded
// deadline. Absence past the deadline is treated as a failed handoff.
func waitHandoffReady(path string) error {
	deadline := time.Now().Add(handoffReadyWait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("handoff helper did not become ready")
}
