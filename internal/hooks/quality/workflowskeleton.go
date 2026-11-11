package quality

import (
	"fmt"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/ui/helptext"
)

const skeletonUsage = "usage: claude-code workflow skeleton [-h] [--name NAME]\n" +
	"                                     {evaluator,orchestrator,parallel,pipeline}\n"

const noteText = "\n// ── Fill in before running ──\n" +
	"//  • Replace *_SCHEMA with JSON Schemas (or drop the {schema:...} opt for free-text agents).\n" +
	"//  • Every agent() payload needs a self-contained handoff (objective + constraints + artifacts\n" +
	"//    + acceptance + verify command) — see skills/subagent-workflows/SKILL.md. Cold-start test:\n" +
	"//    a fresh agent with zero context can execute it.\n" +
	"//  • Pipeline is the default (no barrier). Use parallel() only when a step needs ALL prior results.\n" +
	"//  • 'router' isn't a Workflow shape — it's an inline branch on the input, then dispatch. No skeleton.\n"

// jsSingleQuoteEscaper makes a user-supplied name safe inside a '...' JS literal.
var jsSingleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`, "\n", `\n`, "\r", `\r`)

func metaBlock(name, desc, phases string) string {
	return fmt.Sprintf("export const meta = {\n  name: '%s',\n  description: '%s',\n  phases: [%s],\n}", jsSingleQuoteEscaper.Replace(name), desc, phases)
}

func pipelinePattern(name string) string {
	return metaBlock(name, "Pipeline: each item through ordered stages, no barrier", "{ title: 'Work' }, { title: 'Verify' }") +
		"\n\nconst ITEMS = args ?? []   // pass the work-list via Workflow `args`\n" +
		"const results = await pipeline(\n" +
		"  ITEMS,\n" +
		"  (item) => agent(`Do the work for: ${JSON.stringify(item)}`, {phase: 'Work', model: 'sonnet', effort: 'medium', schema: WORK_SCHEMA}),\n" +
		"  (work, item) => agent(`Adversarially verify: ${work.summary}`, {phase: 'Verify', model: 'sonnet', effort: 'medium', schema: VERDICT_SCHEMA})\n" +
		"    .then(v => ({...work, verdict: v})),\n" +
		")\n" +
		"return results.filter(Boolean).filter(r => r.verdict?.ok)"
}

func parallelPattern(name string) string {
	return metaBlock(name, "Parallel fan-out with a barrier, then synthesize", "{ title: 'Fan-out' }, { title: 'Synthesize' }") +
		"\n\nconst TASKS = args ?? []\n" +
		"phase('Fan-out')\n" +
		"const found = (await parallel(TASKS.map(t => () =>\n" +
		"  agent(`Investigate: ${JSON.stringify(t)}`, {model: 'sonnet', effort: 'medium', schema: FINDING_SCHEMA})))).filter(Boolean)\n" +
		"// Barrier justified: synthesis needs the FULL set (dedup/merge across all findings).\n" +
		"phase('Synthesize')\n" +
		"return await agent(`Synthesize these findings into one report: ${JSON.stringify(found)}`, {model: 'sonnet', effort: 'medium'})"
}

func evaluatorPattern(name string) string {
	return metaBlock(name, "Generate, then adversarially verify before accepting", "{ title: 'Generate' }, { title: 'Verify' }") +
		"\n\nphase('Generate')\n" +
		"const draft = await agent('Produce the artifact.', {model: 'sonnet', effort: 'medium', schema: DRAFT_SCHEMA})\n" +
		"phase('Verify')\n" +
		"// N independent skeptics, each prompted to REFUTE; accept only if a majority can't.\n" +
		"const votes = (await parallel(['correctness', 'security', 'repro'].map(lens => () =>\n" +
		"  agent(`Try to refute the ${lens} of: ${draft.summary}. Default refuted=true if unsure.`,\n" +
		"        {model: 'sonnet', effort: 'medium', schema: VERDICT_SCHEMA})))).filter(Boolean)\n" +
		"const accepted = votes.filter(v => !v.refuted).length >= 2\n" +
		"return {draft, accepted, votes}"
}

func orchestratorPattern(name string) string {
	return metaBlock(name, "Planner decomposes, then pipeline over the work-list", "{ title: 'Plan' }, { title: 'Build' }") +
		"\n\nphase('Plan')\n" +
		"// Discover the work-list at runtime, then fan out over it (don't hardcode the shape).\n" +
		"const plan = await agent(`Break down this goal into parallel-safe tasks: ${args?.goal ?? ''}`,\n" +
		"                         {model: 'sonnet', effort: 'medium', schema: PLAN_SCHEMA})\n" +
		"phase('Build')\n" +
		"// isolation:'worktree' ONLY if tasks mutate files in parallel (else drop it — it's expensive).\n" +
		"const done = await pipeline(\n" +
		"  plan.tasks,\n" +
		"  (task) => agent(task.brief, {phase: 'Build', isolation: 'worktree', model: 'sonnet', effort: 'medium', schema: RESULT_SCHEMA}),\n" +
		"  (result) => agent(`Review for spec + quality: ${result.summary}`, {phase: 'Build', model: 'sonnet', effort: 'medium', schema: REVIEW_SCHEMA})\n" +
		"    .then(r => ({...result, review: r})),\n" +
		")\n" +
		"return done.filter(Boolean)"
}

var skeletonPatterns = map[string]func(string) string{
	"pipeline":     pipelinePattern,
	"parallel":     parallelPattern,
	"evaluator":    evaluatorPattern,
	"orchestrator": orchestratorPattern,
}

// skeletonArgs is the parsed command line; code/done report an early exit (usage error).
type skeletonArgs struct {
	name                      string
	positionals, unrecognized []string
	code                      int
	done                      bool
}

func parseSkeletonArgs(args []string, stderr io.Writer) skeletonArgs {
	p := skeletonArgs{name: "new-workflow"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--name":
			i++
			if i >= len(args) {
				fmt.Fprintln(stderr, "workflow skeleton: error: argument --name: expected one argument")
				return skeletonArgs{code: 2, done: true}
			}
			p.name = args[i]
		case strings.HasPrefix(a, "--name="):
			p.name = strings.TrimPrefix(a, "--name=")
		case strings.HasPrefix(a, "-") && a != "-":
			p.unrecognized = append(p.unrecognized, a)
		case len(p.positionals) == 1:
			p.unrecognized = append(p.unrecognized, a)
		default:
			p.positionals = append(p.positionals, a)
		}
	}
	return p
}

// RunSkeleton emits a JavaScript workflow skeleton for the selected pattern.
func RunSkeleton(args []string, stdout, stderr io.Writer) int {
	parsed := parseSkeletonArgs(args, stderr)
	if parsed.done {
		return parsed.code
	}
	name, positionals, unrecognized := parsed.name, parsed.positionals, parsed.unrecognized
	if len(unrecognized) > 0 {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintf(stderr, "workflow skeleton: error: unrecognized arguments: %s\n", strings.Join(unrecognized, " "))
		return 2
	}

	if len(positionals) == 0 {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintln(stderr, "workflow skeleton: error: the following arguments are required: pattern")
		return 2
	}

	pattern := positionals[0]
	fn, ok := skeletonPatterns[pattern]
	if !ok {
		helptext.Fprint(stderr, skeletonUsage)
		fmt.Fprintf(stderr, "workflow skeleton: error: argument pattern: invalid choice: %q (choose from 'evaluator', 'orchestrator', 'parallel', 'pipeline')\n", pattern)
		return 2
	}

	if _, err := fmt.Fprintln(stdout, fn(name)); err != nil {
		fmt.Fprintf(stderr, "workflow skeleton: error: writing skeleton: %v\n", err)
		return 1
	}
	if _, err := io.WriteString(stdout, noteText+"\n"); err != nil {
		fmt.Fprintf(stderr, "workflow skeleton: error: writing note: %v\n", err)
		return 1
	}
	return 0
}
