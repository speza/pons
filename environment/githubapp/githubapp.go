// Package githubapp mints GitHub App installation tokens.
// The App private key remains on the pons host; callers pass only short-lived
// installation tokens across the hands boundary.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/samperrin/pons/environment/gitworkspace"
)

const (
	defaultAPIURL        = "https://api.github.com"
	maxResponseBytes     = 1 << 20
	githubAPIVersion     = "2022-11-28"
	installationTokenTTL = time.Hour
)

// Config identifies a deployment-owned GitHub App. PrivateKeyPath names a PEM
// file readable only by its owner.
type Config struct {
	AppID          int64
	InstallationID int64
	PrivateKeyPath string
	APIURL         string
	HTTPClient     *http.Client
	Now            func() time.Time
}

// App authenticates as one deployment-owned GitHub App.
type App struct {
	id             int64
	installationID int64
	key            *rsa.PrivateKey
	apiURL         string
	http           *http.Client
	now            func() time.Time
}

var _ gitworkspace.CredentialSource = (*App)(nil)

// Credentials mints a token restricted to repository unless all is requested.
// The Git header uses the same URL scope as the token.
func (a *App) Credentials(ctx context.Context, repositoryURL string, all bool) (gitworkspace.Credentials, error) {
	token, err := a.InstallationToken(ctx, repositoryURL, all)
	if err != nil {
		return gitworkspace.Credentials{}, err
	}
	scope := repositoryURL
	if all {
		scope = "https://github.com/"
	}
	return gitworkspace.HTTPSBasicCredentials(scope, "x-access-token", token), nil
}

// New loads and validates a GitHub App private key from the host.
func New(cfg Config) (*App, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("github app: app ID must be positive")
	}
	if cfg.InstallationID <= 0 {
		return nil, errors.New("github app: installation ID must be positive")
	}
	if cfg.PrivateKeyPath == "" {
		return nil, errors.New("github app: private key path is required")
	}

	info, err := os.Stat(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github app: private key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("github app: private key must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("github app: private key permissions must not allow group or other access")
	}

	body, err := os.ReadFile(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github app: read private key: %w", err)
	}
	key, err := parsePrivateKey(body)
	if err != nil {
		return nil, err
	}

	apiURL := strings.TrimRight(cfg.APIURL, "/")
	if apiURL == "" {
		apiURL = defaultAPIURL
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &App{id: cfg.AppID, installationID: cfg.InstallationID, key: key, apiURL: apiURL, http: client, now: now}, nil
}

func parsePrivateKey(body []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errors.New("github app: private key is not PEM")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("github app: private key is not a supported RSA key")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github app: private key is not RSA")
	}
	return key, nil
}

// InstallationToken returns a one-hour token with contents write and metadata
// read, restricted to repository unless all installation repositories are requested.
func (a *App) InstallationToken(ctx context.Context, repositoryURL string, all bool) (string, error) {
	var repository string
	if !all {
		var err error
		repository, err = githubRepository(repositoryURL)
		if err != nil {
			return "", err
		}
	}

	jwt, err := a.jwt()
	if err != nil {
		return "", err
	}
	payload := struct {
		Repositories []string          `json:"repositories,omitempty"`
		Permissions  map[string]string `json:"permissions"`
	}{
		Permissions: map[string]string{
			"contents": "write",
			"metadata": "read",
		},
	}
	if !all {
		payload.Repositories = []string{repository}
	}
	var token struct {
		Value     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.request(
		ctx,
		http.MethodPost,
		a.apiURL+"/app/installations/"+strconv.FormatInt(a.installationID, 10)+"/access_tokens",
		payload,
		jwt,
		&token,
	); err != nil {
		return "", fmt.Errorf("github app: create installation token: %w", err)
	}

	if token.Value == "" || !token.ExpiresAt.After(a.now().Add(time.Minute)) || token.ExpiresAt.After(a.now().Add(installationTokenTTL+time.Minute)) {
		return "", errors.New("github app: installation token response is invalid")
	}
	return token.Value, nil
}

func githubRepository(repositoryURL string) (string, error) {
	parsed, err := url.Parse(repositoryURL)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.Port() != "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("github app: repository must use https://github.com/OWNER/REPOSITORY")
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != 2 {
		return "", errors.New("github app: repository must use https://github.com/OWNER/REPOSITORY")
	}
	owner, err := url.PathUnescape(parts[0])
	if err != nil || owner == "" {
		return "", errors.New("github app: repository owner is invalid")
	}
	repository, err := url.PathUnescape(strings.TrimSuffix(parts[1], ".git"))
	if err != nil || repository == "" || strings.ContainsAny(owner+repository, "\x00/") {
		return "", errors.New("github app: repository name is invalid")
	}
	return repository, nil
}

func (a *App) jwt() (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	now := a.now()
	payload, err := json.Marshal(struct {
		IssuedAt  int64 `json:"iat"`
		ExpiresAt int64 `json:"exp"`
		Issuer    int64 `json:"iss"`
	}{IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(9 * time.Minute).Unix(), Issuer: a.id})
	if err != nil {
		return "", err
	}

	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, a.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github app: sign JWT: %w", err)
	}

	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (a *App) request(ctx context.Context, method, endpoint string, payload any, token string, result any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	response, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return err
	}
	if len(responseBody) > maxResponseBytes {
		return fmt.Errorf("GitHub response exceeds %d bytes", maxResponseBytes)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		var failure struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(responseBody, &failure)
		if failure.Message == "" {
			failure.Message = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("GitHub returned %d: %s", response.StatusCode, failure.Message)
	}

	if err := json.Unmarshal(responseBody, result); err != nil {
		return fmt.Errorf("github app: decode response: %w", err)
	}
	return nil
}
