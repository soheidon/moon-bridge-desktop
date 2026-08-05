//go:build windows

package codexlauncher

import "os"

// modeIsPermissive is always false on Windows: Go's os.Stat reports 0666 for
// every normal file because Windows has no POSIX permission bits, so a bit check
// cannot detect anything here. The token file's protection on Windows comes from
// the user-profile ACL the file inherits; enforcing a restrictive DACL is out of
// scope for the publish transaction.
func modeIsPermissive(mode os.FileMode) bool {
	return false
}
