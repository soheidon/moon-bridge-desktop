// Package provider manages multiple upstream LLM providers and routes
// requests to the correct provider based on the requested model.
package provider

import (
	"context"

	"moonbridge/internal/protocol/anthropic"
)

// ProviderClient is the interface for upstream provider API clients.
// Replaces *anthropic.Client in ProviderCandidate, ClientForKey, ClientFor,
// and all callers.
//
// Using any for request/response types avoids protocol-specific coupling
// (anthropic.MessageRequest etc.). Provider adapters handle type assertions
// internally.
type ProviderClient interface {
	// CreateMessage sends a synchronous request to the upstream provider.
	CreateMessage(ctx context.Context, req any) (any, error)

	// StreamMessage sends a streaming request to the upstream provider.
	StreamMessage(ctx context.Context, req any) (<-chan any, error)
}

// UnavailableProviderClient preserves the provider's identity while refusing
// all network operations when its credential is missing or unusable.
type UnavailableProviderClient struct {
	Code string
}

func (c *UnavailableProviderClient) CreateMessage(context.Context, any) (any, error) {
	return nil, &CredentialUnavailableError{Code: c.Code}
}

func (c *UnavailableProviderClient) StreamMessage(context.Context, any) (<-chan any, error) {
	return nil, &CredentialUnavailableError{Code: c.Code}
}

// CredentialUnavailableError is intentionally local and contains no secret or
// upstream response body.
type CredentialUnavailableError struct {
	Code string
}

func (e *CredentialUnavailableError) Error() string {
	if e.Code == "" {
		return "credential_unavailable"
	}
	return e.Code
}

// AnthropicClientAccessor is implemented by ProviderClient implementations
// that wrap an *anthropic.Client. Callers that need the concrete client for
// protocol-specific operations (e.g., web search probing) can use this
// interface to access the underlying client.
type AnthropicClientAccessor interface {
	AnthropicClient() *anthropic.Client
}
