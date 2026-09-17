package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func storeWith(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Legacy single-credential files upgrade transparently: read as "codex",
// and the next save rewrites the file in the store shape.
func TestAuthStoreLegacyUpgrade(t *testing.T) {
	path := storeWith(t, `{"access":"tok","accountId":"acct","refresh":"ref","expires":0}`)
	a, err := loadCodexAuth(path, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if a.id != "codex" || a.Access != "tok" {
		t.Fatalf("legacy load: %+v", a)
	}
	a.Access = "refreshed"
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	var store authStore
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &store); err != nil {
		t.Fatal(err)
	}
	if len(store) != 1 || store["codex"].Access != "refreshed" {
		t.Fatalf("legacy file not upgraded to store shape: %s", data)
	}
}

// One codex entry stands in for any implicitly requested id, so a
// single-subscription setup works regardless of entry naming.
func TestAuthStoreSingleEntryFallback(t *testing.T) {
	path := storeWith(t, `{"codex-work":{"access":"w","refresh":"r"}}`)
	a, err := loadCodexAuth(path, "codex-personal")
	if err != nil {
		t.Fatal(err)
	}
	if a.id != "codex-work" || a.Access != "w" {
		t.Fatalf("fallback resolution: %+v", a)
	}
}

// With several entries, an implicit id must match — no silent wrong-sub.
func TestAuthStoreAmbiguousImplicitID(t *testing.T) {
	path := storeWith(t, `{"codex-a":{"access":"a"},"codex-b":{"access":"b"}}`)
	if _, err := loadCodexAuth(path, "codex-c"); err == nil || !strings.Contains(err.Error(), "codex-a") {
		t.Fatalf("ambiguous implicit id should error with available ids: %v", err)
	}
	// Explicit resolution is exact.
	a, err := loadCodexAuth(path, "codex-b")
	if err != nil || a.id != "codex-b" {
		t.Fatalf("explicit resolution: %+v %v", a, err)
	}
}

// Saving one entry preserves the others (the multi-sub refresh case).
func TestAuthStoreSavePreservesOthers(t *testing.T) {
	path := storeWith(t, `{"codex-a":{"access":"a","refresh":"ra"},"codex-b":{"access":"b","refresh":"rb"}}`)
	a := &codexAuth{storePath: path, id: "codex-b", Access: "b2", Refresh: "rb", ExpiresAt: time.UnixMilli(1)}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	loadedA, err := loadCodexAuth(path, "codex-a")
	if err != nil || loadedA.Access != "a" {
		t.Fatalf("entry a must survive: %+v %v", loadedA, err)
	}
	loadedB, err := loadCodexAuth(path, "codex-b")
	if err != nil || loadedB.Access != "b2" {
		t.Fatalf("entry b update: %+v %v", loadedB, err)
	}
}

func TestAuthStoreCorruptFileErrors(t *testing.T) {
	path := storeWith(t, `not json at all`)
	if _, err := loadCodexAuth(path, "codex"); err == nil {
		t.Fatal("corrupt store should error")
	}
}
