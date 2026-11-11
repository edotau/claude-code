package pack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/proc"
)

// Spec is what to inline around a task: tracked files under Dirs and Files globs, plus the HEAD diff when Diff is set.
type Spec struct {
	Dirs         []string `json:"dirs,omitempty"`
	Files        []string `json:"files,omitempty"`
	Diff         bool     `json:"diff,omitempty"`
	Index        bool     `json:"index,omitempty"`     // prepend a nested index of every matched path
	MaxFiles     int      `json:"max_files,omitempty"` // 0 = DefaultMaxFiles; IndexOnly inlines none
	MaxFileBytes int      `json:"max_file_bytes,omitempty"`
	IndexOnly    bool     `json:"index_only,omitempty"`
}

// Packed is the built prompt, the collected files, and the lines worth telling the user (coverage, dropped files).
type Packed struct {
	Prompt string
	Files  Context
	Notes  []string
}

// bytesPerToken is dense code's worst case (~2.4 bytes/token) as a ratio, so a byte budget never overruns the window.
const bytesPerTokenNum, bytesPerTokenDen = 12, 5

// BudgetBytes is the byte budget for a prompt of promptTokens.
func BudgetBytes(promptTokens int) int { return promptTokens * bytesPerTokenNum / bytesPerTokenDen }

// maxFiles is the inlined-file cap: 0 for an index-only run, else s.MaxFiles or the default.
func maxFiles(s Spec) int {
	switch {
	case s.IndexOnly:
		return 0
	case s.MaxFiles <= 0:
		return DefaultMaxFiles
	}
	return s.MaxFiles
}

// Build inlines s's files from cwd around task, the whole rendered prompt fitted under budget bytes.
func Build(ctx context.Context, s Spec, task, cwd string, budget int) (Packed, error) {
	files, err := changedOrListed(ctx, s, cwd)
	if err != nil {
		return Packed{}, err
	}
	collected, err := Collect(ctx, cwd, s.Dirs, files, maxFiles(s), s.MaxFileBytes, budget)
	if err != nil {
		return Packed{}, err
	}
	prompt, collected, dropped, err := fit(func(c Context) string {
		if s.Index {
			return task + "\n\n" + indexPrompt(c)
		}
		return task
	}, collected, budget)
	if err != nil {
		return Packed{}, err
	}
	p := Packed{Prompt: prompt, Files: collected}
	if w := coverageWarning(collected); w != "" {
		p.Notes = append(p.Notes, w)
	}
	if len(dropped) > 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("pack: prompt over %d KB; left out whole files: %s", budget/1024, strings.Join(dropped, ", ")))
	}
	return p, nil
}

// changedOrListed returns s.Files, plus files changed vs HEAD when s.Diff is set; a git failure or an empty
// diff is a usage error, not a silent full-tree fallback.
func changedOrListed(ctx context.Context, s Spec, cwd string) ([]string, error) {
	files := append([]string{}, s.Files...)
	if !s.Diff {
		return files, nil
	}
	changed, err := changedFiles(ctx, cwd)
	if err != nil {
		return nil, fmt.Errorf("--diff: %w", err)
	}
	if len(changed) == 0 {
		return nil, errors.New("--diff: nothing changed vs HEAD")
	}
	for _, p := range changed {
		files = append(files, globEscape(p)) // literal paths: a `page[1].go` must match only itself
	}
	return files, nil
}

// changedFiles lists tracked files changed against HEAD (staged + unstaged + untracked), which is
// what a review run is actually about — reviewing the whole tree buries the diff. An error from either
// git command (not found, no HEAD, not a repo, …) is returned with its stderr instead of being masked.
func changedFiles(ctx context.Context, root string) ([]string, error) {
	var all []string
	for _, args := range [][]string{
		// --relative matches ls-files's cwd-relative output, so a subdirectory run doesn't mix bases.
		{"diff", "--name-only", "-z", "--relative", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		out, err := proc.RunGitRaw(ctx, root, args...)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("git -C %s %s: %w", root, strings.Join(args, " "), ctx.Err())
			}
			return nil, err
		}
		all = append(all, strings.Split(out, "\x00")...) // -z: raw names, never quoted or newline-split
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range all {
		if p != "" && !seen[p] {
			seen[p] = true
			uniq = append(uniq, p)
		}
	}
	return uniq, nil
}

// fit renders the prompt, moving whole trailing files to Skipped ("prompt-cap") until it fits limit.
// task is rebuilt per trim so an index reflects what stays inlined; it errors if zero files still exceed limit.
func fit(task func(Context) string, ctx Context, limit int) (string, Context, []string, error) {
	var dropped []string
	for {
		prompt := prompt(task(ctx), ctx)
		excess := len(prompt) - limit
		if excess > 0 && len(ctx.Included) == 0 {
			return prompt, ctx, dropped, fmt.Errorf("pack: task and file inventory alone are %d bytes, over the %d byte cap", len(prompt), limit)
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

// prompt renders the context inventory + inline files + task into the prompt template.
func prompt(task string, ctx Context) string {
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

// Structured path index: a nested JSON tree names every matched path at a fraction of inlining's tokens.

// indexLeaf is what the model needs to decide whether a file is worth pulling.
type indexLeaf struct {
	Bytes int    `json:"bytes,omitempty"`
	Body  string `json:"body,omitempty"` // "inlined" | the skip reason — why the content is or isn't present
}

// index renders ctx as a nested directory tree in JSON, including skipped files (with their reason).
func index(ctx Context) string {
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

// indexPrompt renders the index as a prompt section, framing it as a routing map, not data to summarize.
func indexPrompt(ctx Context) string {
	var b strings.Builder
	b.WriteString("\n## Repository index (paths only)\n\n")
	b.WriteString("A nested map of every matched path. `body` says whether that file's contents are in this\n")
	b.WriteString("prompt (\"inlined\") or why they are not. Use it to name exact paths; if a file you need is\n")
	b.WriteString("listed but not inlined, say which path you need rather than guessing its contents.\n\n```json\n")
	b.WriteString(index(ctx))
	b.WriteString("\n```\n")
	return b.String()
}

// coverageWarning is a one-line summary of what a sweep left out, with its distinct skip reasons; "" when nothing was.
func coverageWarning(ctx Context) string {
	if len(ctx.Skipped) == 0 {
		return ""
	}
	var kb int
	for _, f := range ctx.Included {
		kb += len(f.Content)
	}
	kb /= 1024
	total := len(ctx.Included) + len(ctx.Skipped)
	return fmt.Sprintf("pack: inlined %d of %d files (%d KB); skipped %d: %s",
		len(ctx.Included), total, kb, len(ctx.Skipped), strings.Join(sortedSkipReasons(ctx), ", "))
}

// sortedSkipReasons summarizes why files were left out, so an operator can widen the right knob.
func sortedSkipReasons(ctx Context) []string {
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
