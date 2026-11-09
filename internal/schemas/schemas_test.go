package schemas

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type rec struct {
	Schema int    `json:"schema"`
	Name   string `json:"name"`
}

func hermetic(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	return dir
}

func ledgerLines(t *testing.T, format string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(LedgerPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("ledger line not JSON: %q: %v", l, err)
		}
		if m["format"] == format {
			out = append(out, m)
		}
	}
	return out
}

func TestDecodeV0ThroughSameShapeReportsFromZero(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-v0", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	from, err := Decode(f, []byte(`{"name":"a"}`), &got)
	if err != nil {
		t.Fatal(err)
	}
	if from != 0 || got.Name != "a" {
		t.Fatalf("from=%d got=%+v, want from=0 name=a", from, got)
	}
}

func TestDecodeNewerRecordReturnsErrNewer(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-newer", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x", Migrate: SameShape})
	var got rec
	if _, err := Decode(f, []byte(`{"schema":2,"name":"a"}`), &got); !errors.Is(err, ErrNewer) {
		t.Fatalf("err=%v, want ErrNewer", err)
	}
}

func TestDecodeRejectsNegativeSchemaBeforeMigration(t *testing.T) {
	calls := 0
	f := Format{Name: "negative-schema", Version: 1, Migrate: func(_ int, _ []byte) ([]byte, error) {
		calls++
		return nil, errors.New("unexpected migration")
	}}
	var got rec
	_, err := Decode(f, []byte(`{"schema":-2147483648}`), &got)
	if calls != 0 {
		t.Fatalf("negative schema invoked migration %d times, want 0", calls)
	}
	if err == nil || !strings.Contains(err.Error(), "negative schema version -2147483648") {
		t.Fatalf("Decode error = %v, want negative schema version rejection", err)
	}
}

func TestDecodeOlderWithoutMigrateReturnsErrNoMigration(t *testing.T) {
	hermetic(t)
	f := reg(t, Format{Name: "t-decode-nomig", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"})
	var got rec
	if _, err := Decode(f, []byte(`{"name":"a"}`), &got); !errors.Is(err, ErrNoMigration) {
		t.Fatalf("err=%v, want ErrNoMigration", err)
	}
}

// reg registers f for one test and drops the row at cleanup, so -count>1 re-registers cleanly.
func reg(t *testing.T, f Format) Format {
	t.Helper()
	t.Cleanup(func() { mu.Lock(); delete(rows, f.Name); mu.Unlock() })
	return Register(f)
}

func TestRegisterPanicsOnDuplicateName(t *testing.T) {
	f := Format{Name: "t-dup", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return "x" }, New: func() any { return &rec{} }, Clean: "x"}
	reg(t, f)
	defer func() {
		if recover() == nil {
			t.Fatal("second Register of the same Name did not panic")
		}
	}()
	Register(f)
}

func TestLedgerAppendsOneJSONLineMode0600(t *testing.T) {
	hermetic(t)
	LogMigration("t-ledger", 0, 1, 3)
	lines := ledgerLines(t, "t-ledger")
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	m := lines[0]
	if m["schema"] != float64(1) || m["from"] != float64(0) || m["to"] != float64(1) || m["records"] != float64(3) {
		t.Fatalf("line = %v", m)
	}
	if at, _ := m["at"].(string); at == "" {
		t.Fatalf("missing at: %v", m)
	}
	st, err := os.Stat(LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", st.Mode().Perm())
	}
}

func writeN(t *testing.T, dir, prefix string, n int, body string) []string {
	t.Helper()
	var paths []string
	for i := range n {
		p := filepath.Join(dir, fmt.Sprintf("%s%d.json", prefix, i))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	return paths
}

func stamp(from int, raw []byte) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["schema"] = from + 1
	return json.Marshal(m)
}

func migrationFiles(t *testing.T) (rw, ro []string) {
	t.Helper()
	cfg := hermetic(t)
	dir := filepath.Join(cfg, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rwDir, roDir := filepath.Join(dir, "rw"), filepath.Join(dir, "ro")
	for _, d := range []string{rwDir, roDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rw = writeN(t, rwDir, "r", 2, `{"name":"a"}`)
	ro = writeN(t, roDir, "r", 1, `{"name":"b"}`)
	reg(t, Format{Name: "t-mig-rewrite", Owner: "test", Kind: "record", Version: 1, Rewrite: true,
		Glob: func() string { return filepath.Join(rwDir, "*.json") }, New: func() any { return &rec{} },
		Clean: "x", Migrate: stamp})
	reg(t, Format{Name: "t-mig-readonly", Owner: "test", Kind: "record", Version: 1,
		Glob: func() string { return filepath.Join(roDir, "*.json") }, New: func() any { return &rec{} },
		Clean: "x", Migrate: stamp})
	return rw, ro
}

func TestPendingAndMigrateAllSpanFormats(t *testing.T) {
	rw, ro := migrationFiles(t)
	if !t.Run("pending", func(t *testing.T) {
		if p := fmt.Sprintf("%+v", Pending()); !strings.Contains(p, "t-mig-rewrite") || !strings.Contains(p, "t-mig-readonly") {
			t.Fatalf("Pending misses old-version files: %s", p)
		}
	}) {
		return
	}
	if !t.Run("dry run", func(t *testing.T) {
		checkMigrationDryRun(t, append(append([]string{}, rw...), ro...))
	}) {
		return
	}
	t.Run("rewrite", func(t *testing.T) {
		checkMigrationRewrite(t, rw, ro)
	})
}

func TestMigrateAllLedgerCountsLogRecords(t *testing.T) {
	dir := hermetic(t)
	path := filepath.Join(dir, "records.jsonl")
	const staleRecords = 100
	body := strings.Repeat("{\"name\":\"stale\"}\n", staleRecords) + "{\"schema\":1,\"name\":\"current\"}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	f := reg(t, Format{Name: "t-mig-log-count", Owner: "test", Kind: Log, Version: 1, Rewrite: true,
		Glob: func() string { return path }, Clean: "x", Migrate: SameShape})
	if _, err := MigrateAll(false); err != nil {
		t.Fatal(err)
	}
	lines := ledgerLines(t, f.Name)
	if len(lines) != 1 {
		t.Fatalf("migration ledger has %d rows, want 1", len(lines))
	}
	if got := lines[0]["records"]; got != float64(staleRecords) {
		t.Fatalf("migration ledger records = %v, want %d stale records", got, staleRecords)
	}
}

func migrationSnapshot(files []string) string {
	var sb strings.Builder
	for _, p := range files {
		b, _ := os.ReadFile(p)
		sb.Write(b)
	}
	return sb.String()
}

func checkMigrationDryRun(t *testing.T, files []string) {
	t.Helper()
	before := migrationSnapshot(files)

	if _, err := MigrateAll(true); err != nil {
		t.Fatal(err)
	}
	if migrationSnapshot(files) != before {
		t.Fatal("dry run changed files on disk")
	}
	if _, err := os.Stat(LedgerPath()); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote the ledger (stat err=%v)", err)
	}
}

func checkMigrationRewrite(t *testing.T, rw, ro []string) {
	t.Helper()
	if _, err := MigrateAll(false); err != nil {
		t.Fatal(err)
	}
	for _, p := range rw {
		b, _ := os.ReadFile(p)
		if !strings.Contains(string(b), `"schema":1`) {
			t.Fatalf("%s not rewritten: %s", p, b)
		}
	}
	if b, _ := os.ReadFile(ro[0]); string(b) != `{"name":"b"}` {
		t.Fatalf("non-Rewrite row was touched: %s", b)
	}
	if l := ledgerLines(t, "t-mig-rewrite"); len(l) != 1 || l[0]["records"] != float64(2) || l[0]["from"] != float64(0) {
		t.Fatalf("rewrite ledger = %v, want one line from=0 records=2", l)
	}
	if l := ledgerLines(t, "t-mig-readonly"); len(l) != 0 {
		t.Fatalf("non-Rewrite row logged: %v", l)
	}
}

// TestDecodeNullIsErrorNotPanic: a `null` document is rejected, never a nil-map panic.
func TestDecodeNullIsErrorNotPanic(t *testing.T) {
	f := Format{Name: "null-doc", Version: 1, Migrate: SameShape}
	var got map[string]any
	if _, err := Decode(f, []byte("null"), &got); err == nil {
		t.Fatal("Decode(null) returned nil error")
	}
	if _, err := SameShape(0, []byte("null")); err == nil {
		t.Fatal("SameShape(0, null) returned nil error")
	}
}
