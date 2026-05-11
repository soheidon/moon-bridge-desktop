package config

// PluginConfig is the config domain model for the plugin/extension layer.
type PluginConfig struct {
	Extensions map[string]ExtensionSettings
}

// PluginFromGlobalConfig extracts plugin-relevant fields from the global config.
func PluginFromGlobalConfig(cfg *Config) PluginConfig {
	return PluginConfig{
		Extensions: cfg.Extensions,
	}
}
