package plans

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

var waveID = regexp.MustCompile(`(?i)^(?:Phase\s+)?(\d+)\.(\d+|V)\b`)
var waveAttr = regexp.MustCompile(`(?i)\b(leaf|agent|profile|verify|findings|owns):\s*`)
var packageRange = regexp.MustCompile(`(\d+)\.(\d+|V)(?:\s*[-–]\s*(\d+)\.(\d+|V))?`)

// gitWord is one shell word piece: a bare character or a quoted run.
const gitWord = `(?:[^\s;&|"']|"[^"]*"|'[^']*')`

// gitWrite matches a git write verb after any global flags (-C x, -c k=v, --git-dir=x, --no-pager).
var gitWrite = regexp.MustCompile(`(?i)\bgit(?:\s+-` + gitWord + `+(?:\s+(?:[^-\s;&|"']|"[^"]*"|'[^']*')` +
	gitWord + `*)?)*\s+` +
	`(?:(?:add|commit|stash|reset|checkout|switch|restore|clean|push|rebase|merge|rm|mv|cherry-pick|am|tag|pull|revert|` +
	`update-ref)(?:[\s;&|)]|$)|apply\b[^;&|]*\s--(?:index|cached)\b)`)

// GateRefused is the one git-write refusal: wave gates, task verify lines and the gate runner all call it.
func GateRefused(cmd string) error {
	if m := gitWrite.FindString(cmd); m != "" {
		return &WaveError{ID: "gate", Reason: fmt.Sprintf("git write command refused: %q", strings.TrimSpace(m))}
	}
	return nil
}

// ParseWaves reads checklist ownership with Parse, then adds wave metadata and phase bodies.
func ParseWaves(file string) (WavePlan, error) {
	base, err := Parse(file)
	if err != nil {
		return WavePlan{}, err
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return WavePlan{}, err
	}
	p := WavePlan{Path: file}
	lines := strings.Split(string(raw), "\n")
	gate, bodies := phaseDetails(lines)
	for _, task := range base.Tasks {
		m := waveID.FindStringSubmatch(task.Title)
		if m == nil {
			continue
		}
		n, err := strconv.Atoi(m[1])
		if err != nil {
			return p, err
		}
		attrs := waveAttributes(task.Title)
		t := WaveTask{ID: "Phase " + m[1] + "." + strings.ToUpper(m[2]), Wave: n, Agent: attrs["agent"],
			Leaf: attrs["leaf"], Profile: attrs["profile"], Owns: ownEntries(task.Owns, nil),
			Verify: attrs["verify"], Findings: attrs["findings"], Task: bodies[m[1]+"."+strings.ToUpper(m[2])]}
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
	matches := waveAttr.FindAllStringSubmatchIndex(title, -1)
	for i, m := range matches {
		end := len(title)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		key := strings.ToLower(title[m[2]:m[3]])
		out[key] = strings.Trim(strings.TrimSpace(title[m[1]:end]), " ,—–")
	}
	return out
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
				s.gate.WriteString(line + "\n")
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
		if m := waveID.FindStringSubmatch(strings.TrimPrefix(line, "- **")); m != nil {
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
		if !slices.Contains([]string{"claude", "codex", "copilot", "gemini", "opencode", "in-context"}, t.Agent) {
			return &WaveError{t.ID, "unknown Agent: " + t.Agent}
		}
		if err := checkTask(t); err != nil {
			return err
		}
	}
	slices.SortFunc(p.Waves, func(a, b Wave) int { return a.Number - b.Number })
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

func waveOverlap(w Wave) error {
	owners := map[string]string{}
	for _, t := range w.Tasks {
		if t.Manual {
			continue
		}
		for _, own := range t.Owns {
			for prev, id := range owners {
				if id != t.ID && ownsOverlap(prev, own) {
					return &WaveError{t.ID, "owns: overlaps " + id + " at " + own}
				}
			}
			owners[own] = t.ID
		}
	}
	return nil
}

func ownsOverlap(a, b string) bool {
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
	for _, m := range packageRange.FindAllStringSubmatch(packages, -1) {
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
			for id := range expected {
				return &WaveError{id, "Waves table package has no task"}
			}
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
	m := waveID.FindStringSubmatch(t.ID)
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
	return t, nil
}
