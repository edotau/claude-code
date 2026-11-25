package plans

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/proc"
)

// WaveTask is a package or manual verifier read from a plan or manifest.
type WaveTask struct {
	ID       string   `json:"id"`
	Wave     int      `json:"wave"`
	Agent    string   `json:"agent"`
	Profile  string   `json:"profile,omitempty"`
	Owns     []string `json:"owns,omitempty"`
	Task     string   `json:"task"`
	Verify   string   `json:"verify,omitempty"`
	Findings string   `json:"findings,omitempty"`
	Leaf     string   `json:"leaf,omitempty"`
	Manual   bool     `json:"manual,omitempty"`
	Done     bool     `json:"done,omitempty"` // ticked in the plan
	// BlockedBy names the task ids that must be done first; BlockedText is a prose blocker only main can lift.
	BlockedBy   []string `json:"blocked_by,omitempty"`
	BlockedText string   `json:"blocked_text,omitempty"`
}

// Wave groups packages and manual verifiers with their gate and table proof.
type Wave struct {
	Number int
	Tasks  []WaveTask
	Gate   string
	Verify string
}

// WavePlan retains ordered waves and manual baseline tasks outside the wave table.
type WavePlan struct {
	Path   string
	Waves  []Wave
	Manual []WaveTask
}

// WaveError identifies the task or wave that prevents launching a plan.
type WaveError struct{ ID, Reason string }

func (e *WaveError) Error() string { return e.ID + ": " + e.Reason }

const maxWavePackages = 10000

var waveID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)^(?:Phase\s+)?(\d+)\.(\d+|V)\b`) })
var waveAttr = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b(leaf|agent|profile|verify|findings|owns):\s*|\b(?-i:(Blocked\s+by))\b:?\s*`)
})
var blockerID = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\b(?:Phase\s+)?(\d+)\.(\d+|V)\b`) })
var packageRange = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(\d+)\.(\d+|V)(?:\s*[-–]\s*(\d+)\.(\d+|V))?`) })

// gitWord is one shell word piece: a bare character or a quoted run.
const gitWord = `(?:[^\s;&|"']|"[^"]*"|'[^']*')`

// gitWrite matches a git write verb after any global flags (-C x, -c k=v, --git-dir=x, --no-pager).
var gitWrite = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)\bgit(?:\s+-` + gitWord + `+(?:\s+(?:[^-\s;&|"']|"[^"]*"|'[^']*')` +
		gitWord + `*)?)*\s+` +
		`(?:(?:add|commit|stash|reset|checkout|switch|restore|clean|push|rebase|merge|rm|mv|cherry-pick|am|tag|pull|revert|` +
		`update-ref)(?:[\s;&|)]|$)|apply\b[^;&|]*\s--(?:index|cached)\b)`)
})

// GateRefused is the one git-write refusal: wave gates, task verify lines and the gate runner all call it.
func GateRefused(cmd string) error {
	if m := gitWrite().FindString(cmd); m != "" {
		return &WaveError{ID: "gate", Reason: fmt.Sprintf("git write command refused: %q", strings.TrimSpace(m))}
	}
	return nil
}

// ParseWaves reads checklist ownership with Parse, then adds wave metadata and phase bodies.
func ParseWaves(file string) (WavePlan, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return WavePlan{Path: file}, err
	}
	return parseWavesBytes(file, raw)
}

// parseWavesBytes is ParseWaves over the plan text raw, read from file.
func parseWavesBytes(file string, raw []byte) (WavePlan, error) {
	base := parseBytes(file, raw)
	p := WavePlan{Path: file}
	lines := strings.Split(string(raw), "\n")
	gate, bodies := phaseDetails(lines)
	for _, task := range base.Tasks {
		m := waveID().FindStringSubmatch(task.Title)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return p, err
		}
		attrs := waveAttributes(task.Title)
		_, _, owns, _, _, _ := parseTaskText("owns: " + attrs["owns"])
		t := WaveTask{ID: "Phase " + m[1] + "." + strings.ToUpper(m[2]), Wave: n, Agent: attrs["agent"],
			Leaf: attrs["leaf"], Profile: attrs["profile"], Owns: ownEntries(owns, nil),
			Verify: attrs["verify"], Findings: attrs["findings"], Task: bodies[m[1]+"."+strings.ToUpper(m[2])],
			Done: task.closed()}
		t.BlockedBy, t.BlockedText = blockers(attrs["blocked by"])
		if t.Task == "" {
			t.Task = "Implement " + t.ID + " from " + file
		}
		if t.Leaf == "" {
			return p, &WaveError{t.ID, "missing leaf:"}
		}
		p.addTask(t)
	}
	for i := range p.Waves {
		p.Waves[i].Gate = gate
	}
	if err := p.validate(); err != nil {
		return p, err
	}
	return p, p.checkTable(lines)
}

func waveAttributes(title string) map[string]string {
	out := map[string]string{}
	matches := waveAttr().FindAllStringSubmatchIndex(title, -1)
	for i, m := range matches {
		end := len(title)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		key := "blocked by"
		if m[2] >= 0 {
			key = strings.ToLower(title[m[2]:m[3]])
		}
		val := strings.Trim(strings.TrimSpace(title[m[1]:end]), " ,—–")
		if key == "agent" || key == "leaf" || key == "profile" {
			val = strings.TrimRight(val, ".,;:")
		}
		out[key] = val
	}
	return out
}

// blockers splits a Blocked by value into task ids and any leftover prose, which names no task.
func blockers(val string) ([]string, string) {
	var ids []string
	for _, m := range blockerID().FindAllStringSubmatch(val, -1) {
		if id := "Phase " + m[1] + "." + strings.ToUpper(m[2]); !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	rest := strings.FieldsFunc(blockerID().ReplaceAllString(val, " "), func(r rune) bool { return strings.ContainsRune(" ,;.&", r) })
	rest = slices.DeleteFunc(rest, func(w string) bool { return strings.EqualFold(w, "and") })
	if len(rest) == 0 {
		return ids, ""
	}
	return ids, strings.Trim(val, " .")
}

type phaseScan struct {
	section, current string
	open             fenceOpen
	capture          bool
	captured         bool
	gate             strings.Builder
	bodies           map[string]string
}

func phaseDetails(lines []string) (string, map[string]string) {
	s := &phaseScan{bodies: map[string]string{}}
	for _, line := range lines {
		if ch, n, rest := fenceMarker(line); n > 0 {
			s.fence(ch, n, rest)
		} else if s.open.n > 0 {
			if s.capture {
				s.gate.WriteString(line)
				s.gate.WriteByte('\n')
			}
		} else {
			s.text(line)
		}
	}
	return strings.TrimSpace(s.gate.String()), s.bodies
}

func (s *phaseScan) fence(ch byte, n int, rest string) {
	if s.open.n == 0 {
		s.capture = s.section == "## Phases" && rest == "bash" && !s.captured
		s.captured = s.captured || s.capture
	}
	s.open = s.open.next(ch, n, rest)
	if s.open.n == 0 {
		s.capture = false
	}
}

func (s *phaseScan) text(line string) {
	if strings.HasPrefix(line, "## ") {
		s.section, s.current = strings.TrimSpace(line), ""
		return
	}
	if s.section != "## Phases" {
		return
	}
	if strings.HasPrefix(line, "- **") {
		s.current = ""
		if m := waveID().FindStringSubmatch(strings.TrimPrefix(line, "- **")); m != nil {
			s.current = m[1] + "." + strings.ToUpper(m[2])
		}
	}
	if s.current != "" {
		s.bodies[s.current] += line + "\n"
	}
}

func (p *WavePlan) addTask(t WaveTask) {
	t.Manual = t.Agent == "in-context" || strings.HasSuffix(strings.ToUpper(t.ID), ".V")
	if t.Wave == 0 && t.Manual {
		p.Manual = append(p.Manual, t)
		return
	}
	for i := range p.Waves {
		if p.Waves[i].Number == t.Wave {
			p.Waves[i].Tasks = append(p.Waves[i].Tasks, t)
			return
		}
	}
	p.Waves = append(p.Waves, Wave{Number: t.Wave, Tasks: []WaveTask{t}})
}

func (p *WavePlan) validate() error {
	seen := map[string]bool{}
	all := slices.Clone(p.Manual)
	for _, w := range p.Waves {
		if err := GateRefused(w.Gate); err != nil {
			return &WaveError{fmt.Sprintf("wave %d", w.Number), err.Error()}
		}
		all = append(all, w.Tasks...)
		if err := waveOverlap(w); err != nil {
			return err
		}
	}
	for _, t := range all {
		if seen[t.ID] {
			return &WaveError{t.ID, "duplicate id"}
		}
		seen[t.ID] = true
		if !slices.Contains([]string{"claude", "codex", "copilot", "gemini", "opencode", "pi", "in-context"}, t.Agent) {
			return &WaveError{t.ID, "unknown Agent: " + t.Agent}
		}
		if err := checkTask(t); err != nil {
			return err
		}
	}
	slices.SortFunc(p.Waves, func(a, b Wave) int { return a.Number - b.Number })
	return checkBlockers(all)
}

// checkBlockers refuses a Blocked by that names an unknown task, an open task of a later wave, or a cycle.
func checkBlockers(all []WaveTask) error {
	byID := make(map[string]WaveTask, len(all))
	for _, t := range all {
		byID[t.ID] = t
	}
	for _, t := range all {
		for _, id := range t.BlockedBy {
			b, ok := byID[id]
			switch {
			case !ok:
				return &WaveError{t.ID, "Blocked by: unknown task " + id}
			case !b.Done && b.Wave > t.Wave:
				return &WaveError{t.ID, "Blocked by: " + id + " runs in a later wave"}
			}
		}
	}
	state := map[string]int{} // 1 visiting, 2 done
	for _, t := range all {
		if cycle := blockerCycle(t.ID, byID, state, nil); cycle != nil {
			return &WaveError{cycle[0], "Blocked by: cycle " + strings.Join(cycle, " -> ")}
		}
	}
	return nil
}

// blockerCycle walks id's blockers depth first and returns the first cycle found, closed on its first id.
func blockerCycle(id string, byID map[string]WaveTask, state map[string]int, path []string) []string {
	switch state[id] {
	case 1:
		return append(path[slices.Index(path, id):], id)
	case 2:
		return nil
	}
	state[id] = 1
	for _, b := range byID[id].BlockedBy {
		if cycle := blockerCycle(b, byID, state, append(path, id)); cycle != nil {
			return cycle
		}
	}
	state[id] = 2
	return nil
}

// checkTask refuses a git-writing verify line and, for a launched task, any owns: entry that is not inside the repo.
func checkTask(t WaveTask) error {
	if err := GateRefused(t.Verify); err != nil {
		return &WaveError{t.ID, "verify: " + strings.TrimPrefix(err.Error(), "gate: ")}
	}
	if t.Manual {
		return nil
	}
	if len(t.Owns) == 0 {
		return &WaveError{t.ID, "launched task has no owns:"}
	}
	for _, own := range t.Owns {
		if reason := ownsRefusal(own); reason != "" {
			return &WaveError{t.ID, fmt.Sprintf("owns: %q %s", own, reason)}
		}
	}
	return nil
}

// ownsRefusal explains why an owns: entry is not a clean repo-relative path or narrow glob, or returns "".
func ownsRefusal(own string) string {
	clean := path.Clean(strings.TrimSpace(own))
	switch {
	case strings.TrimSpace(own) == "":
		return "is empty"
	case strings.HasPrefix(clean, "/"):
		return "is an absolute path"
	case clean == ".":
		return "is the repo root"
	case clean == ".." || strings.HasPrefix(clean, "../"):
		return "leaves the repo"
	case strings.Trim(clean, "*?[]/") == "":
		return "is a glob that matches the repo root or everything"
	}
	if _, err := path.Match(clean, ""); err != nil {
		return "is a bad glob"
	}
	return ""
}

// closed reports a ticked or struck task: neither launches a leaf.
func (t Task) closed() bool { return t.Done || t.Dropped }

func waveOverlap(w Wave) error {
	owners := map[string]string{}
	for _, t := range w.Tasks {
		if t.Manual || t.Done {
			continue // a ticked task launches nothing, so its files are free
		}
		for _, own := range t.Owns {
			if strings.EqualFold(strings.TrimSpace(own), "none") {
				continue // `owns: none` marks a task that edits no file, so it shares nothing
			}
			for _, prev := range slices.Sorted(maps.Keys(owners)) {
				id := owners[prev]
				if id != t.ID && OwnsOverlap(prev, own) {
					return &WaveError{t.ID, "owns: overlaps " + id + " at " + own}
				}
			}
			owners[own] = t.ID
		}
	}
	return nil
}

// OwnsOverlap reports whether two owned paths or globs name the same file or one contains the other.
func OwnsOverlap(a, b string) bool {
	a, b = path.Clean(a), path.Clean(b)
	if a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
		return true
	}
	x, e := path.Match(a, b)
	y, f := path.Match(b, a)
	return (e == nil && x) || (f == nil && y)
}

func tableCells(line string) []string {
	cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func tableColumns(cells []string) (packages, verify int) {
	packages, verify = -1, -1
	for i, value := range cells {
		if value == "Packages" {
			packages = i
		}
		if strings.HasPrefix(value, "Verify") {
			verify = i
		}
	}
	return packages, verify
}

func (p *WavePlan) checkTable(lines []string) error {
	in, packages, verify := false, -1, -1
	rows := map[int]bool{}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			in = strings.TrimSpace(line) == "## Waves"
			continue
		}
		if !in || !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		c := tableCells(line)
		if c[0] == "Wave" {
			packages, verify = tableColumns(c)
			continue
		}
		n, ok := waveRowNumber(c[0])
		if !ok {
			continue
		}
		if err := p.checkWaveRow(rows, c, n, packages, verify); err != nil {
			return err
		}
	}
	return p.missingRow(packages >= 0, rows)
}

func (p *WavePlan) missingRow(hasTable bool, rows map[int]bool) error {
	for _, w := range p.Waves {
		if hasTable && !rows[w.Number] {
			return &WaveError{w.Tasks[0].ID, "missing Waves row"}
		}
	}
	return nil
}

func waveRowNumber(cell string) (int, bool) {
	fields := strings.Fields(cell)
	if len(fields) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(fields[0])
	return n, err == nil
}

func (p *WavePlan) checkWaveRow(rows map[int]bool, c []string, n, packages, verify int) error {
	if packages < 0 || packages >= len(c) || verify < 0 || verify >= len(c) {
		return &WaveError{c[0], "invalid Waves table"}
	}
	if rows[n] {
		return &WaveError{c[0], "duplicate Waves row"}
	}
	rows[n] = true
	return p.checkRow(n, c[packages], c[verify])
}

func tablePackages(packages string) (map[string]bool, error) {
	expected := map[string]bool{}
	for _, m := range packageRange().FindAllStringSubmatch(packages, -1) {
		first, e := strconv.Atoi(m[2])
		if e != nil {
			return nil, &WaveError{m[0], "invalid package range"}
		}
		last := first
		if m[4] != "" {
			var err error
			last, err = strconv.Atoi(m[4])
			if err != nil || m[1] != m[3] {
				return nil, &WaveError{m[0], "invalid package range"}
			}
		}
		if last < first || last-first > maxWavePackages {
			return nil, &WaveError{m[0], "invalid package range"}
		}
		for k := first; k <= last; k++ {
			expected["Phase "+m[1]+"."+strconv.Itoa(k)] = true
		}
	}
	return expected, nil
}

func (p *WavePlan) checkRow(n int, packages, verify string) error {
	expected, err := tablePackages(packages)
	if err != nil {
		return err
	}
	for i, w := range p.Waves {
		if w.Number != n {
			continue
		}
		for _, t := range w.Tasks {
			if strings.HasSuffix(t.ID, ".V") {
				continue
			}
			if !expected[t.ID] {
				return &WaveError{t.ID, "Waves table membership disagrees"}
			}
			delete(expected, t.ID)
		}
		if len(expected) > 0 {
			return &WaveError{slices.Sorted(maps.Keys(expected))[0], "Waves table package has no task"}
		}
		p.Waves[i].Verify = verify
		return nil
	}
	return &WaveError{fmt.Sprintf("wave %d", n), "Waves table has no tasks"}
}

// ParseManifest reads JSONL tasks and optional per-wave gate records from a file.
func ParseManifest(file string) (WavePlan, error) {
	p := WavePlan{Path: file}
	f, err := os.Open(file)
	if err != nil {
		return p, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	gates := map[int]string{}
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		if err := p.manifestRow(scanner.Bytes(), gates); err != nil {
			return p, err
		}
	}
	if err := scanner.Err(); err != nil {
		return p, err
	}
	for i := range p.Waves {
		p.Waves[i].Gate = gates[p.Waves[i].Number]
		delete(gates, p.Waves[i].Number)
	}
	if len(gates) > 0 {
		return p, &WaveError{"gate", "wave has no tasks"}
	}
	return p, p.validate()
}

func (p *WavePlan) manifestRow(line []byte, gates map[int]string) error {
	var row struct {
		WaveTask
		Gate *string `json:"gate"`
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	if row.Gate != nil {
		if err := GateRefused(*row.Gate); err != nil {
			return err
		}
		gates[row.Wave] = *row.Gate
		return nil
	}
	task, err := manifestTask(row.WaveTask)
	if err != nil {
		return err
	}
	p.addTask(task)
	return nil
}

func manifestTask(t WaveTask) (WaveTask, error) {
	m := waveID().FindStringSubmatch(t.ID)
	if m == nil || m[0] != t.ID {
		return t, &WaveError{t.ID, "invalid task id"}
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return t, err
	}
	if n != t.Wave {
		return t, &WaveError{t.ID, "id disagrees with wave"}
	}
	t.ID = "Phase " + m[1] + "." + strings.ToUpper(m[2])
	t.Owns = ownEntries(t.Owns, nil)
	for i, b := range t.BlockedBy { // an id that does not parse stays as written and is refused as unknown
		if m := waveID().FindStringSubmatch(b); m != nil && m[0] == b {
			t.BlockedBy[i] = "Phase " + m[1] + "." + strings.ToUpper(m[2])
		}
	}
	return t, nil
}

// TaskLine renders t as one plan task line in the documented attribute order; Go writes it, the model gives fields.
func TaskLine(t WaveTask) string {
	parts := []string{"- [ ] " + t.ID + " — " + t.Task}
	if t.Leaf != "" || t.Agent != "" {
		var attrs []string
		for _, kv := range [][2]string{{"leaf", t.Leaf}, {"Agent", t.Agent}} {
			if kv[1] != "" {
				attrs = append(attrs, kv[0]+": "+kv[1])
			}
		}
		parts = append(parts, strings.Join(attrs, ", "))
	}
	if t.Profile != "" {
		parts = append(parts, "profile: "+t.Profile)
	}
	if t.Verify != "" {
		parts = append(parts, "verify: "+t.Verify)
	}
	if len(t.BlockedBy) > 0 {
		parts = append(parts, "Blocked by: "+strings.Join(t.BlockedBy, ", "))
	}
	if len(t.Owns) > 0 {
		parts = append(parts, "owns: "+strings.Join(t.Owns, ", "))
	}
	return strings.Join(parts, " — ")
}

// checkNewTask refuses a task with no id or title, a field the line cannot hold, or an unlaunchable owns/verify.
func checkNewTask(t WaveTask) error {
	if t.ID == "" || t.Task == "" {
		return errors.New("plans: a new task needs an id and a title")
	}
	for _, s := range slices.Concat([]string{t.ID, t.Task, t.Leaf, t.Agent, t.Profile, t.Verify}, t.Owns, t.BlockedBy) {
		if strings.Contains(s, " — ") || waveAttr().MatchString(s) {
			return fmt.Errorf("plans: field %q holds an em dash separator or an attribute key such as \"profile:\"", s)
		}
	}
	t.Manual = t.Agent == "in-context" || strings.HasSuffix(strings.ToUpper(t.ID), ".V")
	return checkTask(t)
}

// AddTask validates t and appends its rendered line after the plan's last task, under the plans lock.
func AddTask(planPath string, t WaveTask) error {
	t.ID, t.Task = strings.TrimSpace(t.ID), strings.Join(strings.Fields(t.Task), " ")
	if err := checkNewTask(t); err != nil {
		return err
	}
	return editPlan(planPath, func(text string) (string, error) {
		p := parseBytes(planPath, []byte(text))
		if _, ok := findTask(p, t.ID); ok {
			return "", fmt.Errorf("plans: task %q already exists in %s", t.ID, planPath)
		}
		for _, b := range t.BlockedBy {
			if ids, rest := blockers(b); rest != "" || !slices.Equal(ids, []string{b}) ||
				!slices.ContainsFunc(p.Tasks, func(o Task) bool { return o.ID == b }) {
				return "", fmt.Errorf("plans: blocked-by %q is not a \"Phase N.M\" task id of %s", b, planPath)
			}
		}
		at, err := tasksEnd(strings.Split(text, "\n"), p)
		if err != nil {
			return "", err
		}
		out := strings.Join(slices.Insert(strings.Split(text, "\n"), at, TaskLine(t)), "\n")
		return out, checkRendered(planPath, out, t, len(p.Tasks))
	})
}

// tasksSpan is the line range of the "## Tasks" section: its heading and the next "## " heading or the end.
func tasksSpan(lines []string) (start, stop int, ok bool) {
	start = slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == "## Tasks" })
	if start < 0 {
		return 0, 0, false
	}
	next := slices.IndexFunc(lines[start+1:], func(l string) bool { return strings.HasPrefix(l, "## ") })
	if next < 0 {
		return start, len(lines), true
	}
	return start, start + 1 + next, true
}

// tasksEnd is the line after the last task of the "## Tasks" section, or its last non-blank line when it has none.
func tasksEnd(lines []string, p Plan) (int, error) {
	start, stop, ok := tasksSpan(lines)
	if !ok {
		return 0, errors.New("plans: the plan has no \"## Tasks\" heading")
	}
	end := start
	for _, t := range p.Tasks {
		if t.Start > start && t.End < stop {
			end = max(end, t.End)
		}
	}
	for end == start && stop-1 > start && strings.TrimSpace(lines[stop-1]) == "" {
		stop--
	}
	if end == start {
		end = stop - 1
	}
	return end + 1, nil
}

// checkRendered refuses text where the new line does not parse back as t, or a Phase task the wave parser rejects.
func checkRendered(planPath, text string, t WaveTask, before int) error {
	np := parseBytes(planPath, []byte(text))
	if !readsBack(np, t, before) {
		return fmt.Errorf("plans: task %q does not read back from its rendered line; use a plain id such as \"Phase 2.1\"", t.ID)
	}
	if !waveID().MatchString(t.ID) {
		return nil
	}
	_, err := parseWavesBytes(planPath, []byte(text))
	var we *WaveError
	if !errors.As(err, &we) {
		return err
	}
	if we.ID == t.ID && (we.Reason == "missing Waves row" || strings.Contains(we.Reason, "membership")) {
		return fmt.Errorf("plans: add %q to the Packages cell of the \"## Waves\" row for wave %s (or a new row), then re-run",
			t.ID, strings.TrimPrefix(strings.SplitN(t.ID, ".", 2)[0], "Phase "))
	}
	return fmt.Errorf("plans: the new task breaks the wave plan: %w", err)
}

// readsBack reports whether np holds exactly one new task whose parsed fields equal t's rendered line.
func readsBack(np Plan, t WaveTask, before int) bool {
	got, ok := findTask(np, t.ID)
	return ok && (got.ID == "" || got.ID == t.ID) && len(np.Tasks) == before+1 && slices.Equal(got.Owns, t.Owns) &&
		got.Title == strings.TrimPrefix(TaskLine(t), "- [ ] ") && got.Sha == ""
}

// TaskVerify is the task line's `verify:` command, "" when it has none.
func TaskVerify(t Task) string { return strings.TrimSpace(waveAttributes(t.Title)["verify"]) }

// OwnedPaths is t's owns: entries expanded against each other (abbreviations spliced).
func OwnedPaths(t Task) []string { return ownEntries(t.Owns, nil) }

var errShape = errors.New("verify not run: shape")

// VerifyStep is one && segment of a verify line: its GOOS/GOARCH/CGO_ENABLED assignments and the argv they prefix.
type VerifyStep struct{ Env, Argv []string }

// VerifyShape allows only go build|vet|test on ./ packages, agents diff --check and doctor, rtk forms, && chained.
func VerifyShape(cmd string) error {
	steps, err := VerifyArgv(cmd)
	if err != nil {
		return err
	}
	for _, s := range steps {
		if !allowedVerify(s.Argv) {
			return errShape
		}
	}
	return GateRefused(cmd)
}

// verifyBanned is every byte with shell meaning outside single quotes; && is the one separator taken.
const verifyBanned = "$`<>();|&\\\"#!*?[]{}~\x7f"

// andSep stands for && among verifyTokens' words; a NUL never survives the banned-byte check.
const andSep = "\x00&&"

// VerifyArgv splits cmd into argv steps with no shell: single quotes are literal, any other shell syntax is refused.
func VerifyArgv(cmd string) ([]VerifyStep, error) {
	toks, ok := verifyTokens(cmd)
	if !ok {
		return nil, errShape
	}
	var steps []VerifyStep
	start := 0
	for i := 0; i <= len(toks); i++ {
		if i < len(toks) && toks[i] != andSep {
			continue
		}
		if i == start {
			return nil, errShape
		}
		steps, start = append(steps, splitEnv(toks[start:i])), i+1
	}
	return steps, nil
}

func verifyTokens(cmd string) ([]string, bool) {
	var toks []string
	var cur strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			toks, inWord = append(toks, cur.String()), false
			cur.Reset()
		}
	}
	for i := 0; i < len(cmd); i++ {
		switch c := cmd[i]; {
		case c == '\'':
			lit, ok := singleQuoted(cmd[i+1:])
			if !ok {
				return nil, false
			}
			cur.WriteString(lit)
			inWord, i = true, i+1+len(lit)
		case strings.HasPrefix(cmd[i:], "&&"):
			flush()
			toks, i = append(toks, andSep), i+1
		case c == ' ' || c == '\t':
			flush()
		case c < 0x20 || strings.ContainsRune(verifyBanned, rune(c)):
			return nil, false
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return toks, true
}

// singleQuoted is the literal up to the closing quote; an unclosed quote or a control byte inside refuses.
func singleQuoted(s string) (string, bool) {
	end := strings.IndexByte(s, '\'')
	return s[:max(end, 0)], end >= 0 && !strings.ContainsFunc(s[:end], unicode.IsControl)
}

var verifyEnv = []string{"GOOS", "GOARCH", "CGO_ENABLED"}
var goValueFlags = []string{"run", "skip", "count", "timeout", "tags", "p", "cpu", "bench"}
var goBoolFlags = []string{"v", "race", "short", "json", "cover", "failfast"}

// splitEnv peels leading GOOS/GOARCH/CGO_ENABLED=<word> assignments off words; rtk stays argv[0].
func splitEnv(words []string) VerifyStep {
	n := 0
	for n < len(words)-1 && envAssign(words[n]) {
		n++
	}
	return VerifyStep{Env: words[:n], Argv: words[n:]}
}

func envAssign(w string) bool {
	name, val, ok := strings.Cut(w, "=")
	return ok && slices.Contains(verifyEnv, name) && onlyRunes(val, "_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

func allowedVerify(w []string) bool {
	if len(w) > 0 && w[0] == "rtk" {
		w = w[1:]
	}
	if len(w) >= 3 && w[0] == "go" && slices.Contains([]string{"build", "vet", "test"}, w[1]) {
		return goArgsOK(w[2:])
	}
	return claudeCodeCheck(w)
}

// claudeCodeCheck is a read-only `claude-code doctor` mode or `claude-code agents diff --check` on repo paths.
func claudeCodeCheck(w []string) bool {
	if len(w) < 2 || w[0] != "claude-code" {
		return false
	}
	if w[1] == "doctor" {
		return doctorArgsOK(w[2:])
	}
	return len(w) >= 4 && w[1] == "agents" && w[2] == "diff" && diffArgsOK(w[3:])
}

// doctorArgsOK is at most one of --prompts, --mcp plus --format; --fix, --scan and the rest write.
func doctorArgsOK(args []string) bool {
	modes := 0
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--format" && i+1 < len(args) && slices.Contains([]string{"json", "text"}, args[i+1]):
			i++
		case a == "--format=json" || a == "--format=text":
		case slices.Contains([]string{"--prompts", "--mcp"}, a):
			modes++
		default:
			return false
		}
	}
	return modes <= 1
}

// diffArgsOK is --check with --path <repo-relative> and --gates <names>; the harness supplies --base itself.
func diffArgsOK(args []string) bool {
	check := false
	for i := 0; i < len(args); i++ {
		if args[i] == "--check" {
			check = true
			continue
		}
		name, val, inline := strings.Cut(args[i], "=")
		if !inline && i+1 < len(args) {
			i, val = i+1, args[i+1]
		}
		if ok := name == "--path" && repoRelative(val) || name == "--gates" && gateNames(val); !ok {
			return false
		}
	}
	return check
}

// repoRelative is a non-empty relative path with no .. element, so it cannot leave the tree it runs in.
func repoRelative(p string) bool {
	return p != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "-") &&
		!slices.Contains(strings.Split(filepath.ToSlash(p), "/"), "..")
}

func gateNames(v string) bool {
	return v != "" && onlyRunes(v, ",-abcdefghijklmnopqrstuvwxyz")
}

func onlyRunes(s, set string) bool { return strings.Trim(s, set) == "" }

func goArgsOK(args []string) bool {
	pkgs := 0
	for i := 0; i < len(args); i++ {
		name, _, inline := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		switch {
		case strings.HasPrefix(args[i], "./"):
			if !repoRelative(args[i]) {
				return false
			}
			pkgs++
		case !strings.HasPrefix(args[i], "-"):
			return false
		case slices.Contains(goValueFlags, name):
			if !inline {
				i++
			}
		case !slices.Contains(goBoolFlags, name):
			return false
		}
	}
	return pkgs > 0
}

var shaPattern = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[0-9a-f]{7,40}$`) })

// ValidSHA is a 7-40 digit lower-case hex commit id, safe to use in a state path.
func ValidSHA(sha string) bool { return shaPattern().MatchString(sha) }

// DeferTick reports what sha would tick and starts one detached verifier per plan; it ticks the green ones later.
func DeferTick(top, sha string, files []string, session string) ([]PlanTick, []string, error) {
	if len(sha) != 40 { // a full sha needs no resolving; a short one does
		if full, err := proc.RunGit(context.Background(), top, "rev-parse", "--verify", sha+"^{commit}"); err == nil && ValidSHA(strings.TrimSpace(full)) {
			sha = strings.TrimSpace(full)
		}
	}
	pending, tied, err := PendingCommit(top, sha, files)
	var errs []error
	for i, r := range pending {
		if len(r.IDs) == 0 {
			continue
		}
		started, serr := spawnVerify(top, r.Plan, sha, r.IDs, session)
		errs = append(errs, serr)
		pending[i].Ticked = nil
		if started {
			pending[i].Notes = append([]string{"plans: verifying " + strings.Join(r.IDs, ", ") + " on " + sha[:min(7, len(sha))] +
				" before ticking; the result is a notice on your next prompt"}, r.Notes...)
		}
	}
	return pending, tied, errors.Join(append(errs, err)...)
}

// verifyKeep is how long verify markers, logs and outputs stay in state/verify.
const verifyKeep = 7 * 24 * time.Hour

// spawnVerify starts `plan tick --verify` once per sha and plan; a live or finished marker means no new run.
func spawnVerify(top, plan, sha string, ids []string, session string) (bool, error) {
	if !ValidSHA(sha) {
		return false, errors.New("plans: not a commit sha: " + sha)
	}
	dir := filepath.Join(paths.StateDir(), "verify")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	PruneVerify(dir, "*.log*")
	marker := filepath.Join(dir, sha[:min(12, len(sha))]+"-"+paths.Slug(filepath.Base(plan), "", 40, "plan")+".log")
	f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) && staleMarker(marker) {
		_ = os.Remove(marker + ".pid")
		_ = os.Remove(marker)
		f, err = os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // a peer that re-took it wins
	}
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	bin, err := paths.SelfBinary()
	if err != nil {
		_ = os.Remove(marker)
		return false, err
	}
	args := []string{"plan", "tick", "--verify", "--plan", plan, "--sha", sha, "--session", session}
	for _, id := range ids {
		args = append(args, "--task", id)
	}
	pid := proc.SpawnDetachedPID(bin, args, top, f)
	if pid == 0 {
		_ = os.Remove(marker)
		return false, errors.New("plans: the verifier did not start; tick by hand after checking: claude-code plan task <id> --done --sha " + sha)
	}
	start, _ := proc.StartTime(pid)
	return true, os.WriteFile(marker+".pid", []byte(strconv.Itoa(pid)+"\n"+start+"\n"), 0o600)
}

// staleMarker is a marker whose verifier died before finishing (no "plans: " result line) and no live pid holds.
func staleMarker(marker string) bool {
	log, err := os.ReadFile(marker)
	if err != nil || strings.HasPrefix(string(log), "plans: ") || strings.Contains(string(log), "\nplans: ") {
		return false
	}
	raw, err := os.ReadFile(marker + ".pid")
	if err != nil {
		st, serr := os.Stat(marker)
		return serr == nil && time.Since(st.ModTime()) > time.Minute // older than any spawn still writing its pid
	}
	pidText, start, _ := strings.Cut(strings.TrimSpace(string(raw)), "\n")
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		return true
	}
	got, err := proc.StartTime(pid)
	return errors.Is(err, proc.ErrNoProcess) || err == nil && got != start
}

// PruneVerify removes entries of dir matching glob that are older than verifyKeep.
func PruneVerify(dir, glob string) {
	old, _ := filepath.Glob(filepath.Join(dir, glob))
	for _, p := range old {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > verifyKeep {
			_ = os.RemoveAll(p)
		}
	}
}
