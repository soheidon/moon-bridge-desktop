//go:build windows

package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"moonbridge/internal/service/trafficanalysis"
	"moonbridge/internal/service/traffictransaction"
)

// handoffBindTimeout bounds how long the helper retries binding :38440 while it
// waits for MBD to release the listener. It is a few seconds longer than MBD's
// handoffReadyWait so the helper gives up promptly after MBD rolls back.
const handoffBindTimeout = 8 * time.Second

// runHandoffRelay is the helper entry point (MOONBRIDGE_HANDOFF_RELAY=1). It
// relays :38440 → original until the target app-server PID exits, then
// self-terminates. It returns an exit code for os.Exit.
func runHandoffRelay() int {
	upstream := os.Getenv("MOONBRIDGE_HANDOFF_UPSTREAM")
	pidStr := os.Getenv("MOONBRIDGE_HANDOFF_PID")
	readyFile := os.Getenv("MOONBRIDGE_HANDOFF_READY_FILE")
	if upstream == "" || pidStr == "" || readyFile == "" {
		log.Printf("handoff helper: missing configuration")
		return 1
	}
	pid, err := strconv.ParseUint(pidStr, 10, 32)
	if err != nil {
		log.Printf("handoff helper: invalid pid")
		return 1
	}

	cp := trafficanalysis.NewCaptureProxy(trafficanalysis.CaptureConfig{
		ListenAddr:   traffictransaction.FrontDoorAddress,
		UpstreamBase: upstream,
	})
	// Retry Start() until :38440 becomes free (MBD releases it). Start() leaves
	// state alone on bind failure, so re-Start() on the same instance is safe.
	deadline := time.Now().Add(handoffBindTimeout)
	for {
		if err := cp.Start(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("handoff helper: bind failed")
			_ = cp.Close()
			return 1
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Acquire the app-server handle BEFORE reporting READY so READY guarantees
	// lifetime monitoring is established. Distinguish "already exited" from a
	// generic open failure for the log; both are fatal (no READY).
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		reason := "open_failed"
		if err == windows.ERROR_INVALID_PARAMETER {
			reason = "already_exited"
		}
		log.Printf("handoff helper: process handle open failed: reason=%s", reason)
		_ = cp.Close()
		return 1
	}

	_ = cp.Pause() // pure relay: record nothing
	if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
		log.Printf("handoff helper: ready marker write failed")
		windows.CloseHandle(handle)
		_ = cp.Close()
		return 1
	}

	_, _ = windows.WaitForSingleObject(handle, windows.INFINITE)
	windows.CloseHandle(handle)
	_ = cp.Close()
	_ = os.Remove(readyFile)
	return 0
}

// spawnHandoffHelper re-executes this binary detached with the relay env vars.
func spawnHandoffHelper(ctx context.Context, upstream string, pid uint32, readyFile string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exePtr, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	env, err := handoffEnvBlock(map[string]string{
		"MOONBRIDGE_HANDOFF_RELAY":      "1",
		"MOONBRIDGE_HANDOFF_UPSTREAM":   upstream,
		"MOONBRIDGE_HANDOFF_PID":        strconv.FormatUint(uint64(pid), 10),
		"MOONBRIDGE_HANDOFF_READY_FILE": readyFile,
	})
	if err != nil {
		return err
	}
	si := &windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	pi := &windows.ProcessInformation{}
	if err := windows.CreateProcess(exePtr, nil, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.DETACHED_PROCESS,
		&env[0], nil, si, pi); err != nil {
		return err
	}
	windows.CloseHandle(pi.Process)
	windows.CloseHandle(pi.Thread)
	return nil
}

// handoffEnvBlock builds a Windows environment block (UTF-16, null-terminated)
// from the current environment with the given overrides applied.
func handoffEnvBlock(extra map[string]string) ([]uint16, error) {
	env := make(map[string]string, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			env[kv[:i]] = kv[i+1:]
		}
	}
	for k, v := range extra {
		env[k] = v
	}
	block := make([]uint16, 0, 1024)
	for k, v := range env {
		block = append(block, windows.StringToUTF16(k+"="+v)...)
	}
	block = append(block, 0) // final block terminator
	return block, nil
}
