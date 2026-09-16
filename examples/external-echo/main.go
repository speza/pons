// Command external-echo is a tiny hands-side pons tool provider. Build it
// beside plugin.json, then activate that manifest with:
//
//	pons --plugin ./plugin.json
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/samperrin/pons/plugins/external/sdk"
	"github.com/samperrin/pons/protocol"
)

const echoSchema = `{
  "type": "object",
  "properties": {
    "text": {"type": "string", "description": "Text to echo"},
    "loud": {"type": "boolean", "description": "Upper-case the text"},
    "repeat": {"type": "integer", "description": "Number of copies"}
  },
  "required": ["text"],
  "additionalProperties": false
}`

type echoInput struct {
	Text   string `json:"text"`
	Loud   bool   `json:"loud,omitempty"`
	Repeat int    `json:"repeat,omitempty"`
}

func main() {
	err := sdk.Serve(context.Background(), os.Stdin, os.Stdout, sdk.Server{
		Name:    "example.echo",
		Version: "1.0.0",
		Tools: []sdk.Tool{{
			Kind:        "echo_text",
			Description: "Echo typed text without running in the pons process.",
			InputSchema: []byte(echoSchema),
			Handler: func(ctx context.Context, action protocol.Action) (protocol.ToolResult, error) {
				var input echoInput
				if err := action.DecodeArgs(&input); err != nil {
					return protocol.ToolResult{OK: false, Error: "invalid echo input: " + err.Error()}, nil
				}
				if input.Repeat <= 0 {
					input.Repeat = 1
				}
				text := input.Text
				if input.Loud {
					text = strings.ToUpper(text)
				}
				return protocol.ToolResult{OK: true, Output: strings.Repeat(text, input.Repeat)}, nil
			},
		}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "external-echo:", err)
		os.Exit(1)
	}
}
