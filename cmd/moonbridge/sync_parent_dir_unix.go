//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func syncParentDir(path string) error {
	dirPath := filepath.Dir(path)
	dir, err := os.Open(dirPath)
	if err != nil {
		return fmt.Errorf("open config directory %s: %w", dirPath, err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		if closeErr != nil {
			return errors.Join(fmt.Errorf("sync config directory %s: %w", dirPath, err), fmt.Errorf("close config directory %s: %w", dirPath, closeErr))
		}
		return fmt.Errorf("sync config directory %s: %w", dirPath, err)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close config directory %s: %w", dirPath, err)
	}
	return nil
}
