package db

import "errors"

// Sentinel errors returned by the persistence layer.
var (
	// ErrNoProvider is returned when Init is called without any registered provider.
	ErrNoProvider = errors.New("no persistence provider registered")

	// ErrMultipleProviders is returned when multiple providers are registered but
	// activeProviderName is empty.
	ErrMultipleProviders = errors.New("multiple persistence providers registered; specify active_provider")

	// ErrProviderNotFound is returned when the specified activeProviderName does not
	// match any registered provider.
	ErrProviderNotFound = errors.New("specified persistence provider not found")

	// ErrNotSupported is returned when an operation is not supported by the backend.
	ErrNotSupported = errors.New("operation not supported by this backend")

	// ErrTableNotRegistered is returned when a consumer tries to look up a table
	// that was not declared in its Tables().
	ErrTableNotRegistered = errors.New("table not registered by this consumer")

	// ErrTableNameInvalid is returned when a consumer name, table name, or index
	// name does not match the required pattern.
	ErrTableNameInvalid = errors.New("name must match ^[a-z][a-z0-9_]*$")

	// ErrNoTables is returned when a consumer declares zero tables.
	ErrNoTables = errors.New("consumer must declare at least one table")
)
