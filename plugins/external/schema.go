package external

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

// ValidateToolSchema validates the intentionally small JSON-Schema subset
// promised by tool_provider/v1.  The runtime transports schemas but does not
// reinterpret them; composition fails rather than silently dropping a
// keyword a model or SDK might rely on.
func ValidateToolSchema(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("input_schema is required")
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("input_schema is not valid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("input_schema contains trailing JSON")
		}
		return fmt.Errorf("input_schema has trailing data: %w", err)
	}

	object, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("input_schema root must be an object")
	}
	return validateSchemaObject(object, "$", true)
}

var schemaKeywords = map[string]bool{
	"type":                 true,
	"properties":           true,
	"required":             true,
	"description":          true,
	"enum":                 true,
	"items":                true,
	"additionalProperties": true,
}

var schemaTypes = map[string]bool{
	"object":  true,
	"string":  true,
	"integer": true,
	"number":  true,
	"boolean": true,
	"array":   true,
	"null":    true,
}

func validateSchemaObject(schema map[string]any, path string, root bool) error {
	for key := range schema {
		if !schemaKeywords[key] {
			return fmt.Errorf("%s uses unsupported keyword %q", path, key)
		}
	}
	typeName, ok := schema["type"].(string)
	if !ok || !schemaTypes[typeName] {
		return fmt.Errorf("%s.type must be one of object, string, integer, number, boolean, array, or null", path)
	}
	if root && typeName != "object" {
		return fmt.Errorf("input_schema root type must be object")
	}
	if description, ok := schema["description"]; ok {
		if _, ok := description.(string); !ok {
			return fmt.Errorf("%s.description must be a string", path)
		}
	}
	if enum, ok := schema["enum"]; ok {
		values, ok := enum.([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("%s.enum must be a non-empty array", path)
		}
	}

	if props, exists := schema["properties"]; exists {
		if typeName != "object" {
			return fmt.Errorf("%s.properties is only valid for object schemas", path)
		}
		properties, ok := props.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.properties must be an object", path)
		}
		keys := make([]string, 0, len(properties))
		for name := range properties {
			keys = append(keys, name)
		}
		sort.Strings(keys)
		for _, name := range keys {
			child, ok := properties[name].(map[string]any)
			if !ok {
				return fmt.Errorf("%s.properties[%q] must be an object", path, name)
			}
			if err := validateSchemaObject(child, path+".properties["+name+"]", false); err != nil {
				return err
			}
		}
	}

	if required, exists := schema["required"]; exists {
		if typeName != "object" {
			return fmt.Errorf("%s.required is only valid for object schemas", path)
		}
		values, ok := required.([]any)
		if !ok {
			return fmt.Errorf("%s.required must be an array of strings", path)
		}
		properties, _ := schema["properties"].(map[string]any)
		seen := make(map[string]bool, len(values))
		for _, value := range values {
			name, ok := value.(string)
			if !ok || name == "" {
				return fmt.Errorf("%s.required must be an array of strings", path)
			}
			if seen[name] {
				return fmt.Errorf("%s.required contains duplicate %q", path, name)
			}
			seen[name] = true
			if properties == nil {
				return fmt.Errorf("%s.required references %q but properties is absent", path, name)
			}
			if _, ok := properties[name]; !ok {
				return fmt.Errorf("%s.required references unknown property %q", path, name)
			}
		}
	}

	if items, exists := schema["items"]; exists {
		if typeName != "array" {
			return fmt.Errorf("%s.items is only valid for array schemas", path)
		}
		child, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%s.items must be an object schema", path)
		}
		if err := validateSchemaObject(child, path+".items", false); err != nil {
			return err
		}
	} else if typeName == "array" {
		return fmt.Errorf("%s.items is required for array schemas", path)
	}

	if extra, exists := schema["additionalProperties"]; exists {
		if typeName != "object" {
			return fmt.Errorf("%s.additionalProperties is only valid for object schemas", path)
		}
		allowed, ok := extra.(bool)
		if !ok || allowed {
			return fmt.Errorf("%s.additionalProperties must be false in tool_provider/v1", path)
		}
	}
	return nil
}

func validateCapability(cap Capability, placement Placement, maxTools int) (ToolProviderConfiguration, error) {
	if cap.Type == "" {
		return ToolProviderConfiguration{}, fmt.Errorf("capability type is required")
	}
	if cap.Type != CapabilityToolProvider || cap.Version != ToolProviderVersion {
		return ToolProviderConfiguration{}, fmt.Errorf("unsupported capability %q/v%d", cap.Type, cap.Version)
	}
	if placement != PlacementHands {
		return ToolProviderConfiguration{}, fmt.Errorf("capability %q/v%d is only valid in hands placement", cap.Type, cap.Version)
	}

	var cfg ToolProviderConfiguration
	if len(cap.Configuration) == 0 || string(cap.Configuration) == "null" {
		return ToolProviderConfiguration{}, fmt.Errorf("tool_provider configuration is required")
	}
	if err := json.Unmarshal(cap.Configuration, &cfg); err != nil {
		return ToolProviderConfiguration{}, fmt.Errorf("tool_provider configuration: %w", err)
	}

	// The wire distinguishes an omitted value (serial by default) from an
	// explicit zero (unbounded, subject to host limits). The public Go struct
	// keeps an int for ergonomic construction, so detect presence here.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(cap.Configuration, &fields); err != nil || fields == nil {
		if err == nil {
			err = fmt.Errorf("value is null")
		}
		return ToolProviderConfiguration{}, fmt.Errorf("tool_provider configuration must be an object: %w", err)
	}
	if _, present := fields["max_concurrency"]; !present {
		cfg.MaxConcurrency = 1
	}
	if cfg.MaxConcurrency < 0 {
		return ToolProviderConfiguration{}, fmt.Errorf("tool_provider max_concurrency must be non-negative")
	}
	if maxTools > 0 && len(cfg.Tools) > maxTools {
		return ToolProviderConfiguration{}, fmt.Errorf("tool_provider advertises %d tools; host limit is %d", len(cfg.Tools), maxTools)
	}

	seen := make(map[string]bool, len(cfg.Tools))
	for i, tool := range cfg.Tools {
		if tool.Kind == "" {
			return ToolProviderConfiguration{}, fmt.Errorf("tool_provider tool %d has an empty kind", i)
		}
		if tool.Kind == "finish" {
			return ToolProviderConfiguration{}, fmt.Errorf("tool_provider tool %q uses the core-reserved finish kind", tool.Kind)
		}
		if seen[tool.Kind] {
			return ToolProviderConfiguration{}, fmt.Errorf("tool_provider advertises duplicate tool kind %q", tool.Kind)
		}
		seen[tool.Kind] = true
		if tool.Description == "" {
			return ToolProviderConfiguration{}, fmt.Errorf("tool_provider tool %q has an empty description", tool.Kind)
		}
		if err := ValidateToolSchema(tool.InputSchema); err != nil {
			return ToolProviderConfiguration{}, fmt.Errorf("tool %q: %w", tool.Kind, err)
		}
	}
	return cfg, nil
}
