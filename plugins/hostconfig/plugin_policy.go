package hostconfig

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"

	"github.com/samperrin/pons"
	"github.com/samperrin/pons/plugins/actionpolicy"
)

type actionPolicyRule struct {
	Action     pons.Permission `json:"action"`
	Tools      []string        `json:"tools"`
	ReasonCode string          `json:"reason_code,omitempty"`
}

type actionPolicySettings struct {
	Classifier               string             `json:"classifier,omitempty"`
	MinSafeConfidence        *float64           `json:"min_safe_confidence,omitempty"`
	AllowGeneratedConfidence bool               `json:"allow_generated_confidence,omitempty"`
	Rules                    []actionPolicyRule `json:"rules,omitempty"`

	id string
}

func decodeActionPolicyPlugin(id string, enabled bool, raw json.RawMessage) (ConfiguredPlugin, error) {
	config, err := decodePluginObject[actionPolicySettings](id, raw)
	if err != nil {
		return nil, err
	}
	if enabled && config.Classifier == "" && len(config.Rules) == 0 {
		return nil, fmt.Errorf("config plugins.%s needs a classifier or rules when enabled", id)
	}
	if config.MinSafeConfidence != nil &&
		(*config.MinSafeConfidence <= 0 || *config.MinSafeConfidence > 1 || math.IsNaN(*config.MinSafeConfidence)) {
		return nil, fmt.Errorf("config plugins.%s.min_safe_confidence must be greater than 0 and at most 1", id)
	}
	for i, rule := range config.Rules {
		switch rule.Action {
		case pons.PermissionAllow, pons.PermissionAsk, pons.PermissionDeny:
		default:
			return nil, fmt.Errorf("config plugins.%s.rules[%d].action must be allow, ask, or deny", id, i)
		}
		if len(rule.Tools) == 0 {
			return nil, fmt.Errorf("config plugins.%s.rules[%d].tools must not be empty", id, i)
		}
		if slices.Contains(rule.Tools, "") {
			return nil, fmt.Errorf("config plugins.%s.rules[%d].tools must not contain empty names", id, i)
		}
	}
	config.id = id
	return config, nil
}

func (p actionPolicySettings) ID() string         { return p.id }
func (p actionPolicySettings) Provides() []string { return nil }
func (p actionPolicySettings) Requires() []string {
	if p.Classifier == "" {
		return nil
	}
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
	for _, rule := range p.Rules {
		reason := rule.ReasonCode
		if reason == "" {
			reason = "rule_" + string(rule.Action)
		}
		policy.Rules = append(policy.Rules, actionpolicy.ToolRule(rule.Action, reason, rule.Tools...))
	}
	build.AddHostPlugin(policy)
	return nil
}
