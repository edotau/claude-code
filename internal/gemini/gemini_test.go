package gemini

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

// D6: agy's model is flag > process env > the GEMINI_MODEL vault pin > agy's default.
func TestAgyModelPrecedence(t *testing.T) {
	vaulttest.Use(t)
	for _, k := range modelEnvKeys {
		t.Setenv(k, "")
	}
	reg, err := providers.Load()
	if err != nil {
		t.Fatal(err)
	}
	sel, _ := providers.Select(reg, "")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != AgyDefaultModel {
		t.Errorf("nothing set = %q, want the default %q", got, AgyDefaultModel)
	}
	vaulttest.SetRecord(t, "gemini", "model", "gemini-pinned")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-pinned" {
		t.Errorf("vault pin = %q, want gemini-pinned", got)
	}
	t.Setenv("GEMINI_MODEL", "gemini-env")
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], ""); got != "gemini-env" {
		t.Errorf("process env = %q, want it to beat the pin", got)
	}
	if got := geminiAgyModel(reg, sel, reg.Providers["anthropic"], "gemini-flag"); got != "gemini-flag" {
		t.Errorf("flag = %q, want it to beat env and pin", got)
	}
}

// agyArgv maps Autonomous onto "--mode auto"; --dangerously-skip-permissions rides on Tools alone, so
// an autonomous-but-tool-less run must never carry it (agy must never auto-grant write access it wasn't
// asked for).
func TestAgyArgvAutonomous(t *testing.T) {
	cases := []struct {
		name               string
		tools, autonomous  bool
		wantMode, wantSkip bool
	}{
		{"neither", false, false, false, false},
		{"tools only", true, false, false, true},
		{"autonomous only", false, true, true, false},
		{"both", true, true, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", Tools: c.tools, Autonomous: c.autonomous})
			hasMode := strings.Contains(strings.Join(argv, " "), "--mode auto")
			hasSkip := slices.Contains(argv, "--dangerously-skip-permissions")
			if hasMode != c.wantMode || hasSkip != c.wantSkip {
				t.Errorf("argv = %q, mode=%v skip=%v, want mode=%v skip=%v", argv, hasMode, hasSkip, c.wantMode, c.wantSkip)
			}
		})
	}
}

// AgyRequest.SessionID rides as "--conversation <id>"; an empty SessionID must never emit the flag
// (agy has no bare --conversation value to resume against).
func TestAgyArgvConversation(t *testing.T) {
	cases := []struct{ name, session string }{{"unset", ""}, {"set", "conv-123"}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			argv := agyArgv(agyRequest{Model: "m", SessionID: c.session})
			idx := slices.Index(argv, "--conversation")
			if has := idx >= 0; has != (c.session != "") {
				t.Fatalf("argv = %q, --conversation present = %v, want %v", argv, has, c.session != "")
			}
			if c.session != "" && argv[idx+1] != c.session {
				t.Errorf("argv = %q, --conversation value = %q, want %q", argv, argv[idx+1], c.session)
			}
		})
	}
}

func TestAgyArgvDebugFile(t *testing.T) {
	if slices.Contains(agyArgv(agyRequest{Model: "m"}), "--log-file") {
		t.Fatal("--log-file without DebugFile")
	}
	argv := agyArgv(agyRequest{Model: "m", DebugFile: "/tmp/l"})
	if i := slices.Index(argv, "--log-file"); i < 0 || argv[i+1] != "/tmp/l" {
		t.Fatalf("argv = %v", argv)
	}
}

func TestGeminiCanceledContext(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.go"), "package a")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, dirs := range [][]string{nil, {"."}} {
		got, err := CollectContext(ctx, dir, dirs, []string{"*.go"}, 40, 100)
		if !errors.Is(err, context.Canceled) || len(got.Included) != 0 {
			t.Errorf("CollectContext(%v) = %+v, %v, want cancellation without files", dirs, got, err)
		}
	}
	if _, err := ChangedFilesVsHEAD(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("ChangedFilesVsHEAD error = %v, want cancellation", err)
	}
	if _, err := diffFiles(ctx, GeminiOptions{Cwd: dir, Diff: true}); !errors.Is(err, context.Canceled) {
		t.Errorf("diffFiles error = %v, want cancellation", err)
	}
}

func TestGeminiGitCommandCancellation(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH")
	}
	for _, tc := range []struct {
		name  string
		stage string
		run   func(context.Context, string) (Context, error)
	}{
		{"collect probe", "rev-parse", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"collect list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			return CollectContext(ctx, dir, []string{"."}, nil, 40, 100)
		}},
		{"changed diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"changed list", "ls-files", func(ctx context.Context, dir string) (Context, error) {
			_, err := ChangedFilesVsHEAD(ctx, dir)
			return Context{}, err
		}},
		{"bridge diff", "diff", func(ctx context.Context, dir string) (Context, error) {
			_, err := diffFiles(ctx, GeminiOptions{Cwd: dir, Diff: true})
			return Context{}, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			marker := filepath.Join(dir, "git-started")
			write(t, filepath.Join(dir, ".gitignore"), "ignored.txt\n")
			write(t, filepath.Join(dir, "ignored.txt"), "must not be inlined")
			script := "#!/bin/sh\n" +
				"if [ \"$3\" = \"" + tc.stage + "\" ]; then\n" +
				"  : > \"$GEMINI_TEST_GIT_MARKER\"\n" +
				"  exec \"" + sleep + "\" 3\n" +
				"fi\n" +
				"case \"$3\" in\n" +
				"  rev-parse) printf 'true\\n' ;;\n" +
				"  diff) printf 'changed.go\\n' ;;\n" +
				"  ls-files) printf 'tracked.go\\000' ;;\n" +
				"  *) exit 1 ;;\n" +
				"esac\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			t.Setenv("GEMINI_TEST_GIT_MARKER", marker)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			type result struct {
				context Context
				err     error
			}
			done := make(chan result, 1)
			go func() {
				got, err := tc.run(ctx, dir)
				done <- result{got, err}
			}()
			deadline := time.NewTimer(2 * time.Second)
			defer deadline.Stop()
			tick := time.NewTicker(5 * time.Millisecond)
			defer tick.Stop()
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				select {
				case <-tick.C:
				case got := <-done:
					t.Fatalf("git returned before synchronization: %+v, %v", got.context, got.err)
				case <-deadline.C:
					t.Fatal("git did not reach the cancellation synchronization marker")
				}
			}
			canceledAt := time.Now()
			cancel()
			select {
			case got := <-done:
				if !errors.Is(got.err, context.Canceled) {
					t.Errorf("error = %v, want cancellation", got.err)
				}
				if len(got.context.Included) != 0 {
					t.Errorf("canceled scan included files: %+v", got.context.Included)
				}
				if elapsed := time.Since(canceledAt); elapsed > time.Second {
					t.Errorf("git took %s to stop after cancellation", elapsed)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("git command did not stop after cancellation")
			}
		})
	}
}

func TestCollectContextGitFailureFallback(t *testing.T) {
	for _, stage := range []string{"rev-parse", "ls-files"} {
		t.Run(stage, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			write(t, filepath.Join(dir, "a.go"), "package a")
			script := "#!/bin/sh\nif [ \"$3\" = \"" + stage + "\" ]; then exit 1; fi\nprintf 'true\\n'\n"
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin)
			got, err := CollectContext(t.Context(), dir, []string{"."}, nil, 40, 100)
			if err != nil || len(got.Included) != 1 || got.Included[0].Path != "a.go" {
				t.Fatalf("CollectContext = %+v, %v, want ordinary git failure fallback", got, err)
			}
		})
	}
}

func TestChangedFilesCancellationMentionsGit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := ChangedFilesVsHEAD(ctx, t.TempDir())
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "git") {
		t.Fatalf("error = %v, want wrapped git cancellation", err)
	}
}

// A bridge dispatch on the agy backend leaves a 0600 <id>.log (record.Log) holding the redacted agy output.
func TestBridgeLogCapturesRedactedAgyOutput(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("HOME", loginHome(t, true))
	script := "cat > /dev/null\n" +
		"echo 'agy-stderr-line' >&2\n" +
		"echo 'export AWS_SECRET=AKIAIOSFODNN7EXAMPLE' >&2\n" +
		`printf '%s\n' '{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"bridge-answer","num_turns":1}}'` + "\n"
	pathstest.FakeVendor(t, "agy", script)

	res, err := RunGemini(context.Background(), GeminiOptions{Cwd: t.TempDir(), Task: "t"}, io.Discard, io.Discard)
	if err != nil || res.Answer != "bridge-answer" {
		t.Fatalf("Run = (%q, %v), want (bridge-answer, nil)", res.Answer, err)
	}
	recs := dispatchRecords(t)
	if len(recs) != 1 {
		t.Fatalf("dispatch records = %d, want 1", len(recs))
	}
	logPath := recs[0].Log
	if logPath == "" {
		t.Fatal("record.Log is empty: the bridge dispatch did not record a log path")
	}
	if logPath != recs[0].LogPath() {
		t.Errorf("record.Log = %q, want %q", logPath, recs[0].LogPath())
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("log file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log mode = %o, want 0600", perm)
	}
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"agy-stderr-line", "bridge-answer", "AWS_SECRET=REDACTED"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("log leaks an unredacted secret:\n%s", got)
	}
	if n := strings.Count(got, "agy-stderr-line"); n != 1 {
		t.Errorf("stderr line appears %d times, want exactly once:\n%s", n, got)
	}
}
