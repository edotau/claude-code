package ask

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/wire"
)

//go:embed review-findings.schema.json
var reviewFindingsSchema string

type reviewEnvelope struct {
	wire.Envelope
	Findings json.RawMessage `json:"findings,omitempty"`
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
		return reviewAskCmd(args, os.Stdout)
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
	if err := agent.WriteJSONLine(os.Stdout, env); err != nil {
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
	if !schemaError && agent.SchemaFor(env.Agent).Eng().Caps().OutputSchema != agent.Native && len(env.Structured) == 0 {
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
	checked, _, err := wire.Structured(string(raw), reviewFindingsSchema)
	if err != nil {
		if _, err := fmt.Fprintln(note, "review: invalid findings; keeping prose answer"); err != nil {
			return env, err
		}
		env.Structured = nil
		return env, nil
	}
	var rows struct{ Findings json.RawMessage }
	if err := json.Unmarshal(checked, &rows); err != nil {
		return env, err
	}
	env.Findings, env.Structured = rows.Findings, nil // the findings ride once
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
		if err := agent.WriteJSONLine(&out, parsed); err != nil {
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
		prose = append(prose, env.Answer)
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
	merged.Answer = mergeReports(o.Adversarial, parts, prose)
	merged.Usage = &usage
	return agent.WriteJSONLine(os.Stdout, merged)
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
