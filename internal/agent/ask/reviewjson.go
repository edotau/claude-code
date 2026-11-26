package ask

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
	"github.com/edotau/claude-code/internal/hooks/quality"
)

// reviewFinding is one structured finding row; its schema is generated from it.
type reviewFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity" jsonschema:"enum=CRITICAL|HIGH|MEDIUM|LOW"`
	Claim    string `json:"claim"`
}

type reviewRows struct {
	Findings []reviewFinding `json:"findings"`
}

var reviewFindingsSchema = wire.SchemaFor[reviewRows]()

type reviewEnvelope struct {
	wire.Envelope
	Findings json.RawMessage `json:"findings,omitempty"`
	reviewVerdict
}

// UnmarshalJSON decodes the embedded Envelope alone, so its promoted method never swallows findings or the verdict.
func (e *reviewEnvelope) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &e.Envelope); err != nil {
		return err
	}
	rest := struct {
		Findings json.RawMessage `json:"findings"`
		*reviewVerdict
	}{reviewVerdict: &e.reviewVerdict}
	if err := json.Unmarshal(b, &rest); err != nil {
		return err
	}
	e.Findings = rest.Findings
	return nil
}

// reviewVerdict is computed from the findings alone (counts are of distinct findings), never from the stated word.
type reviewVerdict struct {
	Computed    string         `json:"computed_verdict,omitempty"` // SHIP | FIX REQUIRED | BLOCK | unjudged
	Counts      map[string]int `json:"counts,omitempty"`
	Mergeable   bool           `json:"mergeable,omitempty"`    // only MEDIUM/LOW: fix on main
	VerdictNote string         `json:"verdict_note,omitempty"` // set when the stated verdict was overruled
}

// reduceFindings: CRITICAL is BLOCK, HIGH FIX REQUIRED, only MEDIUM/LOW FIX REQUIRED but mergeable, none SHIP.
func reduceFindings(fs []quality.Finding, unjudged bool) reviewVerdict {
	v := reviewVerdict{Computed: "SHIP", Counts: map[string]int{"CRITICAL": 0, "HIGH": 0, "MEDIUM": 0, "LOW": 0}}
	for _, f := range fs {
		v.Counts[f.Severity]++
	}
	v.Computed, v.Mergeable = quality.ComputeVerdict(fs, unjudged)
	return v
}

// judged is one report's findings, the verdict they compute and the verdict word the model stated.
type judged struct {
	reviewVerdict
	Findings []quality.Finding
	Stated   string
	Unjudged bool
}

var severityWord = regexp.MustCompile(`(?i)\b(?:CRITICAL|HIGH|MEDIUM|LOW)\b`)

// judgeReport reduces a prose report; findings it cannot parse leave it unjudged, never SHIP.
func judgeReport(answer string) judged {
	j := judged{Findings: quality.ParseFindings(answer), Stated: quality.StatedVerdict(answer)}
	if t := strings.TrimSpace(answer); len(j.Findings) == 0 && !strings.Contains(t, cleanVerdict) {
		j.Unjudged = j.Stated != "SHIP" || severityWord.MatchString(t)
	}
	j.reviewVerdict = reduceFindings(j.Findings, j.Unjudged)
	return j
}

// Note is the one line saying the computed verdict overrides the stated one, or "" when they agree.
func (j judged) Note() string {
	stated := cmp.Or(j.Stated, "none")
	if stated == j.Computed || (j.Stated == "" && !j.Unjudged) {
		return ""
	}
	return "stated " + stated + ", findings compute " + j.Computed
}

// reviewNoteLine is the note line closing a text review whose stated verdict the findings overrule.
func reviewNoteLine(answer string) string {
	if n := judgeReport(answer).Note(); n != "" {
		if answer != "" && !strings.HasSuffix(answer, "\n") {
			n = "\n" + n
		}
		return n + "\n"
	}
	return ""
}

// envJudged judges an envelope: its structured findings when it has them, else its prose answer.
func envJudged(env reviewEnvelope) judged {
	if len(env.Findings) == 0 {
		j := judgeReport(env.Answer)
		if env.Computed == "unjudged" { // set when the findings failed their schema
			j.Unjudged = true
			j.reviewVerdict = reduceFindings(j.Findings, true)
		}
		return j
	}
	var rows []reviewFinding
	j := judged{Stated: quality.StatedVerdict(env.Answer)}
	if err := json.Unmarshal(env.Findings, &rows); err != nil {
		j.Unjudged = true
	}
	for _, r := range rows {
		j.Findings = append(j.Findings, quality.Finding{Severity: r.Severity, File: r.File, Line: r.Line, Text: r.Claim})
	}
	j.reviewVerdict = reduceFindings(j.Findings, j.Unjudged)
	return j
}

// withVerdict sets the computed verdict on a successful envelope.
func withVerdict(env reviewEnvelope) reviewEnvelope {
	if env.Error == nil {
		j := envJudged(env)
		env.reviewVerdict = j.reviewVerdict
		env.VerdictNote = j.Note()
	}
	return env
}

func reviewFormatArgs(name string, args []string, structured bool) []string {
	flags := []string{"--format", "json"}
	if structured && agent.SchemaFor(name).Eng().Caps().OutputSchema == agent.Native {
		flags = append(flags, "--output-schema", reviewFindingsSchema)
	}
	return slices.Insert(slices.Clone(args), len(args)-1, flags...)
}

// reviewAskCmd is the one seam through which a review runs its ask (tests swap it).
var reviewAskCmd = cmdTo

func runReviewAsk(name string, args []string, o ReviewOptions) int {
	if o.Format != "json" {
		if o.Adversarial || o.Background {
			return reviewAskCmd(args, os.Stdout)
		}
		answer, rc := captureAnswer(func(out io.Writer) int { return reviewAskCmd(args, out) })
		if _, err := io.WriteString(os.Stdout, answer+reviewNoteLine(answer)); err != nil {
			return helpReviewError(err)
		}
		return rc
	}
	args = reviewFormatArgs(name, args, true)
	if o.Background {
		return reviewAskCmd(args, os.Stdout)
	}
	answer, rc := captureAnswer(func(out io.Writer) int { return reviewAskCmd(args, out) })
	env, rc, err := parseReviewEnvelope([]byte(answer), rc, reviewNote(rc))
	if err != nil {
		if rc != 0 { // a failed ask may print no envelope; keep its own exit code
			return rc
		}
		return helpReviewError(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(withVerdict(env)); err != nil {
		return helpReviewError(err)
	}
	return rc
}

func parseReviewEnvelope(data []byte, rc int, note io.Writer) (reviewEnvelope, int, error) {
	var env reviewEnvelope
	if err := json.Unmarshal(data, &env.Envelope); err != nil {
		return env, rc, fmt.Errorf("review envelope: %w", err)
	}
	schemaError := env.Error != nil && env.Error.Kind == "schema"
	if !schemaError && agent.SchemaFor(cmp.Or(env.Runner, env.Agent)).Eng().Caps().OutputSchema != agent.Native && len(env.Structured) == 0 {
		return env, rc, nil
	}
	if env.Error != nil && !schemaError {
		return env, rc, nil
	}
	var err error
	env, err = reviewFindings(env, note)
	if schemaError {
		env.Status, env.Error, rc = "ok", nil, 0
	}
	return env, rc, err
}

func reviewFindings(env reviewEnvelope, note io.Writer) (reviewEnvelope, error) {
	raw := env.Structured
	if len(raw) == 0 {
		raw = json.RawMessage(env.Answer)
	}
	rows, _, err := wire.Decode[reviewRows](string(raw))
	if err != nil {
		if _, err := fmt.Fprintln(note, "review: invalid findings; keeping prose answer"); err != nil {
			return env, err
		}
		env.Structured, env.Computed = nil, "unjudged" // schema-invalid findings never pass as SHIP
		return env, nil
	}
	findings, err := json.Marshal(rows.Findings)
	if err != nil {
		return env, err
	}
	env.Findings, env.Structured = findings, nil // the findings ride once
	return env, nil
}

func reviewPartAnswer(name string, args []string, o ReviewOptions) (string, wire.Usage, int) {
	var out bytes.Buffer
	rc := reviewAskCmd(reviewFormatArgs(name, args, o.Format == "json"), &out)
	var env wire.Envelope
	if err := json.Unmarshal(out.Bytes(), &env); err != nil {
		if rc != 0 { // a failed ask may write nothing to stdout; keep its own exit code and message
			return "", wire.Usage{}, rc
		}
		return "", wire.Usage{}, helpReviewError(err)
	}
	usage := wire.Usage{}
	if env.Usage != nil {
		usage = *env.Usage
	}
	if o.Format == "json" {
		parsed, code, err := parseReviewEnvelope(out.Bytes(), rc, reviewNote(rc))
		if err != nil {
			return "", usage, helpReviewError(err)
		}
		out.Reset()
		if err := json.NewEncoder(&out).Encode(withVerdict(parsed)); err != nil {
			return "", usage, helpReviewError(err)
		}
		return out.String(), usage, code
	}
	answer := env.Answer
	if answer != "" && !strings.HasSuffix(answer, "\n") {
		answer += "\n"
	}
	out.Reset()
	out.WriteString(answer)
	if err := printDenials(&out, wire.Result{Denials: env.Denials}); err != nil {
		return "", usage, helpReviewError(err)
	}
	return out.String(), usage, rc
}

func printReviewMerged(o ReviewOptions, parts []part, answers []string) error {
	if o.Format != "json" {
		return printMerged(o.Adversarial, parts, answers)
	}
	var merged reviewEnvelope
	rows := []json.RawMessage{}
	var usage wire.Usage
	var denials *wire.Denials
	prose := make([]string, 0, len(answers))
	js := make([]judged, 0, len(answers))
	for _, answer := range answers {
		var env reviewEnvelope
		if err := json.Unmarshal([]byte(answer), &env); err != nil {
			return err
		}
		if env.Usage != nil {
			usage = sumReviewUsage(usage, *env.Usage)
		}
		if len(env.Findings) > 0 {
			var findings []json.RawMessage
			if err := json.Unmarshal(env.Findings, &findings); err != nil {
				return err
			}
			rows = append(rows, findings...)
		}
		denials = sumReviewDenials(denials, env.Denials)
		prose = append(prose, partProse(env))
		js = append(js, envJudged(env))
		merged = env
	}
	if len(rows) > 0 || len(merged.Findings) > 0 {
		var err error
		merged.Findings, err = json.Marshal(rows)
		if err != nil {
			return err
		}
	}
	merged.Structured, merged.Denials = nil, denials
	merged.Answer = mergeReportsJudged(o.Adversarial, parts, prose, js)
	m := mergeJudged(js, len(answers) < len(parts))
	merged.reviewVerdict, merged.VerdictNote = m.reviewVerdict, m.Note()
	merged.Usage = &usage
	return json.NewEncoder(os.Stdout).Encode(merged)
}

// partProse is a part's answer; a structured part is rendered as finding lines so the merge reads them too.
func partProse(env reviewEnvelope) string {
	if len(env.Findings) == 0 {
		return env.Answer
	}
	var b strings.Builder
	for _, f := range envJudged(env).Findings {
		fmt.Fprintf(&b, "[%s] %s:%d — %s\n", f.Severity, f.File, f.Line, f.Text)
	}
	return cmp.Or(b.String(), env.Answer) // an empty findings array keeps the model's own answer
}

// sumReviewDenials adds b's count to a and keeps the first three labels overall.
func sumReviewDenials(a, b *wire.Denials) *wire.Denials {
	if b == nil {
		return a
	}
	if a == nil {
		a = &wire.Denials{Commands: []string{}}
	}
	a.Count += b.Count
	a.Commands = append(a.Commands, b.Commands...)[:min(3, len(a.Commands)+len(b.Commands))]
	return a
}

func helpReviewError(err error) int {
	fmt.Fprintln(os.Stderr, "review:", err)
	return 1
}

// ask already prints schema failures; a successful dispatch needs its own degradation note.
func reviewNote(rc int) io.Writer {
	if rc != 0 {
		return io.Discard
	}
	return os.Stderr
}
