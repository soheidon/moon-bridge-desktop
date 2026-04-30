package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// Store is the consumer's interface for database CRUD operations.
// Consumers access the database exclusively through Store — they never
// receive a raw *sql.DB.
//
// Table name isolation is enforced via the Table() method which resolves
// local table names to real names using a consumer-specific prefix.
type Store interface {
	// ConsumerName returns the consumer's unique name.
	ConsumerName() string

	// Dialect returns the provider's dialect for SQL adjustments.
	Dialect() Dialect

	// Table resolves a local table name to the real table name in the database.
	// Returns ErrTableNotRegistered if the name was not declared in Tables().
	Table(localName string) (string, error)

	// ExecContext executes a query without returning rows.
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)

	// QueryContext executes a query that returns rows.
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)

	// QueryRowContext executes a query that returns at most one row.
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row

	// WithTx executes fn inside a database transaction.
	WithTx(ctx context.Context, fn func(Tx) error) error
}

// Tx wraps a database transaction with the same table-name isolation as Store.
type Tx interface {
	Table(localName string) (string, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Name validation pattern: lowercase alphanumeric + underscores.
var namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// boundStore implements Store with table-name prefix isolation.
type boundStore struct {
	consumer   string
	dialect    Dialect
	db         *sql.DB
	tableNames map[string]string // localName → realName
}

var _ Store = (*boundStore)(nil)

func newBoundStore(consumer string, dialect Dialect, db *sql.DB, tables []TableSpec) *boundStore {
	names := make(map[string]string, len(tables))
	for _, t := range tables {
		realName := consumer + "_" + t.Name
		names[t.Name] = realName
	}
	return &boundStore{
		consumer:   consumer,
		dialect:    dialect,
		db:         db,
		tableNames: names,
	}
}

func (s *boundStore) ConsumerName() string { return s.consumer }

func (s *boundStore) Dialect() Dialect { return s.dialect }

func (s *boundStore) Table(localName string) (string, error) {
	realName, ok := s.tableNames[localName]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTableNotRegistered, localName)
	}
	return realName, nil
}

func (s *boundStore) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, query, args...)
}

func (s *boundStore) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, query, args...)
}

func (s *boundStore) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, query, args...)
}

func (s *boundStore) WithTx(ctx context.Context, fn func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	btx := &boundTx{tx: tx, consumer: s.consumer, tableNames: s.tableNames}
	if err := fn(btx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("rollback: %w (original: %v)", rbErr, err)
		}
		return err
	}
	return tx.Commit()
}

// boundTx implements Tx for a single transaction.
type boundTx struct {
	tx         *sql.Tx
	consumer   string
	tableNames map[string]string
}

func (t *boundTx) Table(localName string) (string, error) {
	realName, ok := t.tableNames[localName]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrTableNotRegistered, localName)
	}
	return realName, nil
}

func (t *boundTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

func (t *boundTx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

func (t *boundTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// ValidateConsumerName checks that a consumer name matches the required pattern.
func ValidateConsumerName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: consumer %q", ErrTableNameInvalid, name)
	}
	return nil
}

// ValidateTableName checks that a local table name matches the required pattern.
func ValidateTableName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: table %q", ErrTableNameInvalid, name)
	}
	return nil
}

// ValidateIndexName checks that an index name matches the required pattern.
func ValidateIndexName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: index %q", ErrTableNameInvalid, name)
	}
	return nil
}

// RenderDDL replaces {{table}} and {{index}} placeholders in DDL strings with
// the real names. Returns an error if any placeholder remains after replacement.
func RenderDDL(ddl string, realTable string, realIndex string) (string, error) {
	result := strings.ReplaceAll(ddl, "{{table}}", realTable)
	result = strings.ReplaceAll(result, "{{index}}", realIndex)
	// Check for unclosed placeholders.
	if strings.Contains(result, "{{") {
		return "", fmt.Errorf("DDL contains unresolved placeholder after replacement: %q", result)
	}
	return result, nil
}

// ConsumerTablesValidator validates consumer tables spec.
func ValidateConsumerTables(consumer string, tables []TableSpec) error {
	if err := ValidateConsumerName(consumer); err != nil {
		return err
	}
	if len(tables) == 0 {
		return fmt.Errorf("%w: consumer %q", ErrNoTables, consumer)
	}
	seenNames := make(map[string]bool, len(tables))
	for _, t := range tables {
		if err := ValidateTableName(t.Name); err != nil {
			return err
		}
		if seenNames[t.Name] {
			return fmt.Errorf("duplicate table name %q in consumer %q", t.Name, consumer)
		}
		seenNames[t.Name] = true
		if !strings.Contains(t.Schema, "{{table}}") {
			return fmt.Errorf("table %q schema must contain {{table}} placeholder", t.Name)
		}
		for _, idx := range t.Indexes {
			if err := ValidateIndexName(idx.Name); err != nil {
				return err
			}
			if !strings.Contains(idx.SQL, "{{table}}") {
				return fmt.Errorf("index %q SQL must contain {{table}} placeholder", idx.Name)
			}
			if !strings.Contains(idx.SQL, "{{index}}") {
				return fmt.Errorf("index %q SQL must contain {{index}} placeholder", idx.Name)
			}
		}
	}
	return nil
}
