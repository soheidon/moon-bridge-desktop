package db

// Consumer is the interface for plugins that need database persistence.
// A consumer declares its table schemas and receives a Store for CRUD operations.
type Consumer interface {
	// Name returns a unique identifier used for table namespace isolation.
	Name() string

	// Tables returns the table schemas this consumer requires.
	// Must return at least one table.
	Tables() []TableSpec

	// BindStore is called by the Registry after tables are created.
	// The consumer should store the Store reference for CRUD operations.
	BindStore(Store) error

	// DisablePersistence is called when no Provider is available or when
	// table creation fails. The consumer should disable all persistence.
	DisablePersistence(reason error)
}

// TableSpec describes a single table and its indexes.
type TableSpec struct {
	// Name is the local table name (e.g. "request_metrics").
	// Must match ^[a-z][a-z0-9_]*$.
	Name string

	// Schema is the CREATE TABLE DDL. Use {{table}} as a placeholder for the
	// real table name (e.g. "CREATE TABLE IF NOT EXISTS {{table}} (...)").
	Schema string

	// Indexes are optional index definitions.
	Indexes []IndexSpec
}

// IndexSpec describes a single index.
type IndexSpec struct {
	// Name is a short identifier for the index (e.g. "timestamp").
	// Used to generate the real index name: idx_{consumer}_{table}_{name}.
	// Must match ^[a-z][a-z0-9_]*$.
	Name string

	// SQL is the CREATE INDEX DDL. Use {{table}} for the real table name and
	// {{index}} for the real index name.
	SQL string
}
