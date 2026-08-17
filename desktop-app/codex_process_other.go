//go:build !windows

package main

import "context"

// detectChatGPTCodexAppServer is a non-Windows no-op. The handoff relay is a
// Windows-only feature; on other platforms no app-server is ever detected.
func detectChatGPTCodexAppServer(ctx context.Context) (uint32, bool, error) {
	return 0, false, nil
}
