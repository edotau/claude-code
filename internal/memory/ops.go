package memory

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edotau/claude-code/internal/plans"
	"github.com/edotau/claude-code/internal/textutil"
)

const (
	maxBatchOps = 60
	maxOpText   = 1200
	maxNote     = 200
	maxLabel    = 80
	schemaDraft = "https://json-schema.org/draft/2020-12/schema"
	refPattern  = `^z?[0-9a-f]{8}$`
	dayLayout   = "2006-01-02"
)

var datedPointerRe = regexp.MustCompile(`^Replaced by \S+ \(\d{4}-\d{2}-\d{2}\)\.$`)

var (
	opNames        = []string{"add", "update", "supersede", "close", "noop"}
	upgradeOnlyOps = []string{"merge", "identify", "date"}
	closeReasons   = []string{"done", "stale", "superseded-by"}
	targets        = []string{"activeContext", "progress", "decisionLog", "conventions", "sessionHistory",
		"projectContext", "feedback", "note", "techdebt"}
	sectionsOf = map[string][]string{
		"activeContext": {"Current Focus", "Open Questions", "Blockers", "Next Steps", "Parked"},
		"progress":      {"What Works", "In Progress", "What's Next", "Known Issues"},
	}
)

// Op is one change the model proposes; Go writes every heading, date, id, marker, file name and status word.
type Op struct {
	Op      string `json:"op"`
	Target  string `json:"target,omitempty"`  // routes the op to a file; Apply reads it only to check Section
	Section string `json:"section,omitempty"` // add to a state file: one of sectionsOf[Target]
	ID      string `json:"id,omitempty"`      // update, supersede, close: a ref from the listing
	Read    string `json:"read,omitempty"`    // the contentRef of the entry as the writer read it
	Title   string `json:"title,omitempty"`   // one line; decisionLog and sessionHistory entries
	Text    string `json:"text,omitempty"`    // bullet or body; no id marker
	Reason  string `json:"reason,omitempty"`  // close: one of closeReasons (required)
	By      string `json:"by,omitempty"`      // close with superseded-by: the ref that replaces ID
	Note    string `json:"note,omitempty"`    // one free line; logged, never written
}

// Batch is the model's whole answer.
type Batch struct {
	Ops           []Op           `json:"ops"`
	Candidates    []candidate    `json:"candidates,omitempty"`     // dream 5b: feedback candidates, proposed by Go
	TestProposals []testProposal `json:"test_proposals,omitempty"` // dream 5c: tests proposed, not written
	PlanTasks     []PlanTaskOp   `json:"plan_tasks,omitempty"`     // dream 5d: open plan tasks to tick or strike
}

// PlanTaskOp closes one open plan task: Action done (Sha optional) or strike (Reason and Evidence).
type PlanTaskOp struct {
	Plan     string `json:"plan"`
	ID       string `json:"id"`
	Action   string `json:"action"`
	Sha      string `json:"sha,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

const maxSideOps = 10 // each of candidates, test_proposals and plan_tasks

// OpResult is what Go did with one op; Outcome is DreamOK, DreamNoop or DreamFailed.
type OpResult struct {
	Op      Op     `json:"op"`
	ID      string `json:"id,omitempty"` // the entry written or closed
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"` // "duplicate", "changed since read", "unknown id", a validation message
}

// Env is what Apply may not read itself: the time, the id source and the liveness check for By.
type Env struct {
	Now   time.Time
	NewID func(salt int, firstLine string) string // 8 hex digits; nil mints through freshID
	Live  func(ref string) bool                   // nil: the ref must name an entry of the doc being edited
}

// Validate checks every op without touching a document; Apply runs the same per-op checks.
func (b Batch) Validate() error {
	if b.Ops == nil { // a decoded answer without "ops" is malformed; "ops": [] is a valid empty batch
		return errors.New("ops is missing")
	}
	if len(b.Ops) > maxBatchOps {
		return fmt.Errorf("%d ops, at most %d", len(b.Ops), maxBatchOps)
	}
	if max(len(b.Candidates), len(b.TestProposals), len(b.PlanTasks)) > maxSideOps {
		return fmt.Errorf("candidates, test_proposals and plan_tasks take at most %d entries each", maxSideOps)
	}
	for i, o := range b.Ops {
		if err := o.check(); err != nil {
			return fmt.Errorf("op %d: %w", i, err)
		}
	}
	return nil
}

func refOK(s string) bool {
	r, ok := ParseRef(s)
	return ok && r.Alias == ""
}

// opRules each return a validation message, "" when the op passes.
var opRules = []func(Op) string{ruleEnums, ruleRefs, ruleText, ruleID, ruleBody, ruleClose}

func (o Op) check() error {
	for _, rule := range opRules {
		if msg := rule(o); msg != "" {
			return errors.New(msg)
		}
	}
	return nil
}

func ruleEnums(o Op) string {
	switch list := sectionsOf[o.Target]; {
	case slices.Contains(upgradeOnlyOps, o.Op):
		return fmt.Sprintf("op %q is upgrade-only", o.Op)
	case !slices.Contains(opNames, o.Op):
		return fmt.Sprintf("unknown op %q", o.Op)
	case o.Target != "" && !slices.Contains(targets, o.Target):
		return fmt.Sprintf("unknown target %q", o.Target)
	case o.Reason != "" && !slices.Contains(closeReasons, o.Reason):
		return fmt.Sprintf("unknown close reason %q", o.Reason)
	case o.Section != "" && list != nil && !slices.Contains(list, o.Section):
		return fmt.Sprintf("unknown section %q for %s", o.Section, o.Target)
	}
	return ""
}

func ruleRefs(o Op) string {
	for _, f := range [][2]string{{"id", o.ID}, {"read", o.Read}, {"by", o.By}} {
		if f[1] != "" && !refOK(f[1]) {
			return fmt.Sprintf("%s %q is not a ref", f[0], f[1])
		}
	}
	if o.By != "" && o.By == o.ID {
		return "by is the entry itself"
	}
	return ""
}

func ruleText(o Op) string {
	for _, f := range [][2]string{{"section", o.Section}, {"title", o.Title}, {"note", o.Note}} {
		if strings.ContainsAny(f[1], "\r\n") {
			return f[0] + " must be one line"
		}
	}
	if utf8.RuneCountInString(o.Text) > maxOpText || utf8.RuneCountInString(o.Title) > maxOpText {
		return fmt.Sprintf("text over %d characters", maxOpText)
	}
	if strings.Contains(o.Text, "\r") || strings.Contains(o.Text+o.Title, markerOpen) {
		return "text carries a carriage return or an id marker"
	}
	return ""
}

func (o Op) editsEntry() bool { return o.Op == "update" || o.Op == "supersede" || o.Op == "close" }

func ruleID(o Op) string {
	switch {
	case o.editsEntry() && o.ID == "":
		return o.Op + " needs an id"
	case o.editsEntry() && !strings.HasPrefix(o.ID, contentRefPrefix) && o.Read == "":
		return o.Op + " of a stored id needs read"
	case !o.editsEntry() && (o.ID != "" || o.Read != ""):
		return o.Op + " takes no id or read"
	}
	return ""
}

func ruleBody(o Op) string {
	if (o.Op == "add" || o.Op == "update" || o.Op == "supersede") && o.Title == "" && o.Text == "" {
		return o.Op + " needs a title or text"
	}
	return ""
}

func ruleClose(o Op) string {
	switch {
	case o.Op != "close" && (o.Reason != "" || o.By != ""):
		return o.Op + " takes no reason or by"
	case o.Op == "close" && o.Reason == "":
		return "close needs a reason"
	case o.Op == "close" && (o.Reason == "superseded-by") != (o.By != ""):
		return "by is required with superseded-by and allowed only with it"
	}
	return ""
}

// Apply runs the ops in order over d and returns a new Doc and one result per op; d is never modified.
func Apply(d Doc, b Batch, env Env) (Doc, []OpResult) {
	res := make([]OpResult, 0, len(b.Ops))
	for _, o := range b.Ops {
		next, r := applyOp(d, o, env)
		if r.Outcome == DreamOK {
			d = next
		}
		r.Op = o
		res = append(res, r)
	}
	return d, res
}

func failed(format string, a ...any) OpResult {
	return OpResult{Outcome: DreamFailed, Reason: fmt.Sprintf(format, a...)}
}

func applyOp(d Doc, o Op, env Env) (Doc, OpResult) {
	if err := o.check(); err != nil {
		return d, failed("%v", err)
	}
	switch o.Op {
	case "add":
		return opAdd(d, o, env)
	case "update":
		return opUpdate(d, o, env)
	case "supersede":
		return opSupersede(d, o, env)
	case "close":
		return opClose(d, o, env)
	}
	return d, OpResult{Outcome: DreamNoop, Reason: "no change"}
}

func (e DocEntry) block() string { return strings.Join(e.Lines, "\n") }

// refOf is the ref an entry answers to: its stored id, else its contentRef.
func (e DocEntry) refOf() string {
	if e.ID != "" {
		return e.ID
	}
	return contentRef(e.block())
}

func docEntry(lines []string) DocEntry {
	e := newEntry(lines[0], 0)
	e.Lines, e.ID = lines, entryID(lines)
	return e
}

func (d Doc) hasID(id string) bool {
	return slices.ContainsFunc(d.Entries, func(e DocEntry) bool { return e.ID == id })
}

// matches reports whether e answers to ref r.
func (e DocEntry) matches(r Ref) bool {
	if r.Derived {
		return contentRef(e.block()) == contentRefPrefix+r.Hex
	}
	return e.ID == r.Hex
}

// target finds the entry an op names and checks that it is still the one the writer read.
func (d Doc) target(o Op) (int, string) {
	r, _ := ParseRef(o.ID)
	var hits []int
	for i, e := range d.Entries {
		if e.matches(r) {
			hits = append(hits, i)
		}
	}
	if len(hits) != 1 {
		return -1, map[bool]string{true: "unknown id", false: "ambiguous id"}[len(hits) == 0]
	}
	e := d.Entries[hits[0]]
	if e.Kind == KindHeading && d.Grammar != GrammarDated {
		return -1, "not an entry"
	}
	if rd, _ := ParseRef(o.Read); !r.Derived && contentRefPrefix+rd.Hex != contentRef(e.block()) {
		return -1, "changed since read"
	}
	return hits[0], ""
}

func (env Env) mint(d Doc, first string) (string, bool) {
	for salt := range 8 {
		var id string
		if env.NewID != nil {
			id = env.NewID(salt, first)
		} else {
			id = blockID(freshID(d.Name, salt, first, env.Now))
		}
		if len(id) == 8 && isHex(id) && !d.hasID(id) {
			return id, true
		}
	}
	return "", false
}

func (env Env) live(d Doc, ref string) bool {
	if env.Live != nil {
		return env.Live(ref)
	}
	_, msg := d.target(Op{ID: ref, Read: ref})
	return msg == "" || msg == "changed since read"
}

// opTags are the json names of Op's fields, the one reflective read behind the schema.
func opTags() []string {
	var tags []string
	for f := range reflect.TypeFor[Op]().Fields() {
		tags = append(tags, strings.Split(f.Tag.Get("json"), ",")[0])
	}
	return tags
}

func endsBlank(lines []string) bool { return len(lines) > 1 && lines[len(lines)-1] == "" }

func reseq(ents []DocEntry) []DocEntry {
	for i := range ents {
		ents[i].Seq = i
	}
	return ents
}

// insertEntry puts lines at index at; at == len(ents) appends below the last entry and keeps the file's final newline.
func insertEntry(ents []DocEntry, at int, lines []string, sep bool) []DocEntry {
	if at < len(ents) {
		if sep && !endsBlank(lines) {
			lines = append(slices.Clone(lines), "")
		}
		return reseq(slices.Insert(ents, at, docEntry(lines)))
	}
	return appendEntry(ents, lines)
}

func appendEntry(ents []DocEntry, lines []string) []DocEntry {
	for len(lines) > 1 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	n := len(ents)
	tail := n == 0 || endsBlank(ents[n-1].Lines)
	if n > 0 {
		prev := ents[n-1].Lines
		if tail {
			prev = prev[:len(prev)-1]
		}
		if n > 1 && endsBlank(ents[n-2].Lines) && !endsBlank(prev) {
			prev = append(slices.Clone(prev), "")
		}
		ents[n-1] = docEntry(slices.Clone(prev))
	}
	if tail {
		lines = append(slices.Clone(lines), "")
	}
	return reseq(append(ents, docEntry(lines)))
}

// bodyLines is the entry text for an op: a dated heading plus body, or an indented bullet.
func (d Doc) bodyLines(o Op, env Env, sup string) ([]string, error) {
	var lines []string
	text := strings.Split(o.Text, "\n")
	if d.Grammar == GrammarDated {
		if o.Title == "" {
			return nil, errors.New("a dated entry needs a title")
		}
		lines = []string{"## " + env.Now.Format(dayLayout) + " — " + o.Title}
		if sup != "" {
			lines = append(lines, "Supersedes "+sup+".")
		}
		if o.Text != "" {
			lines = append(lines, "")
			lines = append(lines, text...)
		}
		lines = append(lines, "")
	} else {
		if o.Text == "" {
			return nil, errors.New("a bullet needs text")
		}
		lines = []string{"- " + text[0]}
		for _, ln := range text[1:] {
			lines = append(lines, strings.TrimRight("  "+ln, " "))
		}
		if sup != "" {
			lines = append(lines, "  Supersedes "+sup+".")
		}
	}
	return lines, checkLines(d.Grammar, lines)
}

// checkLines rejects text that would parse as another entry or leave a code fence open.
func checkLines(g Grammar, lines []string) error {
	fence := ""
	for i, ln := range lines {
		if i > 0 && fence == "" && g.starts(ln) {
			return fmt.Errorf("line %q would start a new entry", ln)
		}
		fence = nextFence(fence, ln)
	}
	if fence != "" {
		return errors.New("unbalanced code fence")
	}
	return nil
}

// entryKey is an entry's text without its id, date and heading mark, whitespace-normalised.
func entryKey(lines []string) string {
	var b strings.Builder
	for i, ln := range lines {
		if _, lo, hi, ok := marker(ln); ok {
			ln = ln[:lo] + ln[hi:]
		}
		if i == 0 {
			ln = ln[len(titlePrefix().FindString(ln)):]
		}
		b.WriteString(ln + "\n")
	}
	return string(appendFields(nil, b.String()))
}

func (d Doc) duplicateOf(lines []string) (DocEntry, bool) {
	key, kind := entryKey(lines), docEntry(lines).Kind
	for _, e := range d.Entries {
		if e.Kind == kind && entryKey(e.Lines) == key {
			return e, true
		}
	}
	return DocEntry{}, false
}

// withID stamps a fresh id marker on the first line when the file's grammar carries ids.
func (d Doc) withID(lines []string, env Env) ([]string, error) {
	if d.Grammar == GrammarSections {
		return lines, nil
	}
	id, ok := env.mint(d, lines[0])
	if !ok {
		return nil, errors.New("could not mint a free id")
	}
	out := slices.Clone(lines)
	out[0] += " " + markerOpen + id + markerClose
	return out, nil
}

// place puts a new entry where its grammar wants it: dated on top, bullets at the bottom, sections under a heading.
func (d Doc) place(ents []DocEntry, o Op, lines []string) ([]DocEntry, error) {
	switch d.Grammar {
	case GrammarDated:
		return insertEntry(ents, 0, lines, true), nil
	case GrammarBullets:
		return insertEntry(ents, len(ents), lines, false), nil
	}
	if o.Section == "" {
		return insertEntry(ents, len(ents), lines, false), nil
	}
	cur := Doc{Entries: ents}
	lo, _ := cur.Section(o.Section)
	if lo < 0 {
		order := sectionsOf[o.Target]
		if !slices.Contains(order, o.Section) {
			return nil, fmt.Errorf("unknown section %q", o.Section)
		}
		at := headingSlot(ents, order, o.Section)
		ents, lo = insertEntry(ents, at, []string{"## " + o.Section}, false), at+1
	}
	return insertEntry(ents, lo, lines, false), nil
}

// headingSlot is the index before the first listed heading that follows section in the canonical order.
func headingSlot(ents []DocEntry, order []string, section string) int {
	want := slices.Index(order, section)
	for i, e := range ents {
		if e.Kind == KindHeading && slices.Index(order, strings.TrimSpace(strings.TrimPrefix(e.Lines[0], "## "))) > want {
			return i
		}
	}
	return len(ents)
}

func opAdd(d Doc, o Op, env Env) (Doc, OpResult) {
	lines, err := d.bodyLines(o, env, "")
	if err != nil {
		return d, failed("%v", err)
	}
	if dup, ok := d.duplicateOf(lines); ok {
		return d, OpResult{ID: dup.refOf(), Outcome: DreamNoop, Reason: "duplicate"}
	}
	if lines, err = d.withID(lines, env); err == nil {
		d.Entries, err = d.place(slices.Clone(d.Entries), o, lines)
	}
	if err != nil {
		return d, failed("%v", err)
	}
	return d, OpResult{ID: docEntry(lines).refOf(), Outcome: DreamOK}
}

// pointerLines are the supersede and replaced-by lines an update keeps.
func pointerLines(lines []string) []string {
	var keep []string
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); strings.HasPrefix(t, "Supersedes ") && strings.HasSuffix(t, ".") || pointerRef(ln) != "" {
			keep = append(keep, ln)
		}
	}
	return keep
}

func trailingBlanks(lines []string) []string {
	n := len(lines)
	for n > 1 && lines[n-1] == "" {
		n--
	}
	return lines[n:]
}

// rewrite is the entry's new lines for an update: marker, pointers and trailing blanks stay.
func (d Doc) rewrite(e DocEntry, o Op, env Env) ([]string, error) {
	if d.Grammar == GrammarDated {
		return d.rewriteDated(e, o)
	}
	repl, err := d.bodyLines(o, env, "")
	if err != nil {
		return nil, err
	}
	if e.ID != "" {
		repl[0] += " " + markerOpen + e.ID + markerClose
	}
	return slices.Concat(repl, pointerLines(e.Lines[1:]), trailingBlanks(e.Lines)), nil
}

func (d Doc) rewriteDated(e DocEntry, o Op) ([]string, error) {
	head := e.Lines[0]
	if o.Title != "" {
		head = retitle(head, o.Title)
	}
	if o.Text == "" {
		return slices.Concat([]string{head}, e.Lines[1:]), nil
	}
	out := slices.Concat([]string{head}, pointerLines(e.Lines[1:]), []string{""}, strings.Split(o.Text, "\n"))
	return slices.Concat(out, trailingBlanks(e.Lines)), checkLines(d.Grammar, out)
}

// retitle swaps the title text of a dated heading, keeping its mark, date prefix and id marker.
func retitle(head, title string) string {
	tail := ""
	if _, lo, _, ok := marker(head); ok {
		head, tail = strings.TrimRight(head[:lo], " "), " "+head[lo:]
	}
	return head[:len(titlePrefix().FindString(head))] + title + tail
}

func opUpdate(d Doc, o Op, env Env) (Doc, OpResult) {
	i, msg := d.target(o)
	if msg != "" {
		return d, failed("%s", msg)
	}
	lines, err := d.rewrite(d.Entries[i], o, env)
	if err != nil {
		return d, failed("%v", err)
	}
	ents := slices.Clone(d.Entries)
	ents[i] = docEntry(lines)
	ents[i].Seq = i
	d.Entries = ents
	return d, OpResult{ID: ents[i].refOf(), Outcome: DreamOK}
}

// markLines strikes an old entry's title and adds its "Replaced by" pointer, as RememberSupersedes does.
func markLines(e DocEntry, by string, now time.Time) []string {
	return strings.Split(markReplaced(e.block(), by, now.Format(dayLayout)), "\n")
}

func opSupersede(d Doc, o Op, env Env) (Doc, OpResult) {
	if d.Name != "decisionLog.md" && d.Name != "conventions.md" {
		return d, failed("supersede is not allowed in %s", d.Name)
	}
	i, msg := d.target(o)
	if msg != "" {
		return d, failed("%s", msg)
	}
	old := d.Entries[i]
	if isSupersededBlock(old.block()) {
		return d, failed("already superseded")
	}
	lines, err := d.bodyLines(o, env, old.refOf())
	if err != nil {
		return d, failed("%v", err)
	}
	if _, dup := d.duplicateOf(lines); dup {
		return d, OpResult{ID: old.refOf(), Outcome: DreamNoop, Reason: "duplicate"}
	}
	if lines, err = d.withID(lines, env); err != nil {
		return d, failed("%v", err)
	}
	newRef := docEntry(lines).refOf()
	ents := slices.Clone(d.Entries)
	ents[i] = docEntry(markLines(old, newRef, env.Now))
	if d.Entries, err = d.place(ents, o, lines); err != nil {
		return d, failed("%v", err)
	}
	return d, OpResult{ID: newRef, Outcome: DreamOK}
}

// removeEntry drops entry i and keeps the file's final newline when the last entry goes.
func removeEntry(ents []DocEntry, i int) []DocEntry {
	last := i == len(ents)-1
	gone := ents[i]
	ents = slices.Delete(ents, i, i+1)
	if last && i > 0 && endsBlank(gone.Lines) && !endsBlank(ents[i-1].Lines) {
		ents[i-1] = docEntry(append(slices.Clone(ents[i-1].Lines), ""))
	}
	return reseq(ents)
}

func opClose(d Doc, o Op, env Env) (Doc, OpResult) {
	i, msg := d.target(o)
	if msg != "" {
		return d, failed("%s", msg)
	}
	e := d.Entries[i]
	replaced := o.Reason == "superseded-by"
	switch {
	case d.Grammar == GrammarDated && !replaced:
		return d, failed("append-only: a dated entry closes only as superseded-by")
	case replaced && !env.live(d, o.By):
		return d, failed("by is not a live id")
	case replaced && isSupersededBlock(e.block()):
		return retypeSuperseded(d, i, o, env)
	}
	ents := slices.Clone(d.Entries)
	if d.Grammar == GrammarDated {
		ents[i] = docEntry(markLines(e, o.By, env.Now))
		ents[i].Seq = i
	} else {
		ents = removeEntry(ents, i)
	}
	d.Entries = ents
	return d, OpResult{ID: e.refOf(), Outcome: DreamOK}
}

// retypeSuperseded turns a dated entry's legacy pointer to o.By into the typed form; other grammars stay refused.
func retypeSuperseded(d Doc, i int, o Op, env Env) (Doc, OpResult) {
	e := d.Entries[i]
	ptr := replacedBy(e.block())
	cur, _ := ParseRef(ptr)
	by, _ := ParseRef(o.By)
	if d.Grammar != GrammarDated || ptr == "" || cur.Hex != by.Hex || cur.Derived != by.Derived {
		return d, failed("already superseded")
	}
	same := OpResult{ID: e.refOf(), Outcome: DreamNoop, Reason: "already typed"}
	if len(e.Lines) > 1 && datedPointerRe.MatchString(strings.TrimSpace(e.Lines[1])) && isSupersededTitle(e.Lines[0]) {
		return d, same
	}
	pre := docEntry(strings.Split(dropPointer(e.block()), "\n"))
	lines := markLines(pre, o.By, env.Now)
	if strings.Join(lines, "\n") == e.block() {
		return d, same
	}
	ents := slices.Clone(d.Entries)
	ents[i] = docEntry(lines)
	ents[i].Seq = i
	d.Entries = ents
	return d, OpResult{ID: e.refOf(), Outcome: DreamOK}
}

type schemaObj = map[string]any

const oneLinePattern = `^[^\r\n]*$`

// fieldRules constrain each json field of Op; a field without a rule here fails TestBatchSchemaMatchesOp.
var fieldRules = map[string]schemaObj{
	"op":      {"enum": opNames},
	"target":  {"enum": targets},
	"section": {"maxLength": maxLabel, "pattern": oneLinePattern},
	"id":      {"pattern": refPattern},
	"read":    {"pattern": refPattern},
	"title":   {"maxLength": maxOpText, "pattern": oneLinePattern},
	"text":    {"maxLength": maxOpText},
	"reason":  {"enum": closeReasons},
	"by":      {"pattern": refPattern},
	"note":    {"maxLength": maxNote, "pattern": oneLinePattern},
}

func isOp(names ...string) schemaObj {
	return schemaObj{"properties": schemaObj{"op": schemaObj{"enum": names}}, "required": []string{"op"}}
}

func has(fields ...string) schemaObj { return schemaObj{"required": fields} }

func anyOf(fields ...string) schemaObj {
	var alts []schemaObj
	for _, f := range fields {
		alts = append(alts, has(f))
	}
	return schemaObj{"anyOf": alts}
}

func when(cond, then schemaObj) schemaObj { return schemaObj{"if": cond, "then": then} }

// opRulesSchema is Validate's cross-field rules as if/then clauses.
func opRulesSchema() []schemaObj {
	storedID := schemaObj{"properties": schemaObj{"id": schemaObj{"pattern": "^[0-9a-f]"}}, "required": []string{"op", "id"}}
	edits := isOp("update", "supersede", "close")
	storedEdit := schemaObj{"allOf": []schemaObj{edits, storedID}}
	return []schemaObj{
		when(isOp("add", "update", "supersede"), anyOf("title", "text")),
		when(edits, has("id")),
		when(storedEdit, has("read")),
		when(isOp("close"), has("reason")),
		when(has("by"), schemaObj{"properties": schemaObj{"reason": schemaObj{"const": "superseded-by"}}, "required": []string{"reason"}}),
		when(schemaObj{"properties": schemaObj{"reason": schemaObj{"const": "superseded-by"}}, "required": []string{"reason"}}, has("by")),
		when(isOp("add", "noop"), schemaObj{"not": anyOf("id", "read")}),
		when(schemaObj{"not": isOp("close")}, schemaObj{"not": schemaObj{"anyOf": []schemaObj{has("reason"), has("by")}}}),
	}
}

// BatchSchema is the JSON Schema of Batch's ops, built from Op's json fields and the enum lists above.
func BatchSchema() string { return jsonString(batchSchema(nil)) }

// DreamBatchSchema is BatchSchema plus the dream-only candidates, test_proposals and plan_tasks arrays.
func DreamBatchSchema() string { return jsonString(batchSchema(dreamSideSchema())) }

func strField(rules schemaObj) schemaObj {
	f := schemaObj{"type": "string", "pattern": oneLinePattern}
	maps.Copy(f, rules)
	return f
}

func sideArray(required []string, props schemaObj) schemaObj {
	item := schemaObj{"type": "object", "additionalProperties": false, "required": required, "properties": props}
	return schemaObj{"type": "array", "maxItems": maxSideOps, "items": item}
}

// dreamSideSchema types the three dream-only arrays; Go writes every file from them.
func dreamSideSchema() schemaObj {
	return schemaObj{
		"candidates": sideArray([]string{"id", "evidence", "repeats", "change", "target", "kind", "size"}, schemaObj{
			"id": strField(schemaObj{"pattern": `^fc-[a-z0-9][a-z0-9-]*$`}), "repeats": schemaObj{"type": "integer", "minimum": 0},
			"evidence": schemaObj{"type": "array", "maxItems": 8, "items": strField(nil)},
			"change":   strField(schemaObj{"maxLength": candidateMaxText}), "target": strField(nil),
			"kind": schemaObj{"enum": candidateKinds}, "size": schemaObj{"enum": candidateSizes}}),
		"test_proposals": sideArray([]string{"pkg", "case", "reason"}, schemaObj{
			"pkg": strField(nil), "case": strField(nil), "reason": strField(nil), "runtime": strField(nil)}),
		"plan_tasks": sideArray([]string{"plan", "id", "action"}, schemaObj{
			"plan": strField(nil), "id": strField(nil), "action": schemaObj{"enum": []string{"done", "strike"}},
			"sha": strField(nil), "reason": schemaObj{"enum": plans.StrikeReasons}, "evidence": strField(nil)}),
	}
}

func batchSchema(side schemaObj) schemaObj {
	props := schemaObj{}
	for _, tag := range opTags() {
		p := schemaObj{"type": "string"}
		maps.Copy(p, fieldRules[tag])
		props[tag] = p
	}
	op := schemaObj{
		"type": "object", "additionalProperties": false, "required": []string{"op"},
		"properties": props, "allOf": opRulesSchema(),
	}
	batch := schemaObj{"ops": schemaObj{"type": "array", "maxItems": maxBatchOps, "items": op}}
	maps.Copy(batch, side)
	return schemaObj{
		"$schema": schemaDraft, "type": "object", "additionalProperties": false,
		"required": []string{"ops"}, "properties": batch,
	}
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // the schema is built from literals; a failure is a programming error
	}
	return string(b)
}

// beforeBatchCommit runs between ApplyBatch's render and its journaled write; tests race an editor there.
var beforeBatchCommit = func() {}

// ApplyBatch applies b to the bank files under the bank lock; nothing is written unless every op passes.
func ApplyBatch(b Batch, base Versions, now time.Time) ([]OpResult, error) {
	var results []OpResult
	err := withFlatLock(BankLockWait, func() error {
		var err error
		results, err = applyBatchLocked(b, base, now)
		return err
	})
	return results, err
}

// applyBatchLocked is ApplyBatch for a caller already holding the bank lock.
func applyBatchLocked(b Batch, base Versions, now time.Time) ([]OpResult, error) {
	b, err := withBase(b, base)
	if err == nil {
		err = b.Validate()
	}
	if err != nil {
		return nil, err
	}
	if _, err := recoverEach(); err != nil { // replay older journals first so the bytes read below are current
		return nil, err
	}
	docs, raw, err := loadBatchDocs()
	if err != nil {
		return nil, err
	}
	results := runBatchOps(docs, b, now)
	if n := countFailedOps(results); n > 0 {
		return results, fmt.Errorf("%d of %d ops failed: nothing written", n, len(b.Ops))
	}
	return results, commitBatchDocs(docs, raw, now)
}

// withBase fills each op's empty Read from base and refuses a Read that disagrees with it; b is not modified.
func withBase(b Batch, base Versions) (Batch, error) {
	ops := slices.Clone(b.Ops)
	for i, o := range ops {
		want := string(base[o.ID])
		switch {
		case want == "":
		case o.Read == "":
			ops[i].Read = want
		case o.Read != want:
			return b, fmt.Errorf("op %d: read %s disagrees with base %s", i, o.Read, want)
		}
	}
	return Batch{Ops: ops}, nil // Clone keeps a nil Ops nil, so Validate still reports it missing
}

// loadBatchDocs reads every bank file; a missing one is an empty doc, any other read error stops the batch.
func loadBatchDocs() (map[string]Doc, map[string][]byte, error) {
	docs, raw := map[string]Doc{}, map[string][]byte{}
	for _, name := range bankFiles {
		data, err := os.ReadFile(Path(name))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, err
		}
		raw[name], docs[name] = data, ParseDoc(name, data)
	}
	return docs, raw, nil
}

func countFailedOps(rs []OpResult) int {
	return len(slices.DeleteFunc(slices.Clone(rs), func(r OpResult) bool { return r.Outcome != DreamFailed }))
}

// runBatchOps applies the ops one by one in batch order, each to the file it routes to; every op reports.
func runBatchOps(docs map[string]Doc, b Batch, now time.Time) []OpResult {
	results := make([]OpResult, 0, len(b.Ops))
	for _, o := range b.Ops {
		name, r := routeBatchOp(docs, o)
		if name != "" {
			var rs []OpResult
			docs[name], rs = Apply(docs[name], Batch{Ops: []Op{o}}, batchEnv(docs, name, now))
			r = rs[0]
		}
		r.Op = o
		results = append(results, r)
	}
	return results
}

// batchEnv checks liveness and mints ids against every loaded bank doc, not only the one being edited.
func batchEnv(docs map[string]Doc, name string, now time.Time) Env {
	live := func(ref string) bool {
		for _, d := range docs {
			if _, msg := d.target(Op{ID: ref, Read: ref}); msg == "" || msg == "changed since read" {
				return true
			}
		}
		return false
	}
	newID := func(salt int, first string) string {
		id := blockID(freshID(name, salt, first, now))
		for _, d := range docs {
			if d.hasID(id) {
				return "" // mint skips an invalid id and tries the next salt
			}
		}
		return id
	}
	return Env{Now: now, Live: live, NewID: newID}
}

// routeBatchOp names the bank file for o; a non-empty name means apply, else the result is final.
func routeBatchOp(docs map[string]Doc, o Op) (string, OpResult) {
	switch {
	case o.Op == "noop":
		return "", OpResult{Outcome: DreamNoop, Reason: "no change"}
	case o.Target != "":
		if name := o.Target + ".md"; slices.Contains(bankFiles, name) {
			return name, OpResult{}
		}
		return "", failed("target %s is not a bank file", o.Target)
	case !o.editsEntry():
		return "", failed("add needs a target")
	}
	r, _ := ParseRef(o.ID)
	var hits []string
	for _, name := range bankFiles {
		if slices.ContainsFunc(docs[name].Entries, func(e DocEntry) bool { return e.matches(r) }) {
			hits = append(hits, name)
		}
	}
	switch len(hits) {
	case 0:
		return "", failed("unknown id")
	case 1:
		return hits[0], OpResult{}
	}
	return "", failed("ambiguous id")
}

// commitBatchDocs commits every changed file as one journaled batch, rolled back on failure, then settles.
func commitBatchDocs(docs map[string]Doc, raw map[string][]byte, now time.Time) error {
	changes := batchChanges(docs, raw)
	if len(changes) == 0 {
		return nil
	}
	beforeBatchCommit()
	rec, err := commitChangesLocked("settle: "+stateKey(), changes)
	if errors.Is(err, errDivergent) {
		err = fmt.Errorf("files changed during apply (%s): %w", strings.Join(rec.Divergent, ", "), err)
	}
	if err != nil {
		return rollbackBatch(raw, rec, err)
	}
	// the batch is committed whole; settle only derives files (rotation, index) and the next settle redoes it
	if _, err := settleBankLocked(now); tolerateIndex(err) != nil {
		return fmt.Errorf("batch committed as %s; settle: %w", rec.Commit, tolerateIndex(err))
	}
	return nil
}

// batchChanges lists a planned write for each bank file whose rendering differs from the bytes read.
func batchChanges(docs map[string]Doc, raw map[string][]byte) []storeChange {
	var changes []storeChange
	for _, name := range bankFiles {
		out := docs[name].Render()
		if bytes.Equal(out, raw[name]) {
			continue
		}
		sha := ""
		if raw[name] != nil {
			sha = textutil.SHA256Hex(raw[name])
		}
		changes = append(changes, storeChange{Rel: name, Data: out, Planned: true, PreSHA256: sha})
	}
	return changes
}

// rollbackBatch drops the batch's journal, restores the files it applied from the bytes read, and undoes its commit.
func rollbackBatch(raw map[string][]byte, rec commitReceipt, err error) error {
	if rec.ID != "" { // a kept journal would replay the aborted write on the next commit
		if rerr := os.Remove(filepath.Join(JournalDir(), rec.ID+".json")); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return errors.Join(err, fmt.Errorf("drop the journal: %w", rerr))
		}
	}
	var snaps []fileSnapshot
	for _, rel := range rec.Applied {
		snaps = append(snaps, fileSnapshot{path: Path(rel), prev: raw[rel], existed: raw[rel] != nil})
	}
	if rerr := restoreSnaps(snaps); rerr != nil { // half restored: keep the commit as the recovery point
		return errors.Join(err, fmt.Errorf("roll back the write: %w", rerr))
	}
	return errors.Join(fmt.Errorf("%w (write rolled back)", err), undoBatchCommit(rec.Commit))
}

// undoBatchCommit moves HEAD off the batch's own commit only while it is still HEAD; a native capture stays.
func undoBatchCommit(sha string) error {
	if sha == "" {
		return nil
	}
	repo := StoreRepo()
	full, err := repo.run("rev-parse", "--verify", sha+"^{commit}") // Commit holds the short form
	if head, ok := repo.headSHA(); err != nil || !ok || head != full {
		return fmt.Errorf("batch commit %s is no longer HEAD: left in place", sha)
	}
	_, err = repo.run("reset", "--quiet", full+"^")
	return err
}
