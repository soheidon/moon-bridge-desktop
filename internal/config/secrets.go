package config

import (
	"fmt"

	"moonbridge/internal/secretstore"
)

// NormalizeProviderSecrets encrypts any plaintext provider API keys so the
// config graph never carries plaintext. Already-encrypted values and empty
// values pass through unchanged (no double encryption). A nil codec leaves the
// config untouched (tests, or paths that bypass encryption).
func (fc *FileConfig) NormalizeProviderSecrets(codec secretstore.SecretCodec) error {
	if codec == nil {
		return nil
	}
	for key, def := range fc.Providers {
		normalized, err := secretstore.EncryptIfPlaintext(codec, def.APIKey)
		if err != nil {
			return fmt.Errorf("providers.%s.api_key: %w", key, err)
		}
		def.APIKey = normalized
		fc.Providers[key] = def
	}
	return nil
}

// NormalizeProviderSecrets is the Config-graph variant of the same helper, used
// by bootstrap paths that build the graph from a Config (e.g. YAML load).
func (cfg *Config) NormalizeProviderSecrets(codec secretstore.SecretCodec) error {
	if codec == nil {
		return nil
	}
	for key, def := range cfg.ProviderDefs {
		normalized, err := secretstore.EncryptIfPlaintext(codec, def.APIKey)
		if err != nil {
			return fmt.Errorf("providers.%s.api_key: %w", key, err)
		}
		def.APIKey = normalized
		cfg.ProviderDefs[key] = def
	}
	return nil
}
