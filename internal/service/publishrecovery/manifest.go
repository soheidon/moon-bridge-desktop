package publishrecovery

// BackoutSchemaVersion is the current backout manifest schema version.
const BackoutSchemaVersion = 1

// backoutManifestFileName is the manifest file name inside a transaction directory.
const backoutManifestFileName = "manifest.json"

// BackoutEntry records the pre-publish state of one publish target: whether the
// file existed and, if so, the SHA-256 of its original bytes (kept in a
// <name>.backup file beside the manifest). The manifest never carries paths or
// raw secrets — only the enum FileID and a content hash.
type BackoutEntry struct {
	File           FileID `json:"file"`
	PreviousExists bool   `json:"previousExists"`
	SHA256         string `json:"sha256,omitempty"`
}

// BackoutManifest is the durable index of a transaction's backout data. It is
// written atomically and its exact bytes are hashed into the journal's
// BackoutManifestSHA256, so tampering with either side is detectable.
type BackoutManifest struct {
	SchemaVersion int            `json:"schemaVersion"`
	TransactionID string         `json:"transactionId"`
	Entries       []BackoutEntry `json:"entries"`
}

// backoutOrder is the fixed entry order: the publish order (catalog, then auth,
// then config). Entries must be exactly these three in this order.
var backoutOrder = []FileID{FileModelsCatalog, FileAuth, FileConfig}

// Validate enforces the backout manifest contract. The manifest has no path
// fields at all: FileIDs are the enum-only publish targets (validated by the
// fixed order below) and SHA256 is a 64-hex content hash, so an absolute path
// can never be represented in a manifest.
func (m *BackoutManifest) Validate() error {
	if m.SchemaVersion != BackoutSchemaVersion {
		return newError(KindTransactionInvalid, "unsupported backout manifest schema version")
	}
	if err := ValidateTransactionID(m.TransactionID); err != nil {
		return err
	}
	if len(m.Entries) != len(backoutOrder) {
		return newError(KindTransactionInvalid, "backout entries must contain exactly the three publish targets")
	}
	for i, id := range backoutOrder {
		if m.Entries[i].File != id {
			return newError(KindTransactionInvalid, "backout entries must be ordered models_catalog, auth, config")
		}
	}
	for _, e := range m.Entries {
		if e.PreviousExists {
			if !hex64RE.MatchString(e.SHA256) {
				return newError(KindTransactionInvalid, "an existing entry requires its sha256 hash")
			}
		} else if e.SHA256 != "" {
			return newError(KindTransactionInvalid, "an absent entry must not carry a sha256 hash")
		}
	}
	return nil
}
