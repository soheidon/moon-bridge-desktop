//go:build !windows

package codexlauncher

import "os"

// modeIsPermissive reports whether a file's POSIX mode makes it group/world
// readable or writable. The token-bearing auth.json and config.toml are written
// 0600 and must not drift to a looser mode.
func modeIsPermissive(mode os.FileMode) bool {
	return mode&0o077 != 0
}
