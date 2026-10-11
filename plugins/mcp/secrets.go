package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	if secrets == nil {
		secrets = map[string]string{}
	}
	return secrets, nil
}

// SetSecret stores one named secret, keeping every other entry. Concurrent
// pons processes serialize on an advisory lock beside the store, and the
// store is replaced in one step so a reader never sees it half written.
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

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	defer lock.Close()
	defer advisorylock.Exclusive(lock)()

	secrets, err := LoadSecrets(path)
	if err != nil {
		return err
	}
	secrets[name] = value
	encoded, err := json.MarshalIndent(secrets, "", "  ")
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}

	// CreateTemp makes the file readable only by its owner.
	temp, err := os.CreateTemp(dir, ".secrets-*")
	if err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(append(encoded, '\n'))
	if err := errors.Join(writeErr, temp.Sync(), temp.Close()); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return fmt.Errorf("secret store: %w", err)
	}
	return nil
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

// newRedactor also covers each secret as it appears inside a JSON string,
// where characters such as & and " are escaped.
func newRedactor(secrets []string) redactor {
	var pairs []string
	for _, secret := range secrets {
		if len(secret) < minRedactedSecret {
			continue
		}
		for _, form := range []string{jsonString(secret, true), jsonString(secret, false), secret} {
			pairs = append(pairs, form, "[secret]")
		}
	}
	if len(pairs) == 0 {
		return redactor{}
	}
	return redactor{replacer: strings.NewReplacer(pairs...)}
}

// jsonString returns value as the contents of a JSON string literal.
func jsonString(value string, escapeHTML bool) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(escapeHTML)
	if err := encoder.Encode(value); err != nil {
		return value
	}
	quoted := strings.TrimSpace(buffer.String())
	return strings.TrimSuffix(strings.TrimPrefix(quoted, `"`), `"`)
}

func (r redactor) text(value string) string {
	if r.replacer == nil {
		return value
	}
	return r.replacer.Replace(value)
}
