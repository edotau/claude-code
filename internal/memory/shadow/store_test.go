package shadow

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/memory"
)

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
