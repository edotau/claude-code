package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/proc"
)

var unfixedLine = regexp.MustCompile(`(?im)^[\s>*_-]*UNFIXED:[ \t]*(.*)$`)

const maxHandbackItems = 12

// ReviewHandback tells the main session to fix what a leaf report's last UNFIXED line lists; it never blocks.
func ReviewHandback(in *core.Input) core.Result {
	report := strings.Join(responseStrings(in.Field("tool_response"), nil), "\n")
	if strings.Contains(in.Prompt, "<task-notification") {
		report = in.Prompt // a background leaf's report arrives as a prompt
	}
	if strings.TrimSpace(report) == "" {
		return core.Result{} // a prompt that is not a notification, or an empty response
	}
	const noLine = "The leaf report has no UNFIXED line; read it and fix anything left before reporting."
	m := unfixedLine.FindAllStringSubmatch(report, -1)
	if len(m) == 0 {
		return core.Result{Context: noLine}
	}
	var items []string
	for it := range strings.SplitSeq(strings.Trim(m[len(m)-1][1], " \t*_`"), ";") {
		if it = strings.TrimSpace(it); it != "" {
			items = append(items, "- "+it)
		}
	}
	if len(items) == 0 {
		return core.Result{Context: noLine} // an empty footer is not a clean one
	}
	if len(items) == 1 && strings.EqualFold(strings.Trim(items[0], "-. *_`"), "none") {
		return core.Result{}
	}
	if extra := len(items) - maxHandbackItems; extra > 0 {
		items = append(items[:maxHandbackItems], "- and "+strconv.Itoa(extra)+" more: read the full UNFIXED line")
	}
	return core.Result{Context: "Fix these now; do not relay them to the owner as questions:\n" + strings.Join(items, "\n")}
}

// responseStrings is every string in a tool response except the echoed prompt, in key-sorted order.
func responseStrings(v any, out []string) []string {
	switch v := v.(type) {
	case string:
		return append(out, v)
	case []any:
		for _, e := range v {
			out = responseStrings(e, out)
		}
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(v)) {
			if k != "prompt" {
				out = responseStrings(v[k], out)
			}
		}
	}
	return out
}

var (
	askTopic = regexp.MustCompile(`(?i)\b(?:fix(?:es|ed|ing)?|findings?|reviews?)\b`)
	askFix   = regexp.MustCompile(`(?i)\b(?:want me to|should i|shall i|would you like me to|do you want me to) fix\b`)
	quoted   = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|(?m:^(?: {4}|\\t)[^\\n]*$)|(?m:^[ \\t]*>[^\\n]*$)|`[^`\\n]*`|“[^”]*”|‘[^’]*’|\"[^\"]*\"|(?:^|[\\s(])'[^'\\n]*'")
)

const decideReason = "Decide and act: the owner's standing rule is that decisions are made now, not asked (R28)"

// ReviewDecide (Stop, main session) blocks once when the last turn asks the owner whether to fix something.
func ReviewDecide(in *core.Input) core.Result {
	if in.StopHookActive || in.AgentID != "" || proc.InDispatch(context.Background()) {
		return core.Result{}
	}
	text, asks := lastTurn(in.TranscriptPath)
	if text == "" {
		text = in.Nested("last_assistant_message") // the record was cut by the tail cap or the transcript is unreadable
	}
	if askFix.MatchString(quoted.ReplaceAllString(text, " ")) || askTopic.MatchString(asks) {
		return core.Result{Decision: core.Block, Reason: decideReason}
	}
	return core.Result{}
}

const turnTailBytes = 512 << 10

type turnBlock struct {
	Type, Text, Name, ID string
	ToolUseID            string `json:"tool_use_id"`
	Input                json.RawMessage
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
	var askList []string
	answered, sawResult := map[string]bool{}, false
	for _, line := range slices.Backward(bytes.Split(tailOf(path), []byte("\n"))) {
		role, blocks, ok := turnEntry(line)
		if !ok {
			continue
		}
		if role == "user" {
			if !isResults(blocks) {
				break // a prompt: what follows it is the turn
			}
			sawResult = true // text before a tool result is not the final message
			for _, b := range blocks {
				answered[b.ToolUseID] = true
			}
			continue
		}
		for _, b := range slices.Backward(blocks) {
			if t, q := blockText(b, sawResult, answered); t != "" {
				texts = append(texts, t)
			} else {
				askList = append(askList, q)
			}
		}
	}
	slices.Reverse(texts)
	return strings.Join(texts, "\n"), strings.Join(askList, "")
}

func isResults(blocks []turnBlock) bool {
	return len(blocks) > 0 && !slices.ContainsFunc(blocks, func(b turnBlock) bool { return b.Type != "tool_result" })
}

// blockText is a text block's text (unless a tool result follows it) or an unanswered AskUserQuestion's text.
func blockText(b turnBlock, sawResult bool, answered map[string]bool) (text, ask string) {
	if b.Type == "text" && !sawResult {
		return b.Text, ""
	}
	if b.Name == "AskUserQuestion" && !answered[b.ID] {
		return "", askText(b.Input)
	}
	return "", ""
}
