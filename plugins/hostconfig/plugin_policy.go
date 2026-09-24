package hostconfig

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/samperrin/pons/plugins/actionpolicy"
)

type actionPolicySettings struct {
	Classifier               string   `json:"classifier,omitempty"`
	MinSafeConfidence        *float64 `json:"min_safe_confidence,omitempty"`
	AllowGeneratedConfidence bool     `json:"allow_generated_confidence,omitempty"`

	id string
}

func decodeActionPolicyPlugin(id string, enabled bool, raw json.RawMessage) (ConfiguredPlugin, error) {
	config, err := decodePluginObject[actionPolicySettings](id, raw)
	if err != nil {
		return nil, err
	}
	if enabled && config.Classifier == "" {
		return nil, fmt.Errorf("config plugins.%s.classifier is required when enabled", id)
	}
	if config.MinSafeConfidence != nil &&
		(*config.MinSafeConfidence <= 0 || *config.MinSafeConfidence > 1 || math.IsNaN(*config.MinSafeConfidence)) {
		return nil, fmt.Errorf("config plugins.%s.min_safe_confidence must be greater than 0 and at most 1", id)
	}
	config.id = id
	return config, nil
}

func (p actionPolicySettings) ID() string         { return p.id }
func (p actionPolicySettings) Provides() []string { return nil }
func (p actionPolicySettings) Requires() []string {
	return []string{actionpolicy.ClassifierCapability(p.Classifier)}
}

func (p actionPolicySettings) Build(build *BuildContext) error {
	policy := actionpolicy.Policy{
		ClassifierID:             p.Classifier,
		AllowGeneratedConfidence: p.AllowGeneratedConfidence,
	}
	if p.MinSafeConfidence != nil {
		policy.MinSafeConfidence = *p.MinSafeConfidence
	}
	build.AddHostPlugin(policy)
	return nil
}
