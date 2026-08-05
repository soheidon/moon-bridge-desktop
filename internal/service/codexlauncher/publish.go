package codexlauncher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"moonbridge/internal/service/publishrecovery"
)

// codexHomeFiles are the files a codex-home may contain. auth.json is
// conditional: it is published only when a server token is configured.
var codexHomeFiles = []string{"config.toml", "models_catalog.json", "auth.json"}

// GenerateConfigFunc writes the codex-home files into stagingHome.
type GenerateConfigFunc func(stagingHome string) error

// homePublisher publishes staged codex-home bytes into a target home durably.
// *publishrecovery.Service satisfies it: Publish runs the crash-journaled
// transaction (durable journal + backout, fixed publish order, stale auth
// removal, verification, immediate rollback on ordinary I/O failure). The
// launcher builds the PublishInput from staging bytes and delegates the real
// mutation of the target home to the publisher — it never touches the target
// directly.
type homePublisher interface {
	Publish(ctx context.Context, in publishrecovery.PublishInput) error
}

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
// The caller must remove it (defer os.RemoveAll) after publishing so the staged
// auth.json secret never survives.
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

// publishStaged builds a publishrecovery.PublishInput from the staged bytes and
// delegates the target mutation to pub. It is the shared staging→publish glue;
// production uses the launcher's injected publisher, and tests drive a real
// publishrecovery.Service rooted at a temp recovery dir. auth.json is
// conditional: requireAuth=false passes no auth bytes and the publish
// transaction removes any stale auth.json in the target (so codex never keeps
// an old token), restoring it on rollback.
//
// The returned error maps a publishrecovery failure to a sanitized
// KindConfigPublishFailed whose Details carry only logical, non-confidential
// values (cause kind, rolledBack) — never paths, tokens, hashes, or raw bytes.
func publishStaged(ctx context.Context, pub homePublisher, staging, targetHome string, requireAuth bool) error {
	// publishrecovery is the sole authority for target CODEX_HOME mutation;
	// codexlauncher never creates or touches the target directory directly.
	in, err := publishInputFromStaging(staging, targetHome, requireAuth)
	if err != nil {
		return mapPublishError(err)
	}
	err = pub.Publish(ctx, in)
	if err == nil {
		return nil
	}
	return mapPublishError(err)
}

// publishInputFromStaging reads the publish target bytes from staging and
// assembles the PublishInput for publishrecovery. auth.json is included only
// when requireAuth is true; otherwise the publish expects auth to be absent.
func publishInputFromStaging(staging, targetHome string, requireAuth bool) (publishrecovery.PublishInput, error) {
	catalog, err := os.ReadFile(filepath.Join(staging, "models_catalog.json"))
	if err != nil {
		return publishrecovery.PublishInput{}, fmt.Errorf("read staged models_catalog.json: %w", err)
	}
	config, err := os.ReadFile(filepath.Join(staging, "config.toml"))
	if err != nil {
		return publishrecovery.PublishInput{}, fmt.Errorf("read staged config.toml: %w", err)
	}
	in := publishrecovery.PublishInput{
		TargetHome:    targetHome,
		ModelsCatalog: catalog,
		ConfigTOML:    config,
		AuthRequired:  requireAuth,
	}
	if requireAuth {
		auth, err := os.ReadFile(filepath.Join(staging, "auth.json"))
		if err != nil {
			return publishrecovery.PublishInput{}, fmt.Errorf("read staged auth.json: %w", err)
		}
		in.AuthJSON = auth
	}
	return in, nil
}

// mapPublishError converts a publish failure into a sanitized launcher *Error.
// Details never carry paths, tokens, hashes, or raw error strings: the cause is
// a publishrecovery kind string (non-confidential) when available.
//
// rolledBack is only set when the outcome is unambiguous:
//   - KindRollbackFailed → rolledBack=false (rollback attempted but failed).
//   - Any other kind → rolledBack omitted (unfinished journal, init failure,
//     or pre-prepare failure means no rollback was attempted or the outcome
//     cannot be determined from the error kind alone).
func mapPublishError(cause error) error {
	var pr *publishrecovery.Error
	details := map[string]any{}
	if errors.As(cause, &pr) {
		details["cause"] = string(pr.Kind)
		if pr.Kind == publishrecovery.KindRollbackFailed {
			details["rolledBack"] = false
			details["rollbackError"] = "rollback did not complete"
		}
	}
	return &Error{Kind: KindConfigPublishFailed, Message: "publish codex home files failed", Details: details}
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
