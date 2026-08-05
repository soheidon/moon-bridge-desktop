//go:build windows

package codexlauncher

import "golang.org/x/sys/windows"

// buildEnvBlock converts environment entries to the double-null-terminated
// UTF-16 block CreateProcess expects (CREATE_UNICODE_ENVIRONMENT).
// UTF16FromString already appends the per-entry NUL, so only the block
// terminator is added here; otherwise a double NUL would prematurely end the
// block and drop every entry after it.
func buildEnvBlock(entries []string) ([]uint16, error) {
	var block []uint16
	for _, entry := range entries {
		u, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	return block, nil
}
