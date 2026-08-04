package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishConfigFileDoesNotOverwriteExistingFinalFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "config.yml")
	tempPath := filepath.Join(dir, ".config.yml.tmp")
	if err := os.WriteFile(finalPath, []byte("existing-config"), 0o600); err != nil {
		t.Fatalf("WriteFile(final) error = %v", err)
	}
	if err := os.WriteFile(tempPath, []byte("starter-config"), 0o600); err != nil {
		t.Fatalf("WriteFile(temp) error = %v", err)
	}

	created, err := publishConfigFile(tempPath, finalPath)

	if err != nil {
		t.Fatalf("publishConfigFile() error = %v", err)
	}
	if created {
		t.Fatal("publishConfigFile() created = true, want false")
	}
	got, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("ReadFile(final) error = %v", err)
	}
	if string(got) != "existing-config" {
		t.Fatalf("final file content = %q, want existing-config", got)
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Fatalf("temp file stat error = %v, want not exist", err)
	}
}
