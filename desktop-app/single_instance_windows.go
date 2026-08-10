//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const singleInstanceNamePrefix = `Global\MoonBridgeDesktop-`

// acquireSingleInstance creates a per-user named mutex before any App or
// Gateway object is constructed. The mutex name contains only a stable digest
// of the account name, so account/path/config data never enters the public
// process surface.
func acquireSingleInstance() (func(), error) {
	account := os.Getenv("USERNAME")
	if account == "" {
		account = "unknown-user"
	}
	digest := sha256.Sum256([]byte(account))
	name := singleInstanceNamePrefix + hex.EncodeToString(digest[:8])
	return acquireNamedSingleInstance(name)
}

func acquireNamedSingleInstance(name string) (func(), error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("single instance name unavailable")
	}
	handle, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil && err != windows.ERROR_ALREADY_EXISTS {
		return nil, fmt.Errorf("single instance lock unavailable")
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		if closeErr := windows.CloseHandle(handle); closeErr != nil {
			return nil, fmt.Errorf("single instance lock unavailable")
		}
		return nil, errSingleInstanceAlreadyRunning
	}

	return func() {
		_ = windows.CloseHandle(handle)
	}, nil
}
