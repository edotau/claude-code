package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var upgradeFixtures = map[string]string{
	"progress.md": "# Progress\n\n## What Works\n- a\n\n## Known Issues\n- x\n\n## What Works\n- b\n",
	"decisionLog.md": "# Log\n\n## 2026-11-01 — first\nbody\n\n## undated one\nbody\n\n" +
		"## 2026-11-03 — third <!-- id:aaaaaaaa -->\nbody\n",
	"conventions.md": "# Conventions\n\n- rule one\n  more\n- rule two <!-- id:bbbbbbbb -->\n",
}

func TestUpgradeStepsIdempotent(t *testing.T) {
	n := 0
	env := Env{Now: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC), NewID: func(int, string) string {
		n++
		return fmt.Sprintf("%08x", n)
	}}
	for _, step := range upgradeSteps {
		for name, src := range upgradeFixtures {
			d := ParseDoc(name, []byte(src))
			plan := step.Plan(d)
			out, res := applyUpgrade(d, plan, env)
			for _, r := range res {
				if r.Outcome != DreamOK {
					t.Fatalf("%s %s: %+v", step.Name, name, r)
				}
			}
			if again := step.Plan(out); len(again.Ops) != 0 {
				t.Errorf("%s %s: second plan not empty: %+v", step.Name, name, again.Ops)
			}
			re := ParseDoc(name, out.Render())
			if string(re.Render()) != string(out.Render()) {
				t.Errorf("%s %s: render changed on reparse", step.Name, name)
			}
		}
	}
	d := ParseDoc("progress.md", []byte(upgradeFixtures["progress.md"]))
	out, _ := applyUpgrade(d, planNormalise(d), env)
	if want := "# Progress\n\n## What Works\n- a\n- b\n\n## Known Issues\n- x\n"; string(out.Render()) != want {
		t.Errorf("merge result %q", out.Render())
	}
}

func TestUpgradeStepsContiguous(t *testing.T) {
	for i, s := range upgradeSteps {
		if s.From != 3+i {
			t.Errorf("step %d: From %d, want %d", i, s.From, 3+i)
		}
	}
	if last := upgradeSteps[len(upgradeSteps)-1]; last.From+1 != storeVersion {
		t.Errorf("last step ends at %d, storeVersion %d", last.From+1, storeVersion)
	}
}

func TestUpgradeStepsDateParenthetical(t *testing.T) {
	src := "# Log\n\n## 2026-10-14 (night) — late <!-- id:cccccccc -->\nbody\n\n## undated <!-- id:dddddddd -->\nbody\n"
	d := ParseDoc("decisionLog.md", []byte(src))
	ops := planDate(d)
	if len(ops) != 1 {
		t.Fatalf("want one date op, got %+v", ops)
	}
	out, _ := applyUpgrade(d, Batch{Ops: ops}, Env{})
	want := "## 2026-10-14 — undated <!-- id:dddddddd -->"
	if got := out.Entries[len(out.Entries)-1].Lines[0]; got != want {
		t.Errorf("dated heading %q, want %q", got, want)
	}
	if again := planDate(out); len(again) != 0 {
		t.Errorf("second plan not empty: %+v", again)
	}
}

func TestUpgradeStepsOneMarker(t *testing.T) {
	lines := []string{"## 2026-11-01 — entry", "body", "tail <!-- id:eeeeeeee -->"}
	env := Env{NewID: func(int, string) string { return "ffffffff" }}
	got := identified("decisionLog.md", 0, lines, env)
	if n := strings.Count(strings.Join(got, "\n"), "<!-- id:"); n != 1 {
		t.Errorf("want one marker, got %d: %q", n, got)
	}
}

// Two machines upgrading the same entry at different times and positions must mint the same id.
func TestUpgradeContentIDsMachineIndependent(t *testing.T) {
	render := func(src string, now time.Time) string {
		d := ParseDoc("conventions.md", []byte(src))
		out, _ := applyUpgrade(d, upgradeSteps[0].Plan(d), Env{Now: now, NewID: contentIDs(d)})
		return string(out.Render())
	}
	a := render("# Conventions\n\n- rule one\n", time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC))
	b := render("# Conventions\n\n- other first\n- rule one\n", time.Date(2026, 11, 26, 9, 0, 0, 1, time.UTC))
	line := func(s string) string {
		for ln := range strings.SplitSeq(s, "\n") {
			if strings.HasPrefix(ln, "- rule one") {
				return ln
			}
		}
		return ""
	}
	if line(a) == "" || !strings.Contains(line(a), "<!-- id:") || line(a) != line(b) {
		t.Errorf("ids differ or missing: %q vs %q", line(a), line(b))
	}
}
