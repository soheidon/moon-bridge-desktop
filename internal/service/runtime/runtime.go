// Package runtime provides a snapshot-based runtime that holds the active
// configuration, provider manager, and pricing data. The snapshot is
// updated atomically via an atomic.Pointer, allowing safe concurrent reads
// without locking.
package runtime

import (
	"fmt"
	"sync"
	"sync/atomic"

	"moonbridge/internal/config"
	"moonbridge/internal/service/provider"
	"moonbridge/internal/service/stats"
)

// ConfigSnapshot is an immutable snapshot of the runtime state.
type ConfigSnapshot struct {
	// Config is the resolved runtime configuration.
	Config config.Config

	// ProviderMgr is the fully initialized provider manager.
	ProviderMgr *provider.ProviderManager

	// Pricing maps model identifiers to their pricing details.
	Pricing map[string]stats.ModelPricing
}

// Runtime holds the active ConfigSnapshot and provides atomic access
// and reload capability.
type Runtime struct {
	snapshot        atomic.Pointer[ConfigSnapshot]
	resolver        *provider.CredentialResolver // shared resolver re-injected on Reload
	migrationIssues []provider.CredentialInfo
	mu              sync.Mutex // guards Reload; not needed for Current()
}

// NewRuntime creates a Runtime with the given initial configuration. An
// optional shared CredentialResolver is stored so every Reload rebuilds the
// provider manager with it, keeping the credential status registry fresh
// (statuses are re-recorded at each client-generation pass).
func NewRuntime(cfg config.Config, providerMgr *provider.ProviderManager, pricing map[string]stats.ModelPricing, resolvers ...*provider.CredentialResolver) *Runtime {
	var resolver *provider.CredentialResolver
	if len(resolvers) > 0 {
		resolver = resolvers[0]
	}
	rt := &Runtime{resolver: resolver}
	snapshot := &ConfigSnapshot{
		Config:      cfg,
		ProviderMgr: providerMgr,
		Pricing:     pricing,
	}
	rt.snapshot.Store(snapshot)
	return rt
}

// Current returns the current ConfigSnapshot. The returned pointer is safe
// to use and will not be mutated by future Reload calls.
func (rt *Runtime) Current() *ConfigSnapshot {
	return rt.snapshot.Load()
}

// Resolver returns the shared CredentialResolver injected at construction, or
// nil if none was provided. The provider manager is rebuilt with this same
// resolver on every Reload, so connection probing and client generation must
// share it to keep the credential status registry consistent.
func (rt *Runtime) ClearMigrationIssue(providerID string) {
	if snapshot := rt.snapshot.Load(); snapshot != nil && snapshot.ProviderMgr != nil {
		snapshot.ProviderMgr.ClearMigrationIssue(providerID)
	}
}

func (rt *Runtime) Resolver() *provider.CredentialResolver {
	return rt.resolver
}

func (rt *Runtime) ValidateCandidate(cfg config.Config) error {
	// No resolver: validating a draft must not churn the shared credential
	// registry with the draft's statuses. Only a committed Reload re-records.
	if _, err := buildSnapshot(cfg, "runtime candidate", nil); err != nil {
		return err
	}
	return nil
}

// Reload validates the given config, builds a new ProviderManager, computes
// pricing, and atomically replaces the snapshot. Returns an error if
// validation or ProviderManager construction fails; the existing snapshot
// remains unchanged.
func (rt *Runtime) Reload(cfg config.Config) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	snapshot, err := buildSnapshot(cfg, "runtime reload", rt.resolver)
	if err != nil {
		return err
	}
	rt.snapshot.Store(snapshot)
	return nil
}

func buildSnapshot(cfg config.Config, errorPrefix string, resolver *provider.CredentialResolver) (*ConfigSnapshot, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: config validation: %w", errorPrefix, err)
	}

	providerCfg := config.ProviderFromGlobalConfig(&cfg)
	providerDefs := provider.BuildProviderDefsFromConfig(providerCfg)
	modelRoutes := provider.BuildModelRoutesFromConfig(providerCfg)

	var providerMgr *provider.ProviderManager
	var err error
	if resolver == nil {
		providerMgr, err = provider.NewProviderManager(providerDefs, modelRoutes)
	} else {
		providerMgr, err = provider.NewSecureProviderManager(providerDefs, modelRoutes, resolver)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: provider manager: %w", errorPrefix, err)
	}

	pricing := provider.BuildPricingFromConfig(providerCfg)

	return &ConfigSnapshot{
		Config:      cfg,
		ProviderMgr: providerMgr,
		Pricing:     pricing,
	}, nil
}
