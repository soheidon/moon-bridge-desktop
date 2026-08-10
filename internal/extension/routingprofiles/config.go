// Package routingprofiles registers the routing_profiles extension name so the
// config graph can persist the Codex routing profile table (Sol/Terra/Luna slot
// assignments) as a global-scope extension resource. The config is opaque
// application data: there is no plugin runtime behavior and no typed config
// validation, so the spec carries no Factory or Validate.
package routingprofiles

import "moonbridge/internal/config"

const ExtensionName = "routing_profiles"

func ConfigSpecs() []config.ExtensionConfigSpec {
	return []config.ExtensionConfigSpec{{
		Name:   ExtensionName,
		Scopes: []config.ExtensionScope{config.ExtensionScopeGlobal},
	}}
}
