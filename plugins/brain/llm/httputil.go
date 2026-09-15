// Shared plumbing for the LLM adapters.
package llm

import (
	"net/http"
	"strings"
	"time"

	"github.com/samperrin/pons"
)

// defaultHTTPClient is generous: long model generations and long tool loops.
func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Minute}
}

// jsonSchema renders []pons.ToolParam as a JSON-Schema object (shared by the
// openai chat and responses adapters; anthropic has its own renderer).
func jsonSchema(params []pons.ToolParam) map[string]any {
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
