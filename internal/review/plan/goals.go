package plan

import (
	"fmt"
	"github.com/edotau/claude-code/internal/review/pyjson"
	"io"
	"regexp"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/textutil"
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
	Status          string          `json:"status"`
	Source          string          `json:"source"`
	StepsFound      int             `json:"steps_found"`
	Verdict         string          `json:"verdict"`
	Score           int             `json:"score"`
	MaxScore        int             `json:"max_score"`
	StepResults     []StepResult    `json:"step_results"`
	Recommendations []string        `json:"recommendations"`
	OversizedTasks  []OversizedTask `json:"oversized_tasks,omitempty"`
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
	StepResults          []StepResult    `json:"step_results"`
	Recommendations      []string        `json:"recommendations"`
	OversizedTasks       []OversizedTask `json:"oversized_tasks,omitempty"`
}

// OversizedTask is a `## Tasks` (or phase task) line that isn't leaf-sized: too many packages, too many
// findings/items, or a code task with no `owns:` segment (agents/Plan.md's leaf-sized task rule).
type OversizedTask struct {
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

var (
	concreteRe = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`(?i)\b(?:assert\w*|expect\(|\.toBe|\.toEqual|pytest|jest|npm\s+test|go\s+test|exit\s*(?:code)?\s*[=:]?\s*0|status\s*[=:]?\s*200|curl\b|grep\b|diff\b|benchmark|latency\s*<|throughput\s*>)\b`)
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
	codeTaskRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\brunner:|\.go\b`) })
)

// runnerNames head runner alternatives like `(codex/copilot)`, which are not package paths.
var runnerNames = map[string]bool{"copilot": true, "codex": true, "gemini": true, "opencode": true, "claude": true, "agy": true}

// countPackageMentions counts distinct internal/cmd package pairs and other path heads, excluding runner names.
func countPackageMentions(line string) int {
	pkgs := map[string]bool{}
	for _, m := range internalPkgRe().FindAllStringSubmatch(line, -1) {
		pkgs[m[1]+"/"+m[2]] = true
	}
	for _, m := range topDirRe().FindAllStringSubmatch(line, -1) {
		if m[1] == "internal" || m[1] == "cmd" || runnerNames[m[1]] {
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
func findOversizedTasks(text string) []OversizedTask {
	var out []OversizedTask
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
		if len(reasons) == 0 {
			continue
		}
		out = append(out, OversizedTask{Title: textutil.TruncateRunes(line, 120), Reason: strings.Join(reasons, "; ")})
	}
	return out
}

func extractSteps(text string) []string {
	if phaseRe().MatchString(text) {
		var steps []string
		var current []string
		level := 0
		for _, line := range splitLines(text) {
			heading := headingRe().FindStringSubmatch(line)
			phase := phaseRe().MatchString(line)
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

	var steps []string
	var current []string
	building := false
	for _, line := range splitLines(text) {
		stripped := strings.TrimSpace(line)
		if stepRe().MatchString(stripped) {
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

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
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
	oversizedTasks       []OversizedTask
}

func analyze(text, source string) analysis {
	oversized := findOversizedTasks(text)
	steps := extractSteps(text)
	if len(steps) == 0 {
		return analysis{
			source:          source,
			noPlan:          true,
			verdict:         "NO_PLAN",
			stepResults:     []StepResult{},
			recommendations: []string{"No numbered/bulleted steps found. Is this a plan?"},
			oversizedTasks:  oversized,
		}
	}

	results := make([]StepResult, 0, len(steps))
	total := 0
	for _, step := range steps {
		pts, level := scoreStep(step)
		total += pts
		results = append(results, StepResult{Title: textutil.TruncateRunes(firstLine(step), 120), Score: pts, Level: level})
	}

	maxScore := len(steps) * 3
	pct := 0.0
	if maxScore > 0 {
		pct = pyRound1(float64(total) / float64(maxScore) * 100)
	}
	hasFinal := finalRe().MatchString(steps[len(steps)-1])
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
	text, source, ok := readInput(input, stdin, stderr)
	if !ok {
		return 0
	}

	a := analyze(text, source)

	if jsonOut {
		var b []byte
		if a.noPlan {
			b, _ = pyjson.MarshalIndent(noStepsResult{
				Status: "ok", Source: a.source, StepsFound: 0, Verdict: a.verdict,
				Score: 0, MaxScore: 0, StepResults: a.stepResults, Recommendations: a.recommendations,
				OversizedTasks: a.oversizedTasks,
			})
		} else {
			b, _ = pyjson.MarshalIndent(stepsResult{
				Status: "ok", Source: a.source, StepsFound: a.stepsFound, Score: a.score, MaxScore: a.maxScore,
				Percentage: pyFloat(a.percentage), HasFinalVerification: a.hasFinalVerification, Verdict: a.verdict,
				StepResults: a.stepResults, Recommendations: a.recommendations, OversizedTasks: a.oversizedTasks,
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
	return 0
}
