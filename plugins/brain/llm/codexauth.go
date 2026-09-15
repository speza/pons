// ChatGPT-subscription credentials in pons's own auth file
// (~/.pons/auth.json, 0600). Pons owns this file: refreshed tokens are
// written back so a subscription stays usable across runs without
// re-authenticating.
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
	"time"
)

// codexAuth holds ChatGPT-subscription credentials (pons auth-file shape:
// {access, accountId, expires, refresh} at ~/.pons/auth.json).
type codexAuth struct {
	http       *http.Client
	authPath   string
	Access     string
	AccountID  string
	Refresh    string
	ExpiresAt  time.Time
	refreshURL string // overridable for tests
}

// DefaultCodexAuthPath is where pons stores its own codex credentials.
func DefaultCodexAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pons", "auth.json"), nil
}

// ponsAuthFile is the on-disk shape of ~/.pons/auth.json.
type ponsAuthFile struct {
	Access    string `json:"access"`
	AccountID string `json:"accountId"`
	Refresh   string `json:"refresh"`
	Expires   int64  `json:"expires"` // unix ms
}

func loadCodexAuth(path string) (*codexAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("codex auth: %s does not exist — run: pons -provider codex --login", path)
		}
		return nil, fmt.Errorf("codex auth: %w", err)
	}
	var file ponsAuthFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("codex auth: parse %s: %w", path, err)
	}
	if file.Access == "" {
		return nil, fmt.Errorf("codex auth: %s has no access token — run: pons -provider codex --login", path)
	}
	return &codexAuth{
		http:       defaultHTTPClient(),
		authPath:   path,
		Access:     file.Access,
		AccountID:  file.AccountID,
		Refresh:    file.Refresh,
		ExpiresAt:  time.UnixMilli(file.Expires),
		refreshURL: refreshTokenURL,
	}, nil
}

// save persists credentials back to the auth file (0600) after a refresh.
func (a *codexAuth) save() error {
	expires := int64(0)
	if !a.ExpiresAt.IsZero() {
		expires = a.ExpiresAt.UnixMilli()
	}
	b, err := json.MarshalIndent(ponsAuthFile{
		Access:    a.Access,
		AccountID: a.AccountID,
		Refresh:   a.Refresh,
		Expires:   expires,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.authPath), 0o700); err != nil {
		return err
	}
	return os.WriteFile(a.authPath, b, 0o600)
}

// current returns credentials that are valid right now, refreshing
// in-memory when the access token has expired (1-minute safety margin).
func (a *codexAuth) current(ctx context.Context) (access, accountID string, err error) {
	if a.Access != "" && (a.ExpiresAt.IsZero() || time.Now().Before(a.ExpiresAt.Add(-time.Minute))) {
		return a.Access, a.AccountID, nil
	}
	if a.Refresh == "" {
		return "", "", fmt.Errorf("codex auth: token expired and no refresh token available (run /login in pi)")
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
	// Pons owns the auth file now: persist refreshed credentials.
	return a.save()
}
