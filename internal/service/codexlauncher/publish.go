package codexlauncher

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"moonbridge/internal/service/codexconfig"
)

// codexHomeFiles are the files a codex-home may contain. auth.json is
// conditional: it is published only when a server token is configured.
var codexHomeFiles = []string{"config.toml", "models_catalog.json", "auth.json"}

// configCommitOrder is the atomic-replace order. config.toml is committed last
// so a partially published home is never mistaken for a committed one.
var configCommitOrder = []string{"models_catalog.json", "auth.json", "config.toml"}

// atomicWriteFile is a seam so tests can force a mid-transaction write failure
// and exercise the rollback path.
var atomicWriteFile = codexconfig.AtomicWrite

// GenerateConfigFunc writes the codex-home files into stagingHome.
type GenerateConfigFunc func(stagingHome string) error

// requiredFileSet returns the files a published home must contain. auth.json is
// included only when a server token is configured; with an empty token it must
// not exist (config.toml then declares no requires_openai_auth).
func requiredFileSet(requireAuth bool) []string {
	if requireAuth {
		return codexHomeFiles
	}
	return []string{"config.toml", "models_catalog.json"}
}

// CreateStagingHome creates a sibling staging directory for a target codex-home.
// The caller must remove it (defer os.RemoveAll) after PublishStaged so the
// staged auth.json secret never survives.
func CreateStagingHome(targetHome string) (string, error) {
	parent := filepath.Dir(targetHome)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	return os.MkdirTemp(parent, ".codex-home-staging-*")
}

// GenerateAndVerify writes the codex-home files into staging via generate and
// verifies them (parse + permissions) before any publish step. requireAuth
// reflects whether the staged set must carry an auth.json (server token set).
func GenerateAndVerify(staging string, generate GenerateConfigFunc, requireAuth bool) error {
	if err := generate(staging); err != nil {
		return err
	}
	return verifyHome(staging, requireAuth)
}

// PublishStaged replaces the target codex-home's files with the staged ones,
// committing config.toml last. Existing files are copied (not moved) to a
// backout directory so the target never goes absent mid-transaction; on any
// ordinary I/O or verification error the previous files are restored from the
// backout. auth.json is conditional: with requireAuth=false it is not published
// and any stale auth.json in the target is removed within the transaction (so
// Codex never keeps using an old token), with rollback restoring it on failure.
// Crash recovery (the desktop process dying mid-publish) is out of scope for
// this package (Boundary 4).
func PublishStaged(staging, targetHome string, requireAuth bool) error {
	if err := os.MkdirAll(targetHome, 0o755); err != nil {
		return publishFailure(err, "create target codex home failed", false, nil)
	}
	backout, err := os.MkdirTemp(filepath.Dir(targetHome), ".codex-home-backout-*")
	if err != nil {
		return publishFailure(err, "create backout directory failed", false, nil)
	}
	defer os.RemoveAll(backout)

	existed, err := backoutCopy(targetHome, backout)
	if err != nil {
		return publishFailure(err, "backup existing codex home files failed", false, nil)
	}
	if err := replaceAll(staging, targetHome, requireAuth); err != nil {
		return publishFailure(err, "replace codex home files failed", true, restoreFromBackout(targetHome, backout, existed))
	}
	if err := verifyHome(targetHome, requireAuth); err != nil {
		return publishFailure(err, "published codex home failed verification", true, restoreFromBackout(targetHome, backout, existed))
	}
	return nil
}

func publishFailure(cause error, msg string, rollbackAttempted bool, restoreErr error) error {
	e := &Error{
		Kind:    KindConfigPublishFailed,
		Message: msg,
		Details: map[string]any{
			"cause": cause.Error(),
		},
	}
	if rollbackAttempted {
		if restoreErr != nil {
			e.Details["rolledBack"] = false
			e.Details["rollbackError"] = restoreErr.Error()
		} else {
			e.Details["rolledBack"] = true
		}
	}
	return e
}

// backoutCopy copies existing target files (not moves) into backoutDir and
// records which existed, so the target stays present throughout the publish and
// the rollback can remove files that did not exist before.
func backoutCopy(targetHome, backoutDir string) (map[string]bool, error) {
	existed := make(map[string]bool, len(codexHomeFiles))
	for _, name := range codexHomeFiles {
		src := filepath.Join(targetHome, name)
		data, err := os.ReadFile(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		existed[name] = true
		if err := os.WriteFile(filepath.Join(backoutDir, name), data, 0o600); err != nil {
			return nil, err
		}
	}
	return existed, nil
}

func replaceAll(staging, targetHome string, requireAuth bool) error {
	for _, name := range configCommitOrder {
		if name == "auth.json" && !requireAuth {
			// A token-less publish must not leave a stale auth.json behind: codex
			// would keep using an old token. Remove it within the transaction; a
			// rollback restores it from the backout copy.
			if err := os.Remove(filepath.Join(targetHome, name)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove stale %s: %w", name, err)
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(staging, name))
		if err != nil {
			return fmt.Errorf("read staged %s: %w", name, err)
		}
		if err := atomicWriteFile(filepath.Join(targetHome, name), data); err != nil {
			return fmt.Errorf("replace %s: %w", name, err)
		}
	}
	return nil
}

// restoreFromBackout writes back the copied files and deletes files that did
// not exist before the publish.
func restoreFromBackout(targetHome, backoutDir string, existed map[string]bool) error {
	var restoreErr error
	for _, name := range codexHomeFiles {
		target := filepath.Join(targetHome, name)
		if !existed[name] {
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				restoreErr = errors.Join(restoreErr, fmt.Errorf("remove %s: %w", name, err))
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(backoutDir, name))
		if err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("read backout %s: %w", name, err))
			continue
		}
		if err := atomicWriteFile(target, data); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore %s: %w", name, err))
		}
	}
	return restoreErr
}

// verifyHome checks the required files parse and hold the required fields, plus
// permissions: config.toml and auth.json must not be group/world readable
// (auth.json carries the codex auth token). models_catalog.json is public data.
// With requireAuth=false, auth.json must be absent (a stale token file would let
// Codex keep using an old credential).
func verifyHome(home string, requireAuth bool) error {
	for _, name := range requiredFileSet(requireAuth) {
		path := filepath.Join(home, name)
		if err := verifyFile(path); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if name == "models_catalog.json" {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if modeIsPermissive(info.Mode()) {
			return fmt.Errorf("%s has a permissive mode %v", name, info.Mode())
		}
	}
	if !requireAuth {
		if _, err := os.Stat(filepath.Join(home, "auth.json")); err == nil {
			return errors.New("auth.json must be absent when no server token is configured")
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("stat auth.json: %w", err)
		}
	}
	return nil
}

// verifyFile validates one published file. Error text never includes the file
// contents, so a token cannot leak through a failure message.
func verifyFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	switch filepath.Base(path) {
	case "config.toml":
		var doc map[string]any
		if _, err := toml.Decode(string(data), &doc); err != nil {
			return fmt.Errorf("not valid TOML: %w", err)
		}
	case "models_catalog.json":
		var catalog map[string]any
		if err := json.Unmarshal(data, &catalog); err != nil {
			return fmt.Errorf("not valid JSON: %w", err)
		}
	case "auth.json":
		var auth map[string]any
		if err := json.Unmarshal(data, &auth); err != nil {
			return fmt.Errorf("not valid JSON: %w", err)
		}
		if _, ok := auth["openai_api_key"]; !ok {
			return errors.New("missing openai_api_key")
		}
	}
	return nil
}
