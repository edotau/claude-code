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
	{regexp.MustCompile(`(?i)\bleft as[ -]is\b|\bleft it\b`), regexp.MustCompile(`(?i)\bcovered\b|\btested\b`)},
	{regexp.MustCompile(`(?i)\bnot fixed\b|\bdid not fix\b`), nil},
	{regexp.MustCompile(`(?i)\bLOW, left\b`), nil},
	{regexp.MustCompile(`(?i)\bfor the owner\b|\bowner decision\b|\bdecisions for the owner\b`), nil},
	{regexp.MustCompile(`(?i)\bsay yes and i['’]ll\b|\bwant me to\b`), nil},
	{regexp.MustCompile(`\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b.*(?i:\b(?:open|remaining|unfixed|deferred|follow-?up|pending|outstanding)\b)|(?i:\b(?:open|remaining|unfixed|deferred|follow-?up|pending|outstanding)\b).*\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`), nil},
}

var (
	clauseSplit  = regexp.MustCompile(`[;.]\s+|;`)
	cleanClause  = regexp.MustCompile(`(?i)^\W*(?:no|none|zero|0)\s|\ball (?:were |are )?(?:fixed|resolved)\b`)
	numberedItem = regexp.MustCompile(`^\W*\d+[.)]\s.*\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`)
	itemResolved = regexp.MustCompile(`(?i)\b(?:fixed|resolved|addressed)\b`)
)

const (
	maxHandbackLines = 12
	handbackLineCap  = 200
)

// ReviewHandback (PostToolUse Agent) tells the main session to fix unfixed findings a leaf reported; it never blocks.
func ReviewHandback(in *core.Input) core.Result {
	var lines []string
	report := strings.Join(responseStrings(in.Field("tool_response"), nil), "\n")
	if strings.Contains(in.Prompt, "<task-notification") {
		report = in.Prompt // a background leaf's report arrives as a prompt
	}
	for l := range strings.SplitSeq(report, "\n") {
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
	if numberedItem.MatchString(l) && !itemResolved.MatchString(l) {
		return true
	}
	for _, c := range clauseSplit.Split(l, -1) {
		if cleanClause.MatchString(c) {
			continue
		}
		for _, r := range unfixedRules {
			if r.match.MatchString(c) && (r.unless == nil || !r.unless.MatchString(c)) {
				return true
			}
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
	questionPhrase  = regexp.MustCompile(`(?i)\b(?:want me to|should i|would you like me to|shall i|can i|may i|which (?:do you|would you) (?:want|prefer))\b`)
	statementPhrase = regexp.MustCompile(`(?i)\byour call\b|\bpick one\b|\bsay yes\b`)
	decideTopic     = regexp.MustCompile(`(?i:\bfix(?:es|ed|ing)?\b|\bfindings?\b|\bfollow-?ups?\b|\breviews?\b|\bissues?\b|\bbugs?\b|\baddress\b)|\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`)
	askTopic        = regexp.MustCompile(`(?i)\bfix(?:es|ed|ing)?\b|\bfindings?\b|\bfollow-?ups?\b|\baddress\b`)
	quoted          = regexp.MustCompile("(?s)```.*?```|`[^`\\n]*`|“[^”]*”|‘[^’]*’|\"[^\"\\n]*\"|(?:^|[\\s(])'[^'\\n]*'")
	sentence        = regexp.MustCompile(`[^.!?\n]+[.!?]*`)
)

const decideReason = "Decide and act: the owner's standing rule is that decisions are made now, not asked (R28)"

// ReviewDecide (Stop, main session) blocks once when the last turn asks the owner to decide a fix or finding.
func ReviewDecide(in *core.Input) core.Result {
	if in.StopHookActive || in.AgentID != "" || proc.InDispatch(context.Background()) {
		return core.Result{}
	}
	text, asks := lastTurn(in.TranscriptPath)
	if text == "" {
		text = in.Nested("last_assistant_message") // the record was cut by the tail cap or the transcript is unreadable
	}
	if asksDecision(text) || askTopic.MatchString(asks) {
		return core.Result{Decision: core.Block, Reason: decideReason}
	}
	return core.Result{}
}

// asksDecision: a question or "your call" sentence with a fix or finding topic in it or the sentence before.
func asksDecision(text string) bool {
	ss := sentence.FindAllString(quoted.ReplaceAllString(text, " "), -1)
	for i, s := range ss {
		asked := questionPhrase.MatchString(s) && strings.HasSuffix(strings.TrimSpace(s), "?")
		if !asked && !statementPhrase.MatchString(s) {
			continue
		}
		if decideTopic.MatchString(s) || (i > 0 && decideTopic.MatchString(ss[i-1])) {
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

// turnEntry is one transcript line's role and blocks; ok is false for a synthetic (isMeta) or non-message line.
func turnEntry(line []byte) (role string, blocks []turnBlock, ok bool) {
	var e struct {
		Type    string `json:"type"`
		IsMeta  bool   `json:"isMeta"`
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.IsMeta || (e.Type != "user" && e.Type != "assistant") {
		return "", nil, false
	}
	_ = json.Unmarshal(e.Message.Content, &blocks) // a string content leaves blocks nil
	return e.Type, blocks, true
}

// tailOf is the last turnTailBytes of the file at path, starting at a whole line; empty when unreadable.
func tailOf(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var r io.Reader = f
	cut := false
	if st, err := f.Stat(); err == nil && st.Size() > turnTailBytes {
		r, cut = io.NewSectionReader(f, st.Size()-turnTailBytes, turnTailBytes), true
	}
	raw, _ := io.ReadAll(r)
	if _, rest, found := bytes.Cut(raw, []byte("\n")); cut && found {
		return rest // the first line is cut mid-record
	} else if cut {
		return nil
	}
	return raw
}

// askText is the question and option labels of an AskUserQuestion input; descriptions are not what is asked.
func askText(input json.RawMessage) string {
	var in struct {
		Questions []struct {
			Question string
			Options  []struct{ Label string }
		}
	}
	_ = json.Unmarshal(input, &in)
	var b strings.Builder
	for _, q := range in.Questions {
		b.WriteString(q.Question + "\n")
		for _, o := range q.Options {
			b.WriteString(o.Label + "\n")
		}
	}
	return b.String()
}

// lastTurn is the assistant text and unanswered AskUserQuestion text after the last user entry of the transcript.
func lastTurn(path string) (text, asks string) {
	var texts []string
	for _, line := range slices.Backward(bytes.Split(tailOf(path), []byte("\n"))) {
		role, blocks, ok := turnEntry(line)
		if !ok {
			continue
		}
		if role == "user" {
			break // a prompt or a tool result: what follows it is the final message
		}
		for _, b := range slices.Backward(blocks) {
			if b.Type == "text" {
				texts = append(texts, b.Text)
			} else if b.Name == "AskUserQuestion" {
				asks += askText(b.Input)
			}
		}
	}
	slices.Reverse(texts)
	return strings.Join(texts, "\n"), asks
}
