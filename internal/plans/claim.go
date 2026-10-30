package plans

import (
	"errors"
	"fmt"
	"strings"
)

// Claim writes <!-- text --> at a task's last line, replacing its working:/finished: comment.
func Claim(planPath, taskID, text string) error { return claim(planPath, taskID, text, false) }

// ClaimFree is Claim that leaves a task already carrying a working:/finished: comment alone, decided under the lock.
func ClaimFree(planPath, taskID, text string) error { return claim(planPath, taskID, text, true) }

func claim(planPath, taskID, text string, onlyFree bool) error {
	text = strings.TrimSpace(text)
	if strings.Contains(text, "-->") {
		return fmt.Errorf("plans: claim text must not contain %q", "-->")
	}
	if !claimWord.MatchString(text) {
		return errors.New("plans: claim text must start with working: or finished: prefix")
	}
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if onlyFree && t.Claim != "" {
			return lines
		}
		lines = removeClaims(lines, t)
		lines[t.End] = appendSuffix(lines[t.End], " <!-- "+text+" -->")
		return lines
	})
}

// ClearClaim removes a task's working:/finished: comment; a task without one is left untouched.
func ClearClaim(planPath, taskID string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		return removeClaims(lines, t)
	})
}

// ClearClaimOwned removes a task's claim only when its text contains owner, decided under the lock.
func ClearClaimOwned(planPath, taskID, owner string) error {
	return editTask(planPath, taskID, func(t Task, lines []string) []string {
		if !strings.Contains(t.Claim, owner) {
			return lines
		}
		return removeClaims(lines, t)
	})
}

// findTask resolves taskID to a task: an exact Task.ID first, then a Title prefix.
func findTask(p Plan, taskID string) (Task, bool) {
	for _, t := range p.Tasks {
		if t.ID != "" && t.ID == taskID {
			return t, true
		}
	}
	for _, t := range p.Tasks {
		if taskID != "" && strings.HasPrefix(t.Title, taskID) {
			return t, true
		}
	}
	return Task{}, false
}
