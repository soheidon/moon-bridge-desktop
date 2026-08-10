package deepseek

import (
	"context"
	"errors"
	"testing"

	"moonbridge/internal/service/configgraph"
	"moonbridge/internal/service/provider"
)

// A configured stored key, while running, is graph-derived as stored/available.
// The registry overlay must downgrade it when the shared resolver actually
// failed to decrypt it (e.g. migrated on another Windows user).
func TestLoadOverridesStoredStatusWithRegistryFailure(t *testing.T) {
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": apiKeyMasked, "base_url": BaseURL},
	}))
	fake.revision = 1
	fake.credentialStatus = []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceStored, State: provider.StateUnavailable, ErrorCode: provider.ErrCodeDecryptFailed},
	}
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnavailable || snap.CredentialErrorCode != provider.ErrCodeDecryptFailed {
		t.Fatalf("credential = %q/%q/%q, want stored/unavailable/decrypt_failed",
			snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

func TestLoadOverridesStoredStatusWithMigrationFailure(t *testing.T) {
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": apiKeyMasked, "base_url": BaseURL},
	}))
	fake.revision = 1
	fake.credentialStatus = []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceStored, State: provider.StateUnavailable, ErrorCode: provider.ErrCodeMigrationFailed},
	}
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnavailable || snap.CredentialErrorCode != provider.ErrCodeMigrationFailed {
		t.Fatalf("credential = %q/%q/%q, want stored/unavailable/migration_failed",
			snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

func TestLoadRegistryEnvironmentAvailable(t *testing.T) {
	t.Setenv("MB_TEST_KEY", "sk-env-key-12345")
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"base_url": BaseURL, "api_key": "", "api_key_env": "MB_TEST_KEY"},
	}))
	fake.revision = 1
	fake.credentialStatus = []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceEnvironment, State: provider.StateAvailable},
	}
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceEnvironment || snap.CredentialState != provider.StateAvailable || snap.CredentialErrorCode != "" {
		t.Fatalf("credential = %q/%q/%q, want environment/available", snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

func TestLoadRegistryMissing(t *testing.T) {
	t.Setenv("MB_TEST_KEY", "")
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"base_url": BaseURL, "api_key": "", "api_key_env": "MB_TEST_KEY"},
	}))
	fake.revision = 1
	fake.credentialStatus = []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceNone, State: provider.StateMissing},
	}
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceNone || snap.CredentialState != provider.StateMissing {
		t.Fatalf("credential = %q/%q, want none/missing", snap.CredentialSource, snap.CredentialState)
	}
}

// No registry entry for the provider: the graph-derived running state is kept
// (a configured stored key reports available until the resolver says otherwise).
func TestLoadEmptyRegistryFallsBackToGraph(t *testing.T) {
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": apiKeyMasked, "base_url": BaseURL},
	}))
	fake.revision = 1
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateAvailable {
		t.Fatalf("credential = %q/%q, want stored/available (graph fallback)", snap.CredentialSource, snap.CredentialState)
	}
}

func TestLoadStatusFetchErrorFallsBackToGraph(t *testing.T) {
	fake := newFakeAPI()
	fake.resources = append(fake.resources, cloneResource(configgraph.Resource{
		Kind: configgraph.ResourceProvider, ID: ProviderID,
		Value: map[string]any{"api_key": apiKeyMasked, "base_url": BaseURL},
	}))
	fake.revision = 1
	fake.statusErr = errors.New("registry unavailable")
	svc := NewService(fake)

	snap, err := svc.Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateAvailable {
		t.Fatalf("credential = %q/%q, want stored/available despite status fetch error", snap.CredentialSource, snap.CredentialState)
	}
}

func TestApplyMigrationIssuesOverridesUnverified(t *testing.T) {
	snap := SnapshotFromGraph(graphWithProviderAPIKey("sk-legacy-plaintext-12345"), false)
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnverified {
		t.Fatalf("precondition credential = %q/%q, want stored/unverified", snap.CredentialSource, snap.CredentialState)
	}
	issues := []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceStored, State: provider.StateUnavailable, ErrorCode: provider.ErrCodeMigrationFailed},
	}
	ApplyMigrationIssues(&snap, issues)
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnavailable || snap.CredentialErrorCode != provider.ErrCodeMigrationFailed {
		t.Fatalf("credential = %q/%q/%q, want stored/unavailable/migration_failed",
			snap.CredentialSource, snap.CredentialState, snap.CredentialErrorCode)
	}
}

func TestApplyMigrationIssuesUnsupportedPlatform(t *testing.T) {
	snap := SnapshotFromGraph(graphWithProviderAPIKey("sk-legacy-plaintext-12345"), false)
	issues := []provider.CredentialInfo{
		{ProviderID: ProviderID, Source: provider.SourceStored, State: provider.StateUnavailable, ErrorCode: provider.ErrCodeUnsupportedPlatform},
	}
	ApplyMigrationIssues(&snap, issues)
	if snap.CredentialErrorCode != provider.ErrCodeUnsupportedPlatform {
		t.Fatalf("credential error code = %q, want unsupported_platform", snap.CredentialErrorCode)
	}
}

func TestApplyMigrationIssuesIgnoresOtherProviders(t *testing.T) {
	snap := SnapshotFromGraph(graphWithProviderAPIKey("sk-legacy-plaintext-12345"), false)
	issues := []provider.CredentialInfo{
		{ProviderID: "openai", Source: provider.SourceStored, State: provider.StateUnavailable, ErrorCode: provider.ErrCodeMigrationFailed},
	}
	ApplyMigrationIssues(&snap, issues)
	if snap.CredentialSource != provider.SourceStored || snap.CredentialState != provider.StateUnverified {
		t.Fatalf("credential = %q/%q, want stored/unverified (other provider issue ignored)",
			snap.CredentialSource, snap.CredentialState)
	}
}
