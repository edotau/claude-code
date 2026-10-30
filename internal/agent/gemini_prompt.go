package agent

import (
	"fmt"
	"strconv"
	"strings"
)

// PromptCap bounds the whole rendered prompt (task, inventory, tags), not just file bodies; over it agy cuts the tail.
const PromptCap = DefaultTotalBytes

// FitPrompt renders the prompt, moving whole trailing files to Skipped ("prompt-cap") until it fits limit.
// task is rebuilt per trim so an index reflects what stays inlined; it errors if zero files still exceed limit.
func FitPrompt(task func(Context) string, ctx Context, limit int) (string, Context, []string, error) {
	var dropped []string
	for {
		prompt := BuildPrompt(task(ctx), ctx)
		excess := len(prompt) - limit
		if excess > 0 && len(ctx.Included) == 0 {
			return prompt, ctx, dropped, fmt.Errorf("gemini bridge: task and file inventory alone are %d bytes, over the %d byte cap", len(prompt), limit)
		}
		if excess <= 0 {
			return prompt, ctx, dropped, nil
		}
		inc := append([]IncludedFile(nil), ctx.Included...)
		skipped := append([]SkippedFile(nil), ctx.Skipped...)
		for freed := 0; freed < excess && len(inc) > 0; {
			f := inc[len(inc)-1]
			inc = inc[:len(inc)-1]
			skipped = append(skipped, SkippedFile{f.Path, "prompt-cap"})
			dropped = append(dropped, f.Path)
			freed += len(promptBlock(f)) + 2 + len(inventoryLine(f)) // its block, separator and inventory line
		}
		ctx.Included, ctx.Skipped = inc, skipped
	}
}

func inventoryLine(f IncludedFile) string {
	return fmt.Sprintf("- %s | %s | %d bytes | truncated=%t\n", f.Path, f.MediaType, f.Bytes, f.Truncated)
}

func promptBlock(f IncludedFile) string {
	return fmt.Sprintf("<file path=%q media_type=%q truncated=%q>%s</file>",
		f.Path, f.MediaType, strconv.FormatBool(f.Truncated), f.Content)
}

// BuildPrompt renders the context inventory + inline files + task into the bridge's prompt template.
func BuildPrompt(task string, ctx Context) string {
	var inv strings.Builder
	if len(ctx.Included) > 0 {
		inv.WriteString("Included files:\n")
		for _, f := range ctx.Included {
			inv.WriteString(inventoryLine(f))
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
			blocks.WriteString(promptBlock(f))
		}
	}

	// Task and constraints lead: agy caps its input and cuts the tail, which must never be the question.
	return fmt.Sprintf(`<task>
%s
</task>

<constraints>
- Use the provided workspace context when it is relevant.
- Cite file paths when referring to evidence from inline context.
- Call out when the context is partial, skipped, or truncated (name the last file you saw in full).
- Do not invent files or data that are not present in the provided payloads.
</constraints>

<context_inventory>
%s</context_inventory>

<context_files>
%s
</context_files>`, task, inv.String(), blocks.String())
}
