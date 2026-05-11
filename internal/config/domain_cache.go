package config

// CacheFromGlobalConfig extracts cache-relevant fields from the global config.
// Returns the existing foundation CacheConfig type.
func CacheFromGlobalConfig(cfg *Config) CacheConfig {
	return cfg.Cache
}
