package agent

import (
	"fmt"
	"strconv"
	"strings"
)

const cleanVerdict = "VERDICT: no actionable findings"

// verdictRank orders the neutral review verdicts, worst highest.
var verdictRank = map[string]int{"SHIP IT": 1, "NEEDS WORK": 2, "FIX REQUIRED": 2, "BLOCK": 3}

// MergeReports folds per-part answers into one report: a worst-of verdict first, then each part's findings
// (identical lines kept once). Neutral parts lead with a verdict line; adversarial parts are findings only.
func MergeReports(adversarial bool, parts []Part, answers []string) string {
	seen := map[string]bool{}
	var body strings.Builder
	worst, best := "", 0
	for i, ans := range answers {
		lines := strings.Split(strings.TrimSpace(ans), "\n")
		if !adversarial && len(lines) > 0 {
			v := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[0]), "*#: ."))
			if r, ok := verdictRank[v]; ok {
				if r > best {
					worst, best = v, r
				}
				lines = lines[1:]
			}
		}
		fmt.Fprintf(&body, "\n## %s\n", parts[i].Header(i+1, len(parts)))
		n := 0
		for _, l := range lines {
			t := strings.TrimSpace(l)
			if t == "" || t == cleanVerdict || seen[t] {
				continue
			}
			seen[t] = true
			body.WriteString(l)
			body.WriteString("\n")
			n++
		}
		if n == 0 {
			body.WriteString("(no findings)\n")
		}
	}
	head := worst
	if adversarial {
		head = cleanVerdict
		if hasFindings(body.String()) {
			head = "VERDICT: findings across " + strconv.Itoa(len(parts)) + " parts"
		}
	} else if head == "" {
		head = "NEEDS WORK" // no part gave a parseable verdict: do not report SHIP IT by default
	}
	return head + "\n" + body.String()
}

// hasFindings reports whether any part contributed a line beyond its header and "(no findings)" marker.
func hasFindings(body string) bool {
	for l := range strings.SplitSeq(body, "\n") {
		if l != "" && !strings.HasPrefix(l, "## ") && l != "(no findings)" {
			return true
		}
	}
	return false
}
