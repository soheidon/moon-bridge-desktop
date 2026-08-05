package publishrecovery

import (
	"encoding/json"
	"strings"
	"testing"
)

// testManifest returns a valid manifest whose three entries are in the fixed
// publish order. exists selects which targets previously existed; existing
// entries carry distinct hashes.
func testManifest(exists [3]bool) *BackoutManifest {
	m := &BackoutManifest{
		SchemaVersion: BackoutSchemaVersion,
		TransactionID: testTransactionID,
		Entries:       make([]BackoutEntry, 0, 3),
	}
	for i, id := range backoutOrder {
		if !exists[i] {
			m.Entries = append(m.Entries, BackoutEntry{File: id, PreviousExists: false})
			continue
		}
		m.Entries = append(m.Entries, BackoutEntry{
			File:           id,
			PreviousExists: true,
			SHA256:         strings.Repeat("abcde"[i:i+1], 64),
		})
	}
	return m
}

func TestManifestValidateValid(t *testing.T) {
	if err := testManifest([3]bool{true, true, true}).Validate(); err != nil {
		t.Fatalf("all exist: %v", err)
	}
	if err := testManifest([3]bool{false, true, true}).Validate(); err != nil {
		t.Fatalf("catalog absent: %v", err)
	}
	if err := testManifest([3]bool{true, false, true}).Validate(); err != nil {
		t.Fatalf("auth absent: %v", err)
	}
	if err := testManifest([3]bool{false, false, false}).Validate(); err != nil {
		t.Fatalf("none exist: %v", err)
	}
}

func TestManifestValidateRejectsSchemaVersion(t *testing.T) {
	m := testManifest([3]bool{true, true, true})
	m.SchemaVersion = 2
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("expected transaction_invalid, got %v", err)
	}
}

func TestManifestValidateRejectsBadTransactionID(t *testing.T) {
	for _, id := range []string{"", "../escape", "C:\\publish", "a/b", "not-a-uuid"} {
		m := testManifest([3]bool{true, true, true})
		m.TransactionID = id
		if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", id, err)
		}
	}
}

func TestManifestValidateRejectsWrongEntryCount(t *testing.T) {
	// Too few: take a prefix of the full manifest.
	short := testManifest([3]bool{true, true, true})
	short.Entries = short.Entries[:2]
	if err := short.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("2 entries: expected transaction_invalid, got %v", err)
	}
	// Too many: an extra duplicate entry.
	long := testManifest([3]bool{true, true, true})
	long.Entries = append(long.Entries, long.Entries[2])
	if err := long.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("4 entries: expected transaction_invalid, got %v", err)
	}
	// Empty.
	none := testManifest([3]bool{true, true, true})
	none.Entries = nil
	if err := none.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("0 entries: expected transaction_invalid, got %v", err)
	}
}

func TestManifestValidateRejectsWrongOrderAndUnknownFile(t *testing.T) {
	// Swap catalog and config: out of order.
	m := testManifest([3]bool{true, true, true})
	m.Entries[0], m.Entries[2] = m.Entries[2], m.Entries[0]
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("reordered entries: expected transaction_invalid, got %v", err)
	}
	// Unknown FileID in a slot: violates the fixed order.
	m = testManifest([3]bool{true, true, true})
	m.Entries[1].File = FileID("backup")
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("unknown file id: expected transaction_invalid, got %v", err)
	}
	// A duplicate would necessarily violate the exact three-in-order contract;
	// verify a duplicated catalog is rejected (the exact-order check collapses it).
	m = testManifest([3]bool{true, true, true})
	m.Entries[0].File = FileModelsCatalog
	m.Entries[1].File = FileModelsCatalog
	m.Entries[2].File = FileConfig
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("duplicate file id: expected transaction_invalid, got %v", err)
	}
}

func TestManifestValidateHashContract(t *testing.T) {
	// Existing entry without hash rejected.
	m := testManifest([3]bool{true, true, true})
	m.Entries[0].SHA256 = ""
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("existing without hash: expected transaction_invalid, got %v", err)
	}
	// Existing entry with malformed hash rejected.
	m = testManifest([3]bool{true, true, true})
	m.Entries[0].SHA256 = "xyz"
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("bad hash: expected transaction_invalid, got %v", err)
	}
	// Absent entry carrying a hash rejected.
	m = testManifest([3]bool{true, true, true})
	m.Entries[0] = BackoutEntry{File: FileModelsCatalog, PreviousExists: false, SHA256: strings.Repeat("a", 64)}
	if err := m.Validate(); asErrorKind(err) != KindTransactionInvalid {
		t.Fatalf("absent with hash: expected transaction_invalid, got %v", err)
	}
}

func TestManifestJSONFieldNames(t *testing.T) {
	m := testManifest([3]bool{true, false, true})
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"schemaVersion", "transactionId", "entries"} {
		if _, ok := top[key]; !ok {
			t.Fatalf("manifest JSON is missing key %q", key)
		}
	}
	if _, ok := top["transactionID"]; ok {
		t.Fatalf("manifest JSON carries the old key %q", "transactionID")
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(top["entries"], &entries); err != nil {
		t.Fatalf("unmarshal entries: %v", err)
	}
	for i, e := range entries {
		if _, ok := e["file"]; !ok {
			t.Fatalf("entries[%d] is missing key %q", i, "file")
		}
		if _, ok := e["previousExists"]; !ok {
			t.Fatalf("entries[%d] is missing key %q", i, "previousExists")
		}
	}
	// The manifest schema has no path-bearing field at all.
	for _, key := range []string{"path", "targetHome", "backupDir", "absolutePath"} {
		if _, ok := top[key]; ok {
			t.Fatalf("manifest JSON must not carry a path field %q", key)
		}
	}
	if strings.Contains(string(data), "://") {
		t.Fatalf("manifest JSON leaks a volume/URL pattern")
	}
}

func TestManifestJSONNeverContainsRecoveryRoot(t *testing.T) {
	// Even a fully-populated manifest must serialize without any host path: its
	// only content fields are the FileID enums and hex hashes.
	m := testManifest([3]bool{true, true, true})
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Simulate an absolute path appearing: this must be impossible structurally,
	// so the round-tripped JSON is free of any path separator run.
	if strings.Contains(string(data), `\`) || strings.Contains(string(data), "/") {
		t.Fatalf("manifest JSON contains a path separator")
	}
}
