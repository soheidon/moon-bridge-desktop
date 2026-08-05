// Package codexconfig manages the user's real Codex config.toml: path
// resolution, targeted edits that preserve comments and ordering, atomic
// updates with backups, and restore from those backups.
package codexconfig

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type ErrorKind string

const (
	KindNotFound         ErrorKind = "codex_config_not_found"
	KindParseFailed      ErrorKind = "codex_config_parse_failed"
	KindValidationFailed ErrorKind = "codex_config_validation_failed"
	KindEditUnsupported  ErrorKind = "codex_config_edit_unsupported"
	KindUpdateFailed     ErrorKind = "codex_config_update_failed"
	KindVerifyFailed     ErrorKind = "codex_config_verify_failed"
	KindBackupFailed     ErrorKind = "codex_config_backup_failed"
	KindRestoreFailed    ErrorKind = "codex_config_restore_failed"
	KindNoBackups        ErrorKind = "codex_config_no_backups"
)

// Error is a typed failure the App binding maps to CommandError codes.
type Error struct {
	Kind    ErrorKind
	Message string
	Field   *string
	Details map[string]any
}

func (e *Error) Error() string { return e.Message }

func parseFailed(msg string) *Error     { return &Error{Kind: KindParseFailed, Message: msg} }
func editUnsupported(msg string) *Error { return &Error{Kind: KindEditUnsupported, Message: msg} }

// atomicWrite is a seam so tests can force a verify-failure rollback.
var atomicWrite = AtomicWrite

func fieldError(kind ErrorKind, field, msg string) *Error {
	e := &Error{Kind: kind, Message: msg}
	if field != "" {
		e.Field = &field
	}
	return e
}

// Snapshot is the readable state of the user's Codex config.
type Snapshot struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	Model         string `json:"model,omitempty"`
	ModelProvider string `json:"modelProvider,omitempty"`
	BaseURL       string `json:"baseUrl,omitempty"`
}

// Input is the set of fields to update. Empty / nil fields are left alone.
type Input struct {
	Model                string `json:"model,omitempty"`
	ModelProvider        string `json:"modelProvider,omitempty"`
	BaseURL              string `json:"baseUrl,omitempty"`
	ModelContextWindow   *int64 `json:"modelContextWindow,omitempty"`
	ModelMaxOutputTokens *int64 `json:"modelMaxOutputTokens,omitempty"`
}

// BackupInfo describes one codex-config backup.
type BackupInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"`
}

// Options controls path resolution for tests. All zero values resolve from the
// environment like the old Tauri implementation.
type Options struct {
	Home      string // codex home override; "" = CODEX_HOME or %USERPROFILE%/.codex
	Env       func(string) string
	BackupDir string // absolute backup root; "" = %LOCALAPPDATA%\Moon Bridge\backups\codex-config
}

func New(opts Options) *Service {
	if opts.Env == nil {
		opts.Env = os.Getenv
	}
	return &Service{opts: opts}
}

type Service struct {
	opts Options
}

func (s *Service) ResolvePath() (string, error) {
	home := s.opts.Home
	if home == "" {
		home = s.opts.Env("CODEX_HOME")
	}
	if home == "" {
		base := s.opts.Env("USERPROFILE")
		if base == "" {
			base = s.opts.Env("HOME")
		}
		if base == "" {
			return "", fmt.Errorf("codex home is unavailable")
		}
		home = filepath.Join(base, ".codex")
	}
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("codex home must be absolute")
	}
	return filepath.Join(home, "config.toml"), nil
}

func (s *Service) backupDir() (string, error) {
	if s.opts.BackupDir != "" {
		return s.opts.BackupDir, nil
	}
	return defaultBackupDir(s.opts.Env)
}

// Load reads the current config. A missing file is not an error: the snapshot
// carries Exists=false. A present-but-invalid file is codex_config_parse_failed.
func (s *Service) Load(ctx context.Context) (Snapshot, error) {
	path, err := s.ResolvePath()
	if err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{Path: path, Exists: false}, nil
		}
		return Snapshot{}, err
	}
	return decodeSnapshot(path, data)
}

// Update edits only the target fields, backs up the original, writes
// atomically, verifies, and rolls back on verify failure.
func (s *Service) Update(ctx context.Context, input Input) (Snapshot, error) {
	path, err := s.ResolvePath()
	if err != nil {
		return Snapshot{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Snapshot{}, &Error{Kind: KindNotFound, Message: "codex config not found"}
		}
		return Snapshot{}, err
	}
	fields, err := fieldsFromInput(input)
	if err != nil {
		return Snapshot{}, err
	}
	newData, changed, err := Apply(data, fields)
	if err != nil {
		return Snapshot{}, err
	}
	if !changed {
		return s.Load(ctx)
	}
	dir, err := s.backupDir()
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := CreateBackup(dir, data); err != nil {
		return Snapshot{}, &Error{Kind: KindBackupFailed, Message: "create backup failed"}
	}
	if err := atomicWrite(path, newData); err != nil {
		return Snapshot{}, &Error{Kind: KindUpdateFailed, Message: "atomic update failed"}
	}
	if err := verifyFile(path, fields); err != nil {
		_ = atomicWrite(path, data)
		return Snapshot{}, &Error{Kind: KindVerifyFailed, Message: "post-update verification failed"}
	}
	return s.Load(ctx)
}

// Restore applies the backup with the given ID. The current config is first
// saved as a pre-restore backup so a failed restore can be rolled back.
func (s *Service) Restore(ctx context.Context, id string) (Snapshot, error) {
	dir, err := s.backupDir()
	if err != nil {
		return Snapshot{}, err
	}
	backups, err := ListBackups(dir)
	if err != nil {
		return Snapshot{}, err
	}
	if len(backups) == 0 {
		return Snapshot{}, &Error{Kind: KindNoBackups, Message: "no codex config backups"}
	}
	var chosen *BackupInfo
	for i := range backups {
		if backups[i].ID == id {
			chosen = &backups[i]
			break
		}
	}
	if chosen == nil {
		return Snapshot{}, &Error{Kind: KindRestoreFailed, Message: "backup not found"}
	}
	resolved, err := ResolveBackupPath(dir, chosen.ID)
	if err != nil {
		return Snapshot{}, &Error{Kind: KindRestoreFailed, Message: "invalid backup id"}
	}
	backupData, err := os.ReadFile(resolved)
	if err != nil {
		return Snapshot{}, &Error{Kind: KindRestoreFailed, Message: "read backup failed"}
	}
	path, err := s.ResolvePath()
	if err != nil {
		return Snapshot{}, err
	}
	var preRestore []byte
	if cur, err := os.ReadFile(path); err == nil {
		if _, err := CreateBackup(dir, cur); err != nil {
			return Snapshot{}, &Error{Kind: KindBackupFailed, Message: "create pre-restore backup failed"}
		}
		preRestore = cur
	} else if !os.IsNotExist(err) {
		return Snapshot{}, err
	}
	if err := atomicWrite(path, backupData); err != nil {
		return Snapshot{}, &Error{Kind: KindRestoreFailed, Message: "restore failed"}
	}
	if err := fileParsesAsTOML(path); err != nil {
		if preRestore != nil {
			_ = atomicWrite(path, preRestore)
		} else {
			_ = os.Remove(path)
		}
		return Snapshot{}, &Error{Kind: KindRestoreFailed, Message: "restored config failed to parse"}
	}
	return s.Load(ctx)
}

// ListBackups returns backups newest-first. A missing backup directory is an
// empty list, not an error.
func (s *Service) ListBackups(ctx context.Context) ([]BackupInfo, error) {
	dir, err := s.backupDir()
	if err != nil {
		return nil, err
	}
	return ListBackups(dir)
}

func fieldsFromInput(input Input) ([]Field, error) {
	var fields []Field
	if input.Model != "" {
		fields = append(fields, Field{Key: "model", Value: input.Model})
	}
	if input.ModelProvider != "" {
		fields = append(fields, Field{Key: "model_provider", Value: input.ModelProvider})
	}
	if input.BaseURL != "" {
		u, err := url.Parse(input.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fieldError(KindValidationFailed, "baseUrl", "base url must be an http(s) URL")
		}
		fields = append(fields, Field{Key: "base_url", Table: []string{"model_providers", "moonbridge"}, Value: input.BaseURL})
	}
	if input.ModelContextWindow != nil {
		if *input.ModelContextWindow <= 0 {
			return nil, fieldError(KindValidationFailed, "modelContextWindow", "model context window must be positive")
		}
		fields = append(fields, Field{Key: "model_context_window", Value: *input.ModelContextWindow})
	}
	if input.ModelMaxOutputTokens != nil {
		if *input.ModelMaxOutputTokens <= 0 {
			return nil, fieldError(KindValidationFailed, "modelMaxOutputTokens", "model max output tokens must be positive")
		}
		fields = append(fields, Field{Key: "model_max_output_tokens", Value: *input.ModelMaxOutputTokens})
	}
	if len(fields) == 0 {
		return nil, fieldError(KindValidationFailed, "", "no update fields provided")
	}
	return fields, nil
}

func verifyFields(data []byte, fields []Field) error {
	var decoded map[string]any
	if _, err := toml.Decode(string(data), &decoded); err != nil {
		return err
	}
	for _, f := range fields {
		cur, ok := lookup(decoded, f.Table, f.Key)
		if !ok || !valuesEqual(cur, f.Value) {
			return fmt.Errorf("field %s not applied", f.Key)
		}
	}
	return nil
}

// verifyFile re-parses the file on disk and checks the target fields match, per
// plan §11 ("置換後ファイルを再解析").
func verifyFile(path string, fields []Field) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return verifyFields(data, fields)
}

func fileParsesAsTOML(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var decoded map[string]any
	_, err = toml.Decode(string(data), &decoded)
	return err
}

func decodeSnapshot(path string, data []byte) (Snapshot, error) {
	var cfg struct {
		Model          string `toml:"model"`
		ModelProvider  string `toml:"model_provider"`
		ModelProviders map[string]struct {
			BaseURL string `toml:"base_url"`
		} `toml:"model_providers"`
	}
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return Snapshot{}, parseFailed("config TOML parse failed")
	}
	return Snapshot{
		Path:          path,
		Exists:        true,
		Model:         cfg.Model,
		ModelProvider: cfg.ModelProvider,
		BaseURL:       cfg.ModelProviders["moonbridge"].BaseURL,
	}, nil
}
