package gemini

// Structured path index: a nested JSON tree names every matched path at a fraction of inlining's tokens.

import (
	"encoding/json"
	"sort"
	"strings"
)

// indexLeaf is what the model needs to decide whether a file is worth pulling.
type indexLeaf struct {
	Bytes int    `json:"bytes,omitempty"`
	Body  string `json:"body,omitempty"` // "inlined" | the skip reason — why the content is or isn't present
}

// BuildIndex renders ctx as a nested directory tree in JSON, including skipped files (with their reason).
func BuildIndex(ctx Context) string {
	root := map[string]any{}
	for _, f := range ctx.Included {
		insertPath(root, f.Path, indexLeaf{Bytes: f.Bytes, Body: "inlined"})
	}
	for _, f := range ctx.Skipped {
		insertPath(root, f.Path, indexLeaf{Body: f.Reason})
	}
	data, err := json.MarshalIndent(root, "", " ")
	if err != nil {
		return "{}"
	}
	return string(data)
}

// insertPath nests leaf under its path segments; a segment colliding with an existing leaf is left alone.
func insertPath(root map[string]any, path string, leaf indexLeaf) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	node := root
	for i, p := range parts {
		if i == len(parts)-1 {
			if _, taken := node[p]; !taken {
				node[p] = leaf
			}
			return
		}
		child, ok := node[p].(map[string]any)
		if !ok {
			if _, taken := node[p]; taken {
				return // a file and a directory share a name — keep the first, skip the second
			}
			child = map[string]any{}
			node[p] = child
		}
		node = child
	}
}

// IndexPrompt renders the index as a prompt section, framing it as a routing map, not data to summarize.
func IndexPrompt(ctx Context) string {
	var b strings.Builder
	b.WriteString("\n## Repository index (paths only)\n\n")
	b.WriteString("A nested map of every matched path. `body` says whether that file's contents are in this\n")
	b.WriteString("prompt (\"inlined\") or why they are not. Use it to name exact paths; if a file you need is\n")
	b.WriteString("listed but not inlined, say which path you need rather than guessing its contents.\n\n```json\n")
	b.WriteString(BuildIndex(ctx))
	b.WriteString("\n```\n")
	return b.String()
}

// IndexPathCount reports how many files the index covers, for the operator-facing summary line.
func IndexPathCount(ctx Context) int { return len(ctx.Included) + len(ctx.Skipped) }

// SortedSkipReasons summarizes why files were left out, so an operator can widen the right knob.
func SortedSkipReasons(ctx Context) []string {
	counts := map[string]int{}
	for _, f := range ctx.Skipped {
		counts[f.Reason]++
	}
	out := make([]string, 0, len(counts))
	for reason := range counts {
		out = append(out, reason)
	}
	sort.Strings(out)
	return out
}
