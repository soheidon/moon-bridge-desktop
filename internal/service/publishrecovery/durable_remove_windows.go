//go:build windows

package publishrecovery

// syncParentDir is a no-op on Windows: there is no portable parent-directory
// sync API equivalent to the Unix dir-fd fsync. The usable durability boundary
// is therefore DeleteFile success: once os.Remove returns nil and the file is
// confirmed absent, the removal is treated as durable. No full metadata
// persistence guarantee across power loss is offered. This mirrors codexconfig's
// private syncParentDir contract, which is unexported and therefore mirrored
// rather than reused.
func syncParentDir(string) error { return nil }
