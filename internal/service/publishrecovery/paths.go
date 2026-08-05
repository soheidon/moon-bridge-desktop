package publishrecovery

import (
	"path/filepath"
	"regexp"
	"strings"
)

// uuidV4RE is the grammar a transactionID must satisfy: a canonical lowercase
// UUID v4. Path characters (/ \ :), traversal, and volume names are impossible
// inside this grammar, so a validated ID is always safe to join onto the
// transaction root.
var uuidV4RE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ValidateTransactionID verifies that a transactionID is a canonical lowercase
// UUID v4. Empty, wrong-length, non-hex, non-v4, or wrong-variant values are
// rejected with KindTransactionInvalid.
func ValidateTransactionID(id string) error {
	if id == "" {
		return newError(KindTransactionInvalid, "transactionID is required")
	}
	if len(id) != 36 || !uuidV4RE.MatchString(id) {
		return newError(KindTransactionInvalid, "transactionID must be a canonical UUID v4")
	}
	return nil
}

// fileNameFor resolves a FileID to its file name inside a codex home. An unknown
// ID returns "" (only the three publish targets are valid file ids).
func fileNameFor(id FileID) string {
	switch id {
	case FileModelsCatalog:
		return "models_catalog.json"
	case FileAuth:
		return "auth.json"
	case FileConfig:
		return "config.toml"
	default:
		return ""
	}
}

// backupFileNameFor returns the backout file name for a FileID: the original
// bytes preserved in the transaction root before the target is replaced.
func backupFileNameFor(id FileID) string {
	if name := fileNameFor(id); name != "" {
		return name + ".backup"
	}
	return ""
}

// transactionRoot joins a validated transactionID onto base. The ID is validated
// before any join so an untrusted value can never escape base.
func transactionRoot(base, id string) (string, error) {
	if err := ValidateTransactionID(id); err != nil {
		return "", err
	}
	return filepath.Join(base, id), nil
}

// pathWithin reports whether p is lexically inside root (or equals it). It is a
// string check only and does not follow symlinks/junctions; use
// pathWithinPhysical when the target may redirect outside root.
func pathWithin(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// pathWithinPhysical reports whether a path resolves to a location inside root
// after following symlinks/junctions. When the target itself exists it is fully
// resolved first, so a leaf link pointing outside root is rejected even though
// its parent is inside root; when it does not exist, only the existing ancestor
// chain is resolved and the remainder is re-joined lexically.
func pathWithinPhysical(root, target string) bool {
	rootPhys, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	if targetPhys, err := filepath.EvalSymlinks(target); err == nil {
		return pathWithin(rootPhys, targetPhys)
	}
	parentPhys, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return false
	}
	return pathWithin(rootPhys, parentPhys)
}
