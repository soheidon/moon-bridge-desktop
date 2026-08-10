package configgraph

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"moonbridge/internal/secretstore"
)

// boundaryFakeCodec encrypts with the dpapi:v1: prefix, mirroring the real
// DPAPI codec's shape without touching the OS.
type boundaryFakeCodec struct{}

func (boundaryFakeCodec) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return "dpapi:v1:" + base64.StdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (boundaryFakeCodec) Decrypt(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !secretstore.IsCiphertext(stored) {
		return "", errors.New("not ciphertext")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, "dpapi:v1:"))
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (boundaryFakeCodec) Supported() bool { return true }

// Patch must persist ciphertext (never the plaintext) and never surface either
// in the response graph.
func TestServicePatchEncryptsProviderKeyBeforeSave(t *testing.T) {
	svc, store, _ := newServiceForTest(testConfig(), "rev-1")
	svc.WithCodec(boundaryFakeCodec{})

	resp, err := svc.Patch(context.Background(), PatchRequest{
		BaseRevision: "rev-1",
		Changes: []PatchOp{
			{Kind: ResourceProvider, ID: "anthropic", Field: "api_key", Value: "sk-new-plaintext"},
		},
	})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if resp.Result != ResultCommitted && resp.Result != ResultRestartRequired {
		t.Fatalf("Patch().Result = %q, want committed", resp.Result)
	}
	persisted := store.cfg.ProviderDefs["anthropic"].APIKey
	if persisted == "sk-new-plaintext" {
		t.Fatal("plaintext api_key persisted")
	}
	if !secretstore.IsCiphertext(persisted) {
		t.Fatalf("persisted api_key = %q, want ciphertext", persisted)
	}
	// The plaintext survives round-trip through the codec.
	if resp.Graph == nil {
		t.Fatal("response graph is nil")
	}
	prov := assertResource(t, *resp.Graph, ResourceProvider, "anthropic")
	graphKey, _ := prov.Value["api_key"].(string)
	if graphKey == "sk-new-plaintext" || graphKey == persisted {
		t.Fatalf("response graph leaks api_key value %q", graphKey)
	}
}

// CreateResource with a provider api_key must encrypt before persisting.
func TestServiceCreateProviderEncryptsKey(t *testing.T) {
	svc, store, _ := newServiceForTest(testConfig(), "rev-1")
	svc.WithCodec(boundaryFakeCodec{})

	resp, err := svc.CreateResource(context.Background(), ResourceProvider, "new-provider", map[string]any{
		"base_url": "https://new.test",
		"api_key":  "sk-create-plaintext",
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if resp.Result != ResultCommitted && resp.Result != ResultRestartRequired {
		t.Fatalf("CreateResource().Result = %q, want committed", resp.Result)
	}
	key := store.cfg.ProviderDefs["new-provider"].APIKey
	if key == "sk-create-plaintext" {
		t.Fatal("plaintext api_key persisted via CreateResource")
	}
	if !secretstore.IsCiphertext(key) {
		t.Fatalf("persisted api_key = %q, want ciphertext", key)
	}
}

// Without a codec the boundary passes keys through unchanged (test/backward
// compat), and the same patch must never double-encrypt already-ciphertext.
func TestServicePatchWithCiphertextDoesNotDoubleEncrypt(t *testing.T) {
	svc, store, _ := newServiceForTest(testConfig(), "rev-1")
	svc.WithCodec(boundaryFakeCodec{})

	cipher, err := (boundaryFakeCodec{}).Encrypt("sk-existing")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := svc.Patch(context.Background(), PatchRequest{
		BaseRevision: "rev-1",
		Changes: []PatchOp{
			{Kind: ResourceProvider, ID: "anthropic", Field: "api_key", Value: cipher},
		},
	})
	if err != nil {
		t.Fatalf("Patch() error = %v", err)
	}
	if resp.Result != ResultCommitted && resp.Result != ResultRestartRequired {
		t.Fatalf("Patch().Result = %q", resp.Result)
	}
	if got := store.cfg.ProviderDefs["anthropic"].APIKey; got != cipher {
		t.Fatalf("persisted api_key = %q, want unchanged ciphertext %q", got, cipher)
	}
}
