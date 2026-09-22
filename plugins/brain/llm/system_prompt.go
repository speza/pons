package llm

import (
	_ "embed"
	"strings"
)

//go:embed system_prompt.txt
var baseSystemPrompt string

func (b *Brain) systemPrompt() string {
	if strings.TrimSpace(b.cfg.SystemExtra) == "" {
		return baseSystemPrompt
	}
	return baseSystemPrompt + "\n<additional_instructions>\n" + b.cfg.SystemExtra + "\n</additional_instructions>\n"
}
