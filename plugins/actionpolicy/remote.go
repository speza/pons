package actionpolicy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/samperrin/pons"
)

const maxClassifierResponse = 1 << 20

// RemoteConfig configures an opt-in hosted classifier. Endpoint overrides are
// intended for testing or a trusted host proxy.
type RemoteConfig struct {
	APIKey     string
	Endpoint   string
	Model      string
	HTTPClient *http.Client
	Timeout    time.Duration
}

type remoteClient struct {
	key, endpoint, model string
	client               *http.Client
	timeout              time.Duration
}

func newRemoteClient(config RemoteConfig, endpoint, model string) (remoteClient, error) {
	if config.APIKey == "" {
		return remoteClient{}, errors.New("actionpolicy: classifier API key is required")
	}
	if config.Endpoint != "" {
		endpoint = config.Endpoint
	}
	if config.Model != "" {
		model = config.Model
	}
	client := config.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	timeout := config.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if timeout < 0 {
		return remoteClient{}, errors.New("actionpolicy: classifier timeout must be positive")
	}
	return remoteClient{config.APIKey, endpoint, model, client, timeout}, nil
}

func (c remoteClient) post(ctx context.Context, payload any, destination any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("actionpolicy: encode classifier request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("actionpolicy: create classifier request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return fmt.Errorf("actionpolicy: classifier request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("actionpolicy: classifier HTTP status %d", response.StatusCode)
	}
	body, err = io.ReadAll(io.LimitReader(response.Body, maxClassifierResponse+1))
	if err != nil {
		return fmt.Errorf("actionpolicy: read classifier response: %w", err)
	}
	if len(body) > maxClassifierResponse {
		return errors.New("actionpolicy: classifier response too large")
	}
	if err := json.Unmarshal(body, destination); err != nil {
		return fmt.Errorf("actionpolicy: decode classifier response: %w", err)
	}
	return nil
}

// classifierState omits the brain's self-reported danger field. Source labels
// keep user instructions distinguishable from tool output and repository text.
func classifierState(req pons.ToolCallStartEvent) map[string]any {
	return map[string]any{
		"current_user_message": req.Message,
		"recent_context":       req.RecentContext,
		"action": map[string]any{
			"id": req.Action.ID, "kind": req.Action.Kind, "args": req.Action.Args,
		},
		"tool": req.Tool, "resources": req.Resources,
		"resource_error": req.ResourceError,
		"workspace":      req.Workspace, "platform": req.Platform,
		"environment": req.Environment,
	}
}

func validAssessment(a pons.ActionAssessment) bool {
	return (a.Risk == "safe" || a.Risk == "review") &&
		!math.IsNaN(a.Confidence) && !math.IsInf(a.Confidence, 0) &&
		a.Confidence >= 0 && a.Confidence <= 1
}
