package api

import (
	"net/http"

	"moonbridge/internal/service/provider"
)

// handleGetCredentialStatus returns the non-secret credential states the shared
// resolver recorded at client generation. It carries source, state, and error
// code only — never a key, ciphertext, or decrypted value. Providers without a
// recorded status are simply absent.
func (r *Router) handleGetCredentialStatus(w http.ResponseWriter, req *http.Request) {
	var items []provider.CredentialInfo
	if snap := r.runtime.Current(); snap != nil && snap.ProviderMgr != nil {
		items = snap.ProviderMgr.CredentialStatus()
	}
	if items == nil {
		items = []provider.CredentialInfo{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"credentials": items})
}
