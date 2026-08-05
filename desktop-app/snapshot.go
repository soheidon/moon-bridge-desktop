package main

import (
	"context"
	"errors"
	"time"

	"moonbridge/internal/service/codexconfig"
	"moonbridge/internal/service/codexlauncher"
	"moonbridge/internal/service/deepseek"
)

// DesktopCommandResult is the single envelope for the DeepSeek / Codex config /
// Codex launcher bindings. OK=true → read Value; OK=false → Value is nil and
// Error is read (safe partial-success info rides in Error.Details, never in
// Value).
type DesktopCommandResult struct {
	OK    bool             `json:"ok"`
	Value *DesktopSnapshot `json:"value,omitempty"`
	Error *CommandError    `json:"error,omitempty"`
}

type DesktopSnapshot struct {
	Codex    *CodexState          `json:"codex,omitempty"`
	Config   *CodexConfigSnapshot `json:"config,omitempty"`
	DeepSeek *DeepSeekSnapshot    `json:"deepseek,omitempty"`
	Backups  []CodexBackupInfo    `json:"backups,omitempty"`
}

// CodexStatus is the terminal-session lifecycle status.
type CodexStatus string

// CodexState describes one codex terminal session. PID is the PowerShell
// (terminal) process, not the codex binary; ExitCode is likewise the
// terminal's exit code.
type CodexState struct {
	Status     CodexStatus `json:"status"` // idle|starting|running|stopping|stopped|error
	PID        int         `json:"pid"`
	CodexHome  string      `json:"codexHome"`
	StartedAt  time.Time   `json:"startedAt,omitempty"`
	StoppedAt  time.Time   `json:"stoppedAt,omitempty"`
	ExitCode   *int        `json:"exitCode,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
	Error      string      `json:"error,omitempty"`
}

type CodexConfigSnapshot struct {
	Path          string `json:"path"`
	Exists        bool   `json:"exists"`
	Model         string `json:"model,omitempty"`
	ModelProvider string `json:"modelProvider,omitempty"`
	BaseURL       string `json:"baseUrl,omitempty"`
}

// DeepSeekSnapshot is the old DeepSeekStatus-compatible shape plus per-model
// config. APIKeyMasked is always a mask form; plaintext never surfaces here.
type DeepSeekSnapshot struct {
	GatewayRunning                bool                `json:"gatewayRunning"`
	ProviderExists                bool                `json:"providerExists"`
	APIKeySet                     bool                `json:"apiKeySet"`
	APIKeyMasked                  string              `json:"apiKeyMasked,omitempty"`
	Configured                    bool                `json:"configured"`
	Active                        bool                `json:"active"`
	SelectedModel                 string              `json:"selectedModel,omitempty"`
	DefaultModel                  string              `json:"defaultModel"`
	ReasoningEffort               string              `json:"reasoningEffort"`
	ReasoningExplicitlyConfigured bool                `json:"reasoningExplicitlyConfigured"`
	AllowedReasoningEfforts       []string            `json:"allowedReasoningEfforts"`
	RouteAlias                    string              `json:"routeAlias"`
	Pro                           DeepSeekModelConfig `json:"pro"`
	Flash                         DeepSeekModelConfig `json:"flash"`
}

type DeepSeekModelConfig struct {
	ModelID   string   `json:"modelId"`
	Reasoning string   `json:"reasoning"`
	Supported []string `json:"supported"`
}

type CodexBackupInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"createdAt"`
	Size      int64     `json:"size"`
}

// ---- envelope constructors ----

func okDesktop(value *DesktopSnapshot) DesktopCommandResult {
	return DesktopCommandResult{OK: true, Value: value}
}

func errDesktop(operation, stage, code, message string, retryable bool) DesktopCommandResult {
	return DesktopCommandResult{
		OK: false,
		Error: &CommandError{
			Operation: operation,
			Stage:     stage,
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	}
}

func hostClosed(operation string) DesktopCommandResult {
	return errDesktop(operation, "host", "codex_host_closed", "desktop host is shut down", false)
}

// ---- snapshot mappers ----

func desktopCodexState(st codexlauncher.State) *CodexState {
	return &CodexState{
		Status:     CodexStatus(st.Status),
		PID:        st.PID,
		CodexHome:  st.CodexHome,
		StartedAt:  st.StartedAt,
		StoppedAt:  st.StoppedAt,
		ExitCode:   st.ExitCode,
		StopReason: string(st.StopReason),
		Error:      st.Error,
	}
}

func desktopCodexConfig(s codexconfig.Snapshot) *CodexConfigSnapshot {
	return &CodexConfigSnapshot{
		Path:          s.Path,
		Exists:        s.Exists,
		Model:         s.Model,
		ModelProvider: s.ModelProvider,
		BaseURL:       s.BaseURL,
	}
}

func desktopDeepSeek(s *deepseek.Snapshot) *DeepSeekSnapshot {
	if s == nil {
		return nil
	}
	return &DeepSeekSnapshot{
		GatewayRunning:                s.GatewayRunning,
		ProviderExists:                s.ProviderExists,
		APIKeySet:                     s.APIKeySet,
		APIKeyMasked:                  s.APIKeyMasked,
		Configured:                    s.Configured,
		Active:                        s.Active,
		SelectedModel:                 s.SelectedModel,
		DefaultModel:                  s.DefaultModel,
		ReasoningEffort:               s.ReasoningEffort,
		ReasoningExplicitlyConfigured: s.ReasoningExplicitlyConfigured,
		AllowedReasoningEfforts:       s.AllowedReasoningEfforts,
		RouteAlias:                    s.RouteAlias,
		Pro:                           desktopDeepSeekModel(s.Pro),
		Flash:                         desktopDeepSeekModel(s.Flash),
	}
}

func desktopDeepSeekModel(m deepseek.ModelConfig) DeepSeekModelConfig {
	return DeepSeekModelConfig{ModelID: m.ModelID, Reasoning: m.Reasoning, Supported: m.Supported}
}

func desktopBackups(bs []codexconfig.BackupInfo) []CodexBackupInfo {
	out := make([]CodexBackupInfo, 0, len(bs))
	for _, b := range bs {
		out = append(out, CodexBackupInfo{
			ID:        b.ID,
			Name:      b.Name,
			Path:      b.Path,
			CreatedAt: b.CreatedAt,
			Size:      b.Size,
		})
	}
	return out
}

// ---- error conversion ----

// deepSeekError maps a deepseek.ServiceError to the CommandError envelope.
// defaultCode is the fallback for the kind classes without a dedicated code
// (e.g. deepseek_load_failed for Load, deepseek_save_failed for Save).
func deepSeekError(operation, stage, defaultCode string, err error) DesktopCommandResult {
	var se *deepseek.ServiceError
	if !errors.As(err, &se) {
		return errDesktop(operation, stage, defaultCode, "deepseek operation failed", true)
	}
	code := defaultCode
	switch se.Kind {
	case deepseek.ServiceErrorKindInvalidInput:
		code = "deepseek_validate_failed"
	case deepseek.ServiceErrorKindAPIKeyRequired:
		code = "deepseek_api_key_required"
	case deepseek.ServiceErrorKindSaveRejected:
		code = "deepseek_save_failed"
	case deepseek.ServiceErrorKindRevisionConflictExceeded:
		code = "deepseek_save_failed"
	case deepseek.ServiceErrorKindVerifyFailed:
		code = "deepseek_save_failed"
	}
	msg := se.Message
	if msg == "" {
		msg = "deepseek operation failed"
	}
	e := &CommandError{
		Operation:       operation,
		Stage:           stage,
		Code:            code,
		Message:         msg,
		Retryable:       se.Retryable,
		MutationStarted: se.MutationStarted,
	}
	if se.Field != nil {
		f := *se.Field
		e.Field = &f
	}
	if len(se.Details) > 0 {
		e.Details = se.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// codexConfigError maps a codexconfig.Error to the CommandError envelope.
func codexConfigError(operation, stage string, err error) DesktopCommandResult {
	var ce *codexconfig.Error
	if !errors.As(err, &ce) {
		return errDesktop(operation, stage, "codex_config_update_failed", "codex config operation failed", true)
	}
	code := "codex_config_update_failed"
	switch ce.Kind {
	case codexconfig.KindNotFound:
		code = "codex_config_not_found"
	case codexconfig.KindParseFailed:
		code = "codex_config_parse_failed"
	case codexconfig.KindValidationFailed:
		code = "codex_config_validation_failed"
	case codexconfig.KindEditUnsupported:
		code = "codex_config_edit_unsupported"
	case codexconfig.KindUpdateFailed:
		code = "codex_config_update_failed"
	case codexconfig.KindVerifyFailed:
		code = "codex_config_verify_failed"
	case codexconfig.KindBackupFailed:
		code = "codex_config_backup_failed"
	case codexconfig.KindRestoreFailed:
		code = "codex_config_restore_failed"
	case codexconfig.KindNoBackups:
		code = "codex_config_no_backups"
	}
	msg := ce.Message
	if msg == "" {
		msg = "codex config operation failed"
	}
	e := &CommandError{Operation: operation, Stage: stage, Code: code, Message: msg}
	if ce.Field != nil {
		f := *ce.Field
		e.Field = &f
	}
	if len(ce.Details) > 0 {
		e.Details = ce.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// codexError maps a codexlauncher.Error to the CommandError envelope. A
// cancellation propagated from the run-scoped context means the host shut down
// mid-operation (the launcher already cleaned up or owns the run).
func codexError(operation, stage string, err error) DesktopCommandResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return hostClosed(operation)
	}
	var le *codexlauncher.Error
	if !errors.As(err, &le) {
		return errDesktop(operation, stage, "codex_start_failed", "codex operation failed", true)
	}
	code := "codex_start_failed"
	switch le.Kind {
	case codexlauncher.KindNotFound:
		code = "codex_not_found"
	case codexlauncher.KindInvalidExecutable:
		code = "codex_invalid_executable"
	case codexlauncher.KindVersionProbeFailed:
		code = "codex_version_probe_failed"
	case codexlauncher.KindRouteNotFound:
		code = "codex_route_not_found"
	case codexlauncher.KindConfigGenerationFailed:
		code = "codex_config_generation_failed"
	case codexlauncher.KindConfigPublishFailed:
		code = "codex_config_publish_failed"
	case codexlauncher.KindAlreadyRunning:
		code = "codex_already_running"
	case codexlauncher.KindStartFailed:
		code = "codex_start_failed"
	case codexlauncher.KindProjectInvalid:
		code = "codex_project_invalid"
	case codexlauncher.KindProjectNotFound:
		code = "codex_project_not_found"
	case codexlauncher.KindProjectNotDirectory:
		code = "codex_project_not_directory"
	case codexlauncher.KindStopFailed:
		code = "codex_stop_failed"
	}
	msg := le.Message
	if msg == "" {
		msg = "codex operation failed"
	}
	retryable := code != "codex_already_running" && code != "codex_stop_failed"
	e := &CommandError{Operation: operation, Stage: stage, Code: code, Message: msg, Retryable: retryable}
	if len(le.Details) > 0 {
		e.Details = le.Details
	}
	return DesktopCommandResult{OK: false, Error: e}
}

// deepSeekGatewayNotRunning is the no-session error for DeepSeek read/write.
func deepSeekGatewayNotRunning(operation string) DesktopCommandResult {
	return errDesktop(operation, "gateway_check", "deepseek_gateway_not_running", "gateway is not running", false)
}
