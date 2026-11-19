package docs

import "github.com/edotau/claude-code/internal/plans"

// BriefTask is the active plan's open task a hand-back worked: the dispatch's plan_task, else the brief's Task: line.
func BriefTask(cwd, brief, planTask string) (plans.Task, bool) {
	_, parsed, _, ok := activePlan(cwd)
	if !ok {
		return plans.Task{}, false
	}
	if planTask != "" {
		return matchTaskByID(parsed, normTask(planTask))
	}
	if m := briefTaskRe().FindStringSubmatch(brief); m != nil {
		return matchTaskByID(parsed, normTask(m[1]))
	}
	return plans.Task{}, false
}
