package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/ask"
	"github.com/edotau/claude-code/internal/agent/waves"
	"github.com/edotau/claude-code/internal/transcript"
)

// pathLaunch is one launch path's launch of one agent, derived from that path's own builders; the zero reason fields
// mean mode (and judge, when the path builds its own policy) are set.
type pathLaunch struct {
	mode, via string
	judge     permVerdict // the policy the path itself builds, in place of the column's (live)
	skip      string      // the agent has no such path
	why       string      // only a live run can derive it
	fail      string      // the path's builder refused or found nothing
}

// pathCase is one row of the launch-path section: derive is what the path launches, ref what the ask path launches for
// the same request (nil: the path has no ask twin, so only the mode's expectations are judged).
type pathCase struct {
	path, name  string
	derive, ref func(agent, wd string) pathLaunch
}

type permPathCell struct {
	Mode    string   `json:"mode,omitempty"`
	Verdict string   `json:"verdict"`
	Denied  []string `json:"denied,omitempty"`
	Via     string   `json:"via,omitempty"`
	Why     string   `json:"why,omitempty"`
}

type permPathRow struct {
	Path  string                  `json:"path"`
	Case  string                  `json:"case"`
	Cells map[string]permPathCell `json:"cells"`
}

// argsLaunch is the launch an `ask` command line (and a batch line's tools setting) amounts to for name.
func argsLaunch(name string, args []string, tools *bool) pathLaunch {
	return pathLaunch{mode: leafMode(name, launchModeOf(args, tools)), via: strings.Join(args, " ")}
}

// askRef is the ask path's launch of the request the flags name.
func askRef(flags ...string) func(string, string) pathLaunch {
	return func(name, _ string) pathLaunch { return argsLaunch(name, flags, nil) }
}

// builtLaunch is argsLaunch over a builder's argv; a refusal is the agent having no such launch.
func builtLaunch(name string, build func() ([]string, error)) pathLaunch {
	args, err := build()
	if err != nil {
		return pathLaunch{skip: err.Error()}
	}
	return argsLaunch(name, args, nil)
}

// exploreLaunch is the `explore` argv, detached or not.
func exploreLaunch(background bool) func(string, string) pathLaunch {
	return func(name, wd string) pathLaunch {
		return builtLaunch(name, func() ([]string, error) { return ask.ExploreArgs(name, wd, "q", false, background) })
	}
}

// transferLaunch is the `transfer` argv, detached or waited on.
func transferLaunch(background bool) func(string, string) pathLaunch {
	return func(name, wd string) pathLaunch {
		return builtLaunch(name, func() ([]string, error) { return transcript.TransferArgs(name, wd, "p", !background), nil })
	}
}

// batchLaunch is the leaf `ask --batch` line a wave launches under the route flags and line tools setting.
func batchLaunch(route []string, tools *bool) func(string, string) pathLaunch {
	return func(name, _ string) pathLaunch {
		args, line, _, err := waves.LeafLaunch(waves.LaunchOptions{Route: route, Tools: tools}, "a.go", "go test ./x")
		if err != nil {
			return pathLaunch{fail: err.Error()}
		}
		return argsLaunch(name, args, line)
	}
}

// jobLaunch is the `ask --job` child: its argv names only the record, which carries the scope through its JSON.
func jobLaunch(readOnly bool) func(string, string) pathLaunch {
	return func(name, wd string) pathLaunch {
		req := agent.RunOptions{Agent: name, Prompt: "q", WorkDir: wd, Tools: true, ReadOnly: readOnly}
		raw, err := json.Marshal(agent.NewRecord(name, req, time.Unix(0, 0)))
		var rec agent.Record
		if err == nil {
			err = json.Unmarshal(raw, &rec)
		}
		if err != nil {
			return pathLaunch{fail: err.Error()}
		}
		args := []string{"agents", "ask", "--job", rec.ID}
		for _, f := range []struct { // the flags the child restores from the record (askTarget.Apply)
			on   bool
			flag string
		}{{rec.Request.Tools, "--tools"}, {rec.Request.ReadOnly, "--read-only"}} {
			if f.on {
				args = append(args, f.flag)
			}
		}
		return argsLaunch(name, args, nil)
	}
}

// pathCases are the launch paths beyond the matrix, one case per request kind the path's builders produce.
func pathCases() []pathCase {
	off := false
	return []pathCase{
		{"job", "tools", jobLaunch(false), askRef("--tools")},
		{"job", "read-only", jobLaunch(true), askRef("--read-only")},
		{"background", "explore", exploreLaunch(true), exploreLaunch(false)},
		{"background", "transfer", transferLaunch(true), transferLaunch(false)},
		{"batch", "line tools", batchLaunch(nil, nil), askRef("--tools")},
		{"batch", "base --read-only", batchLaunch([]string{"--read-only"}, nil), askRef("--read-only")},
		{"batch", "line tools=false", batchLaunch(nil, &off), askRef()},
		{"live", "tools", liveLaunch(false), askRef("--tools")},
		{"live", "read-only", liveLaunch(true), askRef("--read-only")},
		{"relay", "wrapper command", relayLaunch, nil},
	}
}

// pathCell judges the path's launch of name against the ask path's launch of the same request.
func pathCell(pc pathCase, name, wd string) permPathCell {
	got := pc.derive(name, wd)
	switch {
	case got.skip != "":
		return permPathCell{Verdict: "n/a", Why: got.skip}
	case got.why != "":
		return permPathCell{Verdict: verdictRuntime, Why: got.why, Via: got.via}
	case got.fail != "":
		return permPathCell{Verdict: "denied", Denied: []string{got.fail}}
	}
	want := got
	if pc.ref != nil {
		want = pc.ref(name, wd)
	}
	c := permPathCell{Mode: got.mode, Verdict: "ok", Via: got.via}
	if got.mode != want.mode {
		c.Denied = append(c.Denied, "scope: ask runs "+want.mode+", the path runs "+got.mode)
	}
	c.Denied = append(c.Denied, probeDiffs(name, wd, got, want)...)
	if len(c.Denied) > 0 {
		c.Verdict = "denied"
	}
	return c
}

// probeDiffs lists every probe the path's launch judges differently from the ask path's, or fails the mode's expectation.
func probeDiffs(name, wd string, got, want pathLaunch) []string {
	gj, why := got.judge, ""
	if gj == nil {
		gj, why = permJudge(name, got.mode, wd)
	}
	wj, _ := permJudge(name, want.mode, wd)
	if gj == nil || wj == nil {
		return nil
	}
	var out []string
	for _, pr := range permProbes(wd) {
		g, w := gj(pr.tool, pr.arg), wj(pr.tool, pr.arg)
		if c, judged, _ := permCellFor(gj, why, pr, got.mode); judged && !c.Met {
			out = append(out, fmt.Sprintf("%s: path %s, want %s", pr.job, g.verdict, c.Expect))
		} else if g.verdict != w.verdict && g.verdict != verdictRuntime && w.verdict != verdictRuntime {
			out = append(out, fmt.Sprintf("%s: ask %s, path %s", pr.job, w.verdict, g.verdict))
		}
	}
	return out
}

// permPathRows is one row per launch-path case, one cell per agent; n/a and runtime cells are not counted.
func permPathRows(names []string, wd string) ([]permPathRow, permTally) {
	var rows []permPathRow
	var t permTally
	for _, pc := range pathCases() {
		row := permPathRow{Path: pc.path, Case: pc.name, Cells: map[string]permPathCell{}}
		for _, a := range names {
			c := pathCell(pc, a, wd)
			row.Cells[a] = c
			if c.Verdict == "ok" || c.Verdict == "denied" {
				t.total, t.met = t.total+1, t.met+b2i(c.Verdict == "ok")
			}
		}
		rows = append(rows, row)
	}
	return rows, t
}

func pathCellText(c permPathCell) string {
	s := c.Verdict
	if c.Mode != "" {
		s += " [" + c.Mode + "]"
	}
	if len(c.Denied) > 0 {
		s += fmt.Sprintf(" (%d miss)", len(c.Denied))
	}
	return s
}

// writePermPaths prints the launch-path section: rows are paths and request kinds, columns agents, a denial a miss.
func writePermPaths(w io.Writer, names []string, rows []permPathRow) {
	fmt.Fprintln(w, "launch paths: each path's own launch builders, judged against the ask path's launch of the same request")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "  path\tcase\t%s\n", strings.Join(names, "\t"))
	var notes []string
	for _, r := range rows {
		cells := make([]string, len(names))
		for i, a := range names {
			c := r.Cells[a]
			cells[i] = pathCellText(c)
			if c.Why != "" && c.Verdict != "n/a" {
				notes = append(notes, fmt.Sprintf("%s %s %s: %s: %s", r.Path, r.Case, a, c.Verdict, c.Why))
			}
			for _, d := range c.Denied {
				notes = append(notes, fmt.Sprintf("%s %s %s: denied: %s", r.Path, r.Case, a, d))
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", r.Path, r.Case, strings.Join(cells, "\t"))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(w, "  table: %v\n", err)
	}
	for _, n := range notes {
		fmt.Fprintf(w, "  %s\n", n)
	}
	fmt.Fprintln(w)
}
