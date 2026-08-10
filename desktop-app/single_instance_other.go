//go:build !windows

package main

// Wails currently targets Windows for this product. Other platforms must not
// silently start without process-wide ownership, so startup fails closed until
// a platform-specific kernel-backed guard is selected.
func acquireSingleInstance() (func(), error) {
	return nil, errSingleInstanceUnsupported
}
