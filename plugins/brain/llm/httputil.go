// Shared plumbing for the LLM adapters.
package llm

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/samperrin/pons"
)

// defaultHTTPClient is generous: long model generations and long tool loops.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// jsonSchema renders a registered tool's JSON-Schema object.  The Params
// form remains for built-ins written before typed external arguments existed.
func jsonSchema(spec pons.ToolSpec) map[string]any {
	if len(spec.InputSchema) > 0 {
		var schema map[string]any
		if err := json.Unmarshal(spec.InputSchema, &schema); err == nil && schema != nil {
			return schema
		}
	}
	return paramsSchema(spec.Params)
}

func paramsSchema(params []pons.ToolParam) map[string]any {
	props := make(map[string]any, len(params))
	var required []string
	for _, p := range params {
		props[p.Name] = map[string]any{"type": p.Type, "description": p.Description}
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func truncateMsg(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSuffix(s[:n], "") + "…"
}
