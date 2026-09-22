package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeTestKey(t *testing.T, key *rsa.PrivateKey, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github-app.pem")
	body := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, body, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepositoryTokenIsSignedAndRestrictedToRepository(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 22, 12, 0, 0, 0, time.UTC)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		if len(parts) != 3 {
			t.Errorf("authorization is not a JWT")
			http.Error(w, "bad JWT", http.StatusUnauthorized)
			return
		}
		unsigned := parts[0] + "." + parts[1]
		digest := sha256.Sum256([]byte(unsigned))
		signature, decodeErr := base64.RawURLEncoding.DecodeString(parts[2])
		if decodeErr != nil || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil {
			t.Errorf("JWT signature is invalid")
		}
		claimsBody, decodeErr := base64.RawURLEncoding.DecodeString(parts[1])
		var claims struct {
			Issuer int64 `json:"iss"`
		}
		if decodeErr != nil || json.Unmarshal(claimsBody, &claims) != nil || claims.Issuer != 1234 {
			t.Errorf("JWT claims = %s", claimsBody)
		}
		if r.Header.Get("X-GitHub-Api-Version") != githubAPIVersion {
			t.Errorf("API version = %q", r.Header.Get("X-GitHub-Api-Version"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/installation":
			_, _ = io.WriteString(w, `{"id":99}`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/99/access_tokens":
			var payload struct {
				Repositories []string          `json:"repositories"`
				Permissions  map[string]string `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if len(payload.Repositories) != 1 || payload.Repositories[0] != "widgets" ||
				payload.Permissions["contents"] != "write" || payload.Permissions["metadata"] != "read" {
				t.Errorf("token request = %+v", payload)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "installation-secret", "expires_at": now.Add(time.Hour),
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	app, err := New(Config{
		AppID: 1234, PrivateKeyPath: writeTestKey(t, key, 0o600),
		APIURL: server.URL, HTTPClient: server.Client(), Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := app.RepositoryToken(context.Background(), "https://github.com/acme/widgets.git")
	if err != nil {
		t.Fatal(err)
	}
	if token != "installation-secret" || requests.Load() != 2 {
		t.Fatalf("token = %q, requests = %d", token, requests.Load())
	}
}

func TestRepositoryTokenRejectsNonGitHubSourceBeforeRequest(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(Config{AppID: 1, PrivateKeyPath: writeTestKey(t, key, 0o600)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.RepositoryToken(context.Background(), "https://git.example.com/acme/widgets.git"); err == nil {
		t.Fatal("non-GitHub repository was accepted")
	}
}

func TestNewRejectsInsecurePrivateKeyPermissions(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{AppID: 1, PrivateKeyPath: writeTestKey(t, key, 0o644)}); err == nil {
		t.Fatal("world-readable private key was accepted")
	}
}
