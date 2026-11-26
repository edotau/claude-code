package quality

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

	"github.com/edotau/claude-code/internal/assets"
	"github.com/edotau/claude-code/internal/hooks/core"
	"github.com/edotau/claude-code/internal/platform/proc"
)

var unfixedLine = regexp.MustCompile(`(?im)^[\s>*_-]*UNFIXED:[ \t]*(.*)$`)

const maxHandbackItems = 12

var (
	verdictLine = regexp.MustCompile(`(?im)^[\s*_#-]*(?:verdict:?[ \t]*)?[*_]*(SHIP|FIX REQUIRED|BLOCK)[*_.]*[ \t]*$`)
	findingLine = regexp.MustCompile(`(?im)^[\s*_-]*(?:\d+[.)][ \t]*)?[*_]*\[?(CRITICAL|HIGH|MEDIUM|LOW)\]?[*_]*[ \t]+(\S+?):(\d+)\S*[ \t]*(.*)$`)
	quotedBlock = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|(?m:^[ \\t]*>[^\\n]*$)")
)

// Finding is one "[SEVERITY] file:line — text" line of a review report.
type Finding struct {
	Severity, File, Text string
	Line                 int
}

// ParseFindings is the finding lines of a report outside quoted blocks, severity upper-cased.
func ParseFindings(report string) []Finding {
	var out []Finding
	for _, m := range findingLine.FindAllStringSubmatch(unquoted(report), -1) {
		n, _ := strconv.Atoi(m[3])
		out = append(out, Finding{strings.ToUpper(m[1]), m[2], strings.TrimSpace(m[4]), n})
	}
	return out
}

// StatedVerdict is the SHIP, FIX REQUIRED or BLOCK a report states outside quoted blocks, else "".
func StatedVerdict(report string) string {
	if v := verdictLine.FindStringSubmatch(unquoted(report)); v != nil {
		return strings.ToUpper(v[1])
	}
	return ""
}

// unquoted is report without its quoted blocks; a report with no fence or '>' skips the regex scan.
func unquoted(report string) string {
	if !strings.Contains(report, "```") && !strings.Contains(report, "~~~") && !strings.Contains(report, ">") {
		return report
	}
	return quotedBlock.ReplaceAllString(report, "")
}

// ComputeVerdict is the one reducer: CRITICAL BLOCK, HIGH FIX REQUIRED, unjudged, MEDIUM/LOW mergeable, else SHIP.
func ComputeVerdict(fs []Finding, unjudged bool) (verdict string, mergeable bool) {
	n := map[string]int{}
	for _, f := range fs {
		n[f.Severity]++
	}
	switch {
	case n["CRITICAL"] > 0:
		return "BLOCK", false
	case n["HIGH"] > 0:
		return "FIX REQUIRED", false
	case unjudged:
		return "unjudged", false
	case n["MEDIUM"]+n["LOW"] > 0:
		return "FIX REQUIRED", true
	}
	return "SHIP", false
}

// verdictMismatch names the verdict a report's findings compute when its stated verdict is milder, else "".
func verdictMismatch(report string) string {
	stated := StatedVerdict(report)
	if stated == "" || stated == "BLOCK" { // BLOCK is the strictest: no findings can compute past it
		return ""
	}
	var serious []Finding // the hook flags CRITICAL/HIGH only
	for _, f := range ParseFindings(report) {
		if f.Severity == "CRITICAL" || f.Severity == "HIGH" {
			serious = append(serious, f)
		}
	}
	computed, _ := ComputeVerdict(serious, false)
	rank := map[string]int{"SHIP": 0, "FIX REQUIRED": 1, "BLOCK": 2}
	if rank[stated] < rank[computed] {
		return "The report states " + stated + " but its CRITICAL/HIGH findings compute " + computed + ". "
	}
	return ""
}

// ReviewHandback tells the main session to fix what a leaf report's last UNFIXED line lists; it never blocks.
func ReviewHandback(in *core.Input) core.Result {
	report := handbackReport(in)
	r := reviewHandback(report)
	if mm := verdictMismatch(report); mm != "" {
		r.Context = strings.TrimSpace(r.Context + "\n" + mm + "Treat the computed verdict as the verdict.")
	}
	return r
}

func handbackReport(in *core.Input) string {
	report := strings.Join(responseStrings(in.Field("tool_response"), nil), "\n")
	if strings.Contains(in.Prompt, "<task-notification") {
		report = in.Prompt // a background leaf's report arrives as a prompt
	}
	return report
}

func reviewHandback(report string) core.Result {
	if strings.TrimSpace(report) == "" {
		return core.Result{} // a prompt that is not a notification, or an empty response
	}
	noLine := assets.HookReviewNoLine()
	items := UnfixedItems(report)
	if len(items) == 0 {
		return core.Result{Context: noLine} // an empty footer is not a clean one
	}
	if UnfixedNone(items) {
		return core.Result{}
	}
	for i, it := range items {
		items[i] = "- " + it
	}
	if extra := len(items) - maxHandbackItems; extra > 0 {
		items = append(items[:maxHandbackItems], "- and "+strconv.Itoa(extra)+" more: read the full UNFIXED line")
	}
	return core.Result{Context: assets.HookReviewFixHead() + strings.Join(items, "\n")}
}

// UnfixedItems is the items of a report's last UNFIXED line; nil when it has none or the line is empty.
func UnfixedItems(report string) []string {
	m := unfixedLine.FindAllStringSubmatch(report, -1)
	if len(m) == 0 {
		return nil
	}
	var items []string
	for it := range strings.SplitSeq(strings.Trim(m[len(m)-1][1], " \t*_`"), ";") {
		if it = strings.TrimSpace(it); it != "" {
			items = append(items, it)
		}
	}
	return items
}

// UnfixedNone reports whether the UNFIXED items are exactly "none".
func UnfixedNone(items []string) bool {
	return len(items) == 1 && strings.EqualFold(strings.Trim(items[0], "-. *_`"), "none")
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
	text, asks := LastTurn(in.TranscriptPath)
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

// TurnEntry is one transcript line's role and blocks; ok is false for a synthetic (isMeta) or non-message line.
func TurnEntry(line []byte) (role string, blocks []turnBlock, ok bool) {
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

// TailOf is the last turnTailBytes of the file at path, starting at a whole line; empty when unreadable.
func TailOf(path string) []byte {
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

// LastTurn is the assistant text and unanswered AskUserQuestion text after the last user entry of the transcript.
func LastTurn(path string) (text, asks string) {
	var texts []string
	var askList []string
	answered, sawResult := map[string]bool{}, false
	for _, line := range slices.Backward(bytes.Split(TailOf(path), []byte("\n"))) {
		role, blocks, ok := TurnEntry(line)
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
