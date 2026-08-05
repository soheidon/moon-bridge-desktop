package publishrecovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"moonbridge/internal/service/codexconfig"
)

const (
	// journalFileName is the journal file name inside RecoveryDir.
	journalFileName = "codex-home-publish-journal.json"
	// transactionsDirName is the backout transaction root inside RecoveryDir.
	transactionsDirName = "publish-transactions"
)

// Options configures the journal Store.
type Options struct {
	// RecoveryDir is %LOCALAPPDATA%\Moon Bridge\recovery. It must be absolute:
	// the journal is re-discovered by path after a crash, never by cwd.
	RecoveryDir string
}

// Dependencies are the filesystem/time seams. They are injected (never package
// globals) so parallel tests share no mutable state and a fault can be scoped to
// a single Store.
type Dependencies struct {
	AtomicWrite func(path string, data []byte) error
	Remove      func(path string) error
	RemoveAll   func(path string) error
	// DurableRemove removes a file and then makes the removal durable: on Unix by
	// fsyncing the parent directory, on Windows by relying on DeleteFile success
	// as the usable boundary — there is no portable parent-directory sync API and
	// full metadata persistence across power loss is not guaranteed. A crash
	// window against a stale auth.json deletion is judged against this seam, so it
	// must be distinct from a plain Remove.
	DurableRemove func(path string) error
	Now           func() time.Time
	NewID         func() string
	Fault         FaultInjector
}

// Store is a serializing flat-JSON store for the publish journal. Read-modify-
// write is serialized by an internal mutex; persistence goes through the
// injected AtomicWrite dependency so two writers never race and an invalid write
// leaves the existing file untouched. recoveryDir is kept so backout creation and
// reads can verify, level by level, that the recovery root and every directory
// they descend into are real directories and not symlinks/junctions.
type Store struct {
	mu          sync.Mutex
	recoveryDir string
	journalPath string
	txRoot      string
	deps        Dependencies
}

// normalizeDependencies fills every unset seam with its production default. It
// is the single place dependency defaults are resolved: NewStore normalizes
// once and keeps the result in the Store, and Service shares the Store's
// normalized value (transaction.go New). A zero-value Dependencies therefore
// never reaches an operation as a nil function.
func normalizeDependencies(deps Dependencies) Dependencies {
	if deps.AtomicWrite == nil {
		deps.AtomicWrite = codexconfig.AtomicWrite
	}
	if deps.Remove == nil {
		deps.Remove = os.Remove
	}
	if deps.RemoveAll == nil {
		deps.RemoveAll = os.RemoveAll
	}
	if deps.DurableRemove == nil {
		deps.DurableRemove = durableRemove
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.NewID == nil {
		deps.NewID = func() string { return uuid.NewString() }
	}
	if deps.Fault == nil {
		deps.Fault = NoopFaultInjector{}
	}
	return deps
}

// NewStore builds a journal Store rooted at RecoveryDir. A missing or relative
// root is an error (KindJournalWriteFailed). Unset dependencies default to the
// production implementations.
func NewStore(opts Options, deps Dependencies) (*Store, error) {
	if opts.RecoveryDir == "" {
		return nil, newError(KindJournalWriteFailed, "recovery dir is required")
	}
	if !filepath.IsAbs(opts.RecoveryDir) {
		return nil, newError(KindJournalWriteFailed, "recovery dir must be absolute")
	}
	deps = normalizeDependencies(deps)
	return &Store{
		recoveryDir: opts.RecoveryDir,
		journalPath: filepath.Join(opts.RecoveryDir, journalFileName),
		txRoot:      filepath.Join(opts.RecoveryDir, transactionsDirName),
		deps:        deps,
	}, nil
}

// JournalPath returns the journal file path.
func (s *Store) JournalPath() string { return s.journalPath }

// TransactionRoot returns the backout transaction root under RecoveryDir.
func (s *Store) TransactionRoot() string { return s.txRoot }

// Load reads and validates the current journal. A missing file is not an error:
// it returns (nil, nil) so callers can treat "no journal" distinctly.
func (s *Store) Load(ctx context.Context) (*Journal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadUnlocked(ctx)
}

func (s *Store) loadUnlocked(ctx context.Context) (*Journal, error) {
	data, err := os.ReadFile(s.journalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Kind: KindJournalParseFailed, Message: "read publish journal failed"}
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, &Error{Kind: KindJournalParseFailed, Message: "decode publish journal failed"}
	}
	if err := j.Validate(); err != nil {
		return nil, &Error{Kind: KindJournalParseFailed, Message: "invalid publish journal"}
	}
	return &j, nil
}

// Write persists a journal. The journal is validated before writing: an invalid
// journal never modifies an existing one.
func (s *Store) Write(ctx context.Context, j *Journal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeLocked(ctx, j)
}

func (s *Store) writeLocked(ctx context.Context, j *Journal) error {
	if j == nil {
		return newError(KindJournalWriteFailed, "cannot write a nil publish journal")
	}
	if err := j.Validate(); err != nil {
		return newError(KindJournalWriteFailed, "invalid publish journal")
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return &Error{Kind: KindJournalWriteFailed, Message: "encode publish journal failed"}
	}
	if err := s.deps.AtomicWrite(s.journalPath, data); err != nil {
		return &Error{Kind: KindJournalWriteFailed, Message: "atomic write publish journal failed"}
	}
	return nil
}

// Update serializes a read-modify-write: it loads the current journal (nil if
// absent), lets fn mutate it in place, and atomically persists the result. fn
// must leave current non-nil.
func (s *Store) Update(ctx context.Context, fn func(current *Journal) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, err := s.loadUnlocked(ctx)
	if err != nil {
		return err
	}
	if err := fn(cur); err != nil {
		return err
	}
	if cur == nil {
		return newError(KindJournalWriteFailed, "update produced a nil publish journal")
	}
	return s.writeLocked(ctx, cur)
}

// Delete removes the journal. It is idempotent: a missing file is success.
func (s *Store) Delete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.deps.Remove(s.journalPath); err != nil && !os.IsNotExist(err) {
		return &Error{Kind: KindJournalWriteFailed, Message: "remove publish journal failed"}
	}
	return nil
}
