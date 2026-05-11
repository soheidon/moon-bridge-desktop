package config

// PersistenceFromGlobalConfig extracts persistence-relevant fields from the global config.
// Returns the existing foundation PersistenceConfig type.
func PersistenceFromGlobalConfig(cfg *Config) PersistenceConfig {
	return cfg.Persistence
}
