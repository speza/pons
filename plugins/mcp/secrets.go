package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/samperrin/pons/internal/advisorylock"
)

var (
	secretNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	secretRefPattern  = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_-]{1,64})\}`)
)

// minRedactedSecret is the shortest secret value scrubbed from server output.
// Shorter values would turn ordinary text into noise.
const minRedactedSecret = 8

// ValidSecretName reports whether name can be stored and referenced.
func ValidSecretName(name string) bool { return secretNamePattern.MatchString(name) }

// LoadSecrets reads the named secrets. A missing file is an empty store. The
// file must be a regular file only its owner can read.
func LoadSecrets(path string) (map[string]string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("secret store %q must be a regular file readable only by its owner", path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("secret store: %w", err)
	}
	return decodeSecrets(data, path)
}

func decodeSecrets(data []byte, path string) (map[string]string, error) {
	secrets := map[string]string{}
	if len(bytes.TrimSpace(data)) == 0 {
		return secrets, nil
	}
	if err := json.Unmarshal(data, &secrets); err != nil {
		// A decode error can quote the file, so it is not passed on.
		return nil, fmt.Errorf("secret store %q is not a JSON object of names to values", path)
	}
	return secrets, nil
}

// SetSecret stores one named secret, keeping every other entry. Concurrent
// pons processes serialize on an advisory lock of the store file.
func SetSecret(path, name, value string) error {
	if !ValidSecretName(name) {
		return fmt.Errorf("secret name %q must match %s", name, secretNamePattern)
	}
	if value == "" {
		return errors.New("secret value must not be empty")
	}
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("secret value must be a single line")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("secret store %q must be a regular file", path)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	defer file.Close()
	defer advisorylock.Exclusive(file)()

	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	secrets, err := decodeSecrets(data, path)
	if err != nil {
		return err
	}
	secrets[name] = value
	encoded, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	if err := errors.Join(file.Truncate(0), file.Chmod(0o600)); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	if _, err := file.WriteAt(append(encoded, '\n'), 0); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	return file.Sync()
}

// validateHeaderValue accepts ${secret:<name>} references and rejects every
// other ${...} form: environment expansion is not supported.
func validateHeaderValue(value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("value must be a single line")
	}
	if strings.Contains(secretRefPattern.ReplaceAllString(value, ""), "${") {
		return errors.New("only ${secret:<name>} references are supported; environment expansion is not")
	}
	return nil
}

// resolveHeaders substitutes secret references and returns the headers to
// send with the secret values they contain.
func resolveHeaders(headers, secrets map[string]string) (resolved map[string]string, used []string, err error) {
	resolved = make(map[string]string, len(headers))
	for header, value := range headers {
		var missing string
		resolved[header] = secretRefPattern.ReplaceAllStringFunc(value, func(reference string) string {
			name := secretRefPattern.FindStringSubmatch(reference)[1]
			secret, ok := secrets[name]
			if !ok || secret == "" {
				missing = name
				return ""
			}
			used = append(used, secret)
			return secret
		})
		if missing != "" {
			return nil, nil, fmt.Errorf("secret %q is not set; run: pons mcp set-secret %s", missing, missing)
		}
		if strings.ContainsAny(resolved[header], "\r\n") {
			return nil, nil, fmt.Errorf("header %q resolves to more than one line", header)
		}
	}
	return resolved, used, nil
}

// redactor replaces secret values in text a server returned.
type redactor struct{ replacer *strings.Replacer }

func newRedactor(secrets []string) redactor {
	var pairs []string
	for _, secret := range secrets {
		if len(secret) >= minRedactedSecret {
			pairs = append(pairs, secret, "[secret]")
		}
	}
	if len(pairs) == 0 {
		return redactor{}
	}
	return redactor{replacer: strings.NewReplacer(pairs...)}
}

func (r redactor) text(value string) string {
	if r.replacer == nil {
		return value
	}
	return r.replacer.Replace(value)
}
