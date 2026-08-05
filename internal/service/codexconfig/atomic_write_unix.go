//go:build !windows

package codexconfig

import "os"

func replaceFile(src, dst string) error {
	return os.Rename(src, dst)
}

func syncParentDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
