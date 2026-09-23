// Package gitworkspace defines provider-neutral Git workspace policy and
// transient credentials. Environment providers remain responsible for running
// the resulting Git operations in their own execution boundary.
package gitworkspace

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"

	"github.com/samperrin/pons/environment"
	"github.com/samperrin/pons/protocol"
)

// CredentialSource returns transient Git process credentials for a run.
// Long-lived provider credentials remain on the host.
type CredentialSource interface {
	Credentials(context.Context, string, bool) (Credentials, error)
}

// Credentials are explicit process environment and values to redact before a
// tool result crosses back into host persistence. A zero value means public
// repository access.
type Credentials struct {
	environment     map[string]string
	sensitiveValues []string
}

// HTTPSBasicCredentials configures Git's transient HTTPS Authorization header.
// scope is the credential-free URL prefix to which Git should send the header.
func HTTPSBasicCredentials(scope, username, password string) Credentials {
	credential := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	header := "Authorization: Basic " + credential
	sensitiveValues := []string{header, credential}
	if password != "" {
		sensitiveValues = append(sensitiveValues, password)
	}
	return Credentials{
		environment: map[string]string{
			"GIT_CONFIG_COUNT":    "1",
			"GIT_CONFIG_KEY_0":    "http." + scope + ".extraheader",
			"GIT_CONFIG_VALUE_0":  header,
			"GIT_TERMINAL_PROMPT": "0",
		},
		sensitiveValues: sensitiveValues,
	}
}

// Environment returns a copy suitable for one Git or hands process.
func (c Credentials) Environment() map[string]string {
	return maps.Clone(c.environment)
}

// RedactResult removes exact credential representations before persistence.
// It is an accidental-leak safeguard, not a boundary against unrestricted
// hands deliberately transforming delegated credentials.
func (c Credentials) RedactResult(result protocol.ToolResult) protocol.ToolResult {
	if len(c.sensitiveValues) == 0 {
		return result
	}
	result.Output = c.Redact(result.Output)
	result.Error = c.Redact(result.Error)
	if len(result.Payload) != 0 {
		result.Payload = []byte(c.Redact(string(result.Payload)))
	}
	return result
}

// RedactError keeps the original error when it contains no credential. A
// credential-bearing error must lose its wrapped cause before leaving hands.
func (c Credentials) RedactError(err error) error {
	if err == nil || len(c.sensitiveValues) == 0 {
		return err
	}
	redacted := c.Redact(err.Error())
	if redacted == err.Error() {
		return err
	}
	return errors.New(redacted)
}

// Redact replaces exact credential representations in value.
func (c Credentials) Redact(value string) string {
	for _, sensitive := range c.sensitiveValues {
		value = strings.ReplaceAll(value, sensitive, "[REDACTED]")
	}
	return value
}

// ValidatePlan checks the provider-neutral git/v1 wire contract. Providers
// separately enforce placement requirements such as network availability.
func ValidatePlan(plan environment.WorkspacePlan) error {
	if plan.Strategy == "" {
		if plan.SourceRef != "" || plan.BaseRevision != "" {
			return errors.New("environment: workspace plan strategy is required")
		}
		return nil
	}

	if plan.Strategy != environment.WorkspaceStrategyGit {
		return fmt.Errorf("environment: unsupported workspace strategy %q", plan.Strategy)
	}

	repository, err := url.Parse(plan.SourceRef)
	if err != nil || repository.Scheme != "https" || repository.Host == "" ||
		repository.User != nil || repository.RawQuery != "" || repository.Fragment != "" {
		return errors.New("environment: git/v1 source must be a credential-free HTTPS repository URL without query or fragment")
	}

	if !isFullGitObjectID(plan.BaseRevision) {
		return errors.New("environment: git/v1 base revision must be a full 40-character SHA-1 commit ID")
	}
	return nil
}

// BranchName derives an independent work branch from the logical workspace.
func BranchName(workspaceID string) string {
	digest := sha256.Sum256([]byte(workspaceID))
	return "pons/" + hex.EncodeToString(digest[:8]) + "/work"
}

func isFullGitObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'f':
		case char >= 'A' && char <= 'F':
		default:
			return false
		}
	}
	return true
}
