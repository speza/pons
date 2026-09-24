package llm

import (
	_ "embed"
	"strings"
)

// harnessPrompt holds the harness rules every agent follows. The agent's
// identity comes from its persona and is framed ahead of them.
//
//go:embed system_prompt.txt
var harnessPrompt string

const defaultPersona = "You are a helpful, capable assistant."

func (b *Brain) systemPrompt() string {
	persona := strings.TrimSpace(b.cfg.Persona)
	if persona == "" {
		persona = defaultPersona
	}
	return "<persona>\n" + persona + "\n</persona>\n\n" + harnessPrompt
}
