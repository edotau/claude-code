package waves

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

func TestBackgroundArgsPreserveFlags(t *testing.T) {
	args := []string{"--waves", "plan", "--background=true", "--allow", "Bash(go test *)", "--profiles", "qa", "--profiles", "prod", "--format=json"}
	want := append([]string{"agents", "dispatch"}, args...)
	want = append(want, "--background=false", "--job", "id")
	if got := backgroundArgs(args, "id"); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestBackgroundJobSettlesRecord(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	for code := 0; code <= 4; code++ {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			rec := agent.ClaimRecord("waves", agent.RunOptions{Prompt: "plan"}, time.Now())
			rec.Log = rec.LogPath()
			if err := rec.Save(); err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			got := runJob(context.Background(), Options{Plan: "plan", Out: &log, Err: &log}, rec.ID,
				func(_ context.Context, o Options) int {
					running, err := agent.LoadRecord(rec.ID)
					if err != nil || running.PID != os.Getpid() {
						t.Fatalf("running=%+v err=%v", running, err)
					}
					fmt.Fprintln(o.Out, "wave report")
					return code
				})
			checkSettled(t, rec.ID, code, got, log.String())
		})
	}
}

// settledWant is the record status and error text a background job leaves for exit code code.
func settledWant(code int) (status, message string) {
	switch code {
	case 0:
		return "ok", ""
	case 4:
		return "error", "interrupted"
	}
	return "error", fmt.Sprintf("exit %d", code)
}

// checkSettled asserts the record, log tail and wait exit a background job leaves for exit code code.
func checkSettled(t *testing.T, id string, code, got int, log string) {
	t.Helper()
	settled, err := agent.LoadRecord(id)
	if err != nil {
		t.Fatal(err)
	}
	want, message := settledWant(code)
	if got != code || settled.Status != want || settled.Error != message {
		t.Fatalf("exit=%d record=%+v", got, settled)
	}
	if !strings.HasPrefix(log, "wave report\n") || (message != "" && !strings.HasSuffix(log, message+"\n")) {
		t.Fatalf("log=%q", log)
	}
	rows, err := agent.Wait(context.Background(), []string{id}, time.Second, time.Millisecond)
	if err != nil || agent.WaitExit(rows) != min(code, 1) {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestBackgroundJobInterruptOverridesSuccess(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	rec := agent.ClaimRecord("waves", agent.RunOptions{Prompt: "plan"}, time.Now())
	if err := rec.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var log bytes.Buffer
	code := runJob(ctx, Options{Plan: "plan", Out: &log, Err: &log}, rec.ID, func(context.Context, Options) int { cancel(); return 0 })
	settled, err := agent.LoadRecord(rec.ID)
	if err != nil || code != 4 || settled.Status != "error" || settled.Error != "interrupted" {
		t.Fatalf("code=%d rec=%+v err=%v", code, settled, err)
	}
}
