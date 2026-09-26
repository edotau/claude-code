package plan

import (
	"fmt"
	"io"
	"regexp"
	"strings"
)

// StepResult mirrors the Python step-result dict field order.
type StepResult struct {
	Title string `json:"title"`
	Score int    `json:"score"`
	Level string `json:"level"`
}

// pyFloat renders like Python's json.dumps for a round(x, 1) value (keeps "70.0", not Go's "70").
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	return []byte(formatPyFloat(float64(f))), nil
}

// noStepsResult / stepsResult are two distinct shapes because the Python script returns two different
// dict literals (different key sets/order) depending on whether any steps were found.
type noStepsResult struct {
	Status          string       `json:"status"`
	Source          string       `json:"source"`
	StepsFound      int          `json:"steps_found"`
	Verdict         string       `json:"verdict"`
	Score           int          `json:"score"`
	MaxScore        int          `json:"max_score"`
	StepResults     []StepResult `json:"step_results"`
	Recommendations []string     `json:"recommendations"`
}

type stepsResult struct {
	Status               string       `json:"status"`
	Source               string       `json:"source"`
	StepsFound           int          `json:"steps_found"`
	Score                int          `json:"score"`
	MaxScore             int          `json:"max_score"`
	Percentage           pyFloat      `json:"percentage"`
	HasFinalVerification bool         `json:"has_final_verification"`
	Verdict              string       `json:"verdict"`
	StepResults          []StepResult `json:"step_results"`
	Recommendations      []string     `json:"recommendations"`
}

var (
	concreteRe   = regexp.MustCompile(`(?i)\b(?:assert\w*|expect\(|\.toBe|\.toEqual|pytest|jest|npm\s+test|go\s+test|exit\s*(?:code)?\s*[=:]?\s*0|status\s*[=:]?\s*200|curl\b|grep\b|diff\b|benchmark|latency\s*<|throughput\s*>)\b`)
	reasonableRe = regexp.MustCompile(`(?i)\b(?:verify|check|confirm|inspect|review|compare|validate|run\s+and\s+see|manually|open\s+in\s+browser|screenshot)\b`)
	vagueRe      = regexp.MustCompile(`(?i)\b(?:should\s+work|looks?\s+(?:good|right|fine|ok)|seems?\s+(?:correct|fine)|hopefully|probably\s+works?)\b`)
	stepRe       = regexp.MustCompile(`^(?:\d+[.)]\s+|[-*]\s+\[.\]\s+|[-*]\s+Step\s+\d+)`)
	finalRe      = regexp.MustCompile(`(?i)\b(?:final|end-to-end|full\s+test|regression|all\s+(?:tests?\s+)?pass)\b`)
)

func extractSteps(text string) []string {
	var steps []string
	var current []string
	building := false
	for _, line := range splitLines(text) {
		stripped := strings.TrimSpace(line)
		if stepRe.MatchString(stripped) {
			if building {
				steps = append(steps, strings.Join(current, "\n"))
			}
			current = []string{stripped}
			building = true
		} else if building {
			current = append(current, line)
		}
	}
	if building {
		steps = append(steps, strings.Join(current, "\n"))
	}
	return steps
}

func scoreStep(step string) (int, string) {
	if concreteRe.MatchString(step) {
		return 3, "concrete"
	}
	if reasonableRe.MatchString(step) {
		return 2, "reasonable"
	}
	if vagueRe.MatchString(step) {
		return 1, "vague"
	}
	return 0, "none"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// analysis holds every field either JSON shape needs; RunGoals picks the shape at marshal time.
type analysis struct {
	source               string
	stepsFound           int
	score                int
	maxScore             int
	percentage           float64
	hasFinalVerification bool
	verdict              string
	stepResults          []StepResult
	recommendations      []string
	noPlan               bool
}

func analyze(text, source string) analysis {
	steps := extractSteps(text)
	if len(steps) == 0 {
		return analysis{
			source:          source,
			noPlan:          true,
			verdict:         "NO_PLAN",
			stepResults:     []StepResult{},
			recommendations: []string{"No numbered/bulleted steps found. Is this a plan?"},
		}
	}

	results := make([]StepResult, 0, len(steps))
	total := 0
	for _, step := range steps {
		pts, level := scoreStep(step)
		total += pts
		results = append(results, StepResult{Title: truncateRunes(firstLine(step), 120), Score: pts, Level: level})
	}

	maxScore := len(steps) * 3
	pct := 0.0
	if maxScore > 0 {
		pct = pyRound1(float64(total) / float64(maxScore) * 100)
	}
	hasFinal := finalRe.MatchString(steps[len(steps)-1])
	verdict := "MISSING"
	if pct >= 70 {
		verdict = "STRONG"
	} else if pct >= 40 {
		verdict = "WEAK"
	}

	noneN, vagueN := 0, 0
	for _, r := range results {
		switch r.Level {
		case "none":
			noneN++
		case "vague":
			vagueN++
		}
	}
	var recs []string
	if noneN > 0 {
		recs = append(recs, fmt.Sprintf("%d step(s) have no verification. Add 'verify: [check]' to each.", noneN))
	}
	if vagueN > 0 {
		recs = append(recs, fmt.Sprintf("%d step(s) have vague criteria. Replace 'should work' with a concrete check.", vagueN))
	}
	if !hasFinal {
		recs = append(recs, "No final / end-to-end verification step. Add one at the end.")
	}
	if len(recs) == 0 {
		recs = append(recs, "Strong verification coverage. Good to go.")
	}

	return analysis{
		source:               source,
		stepsFound:           len(steps),
		score:                total,
		maxScore:             maxScore,
		percentage:           pct,
		hasFinalVerification: hasFinal,
		verdict:              verdict,
		stepResults:          results,
		recommendations:      recs,
	}
}

// RunGoals is the Go entry point for goal_verifier.py's CLI.
func RunGoals(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	input, jsonOut, code, handled := parseInputJSONArgs("goal_verifier.py", args, stdout, stderr)
	if handled {
		return code
	}
	text, source, ok := readInput(input, stdin, stderr)
	if !ok {
		return 0
	}

	a := analyze(text, source)

	if jsonOut {
		var b []byte
		if a.noPlan {
			b, _ = marshalPyJSON(noStepsResult{
				Status: "ok", Source: a.source, StepsFound: 0, Verdict: a.verdict,
				Score: 0, MaxScore: 0, StepResults: a.stepResults, Recommendations: a.recommendations,
			})
		} else {
			b, _ = marshalPyJSON(stepsResult{
				Status: "ok", Source: a.source, StepsFound: a.stepsFound, Score: a.score, MaxScore: a.maxScore,
				Percentage: pyFloat(a.percentage), HasFinalVerification: a.hasFinalVerification, Verdict: a.verdict,
				StepResults: a.stepResults, Recommendations: a.recommendations,
			})
		}
		fmt.Fprintln(stdout, string(b))
		return 0
	}

	fmt.Fprintf(stdout, "Goal Verifier — %s\n", source)
	pctStr := "0"
	if !a.noPlan {
		pctStr = formatPyFloat(a.percentage)
	}
	fmt.Fprintf(stdout, "Steps: %d   Score: %d/%d (%s%%)\n", a.stepsFound, a.score, a.maxScore, pctStr)
	fmt.Fprintf(stdout, "Verdict: %s\n\n", a.verdict)
	icons := map[string]string{"concrete": "+", "reasonable": "~", "vague": "?", "none": "!"}
	for _, sr := range a.stepResults {
		fmt.Fprintf(stdout, "  [%s] %s  (%s, %d/3)\n", icons[sr.Level], truncateRunes(sr.Title, 100), sr.Level, sr.Score)
	}
	fmt.Fprintln(stdout)
	for _, rec := range a.recommendations {
		fmt.Fprintf(stdout, "  -> %s\n", rec)
	}
	return 0
}
