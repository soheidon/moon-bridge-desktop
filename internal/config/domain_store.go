package config

// StoreConfig is the config domain model for the persistence/store layer.
type StoreConfig struct {
	ActiveProvider string
}

// StoreFromGlobalConfig extracts store-relevant fields from the global config.
// Temporarily accepts *Config; will become *Config after foundation/config moves.
func StoreFromGlobalConfig(cfg *Config) StoreConfig {
	return StoreConfig{
		ActiveProvider: cfg.Persistence.ActiveProvider,
	}
}
