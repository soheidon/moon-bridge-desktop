package config

// ProxyConfig is the config domain model for the proxy layer.
type ProxyConfig struct {
	ResponseModel            string
	ResponseProviderBaseURL  string
	ResponseProviderAPIKey   string
	AnthropicModel           string
	AnthropicProviderBaseURL string
	AnthropicProviderAPIKey  string
	AnthropicProviderVersion string
}

// ProxyFromGlobalConfig extracts proxy-relevant fields from the global config.
func ProxyFromGlobalConfig(cfg *Config) ProxyConfig {
	return ProxyConfig{
		ResponseModel:            cfg.ResponseProxy.Model,
		ResponseProviderBaseURL:  cfg.ResponseProxy.ProviderBaseURL,
		ResponseProviderAPIKey:   cfg.ResponseProxy.ProviderAPIKey,
		AnthropicModel:           cfg.AnthropicProxy.Model,
		AnthropicProviderBaseURL: cfg.AnthropicProxy.ProviderBaseURL,
		AnthropicProviderAPIKey:  cfg.AnthropicProxy.ProviderAPIKey,
		AnthropicProviderVersion: cfg.AnthropicProxy.ProviderVersion,
	}
}
