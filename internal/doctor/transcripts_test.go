package doctor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/hosttest"
	secretredact "github.com/edotau/claude-code/internal/redact"
	"github.com/edotau/claude-code/internal/vault"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestTranscriptFix(t *testing.T) {
	files, original, young, outside := transcriptFixture(t)
	report := transcriptRun(t, 1, "--secrets", "--since", "1")
	if !strings.Contains(report, "matches 12") {
		t.Fatal("report missed encoded matches")
	}
	transcriptUnchanged(t, append(files, young, outside), original)
	fixed := transcriptRun(t, 0, "--secrets", "--fix", "--since", "1")
	for _, want := range []string{"masked files 3, matches masked 9", "temporary task outputs: scanned 1, with matches 1, matches 3, masked files 1", "skipped: not idle 1", "skipped: symlink or outside root 1", "skipped: not yet enabled 1"} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("missing count: %s", want)
		}
	}
	transcriptMasked(t, files, original)
	transcriptUnchanged(t, []string{young, outside}, original)
	again := transcriptRun(t, 0, "--secrets", "--fix", "--since", "1")
	if strings.Contains(again, "masked files 1") || strings.Contains(again, "masked files 3") {
		t.Fatal("repeat changed files")
	}
	invalid := filepath.Join(filepath.Dir(young), "invalid.jsonl")
	transcriptWrite(t, invalid, []byte(transcriptFake), 20*time.Minute)
	result := transcriptRun(t, 0, "--secrets", "--fix", invalid)
	if !strings.Contains(result, "skipped: would not stay valid 1") {
		t.Fatal("missing invalid-file skip")
	}
	transcriptUnchanged(t, []string{invalid}, []byte(transcriptFake))
}

const transcriptFake = "FAKE-SECRET-0123456789abcdef"

func transcriptFixture(t *testing.T) ([]string, []byte, string, string) {
	t.Helper()
	cfg := resolvedPath(vaulttest.UseFreshKey(t))
	home := t.TempDir()
	t.Setenv("HOME", home)
	tmp := t.TempDir() // unresolved: macOS /var is a link, and the store under it must still be fixed
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Chdir(doctorGitFixture(t, false))
	escaped := "FAKE-SECRET-0123456789ab\"cd\\ef"
	for name, value := range map[string]string{"FAKE_TOKEN": transcriptFake, "FAKE_ESCAPED": escaped} {
		if err := vault.Store(name, value); err != nil {
			t.Fatal(err)
		}
	}
	b, err := json.Marshal(map[string]string{"raw": transcriptFake, "escaped": escaped, "base64": base64.StdEncoding.EncodeToString([]byte(transcriptFake))})
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	var files []string
	for _, rel := range []string{"projects/p/s.jsonl", "projects/p/s/subagents/agent-a.jsonl", "projects/p/s/tool-results/a.txt", "tasks/a.output", "state/harness/dispatch/a.log", "state/harness/results/a.txt", "memory/reports/a.md", "history.jsonl", "state/harness/proxy.log"} {
		path := filepath.Join(cfg, rel)
		transcriptWrite(t, path, b, 20*time.Minute)
		files = append(files, path)
	}
	task := filepath.Join(tmp, fmt.Sprintf("claude-%d", os.Getuid()), "p", "tasks", "a.output")
	transcriptWrite(t, task, b, 20*time.Minute)
	files = append(files, task)
	young := filepath.Join(cfg, "projects/p/young.jsonl")
	transcriptWrite(t, young, b, time.Minute)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	transcriptWrite(t, outside, b, 20*time.Minute)
	if err := os.Symlink(outside, filepath.Join(cfg, "projects/p/link.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".codex/sessions/a.jsonl", ".copilot/session-state/a/events.jsonl"} {
		transcriptWrite(t, filepath.Join(home, rel), b, 20*time.Minute)
	}
	return files, b, young, outside
}

func transcriptWrite(t *testing.T, path string, b []byte, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-age)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func transcriptRun(t *testing.T, want int, args ...string) string {
	t.Helper()
	code, out, stderr := hosttest.CaptureOutput(t, func() int { return Run(args) })
	if code != want {
		t.Fatalf("exit=%d want=%d stderr-bytes=%d", code, want, len(stderr))
	}
	if strings.Contains(out+stderr, transcriptFake) {
		t.Fatal("value leaked")
	}
	return out
}

func transcriptUnchanged(t *testing.T, files []string, want []byte) {
	t.Helper()
	for _, path := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatal("file unexpectedly changed")
		}
	}
}

func transcriptMasked(t *testing.T, files []string, original []byte) {
	t.Helper()
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != len(original) || bytes.Count(b, []byte("[redacted]")) != 3 {
			t.Fatal("size or masking assertion failed")
		}
		if bytes.Contains(b, []byte("FAKE-SECRET")) || bytes.Contains(b, []byte(base64.StdEncoding.EncodeToString([]byte(transcriptFake)))) {
			t.Fatal("planted value remains")
		}
		for line := range bytes.SplitSeq(bytes.TrimSpace(b), []byte{'\n'}) {
			if !json.Valid(line) {
				t.Fatal("invalid JSON after masking")
			}
		}
	}
}

func TestTranscriptWindow(t *testing.T) {
	cfg := resolvedPath(vaulttest.UseFreshKey(t))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	path := filepath.Join(cfg, "projects", "old.jsonl")
	transcriptWrite(t, path, []byte("sk-"+strings.Repeat("A", 24)), 8*24*time.Hour)
	_, opts := doctorFlags()
	if opts.days != 7 {
		t.Fatal("default window is not seven days")
	}
	var report bytes.Buffer
	if code := transcriptAudit(&report, false, nil, opts.days); code != 0 {
		t.Fatalf("default exit %d", code)
	}
	if !strings.Contains(report.String(), "claude projects: scanned 0") || !strings.Contains(report.String(), "older (include with --days 0) 1") {
		t.Fatal("old transcript not explicitly skipped")
	}
	report.Reset()
	if code := transcriptAudit(&report, false, nil, 0); code != 1 {
		t.Fatalf("all ages exit %d", code)
	}
	if !strings.Contains(report.String(), "claude projects: scanned 1, with matches 1, matches 1") {
		t.Fatal("all ages did not read old transcript")
	}
}

// The report counts only credential records, the set the hooks mask, and breaks matches down by kind and needle.
func TestTranscriptBreakdown(t *testing.T) {
	cfg := resolvedPath(vaulttest.UseFreshKey(t))
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	const listed = "databricks-fake-model-id"
	vaulttest.Secret(t, "FAKE_TOKEN", transcriptFake)
	if err := vault.UpdateSealed(func(tx *vault.Tx, key *vault.Key) error {
		return tx.PutSealed(key, "model-cache", `{"models":["`+listed+`"]}`, "cache", time.Now())
	}); err != nil {
		t.Fatal("seal")
	}
	transcriptWrite(t, filepath.Join(cfg, "projects", "s.jsonl"), []byte(listed+" "+transcriptFake+" "+listed+"\n"), time.Hour)
	var report bytes.Buffer
	transcriptAudit(&report, false, nil, 0)
	for _, want := range []string{"claude projects: scanned 1, with matches 1, matches 1,", "vault value: 1 from 1 needles",
		fmt.Sprintf(`FAKE_TOKEN "" raw %d B: 1`, len(transcriptFake))} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report lacks %q", want)
		}
	}
}

func TestTranscriptCountBoundary(t *testing.T) {
	m := secretredact.FromValues([]string{transcriptFake})
	counter := secretredact.NewCounter(m)
	forms := []string{
		transcriptFake, base64.StdEncoding.EncodeToString([]byte(transcriptFake)),
		"sk-" + strings.Repeat("A", 64), "BeArEr " + strings.Repeat("A", 64),
		"eyJ" + strings.Repeat("a", 20) + "." + strings.Repeat("b", 20),
		"-----BEGIN PRI" + "VATE KEY-----\n" + strings.Repeat("A", 2*secretredact.CountWindow) + "\n-----END PRIVATE KEY-----",
	}
	for i, form := range forms {
		for _, delta := range []int{-20, -2, 0, 2} {
			input := append(bytes.Repeat([]byte(" "), secretredact.CountWindow+delta), []byte(form)...)
			input = append(input, '\n')
			want := m.Mask(bytes.Clone(input))
			if want != 1 {
				t.Fatal("fixture must contain exactly one match")
			}
			path := filepath.Join(t.TempDir(), "boundary.jsonl")
			transcriptWrite(t, path, input, time.Hour)
			got, err := counter.File(path)
			if err != nil || got != want {
				t.Fatalf("form %d delta %d: count %d want %d error %v", i, delta, got, want, err)
			}
		}
	}
}

func TestTranscriptCountPrefilter(t *testing.T) {
	m := secretredact.FromValues([]string{transcriptFake, "Abcdefghijkl0123456789"})
	counter := secretredact.NewCounter(m)
	forms := []string{"ghp_a", "xoxb-a", "ordinary prose", transcriptFake, "Bearer " + transcriptFake,
		"sk-" + strings.Repeat("a", 24), "dapi" + strings.Repeat("a", 32),
		"nvapi-" + strings.Repeat("A", 24), "ghp_" + strings.Repeat("A", 28),
		"github_pat_" + strings.Repeat("A", 22), "AIza" + strings.Repeat("A", 35),
		"tvly-" + strings.Repeat("A", 16), "gsk_" + strings.Repeat("A", 24),
		"AKIA" + strings.Repeat("A", 16), "ASIA" + strings.Repeat("A", 16),
		"xoxb-" + strings.Repeat("A", 12), "[redacted]************************",
		transcriptFake + transcriptFake, "YOUR_sk-" + strings.Repeat("A", 24), "Bearer\t" + transcriptFake + strings.Repeat("*", 50),
	}
	for i, form := range forms {
		path := filepath.Join(t.TempDir(), "candidate.txt")
		input := []byte(form)
		transcriptWrite(t, path, input, time.Hour)
		want := m.Mask(bytes.Clone(input))
		got, err := counter.File(path)
		if err != nil || got != want {
			t.Fatalf("case %d: count %d want %d error %v", i, got, want, err)
		}
	}
}

func TestTranscriptCountParity(t *testing.T) {
	values := []string{transcriptFake, transcriptFake[10:] + "TAIL", "ghp_Ab12", "Énergie-0123456789", "\xffAbcdefghijkl012"}
	m := secretredact.FromValues(values)
	counter := secretredact.NewCounter(m)
	random := rand.New(rand.NewPCG(1, 2))
	pieces := []string{"Énergie-0123456789", "\xffAbcdefghijkl012", "plain ", "ghp_a", "xoxb-a", "[redacted]******", "ghp_Ab12", transcriptFake,
		transcriptFake[10:] + "TAIL", "Bearer " + strings.Repeat("Ab1_", 12),
		"sk-" + strings.Repeat("a", 24), "github_pat_", "YOUR_sk-" + strings.Repeat("a", 24),
		"-----BEGIN PRI" + "VATE KEY-----\nx\n-----END PRIVATE KEY-----", "\n", " ", "_", "."}
	path := filepath.Join(t.TempDir(), "parity.txt")
	for i := range 200 {
		var input []byte
		for range 20 {
			input = append(input, pieces[random.IntN(len(pieces))]...)
		}
		transcriptWrite(t, path, input, time.Hour)
		want := m.Mask(bytes.Clone(input))
		got, err := counter.File(path)
		if err != nil || got != want {
			t.Fatalf("case %d count %d want %d error %v", i, got, want, err)
		}
	}
}

func BenchmarkTranscriptStore(b *testing.B) {
	for _, count := range []int{1, 500} {
		for _, streaming := range []bool{false, true} {
			b.Run(fmt.Sprintf("files=%d/stream=%t", count, streaming), func(b *testing.B) {
				benchmarkTranscriptStore(b, count, streaming)
			})
		}
	}
}

func benchmarkTranscriptStore(b *testing.B, count int, streaming bool) {
	size := 400 << 10
	if count == 1 {
		size = 300 << 20
	}
	files := transcriptBenchmarkFiles(b, count, size)
	m := secretredact.FromValues([]string{transcriptFake})
	counter := secretredact.NewCounter(m)
	b.SetBytes(int64(count * size))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		for _, path := range files {
			var err error
			if streaming {
				_, err = counter.File(path)
			} else {
				_, err = transcriptFile(filepath.Dir(path), path, m, false)
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func transcriptBenchmarkFiles(b *testing.B, count, size int) []string {
	b.Helper()
	root := b.TempDir()
	block := bytes.Repeat([]byte("ordinary prose. "), 4096)
	files := make([]string, count)
	for i := range count {
		path := filepath.Join(root, fmt.Sprintf("%d.txt", i))
		f, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		for written := 0; written < size; {
			n, err := f.Write(block[:min(len(block), size-written)])
			if err != nil {
				b.Fatal(err)
			}
			written += n
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		files[i] = path
	}
	return files
}
