//go:build windows

package codex_test

import (
	"encoding/json"
	"os"
	"testing"
)

func assertAuthJSONPermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("auth.json is not a regular file: %v", info.Mode())
	}

	// Windows ACLs, not FileMode().Perm(), represent access restrictions;
	// verify that the current user can read the generated auth file instead of
	// comparing Unix 0600 bits that Windows cannot expose reliably.
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer file.Close()
	var payload map[string]string
	if err := json.NewDecoder(file).Decode(&payload); err != nil {
		t.Fatalf("decode auth.json: %v", err)
	}
	if payload["openai_api_key"] == "" {
		t.Fatal("auth.json does not contain openai_api_key")
	}
}
