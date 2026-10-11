// Package toolpattern matches tool names against the glob patterns used by
// action policy rules and MCP tool filters, so every configuration file
// agrees on what a pattern means.
package toolpattern

import "path"

// Valid reports whether pattern can be used with Match.
func Valid(pattern string) bool {
	_, err := path.Match(pattern, "")
	return pattern != "" && err == nil
}

// Match reports whether name matches pattern. "*" matches every name and a
// name always matches itself, whatever characters they contain; otherwise
// pattern uses path.Match syntax, and a malformed one matches nothing.
func Match(pattern, name string) bool {
	if pattern == "*" || pattern == name {
		return true
	}
	matched, err := path.Match(pattern, name)
	return err == nil && matched
}
