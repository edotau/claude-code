package docs

import (
	"regexp"
	"sync"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// BriefTask is the active plan's open task a hand-back worked: the dispatch's plan_task, else the brief's Task: line.
func BriefTask(cwd, brief, planTask string) (plans.Task, bool) {
	_, parsed, _, ok := activePlan(cwd)
	if !ok {
		return plans.Task{}, false
	}
	return PlanBriefTask(parsed, brief, planTask)
}

// PlanBriefTask is BriefTask over an already parsed active plan.
func PlanBriefTask(parsed plans.Plan, brief, planTask string) (plans.Task, bool) {
	if planTask != "" {
		return matchTaskByID(parsed, normTask(planTask))
	}
	if m := briefTaskRe().FindStringSubmatch(brief); m != nil {
		return matchTaskByID(parsed, normTask(m[1]))
	}
	return plans.Task{}, false
}

var (
	briefPlanRe = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(~?/\S*plans/\S+?\.md)`) })
	taskOfRe    = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\bTask\s+([\w.]+)\s+of\b`) })
)

// NamedPlanTask resolves a brief that names a plan file: that plan's open task, never another plan's.
// named is false when the brief names no plan; the brief's own task id beats the dispatch's plan_task.
func NamedPlanTask(brief, planTask string) (planPath string, task plans.Task, named, ok bool) {
	m := briefPlanRe().FindStringSubmatch(brief)
	if m == nil {
		return "", plans.Task{}, false, false
	}
	planPath = paths.ExpandHome(m[1])
	parsed, err := plans.Parse(planPath)
	if err != nil {
		return planPath, plans.Task{}, true, false
	}
	id := planTask
	if t := briefTaskRe().FindStringSubmatch(brief); t != nil {
		id = t[1]
	} else if t := taskOfRe().FindStringSubmatch(brief); t != nil {
		id = t[1]
	}
	task, ok = matchTaskByID(parsed, normTask(id))
	return planPath, task, true, ok
}
