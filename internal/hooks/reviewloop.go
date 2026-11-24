package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

// unfixedRules is the one table of hand-back phrases that report a finding left unfixed or a decision for the owner.
var unfixedRules = []struct{ match, unless *regexp.Regexp }{
	{regexp.MustCompile(`(?i)\bleft as[ -]is\b|\bleft it\b`), nil},
	{regexp.MustCompile(`(?i)\bnot fixed\b|\bdid not fix\b`), nil},
	{regexp.MustCompile(`(?i)\bLOW, left\b`), nil},
	{regexp.MustCompile(`(?i)\bfor the owner\b|\bowner decision\b|\bdecisions for the owner\b`), nil},
	{regexp.MustCompile(`(?i)\bsay yes and i['’]ll\b|\bwant me to\b`), nil},
	{regexp.MustCompile(`\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b.*(?i:\b(?:open|remaining|unfixed)\b)|(?i:\b(?:open|remaining|unfixed)\b).*\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`),
		regexp.MustCompile(`(?i)\b(?:no|none|zero|0)\b`)},
}

const (
	maxHandbackLines = 12
	handbackLineCap  = 200
)

// ReviewHandback (PostToolUse Agent) tells the main session to fix unfixed findings a leaf reported; it never blocks.
func ReviewHandback(in *core.Input) core.Result {
	var lines []string
	for l := range strings.SplitSeq(strings.Join(responseStrings(in.Field("tool_response"), nil), "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" && unfixedLine(l) && len(lines) < maxHandbackLines {
			lines = append(lines, "- "+l[:min(len(l), handbackLineCap)])
		}
	}
	if len(lines) == 0 {
		return core.Result{}
	}
	return core.Result{Context: "Fix these now; do not relay them to the owner as questions:\n" + strings.Join(lines, "\n")}
}

func unfixedLine(l string) bool {
	for _, r := range unfixedRules {
		if r.match.MatchString(l) && (r.unless == nil || !r.unless.MatchString(l)) {
			return true
		}
	}
	return false
}

// responseStrings is every string in a tool response except the echoed prompt, in document order.
func responseStrings(v any, out []string) []string {
	switch v := v.(type) {
	case string:
		return append(out, v)
	case []any:
		for _, e := range v {
			out = responseStrings(e, out)
		}
	case map[string]any:
		for k, e := range v {
			if k != "prompt" {
				out = responseStrings(e, out)
			}
		}
	}
	return out
}

var (
	decidePhrase = regexp.MustCompile(`(?i)\bwant me to\b|\bshould i\b|\byour call\b|\bpick one\b|\bwhich (?:do you|would you) (?:want|prefer)\b|\bsay yes\b`)
	decideTopic  = regexp.MustCompile(`(?i:\bfix(?:es|ed|ing)?\b|\bfindings?\b|\bfollow-?ups?\b|\breviews?\b)|\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`)
	askTopic     = regexp.MustCompile(`(?i)\bfix(?:es|ed|ing)?\b|\bfindings?\b|\bfollow-?ups?\b`)
)

const decideReason = "Decide and act: the owner's standing rule is that decisions are made now, not asked (R28)"

// ReviewDecide (Stop, main session) blocks once when the last turn asks the owner to decide a fix or finding.
func ReviewDecide(in *core.Input) core.Result {
	if in.StopHookActive || in.AgentID != "" || proc.InDispatch(context.Background()) {
		return core.Result{}
	}
	text, asks := lastTurn(in.TranscriptPath)
	if asksDecision(text) || askTopic.MatchString(asks) {
		return core.Result{Decision: core.Block, Reason: decideReason}
	}
	return core.Result{}
}

func asksDecision(text string) bool {
	for l := range strings.SplitSeq(text, "\n") {
		if decidePhrase.MatchString(l) && decideTopic.MatchString(l) {
			return true
		}
	}
	return false
}

const turnTailBytes = 512 << 10

type turnBlock struct {
	Type, Text, Name string
	Input            json.RawMessage
}

// turnEntry is one transcript line's role and blocks; ok is false for a tool result or a non-message line.
func turnEntry(line []byte) (role string, blocks []turnBlock, ok bool) {
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || (e.Type != "user" && e.Type != "assistant") {
		return "", nil, false
	}
	_ = json.Unmarshal(e.Message.Content, &blocks) // a string content leaves blocks nil
	if e.Type == "user" && len(blocks) > 0 && blocks[0].Type == "tool_result" {
		return "", nil, false // a tool result is inside the turn, not its start
	}
	return e.Type, blocks, true
}

// tailOf is the last turnTailBytes of the file at path; empty when unreadable.
func tailOf(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var r io.Reader = f
	if st, err := f.Stat(); err == nil && st.Size() > turnTailBytes {
		r = io.NewSectionReader(f, st.Size()-turnTailBytes, turnTailBytes)
	}
	raw, _ := io.ReadAll(r)
	return raw
}

// lastTurn is the final assistant text and the AskUserQuestion text of the turn after the last real user message.
func lastTurn(path string) (text, asks string) {
	for _, line := range slices.Backward(bytes.Split(tailOf(path), []byte("\n"))) {
		role, blocks, ok := turnEntry(line)
		switch {
		case !ok:
		case role == "user":
			return text, asks // a real user message opens the turn
		case role == "assistant":
			for _, b := range blocks {
				if b.Type == "text" && text == "" {
					text = b.Text
				} else if b.Name == "AskUserQuestion" {
					asks += string(b.Input) + "\n"
				}
			}
		}
	}
	return text, asks
}
