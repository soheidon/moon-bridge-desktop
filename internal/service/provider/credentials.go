package provider

import (
	"os"
	"sync"

	"moonbridge/internal/secretstore"
)

// Credential source / state / error-code values. They are non-secret and match
// the wire strings surfaced by the DeepSeek snapshot.
const (
	SourceStored      = "stored"
	SourceEnvironment = "environment"
	SourceNone        = "none"

	StateAvailable   = "available"
	StateMissing     = "missing"
	StateUnavailable = "unavailable"
	StateUnverified  = "unverified"

	ErrCodeDecryptFailed       = "decrypt_failed"
	ErrCodeMigrationFailed     = "migration_failed"
	ErrCodeUnsupportedPlatform = "unsupported_platform"
)

// CredentialInfo is the runtime credential status for one provider. It is
// derived at client generation and never persisted to the config graph or
// SQLite. The JSON tags shape the non-secret wire form of the registry status
// endpoint.
type CredentialInfo struct {
	ProviderID string `json:"providerId"`
	Source     string `json:"source"`
	State      string `json:"state"`
	ErrorCode  string `json:"errorCode,omitempty"`
}

// CredentialStatusRegistry is a non-persistent runtime registry of provider
// credential states. The provider manager records the state it observes when
// it resolves each provider's key at client generation; snapshot synthesis
// reads it to avoid re-deriving (and re-decrypting) keys.
type CredentialStatusRegistry struct {
	mu sync.RWMutex
	m  map[string]CredentialInfo
}

// NewCredentialStatusRegistry creates an empty registry.
func NewCredentialStatusRegistry() *CredentialStatusRegistry {
	return &CredentialStatusRegistry{m: make(map[string]CredentialInfo)}
}

// Set records the credential status for a provider.
func (r *CredentialStatusRegistry) Set(info CredentialInfo) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[info.ProviderID] = info
}

// Get returns the recorded status for a provider, if any.
func (r *CredentialStatusRegistry) Get(providerID string) (CredentialInfo, bool) {
	if r == nil {
		return CredentialInfo{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	info, ok := r.m[providerID]
	return info, ok
}

// All returns a copy of all recorded statuses.
func (r *CredentialStatusRegistry) All() []CredentialInfo {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CredentialInfo, 0, len(r.m))
	for _, info := range r.m {
		out = append(out, info)
	}
	return out
}

// Clear drops all recorded statuses (e.g. after a reload).
func (r *CredentialStatusRegistry) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m = make(map[string]CredentialInfo)
}

func (r *CredentialResolver) RecordIssue(issue CredentialInfo) {
	if r == nil {
		return
	}
	r.set(issue.ProviderID, issue.Source, issue.State, issue.ErrorCode)
}

func (r *CredentialResolver) ClearStatus(providerID string) {
	if r == nil || r.Registry == nil {
		return
	}
	r.Registry.ClearProvider(providerID)
}

func (r *CredentialStatusRegistry) ClearProvider(providerID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, providerID)
}

// CredentialResolver resolves a provider's effective API key at the
// client-generation boundary. It is the single place where stored ciphertext
// is decrypted: the internal config graph and SQLite carry only ciphertext.
// Every resolution outcome is recorded in the registry so snapshots can report
// available / unavailable / missing without decrypting again.
type CredentialResolver struct {
	Codec     secretstore.SecretCodec
	LookupEnv func(string) (string, bool)
	Registry  *CredentialStatusRegistry
}

// Resolve returns the effective API key for a provider and records the outcome
// in the registry. Empty stored falls back to the configured environment
// variable; stored ciphertext is decrypted only here.
func (r *CredentialResolver) Resolve(providerID, stored, envName string) string {
	return r.ResolveWithIssue(providerID, stored, envName, nil)
}

// ResolveWithIssue refuses a provider whose stored credential migration failed.
// The issue is recorded before returning so every caller observes the same
// stored/unavailable state and no environment fallback can bypass it.
func (r *CredentialResolver) ResolveWithIssue(providerID, stored, envName string, issue *CredentialInfo) string {
	if r == nil {
		return ""
	}
	if issue != nil {
		r.set(providerID, issue.Source, issue.State, issue.ErrorCode)
		return ""
	}
	if stored != "" {
		if r.Codec == nil || !r.Codec.Supported() {
			r.set(providerID, SourceStored, StateUnavailable, ErrCodeUnsupportedPlatform)
			return ""
		}
		if secretstore.IsCiphertext(stored) {
			key, err := r.Codec.Decrypt(stored)
			if err != nil {
				r.set(providerID, SourceStored, StateUnavailable, ErrCodeDecryptFailed)
				return ""
			}
			r.set(providerID, SourceStored, StateAvailable, "")
			return key
		}
		// Legacy plaintext reached the resolver (migration did not run): refuse
		// to start the provider with it.
		r.set(providerID, SourceStored, StateUnavailable, ErrCodeMigrationFailed)
		return ""
	}
	if envName != "" {
		if value, ok := r.lookupEnv(envName); ok && value != "" {
			r.set(providerID, SourceEnvironment, StateAvailable, "")
			return value
		}
	}
	r.set(providerID, SourceNone, StateMissing, "")
	return ""
}

func (r *CredentialResolver) lookupEnv(name string) (string, bool) {
	if r.LookupEnv != nil {
		return r.LookupEnv(name)
	}
	return os.LookupEnv(name)
}

func (r *CredentialResolver) set(providerID, source, state, errorCode string) {
	if r.Registry != nil {
		r.Registry.Set(CredentialInfo{ProviderID: providerID, Source: source, State: state, ErrorCode: errorCode})
	}
}
