package planlint

import (
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"

	reviewsource "github.com/edotau/claude-code/internal/review/source"

	"github.com/edotau/claude-code/internal/textutil"
)

// stepResult mirrors the Python step-result dict field order.
type stepResult struct {
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
	Status          string          `json:"status"`
	Source          string          `json:"source"`
	StepsFound      int             `json:"steps_found"`
	Verdict         string          `json:"verdict"`
	Score           int             `json:"score"`
	MaxScore        int             `json:"max_score"`
	StepResults     []stepResult    `json:"step_results"`
	Recommendations []string        `json:"recommendations"`
	OversizedTasks  []oversizedTask `json:"oversized_tasks,omitempty"`
}

type stepsResult struct {
	Status               string          `json:"status"`
	Source               string          `json:"source"`
	StepsFound           int             `json:"steps_found"`
	Score                int             `json:"score"`
	MaxScore             int             `json:"max_score"`
	Percentage           pyFloat         `json:"percentage"`
	HasFinalVerification bool            `json:"has_final_verification"`
	Verdict              string          `json:"verdict"`
	StepResults          []stepResult    `json:"step_results"`
	Recommendations      []string        `json:"recommendations"`
	OversizedTasks       []oversizedTask `json:"oversized_tasks,omitempty"`
}

// oversizedTask is a `## Tasks` (or phase task) line that isn't leaf-sized: too many packages, too many
// findings/items, or a code task with no `owns:` segment (agents/Plan.md's leaf-sized task rule).
type oversizedTask struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

var (
	concreteRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:assert\w*|\.toBe|\.toEqual|pytest|jest|npm\s+test|go\s+test|exit\s*(?:code)?\s*[=:]?\s*0|status\s*[=:]?\s*200|curl\b|grep\b|diff\b|benchmark)\b|\bexpect\(|\b(?:latency\s*<|throughput\s*>)`)
	})
	reasonableRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:verify|check|confirm|inspect|review|compare|validate|run\s+and\s+see|manually|open\s+in\s+browser|screenshot)\b`)
	})
	vagueRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:should\s+work|looks?\s+(?:good|right|fine|ok)|seems?\s+(?:correct|fine)|hopefully|probably\s+works?)\b`)
	})
	stepRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(?:\d+[.)]\s+|[-*]\s+\[.\]\s+|[-*]\s+Step\s+\d+)`) })
	phaseRe   = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?m)^#{2,3} Phase\s+[^\n]+`) })
	headingRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^(#{1,6})\s+`) })
	finalRe   = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:final|end-to-end|full\s+test|regression|all\s+(?:tests?\s+)?pass)\b`)
	})

	taskLineRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^\s*-\s*\[[ xX]\]\s+(.+)$`) })
	fenceRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile("^\\s*(```|~~~)") })
	// A path's first segment starts at a token start (no lookbehind in RE2), so nested segments never count.
	internalPkgRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?:^|[\\s`(,])(?:\\./)?(internal|cmd)/([A-Za-z0-9_]+)")
	})
	topDirRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile("(?:^|[\\s`(,])(?:\\./)?([a-zA-Z][a-zA-Z0-9_-]*)/[A-Za-z0-9_.\\-]+")
	})
	ownsRe     = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`owns:\s*[^\s—]`) })
	findingsRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(\d+)\s+(?:findings|items|fixes|functions|files)\b`)
	})
	codeTaskRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bagent:|\.go\b`) })
)

// agentNames head agent alternatives like `(codex/copilot)`, which are not package paths.
var agentNames = map[string]bool{"copilot": true, "codex": true, "gemini": true, "opencode": true, "claude": true, "agy": true}

// countPackageMentions counts distinct internal/cmd package pairs and other path heads, excluding agent names.
func countPackageMentions(line string) int {
	pkgs := map[string]bool{}
	for _, m := range internalPkgRe().FindAllStringSubmatch(line, -1) {
		pkgs[m[1]+"/"+m[2]] = true
	}
	for _, m := range topDirRe().FindAllStringSubmatch(line, -1) {
		if m[1] == "internal" || m[1] == "cmd" || agentNames[m[1]] {
			continue
		}
		pkgs[m[1]] = true
	}
	return len(pkgs)
}

// maxFindingsCount returns the largest "<N> findings/items/fixes/functions/files" count on the line, if any.
func maxFindingsCount(line string) (int, bool) {
	best, found := 0, false
	for _, m := range findingsRe().FindAllStringSubmatch(line, -1) {
		n := 0
		_, _ = fmt.Sscanf(m[1], "%d", &n) // m[1] is \d+: only an int overflow fails, and it leaves n = 0
		if n > best {
			best = n
			found = true
		}
	}
	return best, found
}

// findOversizedTasks flags `## Tasks` (or phase task) checklist lines that aren't leaf-sized per
// agents/Plan.md: >3 packages, >10 findings/items, or a code task with no (or an empty) `owns:`; fences skip.
func findOversizedTasks(text string) []oversizedTask {
	var out []oversizedTask
	fence := ""
	for _, raw := range splitLines(text) {
		if f := fenceRe().FindStringSubmatch(raw); f != nil {
			switch fence {
			case "":
				fence = f[1]
			case f[1]:
				fence = ""
			}
			continue
		}
		m := taskLineRe().FindStringSubmatch(raw)
		if fence != "" || m == nil {
			continue
		}
		line := m[1]
		reasons := oversizeReasons(line)
		if len(reasons) == 0 {
			continue
		}
		out = append(out, oversizedTask{Title: textutil.TruncateRunes(line, 120), Reason: strings.Join(reasons, "; ")})
	}
	return out
}

// oversizeReasons lists why a task line is not leaf-sized; empty means it is fine.
func oversizeReasons(line string) []string {
	var reasons []string
	if n := countPackageMentions(line); n > 3 {
		reasons = append(reasons, fmt.Sprintf("names %d packages (limit 3)", n))
	}
	if n, ok := maxFindingsCount(line); ok && n >= 11 {
		reasons = append(reasons, fmt.Sprintf("mentions %d findings/items (limit 10)", n))
	}
	if codeTaskRe().MatchString(line) && !ownsRe().MatchString(line) {
		reasons = append(reasons, "code task has no `owns:` segment")
	}
	return reasons
}

func extractSteps(text string) []string {
	// A phase-looking line inside a code fence yields no phase steps, so fall back to the list.
	if steps := extractPhaseSteps(text); len(steps) > 0 {
		return steps
	}
	return extractListSteps(text)
}

// extractPhaseSteps collects each phase heading and its body, up to the next heading of equal or higher level.
func extractPhaseSteps(text string) []string {
	var steps []string
	var current []string
	level := 0
	var fence fenceState
	for _, line := range splitLines(text) {
		var heading []string
		phase := false
		if !fence.skip(line) {
			heading = headingRe().FindStringSubmatch(line)
			phase = phaseRe().MatchString(line)
		}
		if level > 0 && heading != nil && (len(heading[1]) <= level || phase) {
			steps = append(steps, strings.Join(current, "\n"))
			current = nil
			level = 0
		}
		if phase {
			level = len(heading[1])
			current = []string{line}
		} else if level > 0 {
			// The template label alone is not verification evidence.
			current = append(current, strings.ReplaceAll(line, "**Verify:**", ""))
		}
	}
	if level > 0 {
		steps = append(steps, strings.Join(current, "\n"))
	}
	return steps
}

// fenceState tracks whether the line being read is a code fence or inside one.
type fenceState string

// skip reports whether line is a fence delimiter or inside a fenced block, updating the state.
func (f *fenceState) skip(line string) bool {
	m := fenceRe().FindStringSubmatch(line)
	switch {
	case m == nil:
		return *f != ""
	case *f == "":
		*f = fenceState(m[1])
	case string(*f) == m[1]:
		*f = ""
	}
	return true
}

func extractListSteps(text string) []string {
	var steps []string
	var current []string
	building := false
	var fence fenceState
	for _, line := range splitLines(text) {
		stripped := strings.TrimSpace(line)
		if fence.skip(line) {
			if building {
				current = append(current, line)
			}
		} else if building && headingRe().MatchString(line) && !stepRe().MatchString(stripped) {
			steps = append(steps, strings.Join(current, "\n")) // a heading ends the last step's body
			current, building = nil, false
		} else if stepRe().MatchString(stripped) {
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
	if concreteRe().MatchString(step) {
		return 3, "concrete"
	}
	if reasonableRe().MatchString(step) {
		return 2, "reasonable"
	}
	if vagueRe().MatchString(step) {
		return 1, "vague"
	}
	return 0, "none"
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
	stepResults          []stepResult
	recommendations      []string
	noPlan               bool
	oversizedTasks       []oversizedTask
}

func scoreSteps(steps []string) ([]stepResult, int) {
	results := make([]stepResult, 0, len(steps))
	total := 0
	for _, step := range steps {
		pts, level := scoreStep(step)
		total += pts
		results = append(results, stepResult{Title: textutil.TruncateRunes(textutil.FirstLine(step), 120), Score: pts, Level: level})
	}
	return results, total
}

func goalsVerdict(pct float64) string {
	switch {
	case pct >= 70:
		return "STRONG"
	case pct >= 40:
		return "WEAK"
	}
	return "MISSING"
}

func goalRecommendations(results []stepResult, hasFinal bool) []string {
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
	return recs
}

func analyze(text, source string) analysis {
	oversized := findOversizedTasks(text)
	steps := extractSteps(text)
	if len(steps) == 0 {
		return analysis{
			source:          source,
			noPlan:          true,
			verdict:         "NO_PLAN",
			stepResults:     []stepResult{},
			recommendations: []string{"No numbered/bulleted steps found. Is this a plan?"},
			oversizedTasks:  oversized,
		}
	}

	results, total := scoreSteps(steps)
	maxScore := len(steps) * 3
	pct := pyRound1(float64(total) / float64(maxScore) * 100)
	hasFinal := finalRe().MatchString(steps[len(steps)-1])
	verdict := goalsVerdict(pct)
	recs := goalRecommendations(results, hasFinal)

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
		oversizedTasks:       oversized,
	}
}

// RunGoals is the Go entry point for goal_verifier.py's CLI.
func RunGoals(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	input, jsonOut, code, handled := parseInputJSONArgs("goals",
		"Check every plan step names a verifiable goal and the plan ends on one.", args, stdout, stderr)
	if handled {
		return code
	}
	text, source, exit, ok := readInput(input, stdin, stderr)
	if !ok {
		return exit
	}

	a := analyze(text, source)

	var err error
	if jsonOut {
		_, err = fmt.Fprintln(stdout, string(goalsJSON(a)))
	} else {
		err = printGoals(stdout, a)
	}
	if err != nil {
		fmt.Fprintf(stderr, "goal_verifier.py: error: writing output: %v\n", err)
		return 1
	}
	return 0
}

func goalsJSON(a analysis) []byte {
	var b []byte
	if a.noPlan {
		b, _ = reviewsource.PyMarshalIndent(noStepsResult{
			Status: "ok", Source: a.source, StepsFound: 0, Verdict: a.verdict,
			Score: 0, MaxScore: 0, StepResults: a.stepResults, Recommendations: a.recommendations,
			OversizedTasks: a.oversizedTasks,
		})
		return b
	}
	b, _ = reviewsource.PyMarshalIndent(stepsResult{
		Status: "ok", Source: a.source, StepsFound: a.stepsFound, Score: a.score, MaxScore: a.maxScore,
		Percentage: pyFloat(a.percentage), HasFinalVerification: a.hasFinalVerification, Verdict: a.verdict,
		StepResults: a.stepResults, Recommendations: a.recommendations, OversizedTasks: a.oversizedTasks,
	})
	return b
}

func printGoals(dst io.Writer, a analysis) error {
	stdout := &strings.Builder{}
	fmt.Fprintf(stdout, "Goal Verifier — %s\n", a.source)
	pctStr := "0"
	if !a.noPlan {
		pctStr = formatPyFloat(a.percentage)
	}
	fmt.Fprintf(stdout, "Steps: %d   Score: %d/%d (%s%%)\n", a.stepsFound, a.score, a.maxScore, pctStr)
	fmt.Fprintf(stdout, "Verdict: %s\n\n", a.verdict)
	icons := map[string]string{"concrete": "+", "reasonable": "~", "vague": "?", "none": "!"}
	for _, sr := range a.stepResults {
		fmt.Fprintf(stdout, "  [%s] %s  (%s, %d/3)\n", icons[sr.Level], textutil.TruncateRunes(sr.Title, 100), sr.Level, sr.Score)
	}
	fmt.Fprintln(stdout)
	for _, rec := range a.recommendations {
		fmt.Fprintf(stdout, "  -> %s\n", rec)
	}
	if len(a.oversizedTasks) > 0 {
		fmt.Fprintln(stdout)
		fmt.Fprintln(stdout, "Oversized tasks:")
		for _, t := range a.oversizedTasks {
			fmt.Fprintf(stdout, "  ! %s  (%s)\n", textutil.TruncateRunes(t.Title, 100), t.Reason)
		}
	}
	_, err := io.WriteString(dst, stdout.String())
	return err
}
