package ask

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/job"
	"github.com/edotau/claude-code/internal/hosttest"
)

func TestAskRetryPreservesRecordedLead(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := job.NewRecord("claude", agent.RunOptions{Prompt: "say ok", Lead: "recorded-profile"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, flag, wantLead, wantRoute string
	}{
		{"recorded lead", "", "recorded-profile", ""},
		{"profile override", "--profile", "", "replacement-profile"},
		{"provider override", "--provider", "", "replacement-profile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []string{"--retry", rec.ID}
			if tc.flag != "" {
				args = append(args, tc.flag, "replacement-profile")
			}
			fs, f := askFlagSet()
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			r := &askRun{fs: fs, f: f}
			if code, done := loadAskRetry(fs, args, f.retry, f.format, r.askState(), &r.prompt); done || code != 0 {
				t.Fatalf("loadAskRetry = %d, %v", code, done)
			}
			if got := r.request(r.prompt); got.Lead != tc.wantLead {
				t.Fatalf("replayed lead = %q, want %q", got.Lead, tc.wantLead)
			}
			if got := f.provider + f.leadProvider; got != tc.wantRoute {
				t.Fatalf("explicit route = %q, want %q", got, tc.wantRoute)
			}
		})
	}
}

func TestAskJSONStdoutFailureExitsNonzero(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	args := []string{"--agent", "claude", "--fallback", "none", "--format", "json", "say hello"}
	if code, out := captureAskStdout(t, args...); code != 0 {
		t.Fatalf("healthy stdout: exit %d, output %q", code, out)
	}
	full, err := os.OpenFile("/dev/full", os.O_WRONLY, 0)
	if os.IsNotExist(err) {
		t.Skip("/dev/full is unavailable")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := full.Close(); err != nil {
			t.Error(err)
		}
	})
	code, _, stderr := hosttest.CaptureOutput(t, func() int {
		previous := os.Stdout
		os.Stdout = full
		defer func() { os.Stdout = previous }()
		return Cmd(args)
	})
	if code != 1 || !strings.Contains(stderr, "no space left on device") {
		t.Fatalf("full stdout: exit %d, stderr %q; want exit 1 reporting the write error", code, stderr)
	}
	if records := job.RecentRecords(1); len(records) != 1 || records[0].Status != "ok" {
		t.Fatalf("dispatch must succeed independently of stdout: %+v", records)
	}
}

func TestFollowerParseErrorUsesPhysicalLine(t *testing.T) {
	for _, bad := range []string{`{not json`, `{"unknown":true}`, `{"id":"a"} }`} {
		t.Run(bad, func(t *testing.T) {
			w, path := startFollower(t, strings.Repeat("\n", 17), time.Minute)
			t.Cleanup(func() {
				if err := w.f.Close(); err != nil {
					t.Error(err)
				}
			})
			appendFile(t, path, "\n\n"+bad+"\n")
			lines, _, err := w.next()
			if err != nil || len(lines) != 1 {
				t.Fatalf("next = %+v, %v; want one malformed appended line", lines, err)
			}
			if got := lines[0]; got.N != 20 || got.Err == nil || !strings.HasPrefix(got.Err.Error(), "line 20: ") {
				t.Fatalf("appended line = %+v; want physical line 20 in both N and the parse error", got)
			}
		})
	}
}
