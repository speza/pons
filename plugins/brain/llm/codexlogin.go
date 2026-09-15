// Codex ChatGPT-subscription login: the same OAuth PKCE browser flow the
// Codex CLI performs, owned by pons.
//
//  1. a local HTTP server on 127.0.0.1:1455 waits for the callback
//  2. the browser opens auth.openai.com/authorize (PKCE S256)
//  3. the callback delivers the authorization code
//  4. the code is exchanged for access/id/refresh tokens
//  5. credentials are stored at ~/.pons/auth.json (0600) — pons owns it
package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

const (
	loginRedirectPort = 1455
	loginRedirectURI  = "http://localhost:1455/auth/callback"
	// Scope and params mirror the Codex CLI exactly — the ChatGPT auth
	// server validates this client against that registered shape.
	loginScope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	originator = "codex_cli_rs"
)

// pkce is a one-shot proof-key set for a single login attempt.
type pkce struct{ verifier, challenge, state string }

func newPKCE() pkce {
	verifier := mustRandomB64(32)
	sum := sha256.Sum256([]byte(verifier))
	return pkce{
		verifier:  verifier,
		challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
		state:     mustRandomB64(16),
	}
}

func mustRandomB64(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// authorizeURL builds the auth.openai.com authorize URL for this attempt.
func authorizeURL(p pkce) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", oauthClientID)
	q.Set("redirect_uri", loginRedirectURI)
	q.Set("scope", loginScope)
	q.Set("code_challenge", p.challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", p.state)
	q.Set("originator", originator)
	return "https://auth.openai.com/oauth/authorize?" + q.Encode()
}

// waitForCallback runs the local redirect server until the browser hits it
// (or the context ends). The state parameter is validated on arrival.
func waitForCallback(ctx context.Context, state string) (code string, err error) {
	type result struct {
		code string
		msg  string
	}
	done := make(chan result, 1)
	srvDone := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("/auth/callback", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		msg := ""
		if e := req.URL.Query().Get("error"); e != "" {
			msg = "provider reported: " + e
		}
		code := req.URL.Query().Get("code")
		if msg == "" && code == "" {
			msg = "callback missing code"
		}
		w.Header().Set("content-type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h3>Authenticated - return to the terminal.</h3></body></html>")
		select {
		case done <- result{code: code, msg: msg}:
		default:
		}
	})
	srv := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", loginRedirectPort), Handler: mux}
	go func() {
		defer close(srvDone)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			done <- result{msg: fmt.Sprintf("callback server: %v", err)}
		}
	}()
	defer func() {
		_ = srv.Close()
		<-srvDone
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-done:
		if r.msg != "" {
			return "", fmt.Errorf("login failed: %s", r.msg)
		}
		return r.code, nil
	}
}

// RunLogin performs the full browser login and stores credentials at
// authPath. Guidance is printed for the user; the browser is opened
// automatically when an opener is available.
func RunLogin(ctx context.Context, authPath string) error {
	p := newPKCE()
	authURL := authorizeURL(p)
	fmt.Println("Opening your browser to authenticate with ChatGPT…")
	fmt.Println("If no browser opens, visit this URL and complete the login:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	openBrowser(authURL)

	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	code, err := waitForCallback(waitCtx, p.state)
	if err != nil {
		return err
	}
	fmt.Println("Exchanging authorization code…")

	tokens, err := exchangeCode(ctx, code, p.verifier)
	if err != nil {
		return err
	}

	a := &codexAuth{
		http:       defaultHTTPClient(),
		authPath:   authPath,
		Access:     tokens.Access,
		AccountID:  tokens.AccountID,
		Refresh:    tokens.Refresh,
		ExpiresAt:  tokens.ExpiresAt,
		refreshURL: refreshTokenURL,
	}
	if err := a.save(); err != nil {
		return fmt.Errorf("login succeeded but saving %s failed: %w", authPath, err)
	}
	fmt.Printf("Logged in. Credentials saved to %s (expires %s).\n",
		authPath, a.ExpiresAt.Format(time.RFC3339))
	return nil
}

type loginTokens struct {
	Access    string
	AccountID string
	Refresh   string
	ExpiresAt time.Time
}

// exchangeCode swaps the authorization code for tokens and extracts the
// ChatGPT account id from the id_token claims.
func exchangeCode(ctx context.Context, code, verifier string) (loginTokens, error) {
	var out loginTokens
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("redirect_uri", loginRedirectURI)
	body.Set("client_id", oauthClientID)
	body.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshTokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return out, err
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")

	resp, err := defaultHTTPClient().Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return out, err
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("token exchange: HTTP %d: %s", resp.StatusCode, truncateMsg(strings.TrimSpace(string(raw)), 300))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		IDToken     string `json:"id_token"`
		RefreshTok  string `json:"refresh_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return out, err
	}
	if tok.AccessToken == "" {
		return out, fmt.Errorf("token exchange returned no access_token")
	}
	out.Access = tok.AccessToken
	out.Refresh = tok.RefreshTok
	if tok.ExpiresIn > 0 {
		out.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	out.AccountID = accountIDFromIDToken(tok.IDToken)
	return out, nil
}

// accountIDFromIDToken digs the ChatGPT account id out of the id_token JWT
// payload (claim "https://api.openai.com/auth" → chatgpt_account_id).
func accountIDFromIDToken(idToken string) string {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if auth, ok := claims["https://api.openai.com/auth"].(map[string]any); ok {
		if id, ok := auth["chatgpt_account_id"].(string); ok {
			return id
		}
	}
	if id, ok := claims["chatgpt_account_id"].(string); ok {
		return id
	}
	return ""
}

// openBrowser launches the platform opener, ignoring failures (the printed
// URL is the fallback).
func openBrowser(target string) {
	candidates := [][]string{{"open", target}, {"xdg-open", target}}
	for _, cmd := range candidates {
		if _, err := exec.LookPath(cmd[0]); err == nil {
			if err := exec.Command(cmd[0], cmd[1:]...).Start(); err == nil {
				return
			}
		}
	}
}
