package gemini

import (
	"fmt"
	"strings"
)

// BuildPrompt renders the context inventory + inline files + task into the bridge's prompt template.
func BuildPrompt(task string, ctx Context) string {
	var inv strings.Builder
	if len(ctx.Included) > 0 {
		inv.WriteString("Included files:\n")
		for _, f := range ctx.Included {
			fmt.Fprintf(&inv, "- %s | %s | %d bytes | truncated=%t\n", f.Path, f.MediaType, f.Bytes, f.Truncated)
		}
	} else {
		inv.WriteString("Included files: none\n")
	}
	if len(ctx.Skipped) > 0 {
		inv.WriteString("Skipped files:\n")
		for _, s := range ctx.Skipped {
			fmt.Fprintf(&inv, "- %s (%s)\n", s.Path, s.Reason)
		}
	}

	var blocks strings.Builder
	if len(ctx.Included) == 0 {
		blocks.WriteString("No inline file payloads were collected.")
	} else {
		for i, f := range ctx.Included {
			if i > 0 {
				blocks.WriteString("\n\n")
			}
			fmt.Fprintf(&blocks, "<file path=%q media_type=%q truncated=%q>%s</file>",
				f.Path, f.MediaType, fmt.Sprintf("%t", f.Truncated), f.Content)
		}
	}

	return fmt.Sprintf(`<context_inventory>
%s</context_inventory>

<context_files>
%s
</context_files>

<task>
%s
</task>

<constraints>
- Use the provided workspace context when it is relevant.
- Cite file paths when referring to evidence from inline context.
- Call out when the context is partial, skipped, or truncated.
- Do not invent files or data that are not present in the provided payloads.
</constraints>`, inv.String(), blocks.String(), task)
}
