package doctor

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/edotau/claude-code/internal/agent/waves"
)

// needJobs lists the capabilities a stage can need, in print order, and the permProbes jobs each must be allowed on.
var needJobs = []struct {
	need string
	jobs []string
}{
	{"read", []string{"read a file", "read a diff", "find code"}},
	{"edit", []string{"edit an owned file"}},
	{"write", []string{"write a new file"}},
	{"tests", []string{"run scoped tests"}},
	{"vet", []string{"vet in workdir"}},
	{"gate", []string{"run scoped tests", "vet in workdir"}},
	{"web", []string{"search the web"}},
}

// stageDef is one workflow stage as its script declares it; Tools is the stepOf default (edit, run or "" = read-only).
type stageDef struct {
	Script, Stage, Tools string
	Needs                []string
	Launch               string // when set, the launch mode in place of stepOf's: tools, read-only or no flag
}

// wavesLeafDef is the leaf `agents waves` launches, read from the launch code's own flags, tools setting and brief.
func wavesLeafDef() (stageDef, error) {
	args, tools, brief, err := waves.LeafLaunch(waves.LaunchOptions{}, "a.go", "go test ./x && go vet ./x")
	if err != nil {
		return stageDef{}, err
	}
	needs := stageNeeds(stageTextNeeds([]string{brief}, brief, true, "edit"), false, false)
	return stageDef{Script: "agents waves", Stage: "leaf", Needs: needs, Launch: launchModeOf(args, tools)}, nil
}

// launchModeOf is the launch mode an `ask` command line and a batch line's tools setting amount to.
func launchModeOf(args []string, tools *bool) string {
	switch {
	case slices.Contains(args, "--read-only"):
		return "read-only"
	case slices.Contains(args, "--tools") || (tools != nil && *tools):
		return "tools"
	}
	return "no flag"
}

// leafMode is the permModes column an agent runs a launch mode in.
func leafMode(name, launch string) string {
	if launch == "tools" {
		return stageLaunchMode(name, "edit")
	}
	return launch
}

var (
	stepOfCall = regexp.MustCompile(`stepOf\(\s*[\w.]+,\s*'(\w+)',\s*\{([^}]*)\}`)
	toolsKey   = regexp.MustCompile(`\btools:\s*'(\w+)'`)
	checkRef   = regexp.MustCompile(`\bcheck:\s*(?:\w+\.)*(\w+)`)
	webTool    = regexp.MustCompile(`(?i)websearch|webfetch`)
)

// parseStages reads the stepOf stage table of one script; a stage passed as a stageGate `check:` runs the gate.
func parseStages(script, src string) []stageDef {
	gate := map[string]bool{}
	for _, m := range checkRef.FindAllStringSubmatch(src, -1) {
		gate[m[1]] = true
	}
	var defs []stageDef
	for _, m := range stepOfCall.FindAllStringSubmatch(src, -1) {
		d := stageDef{Script: script, Stage: m[1]}
		if t := toolsKey.FindStringSubmatch(m[2]); t != nil {
			d.Tools = t[1]
		}
		spans, full, ok := promptText(src, d.Stage, stageDepth(d.Tools))
		d.Needs = stageNeeds(stageTextNeeds(spans, full, ok, d.Tools), gate[d.Stage], usesWeb(src, d.Stage))
		defs = append(defs, d)
	}
	return defs
}

// stageDepth is how far into the declarations a prompt names its commands count: a read-only stage's second level
// (a task table) is quoted for it to read, not run.
func stageDepth(tools string) int {
	if tools == "" {
		return 1
	}
	return 2
}

// usesWeb reports whether a line naming the stage (`S.<stage>`) also names a web tool.
func usesWeb(src, stage string) bool {
	ref := regexp.MustCompile(`\w\.` + regexp.QuoteMeta(stage) + `\b`)
	for l := range strings.SplitSeq(src, "\n") {
		if webTool.MatchString(l) && ref.MatchString(l) {
			return true
		}
	}
	return false
}

// stageNeeds is what a stage's job needs: its prompt's needs (textNeeds), plus the gate and web the script wires in.
func stageNeeds(prompt []string, gate, web bool) []string {
	have := map[string]bool{"gate": gate, "web": web}
	for _, n := range prompt {
		have[n] = true
	}
	var out []string
	for _, n := range needJobs {
		if have[n.need] {
			out = append(out, n.need)
		}
	}
	for _, n := range prompt {
		if strings.HasPrefix(n, "run:") {
			out = append(out, n)
		}
	}
	return out
}

// stageLaunchMode is the permModes column a stage runs in on agent name: stepOf passes --read-only only to a
// copilot stage with no tools default, and claude through `ask` is the gateway route.
func stageLaunchMode(name, tools string) string {
	switch {
	case tools == "" && name == "copilot":
		return "read-only"
	case name == "claude":
		return "tools/gateway"
	}
	return "tools"
}

type permStageCell struct {
	Mode    string   `json:"mode"`
	Verdict string   `json:"verdict"`
	Denied  []string `json:"denied,omitempty"`
	Why     string   `json:"not_evaluable,omitempty"`
}

type permStageRow struct {
	Script string                   `json:"script"`
	Stage  string                   `json:"stage"`
	Needs  []string                 `json:"needs"`
	Cells  map[string]permStageCell `json:"cells"`
}

// needVerdict is the first call of the need's probes that agent does not allow, else allow.
func needVerdict(judge permVerdict, need string, probes map[string]probe) permResult {
	if cmd, ok := strings.CutPrefix(need, "run:"); ok {
		return judge("Bash", cmd+" x")
	}
	for _, n := range needJobs {
		if n.need != need {
			continue
		}
		for _, job := range n.jobs {
			pr, ok := probes[job]
			if !ok {
				return permResult{"deny", "no probe " + job}
			}
			if r := judge(pr.tool, pr.arg); r.verdict != "allow" {
				return r
			}
		}
	}
	return permResult{"allow", ""}
}

// stageCell judges every need of a stage against one agent's launch policy: denied beats n/e beats ok.
func stageCell(judge permVerdict, why, mode string, needs []string, probes map[string]probe) permStageCell {
	c := permStageCell{Mode: mode, Verdict: "ok"}
	if judge == nil {
		c.Verdict, c.Why = "n/e", why
		return c
	}
	for _, need := range needs {
		switch r := needVerdict(judge, need, probes); r.verdict {
		case "allow":
		case "n/e":
			c.Why = r.layer
		default:
			c.Denied = append(c.Denied, strings.TrimSpace(need+" "+r.verdict+" "+r.layer))
		}
	}
	switch {
	case len(c.Denied) > 0:
		c.Verdict = "denied"
	case c.Why != "":
		c.Verdict = "n/e"
	}
	return c
}

// permStageRows is one row per stage of every workflows/*.js under dir, plus the `agents waves` leaf, over agents.
func permStageRows(dir string, agents []string, wd string) ([]permStageRow, permTally, error) {
	scripts, err := filepath.Glob(filepath.Join(dir, "*.js"))
	if err != nil {
		return nil, permTally{}, err
	}
	var defs []stageDef
	for _, s := range scripts {
		src, err := os.ReadFile(s)
		if err != nil {
			return nil, permTally{}, err
		}
		defs = append(defs, parseStages(strings.TrimSuffix(filepath.Base(s), ".js"), string(src))...)
	}
	leaf, err := wavesLeafDef()
	if err != nil {
		return nil, permTally{}, err
	}
	defs = append(defs, leaf)
	probes := map[string]probe{}
	for _, p := range permProbes(wd) {
		probes[p.job] = p
	}
	type judged struct {
		judge permVerdict
		why   string
	}
	judges := map[string]judged{}
	var rows []permStageRow
	var t permTally
	for _, d := range defs {
		row := permStageRow{Script: d.Script, Stage: d.Stage, Needs: d.Needs, Cells: map[string]permStageCell{}}
		for _, a := range agents {
			mode := stageLaunchMode(a, d.Tools)
			if d.Launch != "" {
				mode = leafMode(a, d.Launch)
			}
			j, ok := judges[a+"|"+mode]
			if !ok {
				j.judge, j.why = permJudge(a, mode, wd)
				judges[a+"|"+mode] = j
			}
			c := stageCell(j.judge, j.why, mode, d.Needs, probes)
			row.Cells[a] = c
			if c.Verdict != "n/e" {
				t.total, t.met = t.total+1, t.met+b2i(c.Verdict == "ok")
			}
		}
		rows = append(rows, row)
	}
	return rows, t, nil
}

func stageCellText(c permStageCell) string {
	s := c.Verdict
	if len(c.Denied) > 0 {
		s += ": " + strings.Join(c.Denied, ", ")
	}
	if c.Mode == "read-only" {
		s += " [read-only]"
	}
	return s
}

// writePermStages prints the workflow-stage section: rows are stages, columns agents, and a denied need is a miss.
func writePermStages(w io.Writer, agents []string, rows []permStageRow) {
	fmt.Fprintln(w, "workflow stages: needs checked against each agent's grant in the mode its stepOf launch uses")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  script\tstage\tneeds\t%s\n", strings.Join(agents, "\t"))
	var legend []string
	for _, r := range rows {
		cells := make([]string, len(agents))
		for i, a := range agents {
			c := r.Cells[a]
			cells[i] = stageCellText(c)
			if l := "n/e for " + a + ": " + c.Why; c.Why != "" && !slices.Contains(legend, l) {
				legend = append(legend, l)
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", r.Script, r.Stage, strings.Join(r.Needs, ","), strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "  table: %v\n", err)
	}
	for _, l := range legend {
		fmt.Fprintf(w, "  %s\n", l)
	}
	fmt.Fprintln(w)
}
