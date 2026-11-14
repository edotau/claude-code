package doctor

import (
	"regexp"
	"slices"
	"strings"
)

var (
	needNoEdit    = regexp.MustCompile(`(?i)\b(edit|modify|change) nothing\b|read-only|(do not|never) (edit|modify)\b`)
	needNewFile   = regexp.MustCompile(`(?i)\bNEW file\b|\bmktemp\b|\bwrite (a|the|your) (full )?(new |report|file)`)
	needTests     = regexp.MustCompile(`(?i)\bgo test\b|\b(scoped|failing|the|new|fast) tests?\b|\btest suite\b`)
	needGoVet     = regexp.MustCompile(`\bgo vet\b`)
	needHarnessCL = regexp.MustCompile(`claude-code (\w+)(?: ([a-z][\w-]*))?`)
	needRunTick   = regexp.MustCompile("(?i)\\brun(?:ning)? (?:exactly |once: )?\\\\?`([^`$\\\\]+)")
	declStart     = `(?m)^(?:async function|function|const|let|var) %s\b`
	declEnd       = regexp.MustCompile(`(?m)^(?:async function|function|const|let|var|export|class) `)
	callOrIndex   = regexp.MustCompile(`\b([A-Za-z_]\w{3,})[(\[]`)
)

// promptText is the argument of every `<x>.<stage>.task(` call in src (spans), with ok=false when the stage has no
// such call (a shared helper builds its prompt); full adds the declarations the spans name, depth levels deep.
func promptText(src, stage string, depth int) (spans []string, full string, ok bool) {
	var text strings.Builder
	call := regexp.MustCompile(`\b\w+\.` + regexp.QuoteMeta(stage) + `\.task\(`)
	locs := call.FindAllStringIndex(src, -1)
	for _, loc := range locs {
		end := strings.Index(src[loc[1]:], "."+stage+".schema(")
		if end < 0 {
			end = min(1500, len(src)-loc[1])
		}
		spans = append(spans, src[loc[1]:loc[1]+end])
		text.WriteString(spans[len(spans)-1])
		text.WriteByte('\n')
	}
	direct := text.String()
	seen := map[string]bool{}
	frontier := direct
	for range depth { // a prompt builder, then the table it indexes
		var next strings.Builder
		for _, m := range callOrIndex.FindAllStringSubmatch(frontier, -1) {
			if !seen[m[1]] {
				seen[m[1]] = true
				next.WriteString(declBody(src, m[1]))
			}
		}
		text.WriteString(next.String())
		frontier = next.String()
	}
	return spans, text.String(), len(locs) > 0
}

// declBody is the top-level declaration of name in src, up to the next top-level declaration; "" when there is none.
func declBody(src, name string) string {
	loc := regexp.MustCompile(strings.Replace(declStart, "%s", regexp.QuoteMeta(name), 1)).FindStringIndex(src)
	if loc == nil {
		return ""
	}
	rest := src[loc[1]:]
	if n := declEnd.FindStringIndex(rest); n != nil {
		rest = rest[:n[0]]
	}
	return rest + "\n"
}

// stageTextNeeds is what a stage's prompt tells its agent to do. direct and full come from promptText; a stage with
// no direct prompt (ok false) keeps its declared tier's needs. An edit-tier stage whose prompt says "edit nothing"
// does not edit; a command need is `run:<cmd>`, judged as a Bash call (the nested launcher `agents ask` is no need).
func stageTextNeeds(spans []string, full string, ok bool, tools string) []string {
	needs := []string{"read"}
	direct := strings.Join(spans, "\n")
	anyEdits := func(s string) bool { return !needNoEdit.MatchString(s) }
	editing := tools == "edit" && (!ok || slices.ContainsFunc(spans, anyEdits))
	if editing {
		needs = append(needs, "edit")
	}
	if needNewFile.MatchString(full) {
		needs = append(needs, "write")
	}
	if editing && (!ok || needTests.MatchString(direct)) {
		needs = append(needs, "tests")
	}
	if ok && needGoVet.MatchString(direct) {
		needs = append(needs, "vet")
	}
	return append(needs, commandNeeds(full)...)
}

// commandNeeds lists `run:<cmd>` for every harness CLI call and every `python ...` the text tells the agent to run.
func commandNeeds(text string) []string {
	var out []string
	add := func(cmd string) {
		if cmd = "run:" + strings.TrimSpace(cmd); !slices.Contains(out, cmd) {
			out = append(out, cmd)
		}
	}
	for _, m := range needHarnessCL.FindAllStringSubmatch(text, -1) {
		if m[0] != "claude-code agents ask" {
			add(m[0])
		}
	}
	for _, m := range needRunTick.FindAllStringSubmatch(text, -1) {
		if strings.HasPrefix(m[1], "python") {
			add(m[1])
		}
	}
	return out
}
