// Codex ChatGPT-subscription credentials, stored in pons's auth store
// (~/.pons/auth.json, 0600). The store maps auth ids to one credential
// each, so several subscriptions (each its own provider entry) live side
// by side and refresh independently:
//
//	{"codex-personal": {"access": "…", "accountId": "…", "refresh": "…", "expires": 123},
//	 "codex-work":     {…}}
//
// A legacy single-credential file (the pre-multi-provider shape, matching
// the Codex CLI file) is read as the entry "codex" and upgraded in place
// on the next save. Concurrent refreshes across pons processes serialize
// on an advisory lock of the store file.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LegacyAuthID is the store entry for a legacy single-credential auth.json.
const LegacyAuthID = "codex"

// ponsAuthFile is one credential entry: {access, accountId, refresh, expires}.
type ponsAuthFile struct {
	Access    string `json:"access"`
	AccountID string `json:"accountId"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"` // unix ms
}

// authStore is the on-disk shape of ~/.pons/auth.json.
type authStore map[string]ponsAuthFile

// decodeAuthStore parses a store file. A file carrying the legacy
// top-level credential shape is wrapped as the "codex" entry.
func decodeAuthStore(data []byte) (authStore, error) {
	var legacy ponsAuthFile
	if err := json.Unmarshal(data, &legacy); err == nil && legacy.Access != "" {
		return authStore{LegacyAuthID: legacy}, nil
	}
	var store authStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("auth store: %w", err)
	}
	return store, nil
}

// resolve finds the credential for id: an exact entry first; a store with
// exactly one entry stands in for any id — the single-subscription
// default, regardless of entry naming.
func (s authStore) resolve(id string) (string, ponsAuthFile, error) {
	if cred, ok := s[id]; ok {
		if cred.Access == "" {
			return "", ponsAuthFile{}, fmt.Errorf("codex auth: entry %q has no access token — run: pons -provider codex --login -as %s", id, id)
		}
		return id, cred, nil
	}
	if len(s) == 1 {
		for k, cred := range s {
			if cred.Access == "" {
				return "", ponsAuthFile{}, fmt.Errorf("codex auth: entry %q has no access token — run: pons -provider codex --login -as %s", k, k)
			}
			return k, cred, nil
		}
	}
	ids := make([]string, 0, len(s))
	for k := range s {
		ids = append(ids, k)
	}
	return "", ponsAuthFile{}, fmt.Errorf("codex auth: no entry %q in auth store (available: %s) — run: pons -provider codex --login -as %s",
		id, strings.Join(ids, ", "), id)
}

// DefaultCodexAuthPath is where pons stores its codex credentials.
func DefaultCodexAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pons", "auth.json"), nil
}

// codexAuth holds one ChatGPT-subscription credential resolved from the
// store. Refreshed tokens are written back under their auth id.
type codexAuth struct {
	http       *http.Client
	storePath  string
	id         string
	Access     string
	AccountID  string
	Refresh    string
	ExpiresAt  time.Time
	refreshURL string // overridable for tests
}

func loadCodexAuth(storePath, requestedID string) (*codexAuth, error) {
	data, err := os.ReadFile(storePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("codex auth: %s does not exist — run: pons -provider codex --login -as %s", storePath, requestedID)
		}
		return nil, fmt.Errorf("codex auth: %w", err)
	}
	store, err := decodeAuthStore(data)
	if err != nil {
		return nil, fmt.Errorf("codex auth: %s: %w", storePath, err)
	}
	id, cred, err := store.resolve(requestedID)
	if err != nil {
		return nil, fmt.Errorf("codex auth: %s: %w", storePath, err)
	}
	return &codexAuth{
		http:       defaultHTTPClient(),
		storePath:  storePath,
		id:         id,
		Access:     cred.Access,
		AccountID:  cred.AccountID,
		Refresh:    cred.Refresh,
		ExpiresAt:  time.UnixMilli(cred.Expires),
		refreshURL: refreshTokenURL,
	}, nil
}

// save persists this credential back to the store (0600) after a refresh,
// preserving every other entry.
func (a *codexAuth) save() error {
	expires := int64(0)
	if !a.ExpiresAt.IsZero() {
		expires = a.ExpiresAt.UnixMilli()
	}
	return saveAuthEntry(a.storePath, a.id, ponsAuthFile{
		Access:    a.Access,
		AccountID: a.AccountID,
		Refresh:   a.Refresh,
		Expires:   expires,
	})
}

// saveAuthEntry atomically replaces one entry in the auth store under an
// advisory file lock, so concurrent pons processes do not clobber each
// other's refreshed tokens.
func saveAuthEntry(storePath, id string, cred ponsAuthFile) error {
	dir := filepath.Dir(storePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(storePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	unlock := lockFileExclusive(f)
	defer unlock()

	var store authStore
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) > 0 {
		if store, err = decodeAuthStore(data); err != nil {
			return fmt.Errorf("codex auth: %s: %w", storePath, err)
		}
	}
	if store == nil {
		store = authStore{}
	}
	store[id] = cred
	b, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.WriteAt(b, 0); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Chmod(0o600)
}

// current returns credentials that are valid right now, refreshing
// in-memory when the access token has expired (1-minute safety margin).
func (a *codexAuth) current(ctx context.Context) (access, accountID string, err error) {
	if a.Access != "" && (a.ExpiresAt.IsZero() || time.Now().Before(a.ExpiresAt.Add(-time.Minute))) {
		return a.Access, a.AccountID, nil
	}
	if a.Refresh == "" {
		return "", "", fmt.Errorf("codex auth: token expired and no refresh token available (run pons -provider codex --login)")
	}
	if err := a.refresh(ctx); err != nil {
		return "", "", err
	}
	return a.Access, a.AccountID, nil
}

func (a *codexAuth) refresh(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{
		"client_id":     oauthClientID,
		"grant_type":    "refresh_token",
		"refresh_token": a.Refresh,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.refreshURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("codex auth: refresh failed: HTTP %d: %s", resp.StatusCode, truncateMsg(string(raw), 256))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		RefreshTok  string `json:"refresh_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("codex auth: refresh decode: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("codex auth: refresh returned no access_token")
	}
	a.Access = tok.AccessToken
	if tok.RefreshTok != "" {
		a.Refresh = tok.RefreshTok
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	// Pons owns the store: persist refreshed credentials.
	return a.save()
}
