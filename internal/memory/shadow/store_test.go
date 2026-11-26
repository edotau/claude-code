package shadow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/textutil"
)

// openStoreEnv builds the shadow db from a fixture bank under a temp config dir and returns the bank dir.
func openStoreEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := paths.MemoryStoreDir()
	writeShadowStore(t, root, false)
	pinFilesTruth(t)
	if _, err := RebuildShadow(context.Background()); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestOpenBankStoreLoadsImport(t *testing.T) {
	root := openStoreEnv(t)
	for i, locked := range []bool{false, true} {
		newBody := "# Conventions\n\n- use gofmt\n- wrap errors\n- edit " + strings.Repeat("x", i+1) + "\n"
		if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte(newBody), 0o600); err != nil {
			t.Fatal(err)
		}
		s, closeFn, err := memory.OpenBankStore(locked)
		if err != nil {
			t.Fatal(err)
		}
		docs, _, err := s.Load()
		if err != nil || string(docs["conventions.md"].Render()) != newBody {
			t.Errorf("locked=%v render=%q err=%v", locked, docs["conventions.md"].Render(), err)
		}
		if err := closeFn(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFirstOpenAfterTruthFilesImportsFirst(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := paths.MemoryStoreDir()
	writeShadowStore(t, root, false)
	want := "# Conventions\n\n- use gofmt\n- edited under files truth\n"
	if err := os.WriteFile(filepath.Join(root, "conventions.md"), []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	s, closeFn, err := memory.OpenBankStore(false)
	if err != nil {
		t.Fatal(err)
	}
	docs, _, err := s.Load()
	if err != nil || string(docs["conventions.md"].Render()) != want {
		t.Fatalf("first open: render=%q err=%v; want the file content imported before any read", docs["conventions.md"].Render(), err)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := memory.ShadowImport(); err != nil || changed {
		t.Fatalf("second import changed=%v err=%v; want no re-import of unchanged files", changed, err)
	}
}

func TestOpenBankStoreApplyVisibleOnReopen(t *testing.T) {
	openStoreEnv(t)
	s, closeFn, err := memory.OpenBankStore(false)
	if err != nil {
		t.Fatal(err)
	}
	_, v, _ := s.Load()
	b := memory.Batch{Ops: []memory.Op{{Op: "update", ID: "0123abcd", Text: "changed body"}}}
	if _, err := s.Apply(b, v, storeNow); err != nil {
		t.Fatal(err)
	}
	if err := closeFn(); err != nil {
		t.Fatal(err)
	}
	s2, close2, err := memory.OpenBankStore(false)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer close2()
	docs, _, err := s2.Load()
	if err != nil || !strings.Contains(string(docs["decisionLog.md"].Render()), "changed body") {
		t.Errorf("apply not visible: err=%v", err)
	}
}

func TestOpenBankStoreLockedUnderHeldLock(t *testing.T) {
	openStoreEnv(t)
	err := memory.WithBankLock(0, func() error {
		s, closeFn, err := memory.OpenBankStore(true)
		if err != nil {
			return err
		}
		if _, _, err := s.Load(); err != nil {
			t.Error(err)
		}
		if err := closeFn(); err != nil {
			t.Error(err)
		}
		if _, _, err := s.Load(); err == nil {
			t.Error("db still open after close")
		}
		return closeFn()
	})
	if err != nil {
		t.Fatalf("locked open under a held bank lock: %v", err)
	}
}

func TestOpenBankStoreMigrateFailure(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(ShadowDBPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	junk := []byte(strings.Repeat("not a sqlite database ", 20))
	if err := os.WriteFile(ShadowDBPath(), junk, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _, err := memory.OpenBankStore(false); err == nil || s != nil {
		t.Fatalf("store=%v err=%v; want nil store and an error", s, err)
	}
}

var storeNow = time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)

// bankStoreFixture is a db rebuilt from the shadow fixture and the store over it.
func bankStoreFixture(t *testing.T) (memory.BankStore, func(q string) string) {
	t.Helper()
	root := t.TempDir()
	writeShadowStore(t, root, false)
	db := openShadowDB(t, nil)
	if _, err := shadowRebuild(context.Background(), db, root); err != nil {
		t.Fatal(err)
	}
	one := func(q string) string {
		var s string
		if err := db.QueryRowContext(context.Background(), q).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	return NewSQLBankStore(db), one
}

func TestBankStoreLoad(t *testing.T) {
	s, _ := bankStoreFixture(t)
	docs, v, err := s.Load()
	if err != nil || len(docs) != len(memory.BankFiles()) {
		t.Fatalf("docs=%d err=%v", len(docs), err)
	}
	if got := docs["decisionLog.md"].Entries[0].Ref(); got != "0123abcd" || v[got] != "1" {
		t.Errorf("ref %q version %q", got, v[got])
	}
	if got := string(docs["decisionLog.md"].Render()); got != shadowFixture[0].body {
		t.Errorf("render\n%q\nwant\n%q", got, shadowFixture[0].body)
	}
}

func TestBankStoreApplyUpdateAndAdd(t *testing.T) {
	s, one := bankStoreFixture(t)
	_, v, _ := s.Load()
	b := memory.Batch{Ops: []memory.Op{
		{Op: "update", ID: "0123abcd", Text: "changed body"},
		{Op: "add", Target: "decisionLog", Title: "fresh", Text: "t"},
	}}
	rs, err := s.Apply(b, v, storeNow)
	if err != nil || len(rs) != 2 || rs[0].Outcome != memory.DreamOK || rs[1].Outcome != memory.DreamOK {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	docs, v2, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	out := string(docs["decisionLog.md"].Render())
	if !strings.Contains(out, "changed body") || !strings.Contains(out, " — fresh <!-- id:") {
		t.Errorf("not written:\n%s", out)
	}
	if got := v2[rs[0].ID]; got != "2" {
		t.Errorf("updated entry version %q, want 2", got)
	}
	if n := one(`SELECT count(*) FROM entries WHERE doc = 'decisionLog.md' AND deleted = 0`); n != "3" {
		t.Errorf("live decisionLog rows = %s, want 3", n)
	}
}

func TestBankStoreApplyConflict(t *testing.T) {
	s, one := bankStoreFixture(t)
	_, v, _ := s.Load()
	if _, err := s.Apply(memory.Batch{Ops: []memory.Op{{Op: "update", ID: "0123abcd", Text: "first"}}}, v, storeNow); err != nil {
		t.Fatal(err)
	}
	before := one(`SELECT group_concat(lines || version, '|') FROM entries`)
	stale := memory.Batch{Ops: []memory.Op{
		{Op: "add", Target: "decisionLog", Title: "other", Text: "t"},
		{Op: "update", ID: "0123abcd", Text: "second"},
	}}
	_, err := s.Apply(stale, v, storeNow)
	var c *memory.ConflictError
	if !errors.As(err, &c) || c.Op != 1 || c.Ref != "0123abcd" || c.Base != "1" || c.Current != "2" {
		t.Fatalf("err = %v, want conflict on op 1", err)
	}
	if after := one(`SELECT group_concat(lines || version, '|') FROM entries`); after != before {
		t.Error("a conflicting batch wrote rows")
	}
}

// TestBankStoreApplyInTransaction makes the last write fail and checks the earlier ones rolled back with it.
func TestBankStoreApplyInTransaction(t *testing.T) {
	s, one := bankStoreFixture(t)
	st := s.(sqlBankStore)
	if _, err := st.db.Exec(`CREATE TRIGGER boom BEFORE INSERT ON entries WHEN new.lines LIKE '%boom%'
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	before := one(`SELECT group_concat(lines || version || seq, '|') FROM entries`)
	b := memory.Batch{Ops: []memory.Op{
		{Op: "update", ID: "0123abcd", Text: "changed body"},
		{Op: "add", Target: "decisionLog", Title: "boom", Text: "t"},
	}}
	_, v, _ := s.Load()
	if _, err := s.Apply(b, v, storeNow); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want the trigger's abort", err)
	}
	if after := one(`SELECT group_concat(lines || version || seq, '|') FROM entries`); after != before {
		t.Error("the update before the failing insert stayed written: Apply is not one transaction")
	}
}

func TestBankStoreApplyNeedsBase(t *testing.T) {
	s, _ := bankStoreFixture(t)
	b := memory.Batch{Ops: []memory.Op{{Op: "close", ID: "0123abcd", Reason: "done"}}}
	var c *memory.ConflictError
	if _, err := s.Apply(b, nil, storeNow); !errors.As(err, &c) || c.Base != "" {
		t.Fatalf("err = %v, want a conflict for the missing base", err)
	}
}

func TestBankStoreKeepsIdentityAndNeverReusesVersions(t *testing.T) {
	s, one := bankStoreFixture(t)
	_, v, _ := s.Load()
	ref := memory.ParseDoc("conventions.md", []byte(shadowFixture[1].body)).Entries[0].Ref()
	rs, err := s.Apply(memory.Batch{Ops: []memory.Op{{Op: "update", ID: ref, Text: "use gofmt always"}}}, v, storeNow)
	if err != nil {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	if n := one(`SELECT count(*) FROM entries WHERE doc = 'conventions.md'`); n != "2" {
		t.Errorf("rows = %s, want the edited ID-less entry to keep its row (2)", n)
	}
	_, v2, _ := s.Load()
	if v2[rs[0].ID] == "1" {
		t.Errorf("edited entry kept version 1")
	}
	second := memory.ParseDoc("conventions.md", []byte(shadowFixture[1].body)).Entries[1].Ref()
	if _, err := s.Apply(memory.Batch{Ops: []memory.Op{{Op: "close", ID: second, Reason: "stale"}}}, v2, storeNow); err != nil {
		t.Fatal(err)
	}
	if got := one(`SELECT version FROM entries WHERE deleted = 1`); got == "1" {
		t.Errorf("a closed entry kept version 1")
	}
}

// keepsRow applies ops and checks the entry that began with prefix keeps its live row at a new version.
func keepsRow(t *testing.T, doc, prefix string, ops ...memory.Op) {
	t.Helper()
	s, one := bankStoreFixture(t)
	_, v, _ := s.Load()
	q := `SELECT rowid FROM entries WHERE doc = '` + doc + `' AND lines LIKE '` + prefix + `%'`
	rowid := one(q)
	if rs, err := s.Apply(memory.Batch{Ops: ops}, v, storeNow); err != nil {
		t.Fatalf("rs=%+v err=%v", rs, err)
	}
	live := `SELECT version FROM entries WHERE rowid = ` + rowid + ` AND deleted = 0`
	if got := one(live); got == "1" {
		t.Errorf("%s: row %s kept version 1 although its lines changed", prefix, rowid)
	}
}

func conventionRef(i int) string {
	return memory.ParseDoc("conventions.md", []byte(shadowFixture[1].body)).Entries[i].Ref()
}

func TestBankStoreRowChainedUpdates(t *testing.T) {
	mid := memory.ParseDoc("conventions.md", []byte("- mid\n")).Entries[0].Ref()
	keepsRow(t, "conventions.md", "- use gofmt",
		memory.Op{Op: "update", ID: conventionRef(0), Text: "mid"}, memory.Op{Op: "update", ID: mid, Text: "last"})
}

func TestBankStoreRowSupersede(t *testing.T) {
	keepsRow(t, "conventions.md", "- wrap errors",
		memory.Op{Op: "supersede", ID: conventionRef(1), Target: "conventions", Text: "wrap errors with context"})
}

func TestBankStoreRowCloseSupersededBy(t *testing.T) {
	old := memory.ParseDoc("decisionLog.md", []byte(shadowFixture[0].body)).Entries[1].Ref()
	keepsRow(t, "decisionLog.md", "## 2026-01-10",
		memory.Op{Op: "close", ID: old, Reason: "superseded-by", By: "0123abcd"})
}

func TestBankStoreRowTrailingBlankAdd(t *testing.T) {
	keepsRow(t, "conventions.md", "- wrap errors",
		memory.Op{Op: "add", Target: "conventions", Text: "brand new"})
}

// parityQueries is the fixed query set run through the hook's recall and through memory search after every step.
var parityQueries = []string{
	"sqlite store memory bank truth migration rollback export",
	"wrap errors context fmt Errorf percent verb convention",
	"parity test truth files versus database focus",
	"shadow window recall latency measurement progress",
	"session reviewed recall ranking archive rotation",
	"settle probe checkpoint recorded rotation session",
}

// parityKnownDiffs maps a step name to the exact known, documented difference that stops its parity; none today.
var parityKnownDiffs = map[string]string{}

// parityShot is everything read back after one step through the public read paths.
type parityShot struct {
	files  map[string]string // store-relative path to bytes: the six bank files and archive/*.md
	ids    []string          // "file#position:id" in file then entry order
	recall []string          // PromptRecall output per query
	search []string          // SearchBank hits per query
}

// parityEnv is one run of the script in its own temp store under one truth.
type parityEnv struct {
	t      *testing.T
	truth  string
	step   int
	probes int
	ids    map[string]string
}

// parityStep is one scripted operation; the same script runs under both truths.
type parityStep struct {
	name string
	run  func(e *parityEnv)
}

var parityScript = []parityStep{
	{"append", func(e *parityEnv) {
		res := e.apply(
			memory.Op{Op: "add", Target: "activeContext", Section: "Current Focus",
				Text: "Finish the parity test for truth files versus database"},
			memory.Op{Op: "add", Target: "progress", Section: "In Progress",
				Text: "Shadow window recall latency measurement"},
			memory.Op{Op: "add", Target: "decisionLog", Title: "Adopt sqlite store for memory bank truth",
				Text: "Rollback keeps the markdown export so the migration can be reversed."},
			memory.Op{Op: "add", Target: "conventions",
				Text: "Wrap errors with context using fmt Errorf and the percent w verb"},
			memory.Op{Op: "add", Target: "sessionHistory", Title: "Session reviewed recall ranking and archive rotation",
				Text: "Ranking matched the markdown baseline."},
		)
		for i, name := range []string{"active", "progress", "decision", "conv", "session"} {
			e.ids[name] = res[i].ID
		}
	}},
	{"revise", func(e *parityEnv) {
		e.apply(
			memory.Op{Op: "update", ID: e.ids["decision"], Read: e.read("decisionLog.md", e.ids["decision"]),
				Title: "Adopt sqlite store as the memory bank truth",
				Text:  "Rollback keeps the markdown export; the files truth flips back at any time."},
			memory.Op{Op: "update", ID: e.ids["conv"], Read: e.read("conventions.md", e.ids["conv"]),
				Text: "Wrap errors with context: fmt Errorf and the percent w verb, never bare returns"},
			memory.Op{Op: "update", ID: e.ids["progress"], Read: e.read("progress.md", e.ids["progress"]),
				Text: "Shadow window recall latency measurement, second pass"},
		)
	}},
	{"supersede", func(e *parityEnv) {
		res := e.apply(memory.Op{Op: "supersede", ID: e.ids["decision"],
			Read:  e.read("decisionLog.md", e.ids["decision"]),
			Title: "Sqlite stays the truth after the shadow window",
			Text:  "The db is the truth and the markdown files are its export."})
		e.ids["decision2"] = res[0].ID
	}},
	{"settle", func(e *parityEnv) { e.ids["probe"] = e.settle() }},
	{"remember-supersedes", func(e *parityEnv) {
		e.export()
		if _, err := memory.RememberSupersedes("project", "Checkpoint replaced by the second parity record",
			e.ids["probe"], e.now()); err != nil {
			e.t.Fatal(err)
		}
	}},
	{"rotate-sessionHistory", func(e *parityEnv) {
		ops := make([]memory.Op, 31) // the cap is 30 entries, so settle moves the oldest to archive/
		for i := range ops {
			n := strconv.Itoa(i)
			ops[i] = memory.Op{Op: "add", Target: "sessionHistory", Title: "Rotation session " + n,
				Text: "Entry " + n + " fills the history past its cap."}
		}
		e.apply(ops...)
		e.settle()
	}},
}

func newParityEnv(t *testing.T, truth string) *parityEnv {
	t.Helper()
	for k, d := range map[string]string{"CLAUDE_CONFIG_DIR": "", "HOME": "", "XDG_CONFIG_HOME": ".config"} {
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv(k, filepath.Join(dir, d))
	}
	t.Setenv("CLAUDE_RECALL_SEMANTIC", "0") // keyword recall only: no model call, no network
	t.Setenv("CLAUDE_SESSION_UPDATE_CHILD", "")
	e := &parityEnv{t: t, truth: truth, ids: map[string]string{}}
	if _, err := memory.Init(); err != nil {
		t.Fatal(err)
	}
	if truth == "db" {
		if err := memory.SetTruth("db"); err != nil {
			t.Fatal(err)
		}
		e.export()
		return e
	}
	pinFilesTruth(t) // a missing truth file reads db here, so the files run pins its own
	return e
}

// now is the script clock: one distinct November day per step, so both truths write the same dates and archive month.
func (e *parityEnv) now() time.Time {
	return time.Date(2026, time.November, 2+e.step, 9, 0, 0, 0, time.UTC)
}

// apply runs a typed batch through the truth's store and requires every op to land.
func (e *parityEnv) apply(ops ...memory.Op) []memory.OpResult {
	e.t.Helper()
	b := memory.Batch{Ops: ops}
	var res []memory.OpResult
	var err error
	if e.truth == "db" {
		res, err = e.applyDB(b)
	} else {
		res, err = memory.ApplyBatch(b, nil, e.now())
	}
	if err != nil {
		e.t.Fatalf("apply under truth=%s: %v", e.truth, err)
	}
	for i, r := range res {
		if r.Outcome != memory.DreamOK {
			e.t.Fatalf("op %d (%s) under truth=%s: %s %s", i, r.Op.Op, e.truth, r.Outcome, r.Reason)
		}
	}
	return res
}

func (e *parityEnv) applyDB(b memory.Batch) ([]memory.OpResult, error) {
	st, closeFn, err := memory.OpenBankStore(false)
	if err != nil {
		return nil, err
	}
	defer closeFn()
	_, base, err := st.Load()
	if err != nil {
		return nil, err
	}
	return st.Apply(b, base, e.now())
}

// read is the content ref ("z" plus 8 hex of the whitespace-folded block) a writer passes as Read for entry id.
func (e *parityEnv) read(file, id string) string {
	e.t.Helper()
	e.export()
	data, err := os.ReadFile(memory.Path(file))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, en := range memory.ParseDoc(file, data).Entries {
		folded := strings.Join(strings.Fields(strings.Join(en.Lines, "\n")), " ")
		if ref := "z" + textutil.SHA256Hex([]byte(folded))[:8]; en.ID == id || ref == id { // an untitled bullet's id is its ref
			return ref
		}
	}
	e.t.Fatalf("%s has no entry %s", file, id)
	return ""
}

// export brings the Markdown files up to date with the db (truth=db only), as settle and the flip do.
func (e *parityEnv) export() {
	e.t.Helper()
	if e.truth != "db" {
		return
	}
	edb, closeDB, err := memory.OpenExportDB()
	if err != nil {
		e.t.Fatal(err)
	}
	rep, err := memory.ExportGuard(edb, paths.MemoryStoreDir())
	if cerr := closeDB(); err == nil {
		err = cerr
	}
	if err != nil {
		e.t.Fatalf("export guard: %v", err)
	}
	if len(rep.Conflicts) > 0 {
		e.t.Fatalf("export guard conflicts: %v", rep.Conflicts)
	}
}

// settle runs the real settle under the truth, through the entry a remember writes, and returns that entry's id.
func (e *parityEnv) settle() string {
	e.t.Helper()
	e.export()
	e.probes++
	id, err := memory.Remember("project", fmt.Sprintf("Checkpoint %d recorded by the parity script", e.probes), e.now())
	if err != nil {
		e.t.Fatalf("settle under truth=%s: %v", e.truth, err)
	}
	return id
}

// bankMarkdown reads the six bank files and archive/*.md; catalog.md and MEMORY.md carry the wall-clock date.
func bankMarkdown(t *testing.T) map[string]string {
	t.Helper()
	root := paths.MemoryStoreDir()
	rels := memory.BankFiles()
	archives, err := filepath.Glob(filepath.Join(root, "archive", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range archives {
		rel, err := filepath.Rel(root, a)
		if err != nil {
			t.Fatal(err)
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	out := map[string]string{}
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = string(data)
	}
	return out
}

func entryIDs(files map[string]string) []string {
	var out []string
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		for i, en := range memory.ParseDoc(rel, []byte(files[rel])).Entries {
			out = append(out, rel+"#"+strconv.Itoa(i)+":"+en.ID)
		}
	}
	return out
}

// shot exports (truth=db), then reads the bank back through every public path.
func (e *parityEnv) shot() parityShot {
	e.t.Helper()
	e.export()
	root := paths.MemoryStoreDir()
	norm := func(s string) string { return strings.ReplaceAll(s, root, "<store>") }
	s := parityShot{files: bankMarkdown(e.t)}
	s.ids = entryIDs(s.files)
	for _, q := range parityQueries {
		s.recall = append(s.recall, q+"\n"+norm(memory.PromptRecall(context.Background(), "", q, io.Discard)))
		var hits []string
		for _, h := range memory.SearchBank(q, 5) {
			hits = append(hits, fmt.Sprintf("%s|%s|%s|%d|%.6f|%s", h.Bank, norm(h.File), h.ID, h.Matched, h.Score, h.Block))
		}
		s.search = append(s.search, q+"\n"+strings.Join(hits, "\n"))
	}
	if e.truth == "db" {
		e.checkDBDocs(s.files)
	}
	return s
}

// checkDBDocs requires the db's own docs to render to exactly the exported bytes of each bank file.
func (e *parityEnv) checkDBDocs(files map[string]string) {
	e.t.Helper()
	st, closeFn, err := memory.OpenBankStore(false)
	if err != nil {
		e.t.Fatal(err)
	}
	defer closeFn()
	docs, _, err := st.Load()
	if err != nil {
		e.t.Fatal(err)
	}
	for _, name := range memory.BankFiles() {
		if want, ok := files[name]; ok && string(docs[name].Render()) != want {
			e.t.Errorf("db doc %s renders differently from its exported file:\ndb:\n%s\nfile:\n%s", name, docs[name].Render(), want)
		}
	}
}

// runParity runs the script under one truth and returns the shot of the initial bank and one per step.
func runParity(t *testing.T, truth string) []parityShot {
	t.Helper()
	e := newParityEnv(t, truth)
	shots := []parityShot{e.shot()}
	for _, st := range parityScript {
		e.step++
		st.run(e)
		shots = append(shots, e.shot())
	}
	if t.Failed() {
		return shots
	}
	last := shots[len(shots)-1]
	if !slices.ContainsFunc(last.recall, func(r string) bool { return strings.Count(r, "\n") > 1 }) ||
		!slices.ContainsFunc(last.search, func(r string) bool { return strings.Contains(r, "|") }) {
		t.Errorf("truth=%s: the final recall or search found nothing, so parity proves nothing", truth)
	}
	if truth == "db" {
		time.Sleep(paths.RacyWindow + 100*time.Millisecond) // row recall declines files written inside the racy window
		if _, ok := recallRows(strings.Fields(parityQueries[0])); !ok {
			t.Error("truth=db: row recall declined a settled bank, so recall was never served from the db")
		}
		if got := e.shot(); !slices.Equal(got.recall, last.recall) || !slices.Equal(got.search, last.search) {
			t.Error("truth=db: recall or search served from db rows differs from the fresh-file answer")
		}
	}
	before := bankMarkdown(t)
	if truth == "files" {
		e.flip("db")
		e.flip("files")
	} else {
		e.flip("files") // leaving db exports under the bank lock
	}
	if after := bankMarkdown(t); !maps.Equal(before, after) {
		t.Errorf("truth=%s: Markdown changed across the flips:\n%s", truth, diffFiles(before, after))
	}
	return shots
}

func (e *parityEnv) flip(to string) {
	e.t.Helper()
	if err := memory.SetTruth(to); err != nil {
		e.t.Fatalf("SetTruth(%s): %v", to, err)
	}
	if got := memory.Truth(); got != to {
		e.t.Fatalf("Truth() = %q after SetTruth(%q)", got, to)
	}
}

func diffFiles(a, b map[string]string) string {
	var sb strings.Builder
	for _, rel := range slices.Sorted(maps.Keys(mergeKeys(a, b))) {
		if a[rel] != b[rel] {
			fmt.Fprintf(&sb, "--- %s\nfirst:\n%s\nsecond:\n%s\n", rel, a[rel], b[rel])
		}
	}
	return sb.String()
}

func mergeKeys(a, b map[string]string) map[string]bool {
	out := map[string]bool{}
	for k := range a {
		out[k] = true
	}
	for k := range b {
		out[k] = true
	}
	return out
}

func diffLists(t *testing.T, what string, files, db []string) {
	t.Helper()
	if slices.Equal(files, db) {
		return
	}
	for i := range max(len(files), len(db)) {
		var f, d string
		if i < len(files) {
			f = files[i]
		}
		if i < len(db) {
			d = db[i]
		}
		if f != d {
			t.Errorf("%s[%d] differs:\ntruth=files: %s\ntruth=db:    %s", what, i, f, d)
		}
	}
}

// TestTruthParityScript runs one script under truth=files and truth=db, then compares every step's read-backs.
func TestTruthParityScript(t *testing.T) {
	var files, db []parityShot
	t.Run("files", func(t *testing.T) { files = runParity(t, "files") })
	t.Run("db", func(t *testing.T) { db = runParity(t, "db") })
	names := []string{"init"}
	for _, st := range parityScript {
		names = append(names, st.name)
	}
	if len(files) != len(names) || len(db) != len(names) {
		t.Fatalf("shots: files %d, db %d, want %d each", len(files), len(db), len(names))
	}
	for i, name := range names {
		t.Run(name, func(t *testing.T) {
			if why, known := parityKnownDiffs[name]; known {
				t.Skip(why)
			}
			if !maps.Equal(files[i].files, db[i].files) {
				t.Errorf("exported Markdown differs:\n%s", diffFiles(files[i].files, db[i].files))
			}
			diffLists(t, "entry ids", files[i].ids, db[i].ids)
			diffLists(t, "recall", files[i].recall, db[i].recall)
			diffLists(t, "search", files[i].search, db[i].search)
		})
	}
}

func pinFilesTruth(t *testing.T) {
	t.Helper()
	tp := filepath.Join(paths.StateDir(), "memory", "truth")
	if err := os.MkdirAll(filepath.Dir(tp), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tp, []byte("files\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
