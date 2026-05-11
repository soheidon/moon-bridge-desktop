package config

// ProviderConfig is the config domain model for the provider layer.
type ProviderConfig struct {
	Providers        map[string]ProviderDef
	Routes           map[string]RouteEntry
	Models           map[string]ModelDef
	DefaultProvider  string
	WebSearchSupport WebSearchSupport
	WebSearchMaxUses int
	TavilyAPIKey     string
	FirecrawlAPIKey  string
	SearchMaxRounds  int
}

// ProviderFromGlobalConfig extracts provider-relevant fields from the global config.
func ProviderFromGlobalConfig(cfg *Config) ProviderConfig {
	return ProviderConfig{
		Providers:        cfg.ProviderDefs,
		Routes:           cfg.Routes,
		Models:           cfg.Models,
		DefaultProvider:  cfg.DefaultModel,
		WebSearchSupport: cfg.WebSearchSupport,
		WebSearchMaxUses: cfg.WebSearchMaxUses,
		TavilyAPIKey:     cfg.TavilyAPIKey,
		FirecrawlAPIKey:  cfg.FirecrawlAPIKey,
		SearchMaxRounds:  cfg.SearchMaxRounds,
	}
}
