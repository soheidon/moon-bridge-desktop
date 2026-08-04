package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// EnsureStarterConfig creates a first-run starter config at configPath if it
// does not already exist, and reports whether it created one. An existing file
// is never overwritten, even if another process publishes it concurrently.
func EnsureStarterConfig(configPath string, opts LoadOptions) (bool, error) {
	if _, err := os.Stat(configPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("stat config %s: %w", configPath, err)
	}

	dbPath, err := StarterSQLiteDBPath(configPath)
	if err != nil {
		return false, err
	}
	data, err := StarterConfigYAML(dbPath, opts)
	if err != nil {
		return false, err
	}
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return false, fmt.Errorf("create config directory %s: %w", configDir, err)
	}
	if err := os.Chmod(configDir, 0o700); err != nil {
		return false, fmt.Errorf("chmod config directory %s: %w", configDir, err)
	}
	created, err := writeFileExclusive(configPath, data, 0o600)
	if err != nil {
		return false, err
	}
	return created, nil
}

func writeFileExclusive(path string, data []byte, perm os.FileMode) (bool, error) {
	configDir := filepath.Dir(path)
	tempFile, err := os.CreateTemp(configDir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return false, fmt.Errorf("create temp config file in %s: %w", configDir, err)
	}
	tempPath := tempFile.Name()
	if err := tempFile.Chmod(perm); err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("chmod temp config file %s: %w", tempPath, err))
	}
	written, err := tempFile.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("write temp config file %s: %w", tempPath, err))
	}
	if err := tempFile.Sync(); err != nil {
		return false, cleanupTempConfigFile(tempFile, tempPath, fmt.Errorf("sync temp config file %s: %w", tempPath, err))
	}
	if err := tempFile.Close(); err != nil {
		return false, cleanupTempPath(tempPath, fmt.Errorf("close temp config file %s: %w", tempPath, err))
	}
	return publishConfigFile(tempPath, path)
}

func publishConfigFile(tempPath string, finalPath string) (bool, error) {
	if err := os.Link(tempPath, finalPath); err != nil {
		cleanupErr := cleanupTempPath(tempPath, nil)
		if os.IsExist(err) {
			if cleanupErr != nil {
				return false, cleanupErr
			}
			return false, nil
		}
		if cleanupErr != nil {
			return false, errors.Join(fmt.Errorf("publish config file %s from %s: %w", finalPath, tempPath, err), cleanupErr)
		}
		return false, fmt.Errorf("publish config file %s from %s: %w", finalPath, tempPath, err)
	}
	if err := syncParentDir(finalPath); err != nil {
		return false, cleanupTempPath(tempPath, fmt.Errorf("sync config directory after publishing %s: %w", finalPath, err))
	}
	if err := os.Remove(tempPath); err != nil {
		return false, fmt.Errorf("remove published temp config file %s: %w", tempPath, err)
	}
	if err := syncParentDir(finalPath); err != nil {
		return false, fmt.Errorf("sync config directory after removing temp config %s: %w", tempPath, err)
	}
	return true, nil
}

func cleanupTempConfigFile(file *os.File, path string, cause error) error {
	var errs []error
	errs = append(errs, cause)
	if err := file.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close temp config file %s: %w", path, err))
	}
	return cleanupTempPath(path, errors.Join(errs...))
}

func cleanupTempPath(path string, cause error) error {
	var errs []error
	if cause != nil {
		errs = append(errs, cause)
	}
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove temp config file %s: %w", path, err))
		}
	} else if err := syncParentDir(path); err != nil {
		errs = append(errs, fmt.Errorf("sync config directory after removing temp config %s: %w", path, err))
	}
	return errors.Join(errs...)
}
