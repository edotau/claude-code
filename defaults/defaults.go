// Package defaults embeds the shipped default settings files.
package defaults

import _ "embed"

// Claude is defaults/claude/settings.json: the Claude Code settings the harness renders (hooks are added at render time).
//
//go:embed claude/settings.json
var Claude []byte
