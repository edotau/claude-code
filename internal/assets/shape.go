package assets

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/edotau/claude-code/internal/platform/textutil"
)

// Field is one frontmatter key a shape constrains; Max 0 means no length cap, Any empty means no wording rule.
type Field struct {
	Name     string
	Required bool
	Max      int
	Stem     bool     // the value must equal the file's stem (see Shape.StemDir)
	Any      []string // the value must contain one of these, case-insensitively
}

// Section is a markdown heading (levels 2-4) a shape names; Ordered ones must appear in table order.
type Section struct {
	Heading  string
	Required bool
	Ordered  bool
	Why      []string // every list item under the heading must contain one of these markers
	MinItems int
}

type Companion struct {
	Name     string
	Sections []Section
}

// Shape describes one kind of prompt file; a new kind is a new row in Shapes, not a new code path.
type Shape struct {
	ID         string
	Glob       string   // entry files, relative to the config dir
	Exclude    []string // config-dir-relative path prefixes the glob must skip
	StemDir    bool     // the stem is the entry file's directory name, else its file name without extension
	Fields     []Field
	Sections   []Section
	MaxLines   int
	H1Count    int    // when positive, require exactly this many top-level headings
	RefsDir    string // when set, files under it and paths the entry names must match one to one
	Companions []Companion
}

// Finding is one way a file departs from its shape.
type Finding struct{ Shape, Path, Rule, Msg string }

const maxName, maxDescription, maxSkillLines = 64, 1024, 150

// Shapes is the table; MaxLines 150 is above 9 of the 10 hand-written skills (25-149 lines, one outlier at 324).
var Shapes = []Shape{{
	ID: "skill", Glob: "skills/*/SKILL.md", Exclude: []string{"skills/synced/"}, StemDir: true,
	Fields: []Field{
		{Name: "name", Required: true, Max: maxName, Stem: true},
		{Name: "description", Required: true, Max: maxDescription, Any: []string{"use for", "use when"}},
	},
	Sections: []Section{
		{Heading: "When to use", Ordered: true},
		{Heading: "Workflow", Ordered: true},
		{Heading: "Verify", Ordered: true},
		{Heading: "Report", Ordered: true},
		{Heading: "Gotchas", Required: true, Why: []string{"because", " so ", "—", ";"}},
	},
	MaxLines: maxSkillLines, RefsDir: "references",
	Companions: []Companion{{Name: "evals.md", Sections: []Section{
		{Heading: "Should trigger", MinItems: 1},
		{Heading: "Should not trigger", MinItems: 1},
	}}},
}, {
	ID: "command", Glob: "commands/*.md",
	Fields: []Field{
		{Name: "description", Required: true},
		{Name: "argument-hint", Required: true},
		{Name: "disable-model-invocation", Required: true},
		{Name: "allowed-tools", Required: true},
	},
	MaxLines: 60,
}, {
	ID: "rule", Glob: "rules/**/*.md", MaxLines: 200, H1Count: 1,
}, {
	ID: "instruction-index", Glob: "CLAUDE.md",
	Sections: []Section{
		{Heading: "What this repo is", Required: true},
		{Heading: "Where things live", Required: true},
		{Heading: "Hard rules (always on)", Required: true},
		{Heading: "Behavioral guidelines", Required: true},
		{Heading: "How-to — by area", Required: true},
	},
}}

// ShapeByID returns the table row with that id.
func ShapeByID(id string) (Shape, bool) {
	for _, s := range Shapes {
		if s.ID == id {
			return s, true
		}
	}
	return Shape{}, false
}

// Files lists the shape's entry files under root, minus the excluded prefixes.
func (s Shape) Files(root string) []string {
	var all []string
	if prefix, suffix, ok := strings.Cut(s.Glob, "**/"); ok {
		base := filepath.Join(root, prefix)
		_ = filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			rel, _ := filepath.Rel(base, p)
			if match, _ := filepath.Match(filepath.FromSlash(suffix), filepath.Base(rel)); match {
				all = append(all, p)
			}
			return nil
		})
	} else {
		all, _ = filepath.Glob(filepath.Join(root, s.Glob))
	}
	var out []string
	for _, f := range all {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		skip := false
		for _, p := range s.Exclude {
			skip = skip || strings.HasPrefix(rel, p)
		}
		if !skip {
			out = append(out, f)
		}
	}
	return out
}

var (
	headingRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^#{2,4}\s+(.*?)\s*#*\s*$`) })
	refPathRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`references/[A-Za-z0-9_./*<>{}-]+`) })
)

// Validate checks the entry bytes and reads the optional companions and references beside it.
func (s Shape) Validate(path string, b []byte) []Finding {
	fm, body := textutil.ParseFrontmatter(string(b))
	var out []Finding
	add := func(rule, format string, a ...any) {
		out = append(out, Finding{Shape: s.ID, Path: path, Rule: rule, Msg: fmt.Sprintf(format, a...)})
	}
	out = append(out, s.CheckFields(path, fm)...)
	if n := strings.Count(strings.TrimRight(string(b), "\n"), "\n") + 1; s.MaxLines > 0 && n > s.MaxLines {
		add("max-lines", "%d lines exceeds the %d cap", n, s.MaxLines)
	}
	s.checkSections(body, add)
	if s.H1Count > 0 {
		n := countH1(body)
		if n != s.H1Count {
			add("h1-count", "%d top-level headings, want %d", n, s.H1Count)
		}
	}
	s.checkRefs(path, body, add)
	out = append(out, s.checkCompanions(path)...)
	return out
}

func (s Shape) checkCompanions(entry string) []Finding {
	var out []Finding
	for _, c := range s.Companions {
		p := filepath.Join(filepath.Dir(entry), c.Name)
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			out = append(out, Finding{Shape: s.ID, Path: p, Rule: "companion-read", Msg: err.Error()})
			continue
		}
		add := func(rule, format string, a ...any) {
			out = append(out, Finding{Shape: s.ID, Path: p, Rule: rule, Msg: fmt.Sprintf("%s: %s", c.Name, fmt.Sprintf(format, a...))})
		}
		Shape{Sections: c.Sections}.checkSections(string(b), add)
	}
	return out
}

func (s Shape) stem(path string) string {
	if s.StemDir {
		return filepath.Base(filepath.Dir(path))
	}
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

// CheckFields checks fm's keys against the shape's field rules for the entry file at path.
func (s Shape) CheckFields(path string, fm map[string]any) []Finding {
	var out []Finding
	add := func(rule, format string, a ...any) {
		out = append(out, Finding{Shape: s.ID, Path: path, Rule: rule, Msg: fmt.Sprintf(format, a...)})
	}
	for _, f := range s.Fields {
		s.checkField(f, fm, path, add)
	}
	return out
}

// EntryPath is the config-dir-relative entry file for stem, built from Glob so the two cannot drift.
func (s Shape) EntryPath(stem string) string { return strings.Replace(s.Glob, "*", stem, 1) }

func (s Shape) checkField(f Field, fm map[string]any, path string, add func(string, string, ...any)) {
	v, ok := fm[f.Name]
	val := strings.TrimSpace(fmt.Sprint(v))
	if !ok || val == "" {
		if f.Required {
			add("fm-required", "frontmatter %q is missing or empty", f.Name)
		}
		return
	}
	if f.Max > 0 && len(val) > f.Max {
		add("fm-max", "%s is %d characters, over the %d cap", f.Name, len(val), f.Max)
	}
	if f.Stem && val != s.stem(path) {
		add("fm-stem", "%s %q != %q", f.Name, val, s.stem(path))
	}
	if len(f.Any) > 0 && !containsAny(strings.ToLower(val), f.Any) {
		add("fm-wording", "%s must state when to use it (one of %q)", f.Name, f.Any)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// countH1 counts top-level headings outside fenced blocks.
func countH1(body string) int {
	count, fence := 0, false
	for line := range strings.SplitSeq(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			fence = !fence
		} else if !fence && strings.HasPrefix(line, "# ") && strings.TrimSpace(line[2:]) != "" {
			count++
		}
	}
	return count
}

// scanSections maps each heading (lower case) to its first line and its list items; fenced lines are skipped.
func scanSections(body string) (pos map[string]int, items map[string][]string) {
	pos, items = map[string]int{}, map[string][]string{}
	cur, fence := "", false
	for i, l := range strings.Split(body, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
		}
		m := headingRE().FindStringSubmatch(l)
		switch {
		case fence:
		case m != nil:
			cur = strings.ToLower(m[1])
			if _, seen := pos[cur]; !seen {
				pos[cur] = i
			}
		case cur != "" && (strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ")):
			items[cur] = append(items[cur], t)
		}
	}
	return pos, items
}

// checkSections reports missing and out-of-order headings and list items under a Why section that give no reason.
func (s Shape) checkSections(body string, add func(string, string, ...any)) {
	pos, items := scanSections(body)
	last := -1
	for _, sec := range s.Sections {
		key := strings.ToLower(sec.Heading)
		p, ok := pos[key]
		switch {
		case !ok && sec.Required:
			add("sec-required", "section %q is missing", sec.Heading)
		case ok && sec.Ordered && p < last:
			add("sec-order", "section %q is out of order", sec.Heading)
		case ok && sec.Ordered:
			last = p
		}
		checkWhy(sec, items[key], add)
	}
}

// checkWhy reports each list item under a Why section that carries none of its markers.
func checkWhy(sec Section, items []string, add func(string, string, ...any)) {
	if sec.MinItems > 0 && len(items) < sec.MinItems {
		add("sec-items", "section %q needs at least %d list item(s)", sec.Heading, sec.MinItems)
	}
	for _, it := range items {
		if len(sec.Why) > 0 && !containsAny(strings.ToLower(it), sec.Why) {
			add("sec-why", "%q entry gives no reason: %.40s", sec.Heading, it)
		}
	}
}

// checkRefs matches the RefsDir files beside path with the references/ paths the entry body names.
func (s Shape) checkRefs(path, body string, add func(string, string, ...any)) {
	if s.RefsDir == "" {
		return
	}
	dir := filepath.Join(filepath.Dir(path), s.RefsDir)
	named := map[string]bool{}
	for _, p := range refPathRE().FindAllString(body, -1) {
		p = strings.TrimRight(p, ".,;:/")
		named[p] = true
		if strings.ContainsAny(p, "*<>{}") {
			continue // a pattern or placeholder, not a path to stat
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), p)); err != nil {
			add("refs-missing", "%s is named but does not exist", p)
		}
	}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Dir(path), p)
		if rel = filepath.ToSlash(rel); !namedUnder(named, rel) {
			add("refs-unlisted", "%s is not named in the load table", rel)
		}
		return nil
	})
}

// namedUnder reports a file named itself or through one of its parent directories.
func namedUnder(named map[string]bool, rel string) bool {
	for p := rel; strings.Contains(p, "/"); p = p[:strings.LastIndex(p, "/")] {
		if named[p] {
			return true
		}
	}
	return false
}

// SkillInput is the typed input a skill scaffold needs; every path comes from the shape, none from the caller.
type SkillInput struct {
	Name, Description, NotFor              string
	Triggers, Refs, NonTriggers, TaskCases []string   // Refs are file stems under the shape's RefsDir
	Synthesis                              *Synthesis // optional distilled body; nil keeps the placeholders
}

// SkillFile is one file of a scaffold, its path relative to the config dir.
type SkillFile struct{ Path, Body string }

// SkillScaffold is the rendered entry file and its reference stubs (stubs are written before the entry).
type SkillScaffold struct {
	Entry SkillFile
	Evals SkillFile
	Refs  []SkillFile
}

var (
	skillTemplates = loadTemplates("skill/*.tmpl")
	stemRE         = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`) })
)

// RenderSkill builds a skill: headings and their order come from the skill Shapes row, prose from skill.tmpl.
func RenderSkill(in SkillInput) (SkillScaffold, error) {
	shape, _ := ShapeByID("skill")
	for _, stem := range append([]string{in.Name}, in.Refs...) {
		if !stemRE().MatchString(stem) {
			return SkillScaffold{}, fmt.Errorf("%q is not a file stem (lower-case words joined by single hyphens)", stem)
		}
	}
	if err := checkSkillCases(in); err != nil {
		return SkillScaffold{}, err
	}
	desc, err := skillDescription(in)
	if err != nil {
		return SkillScaffold{}, err
	}
	entry := shape.EntryPath(in.Name)
	if fs := shape.CheckFields(entry, map[string]any{"name": in.Name, "description": desc[1 : len(desc)-1]}); len(fs) > 0 {
		return SkillScaffold{}, errors.New(fs[0].Msg)
	}
	set := skillTemplates()
	var secs strings.Builder
	for _, sec := range shape.Sections {
		key := "skill/" + strings.ReplaceAll(strings.ToLower(sec.Heading), " ", "-")
		if _, ok := set[key]; !ok {
			return SkillScaffold{}, fmt.Errorf("skill.tmpl has no block for section %q", sec.Heading)
		}
		fmt.Fprintf(&secs, "## %s\n\n%s\n", sec.Heading, renderSkill(key, sectionFields(sec.Heading, in)))
	}
	syn := cmp.Or(in.Synthesis, &Synthesis{})
	out := SkillScaffold{Entry: SkillFile{entry, renderSkill("skill.tmpl", tmplFields{
		"Name": in.Name, "Description": desc, "Title": strings.ReplaceAll(in.Name, "-", " "),
		"Intro": syn.MentalModel, "Principles": syn.Principles,
		"Sections": strings.TrimRight(secs.String(), "\n") + "\n"})}}
	out.Evals = SkillFile{path.Join(path.Dir(entry), shape.Companions[0].Name), renderSkill("evals.tmpl", tmplFields{
		"Triggers": in.Triggers, "NonTriggers": in.NonTriggers, "TaskCases": in.TaskCases})}
	for _, ref := range in.Refs {
		out.Refs = append(out.Refs, SkillFile{path.Join(path.Dir(entry), shape.RefsDir, ref+".md"),
			skillRefStub(ref)})
	}
	return out, nil
}

// sectionFields is the input of one section's block; only these three blocks take any.
func sectionFields(heading string, in SkillInput) tmplFields {
	syn := cmp.Or(in.Synthesis, &Synthesis{})
	switch heading {
	case "When to use":
		return tmplFields{"Refs": in.Refs, "NotFor": in.NotFor}
	case "Workflow":
		return tmplFields{"Heuristics": slices.Concat(syn.Heuristics, syn.ContextDependent)}
	case "Gotchas":
		return tmplFields{"Gotchas": gotchaLines(syn.Gotchas)}
	}
	return tmplFields{}
}

func renderSkill(name string, input any) string { return renderFrom(skillTemplates(), name, input) }

func skillRefStub(name string) string { return renderSkill("skill/ref-stub", tmplFields{"Name": name}) }

// skillDescription folds the triggers and the not-for clause into the description as one quoted scalar.
func skillDescription(in SkillInput) (string, error) {
	d := strings.TrimSpace(in.Description)
	if d == "" || strings.ContainsAny(d+in.NotFor, "\r\n") {
		return "", errors.New("description is required and must be one line")
	}
	if len(in.Triggers) > 0 {
		d = foldTriggers(d, in.Triggers)
	}
	if nf := strings.TrimSpace(in.NotFor); nf != "" {
		d = strings.TrimRight(d, ".") + ". Not for " + strings.TrimRight(nf, ".") + "."
	}
	for _, q := range []string{"'", `"`} {
		if !strings.Contains(d, q) {
			return q + d + q, nil
		}
	}
	return "", errors.New("description mixes single and double quotes")
}

func checkSkillCases(in SkillInput) error {
	for flag, values := range map[string][]string{"trigger": in.Triggers, "non-trigger": in.NonTriggers, "task-case": in.TaskCases} {
		for _, v := range values {
			if strings.TrimSpace(v) == "" || strings.ContainsAny(v, "\r\n") {
				return fmt.Errorf("--%s must be a non-empty single line", flag)
			}
		}
	}
	return nil
}

var useForRE = sync.OnceValue(func() *regexp.Regexp { return regexp.MustCompile(`(?i)\buse for\b`) })

func foldTriggers(desc string, triggers []string) string {
	loc := useForRE().FindStringIndex(desc)
	if loc == nil {
		desc = strings.TrimRight(desc, ".") + ". Use for"
		loc = []int{len(desc) - len("Use for"), len(desc)}
	}
	end := clauseEnd(desc, loc[1])
	list := strings.TrimSpace(desc[loc[1]:end])
	quote := !strings.Contains(desc+strings.Join(triggers, ""), "'")
	seen := []string{}
	for item := range strings.SplitSeq(list, ",") {
		seen = append(seen, strings.ToLower(strings.Trim(strings.TrimSpace(item), `"'`)))
	}
	for _, trigger := range triggers {
		t := strings.TrimSpace(trigger)
		if slices.Contains(seen, strings.ToLower(t)) || strings.Contains(strings.ToLower(list), `"`+strings.ToLower(t)+`"`) {
			continue
		}
		seen = append(seen, strings.ToLower(t))
		if list != "" {
			list += ", "
		}
		if !quote || strings.Contains(t, `"`) {
			list += t
		} else {
			list += `"` + t + `"`
		}
	}
	suffix := desc[end:]
	if suffix == "" {
		suffix = "."
	}
	return desc[:loc[1]] + " " + list + suffix
}

func clauseEnd(desc string, start int) int {
	quoted := false
	for i := start; i < len(desc); i++ {
		if desc[i] == '"' {
			quoted = !quoted
		}
		if !quoted && (desc[i] == '.' || desc[i] == '!' || desc[i] == '?') {
			return i
		}
	}
	return len(desc)
}
