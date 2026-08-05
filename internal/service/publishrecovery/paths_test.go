package publishrecovery

import (
	"path/filepath"
	"testing"
)

func TestValidateTransactionIDValid(t *testing.T) {
	for _, id := range []string{
		"11111111-1111-4111-8111-111111111111",
		"a1b2c3d4-e5f6-4a7b-9c8d-1e2f3a4b5c6d",
	} {
		if err := ValidateTransactionID(id); err != nil {
			t.Fatalf("expected valid transactionID %q, got %v", id, err)
		}
	}
}

func TestValidateTransactionIDRejects(t *testing.T) {
	for _, id := range []string{
		"",
		"11111111-1111-4111-8111-11111111111",   // 35 chars
		"11111111-1111-4111-8111-1111111111111", // 37 chars
		"11111111-1111-4111-8111-AAAAAAAAAAAA",  // uppercase
		"zzzzzzzz-1111-4111-8111-111111111111",  // non-hex
		"11111111-1111-1111-8111-111111111111",  // version 1, not 4
		"11111111-1111-4111-7111-111111111111",  // variant 7, not 8-9-a-b
		"../etc/passwd",
		"C:\\publish",
		"a/b",
		"..\\..\\journal",
		"11111111-1111-4111-8111-111111111111.", // trailing dot
	} {
		if err := ValidateTransactionID(id); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", id, err)
		}
	}
}

func TestFileNameFor(t *testing.T) {
	for _, tc := range []struct {
		id   FileID
		want string
	}{
		{FileModelsCatalog, "models_catalog.json"},
		{FileAuth, "auth.json"},
		{FileConfig, "config.toml"},
		{FileID("unknown"), ""},
	} {
		if got := fileNameFor(tc.id); got != tc.want {
			t.Fatalf("fileNameFor(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestBackupFileNameFor(t *testing.T) {
	for _, tc := range []struct {
		id   FileID
		want string
	}{
		{FileModelsCatalog, "models_catalog.json.backup"},
		{FileAuth, "auth.json.backup"},
		{FileConfig, "config.toml.backup"},
		{FileID("unknown"), ""},
	} {
		if got := backupFileNameFor(tc.id); got != tc.want {
			t.Fatalf("backupFileNameFor(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestTransactionRootValidatesBeforeJoin(t *testing.T) {
	base := filepath.Join("root", "publish-transactions")
	got, err := transactionRoot(base, testTransactionID)
	if err != nil {
		t.Fatalf("expected valid join: %v", err)
	}
	if want := filepath.Join(base, testTransactionID); got != want {
		t.Fatalf("transactionRoot = %q, want %q", got, want)
	}
	for _, bad := range []string{"", "../escape", "C:\\publish", "a/b", "..\\..\\journal"} {
		if _, err := transactionRoot(base, bad); asErrorKind(err) != KindTransactionInvalid {
			t.Fatalf("transactionID %q: expected transaction_invalid, got %v", bad, err)
		}
	}
}
