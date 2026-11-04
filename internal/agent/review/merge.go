package review

import (
	"fmt"
	"strconv"
	"strings"
)

const cleanVerdict = "VERDICT: no actionable findings"

// verdictRank orders the neutral review verdicts, worst highest.
var verdictRank = map[string]int{"SHIP IT": 1, "NEEDS WORK": 2, "FIX REQUIRED": 2, "BLOCK": 3}

// MergeReports folds per-part answers into one report: worst verdict first, then deduped findings per part.
func MergeReports(adversarial bool, parts []Part, answers []string) string {
	seen := map[string]bool{}
	var body strings.Builder
	worst, best, unjudged, total := "", 0, false, 0
	for i, ans := range answers {
		lines := strings.Split(strings.TrimSpace(ans), "\n")
		if !adversarial {
			v := strings.ToUpper(strings.Trim(strings.TrimSpace(lines[0]), "*#: ."))
			if r, ok := verdictRank[v]; ok {
				if r > best {
					worst, best = v, r
				}
				lines = lines[1:]
			} else {
				unjudged = true
			}
		}
		fmt.Fprintf(&body, "\n## %s\n", parts[i].Header(i+1, len(parts)))
		n := appendFindings(&body, lines, seen)
		total += n
		if n == 0 {
			body.WriteString("(no findings)\n")
		}
	}
	return mergedVerdict(adversarial, worst, best, unjudged, total, len(parts)) + "\n" + body.String()
}

func mergedVerdict(adversarial bool, worst string, best int, unjudged bool, total, parts int) string {
	head := worst
	if adversarial {
		head = cleanVerdict
		if total > 0 {
			head = "VERDICT: findings across " + strconv.Itoa(parts) + " parts"
		}
	} else if head == "" || (unjudged && best < verdictRank["NEEDS WORK"]) {
		head = "NEEDS WORK" // a part without a parseable verdict is never judged SHIP IT
	}
	return head
}

func appendFindings(body *strings.Builder, lines []string, seen map[string]bool) int {
	n := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" || strings.TrimSuffix(t, ".") == cleanVerdict || seen[t] {
			continue
		}
		seen[t] = true
		body.WriteString(l)
		body.WriteString("\n")
		n++
	}
	return n
}
