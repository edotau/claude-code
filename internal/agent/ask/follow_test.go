package ask

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
)

// startFollower writes initial to a file, opens a follower with idle and returns it with the file path.
func startFollower(t *testing.T, initial string, idle time.Duration) (*follower, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tasks.jsonl")
	if err := os.WriteFile(p, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	w, _, err := newFollower(p, idle)
	if err != nil {
		t.Fatal(err)
	}
	return w, p
}

func appendFile(t *testing.T, p, s string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

// ids builds a task whose ID is the line's task id.
func ids(l parsedLine) batchTask { return batchTask{ID: l.Task.ID} }

// drain collects task ids until the channel closes; false when it stays open past limit.
func drain(ch <-chan batchTask, limit time.Duration) (got []string, closed bool, took time.Duration) {
	start := time.Now()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	for {
		select {
		case task, ok := <-ch:
			if !ok {
				return got, true, time.Since(start)
			}
			got = append(got, task.ID)
		case <-timer.C:
			return got, false, time.Since(start)
		}
	}
}

func TestBatchFollowWaitsForInitialTask(t *testing.T) {
	for _, initial := range []string{"", `{"id":"a","prompt":"p"}`} {
		t.Run(initial, func(t *testing.T) {
			cliEnv(t)
			cliFakes(t)
			p := filepath.Join(t.TempDir(), "tasks.jsonl")
			if err := os.WriteFile(p, []byte(initial), 0o600); err != nil {
				t.Fatal(err)
			}
			tail := "\n{\"end\":true}\n"
			if initial == "" {
				tail = `{"id":"a","prompt":"p"}` + tail
			}
			envs := envelopes(t, followCLIUntilAppend(t, p, tail))
			if len(envs) != 1 || envs[0]["task_id"] != "a" || envs[0]["status"] != "ok" {
				t.Fatalf("envelopes = %v, want appended task a to succeed", envs)
			}
		})
	}
}

func followCLIUntilAppend(t *testing.T, p, tail string) string {
	t.Helper()
	done := make(chan int, 1)
	var out, stderr string
	go func() {
		code, stdout, errout := runCLI(t, "ask", "--batch", p, "--follow", "--follow-idle", "1s")
		out, stderr = stdout, errout
		done <- code
	}()
	select {
	case code := <-done:
		t.Fatalf("batch exited before appended task: %d\n%s\n%s", code, out, stderr)
	case <-time.After(followPoll):
	}
	appendFile(t, p, tail)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not finish after end marker")
	}
	return out
}

func TestFollowerStartupEndMarkerStopsInitialAndAppendedTasks(t *testing.T) {
	p := batchFile(t, `{"id":"a","prompt":"p"}`, `{"end":true}`, `{"id":"b","prompt":"p"}`)
	w, initial, err := newFollower(p, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.f.Close() })
	lines, err := readTaskLines(initial)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Err != nil || lines[0].Task.ID != "a" {
		t.Fatalf("initial tasks = %+v, want only a before the end marker", lines)
	}
	appendFile(t, p, `{"id":"c","prompt":"p"}`+"\n")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, closed, _ := drain(w.run(ctx, ids), time.Second)
	if !closed || len(got) != 0 {
		t.Fatalf("followed tasks = %v, closed = %v, want no tasks and immediate close", got, closed)
	}
}

func TestFollowerReadErrorFailsBatch(t *testing.T) {
	cliEnv(t)
	cliFakes(t)
	w, _ := startFollower(t, "", time.Second)
	if err := w.f.Close(); err != nil {
		t.Fatal(err)
	}
	lines, err := readTaskLines(strings.NewReader(`{"id":"a","prompt":"p"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, ok := runBatch(t.Context(), lines, batchOpts{Agent: "claude", Follow: w.run}, agent.RunOptions{}, false, newClient, &out)
	if ok {
		t.Fatalf("batch succeeded after follow read failed: %s", out.String())
	}
	envs := envelopes(t, out.String())
	if len(envs) != 2 {
		t.Fatalf("envelopes = %v, want initial task and follow error", envs)
	}
	byID := indexBatchEnvelopes(t, envs)
	if byID["a"]["status"] != "ok" || !strings.Contains(out.String(), "--follow:") {
		t.Fatalf("envelopes = %v, want initial success and contextual follow error", envs)
	}
}

func TestIsEndLine(t *testing.T) {
	for line, want := range map[string]bool{
		`{"end":true}`: true, `{"end": true}` + "\n": true,
		`{"end":false}`: false, `{"end":true,"x":1}`: false, `{"id":"a","prompt":"p"}`: false, ``: false, "\n": false,
	} {
		if got := isEndLine([]byte(line)); got != want {
			t.Errorf("isEndLine(%q) = %v, want %v", line, got, want)
		}
	}
}

func TestFollowerEndLineClosesChannel(t *testing.T) {
	w, p := startFollower(t, `{"id":"a","prompt":"p"}`+"\n", time.Minute)
	ch := w.run(t.Context(), ids)
	appendFile(t, p, `{"id":"b","prompt":"p"}`+"\n"+`{"end":true}`+"\n"+`{"id":"c","prompt":"p"}`+"\n")
	got, closed, _ := drain(ch, 5*time.Second)
	if !closed || strings.Join(got, ",") != "b" {
		t.Errorf("tasks %v closed=%v, want only b before the end line and a closed channel", got, closed)
	}
}

func TestFollowerIdleClosesChannel(t *testing.T) {
	const idle = 150 * time.Millisecond
	w, _ := startFollower(t, `{"id":"a","prompt":"p"}`+"\n", idle)
	got, closed, took := drain(w.run(t.Context(), ids), 5*time.Second)
	if !closed || len(got) != 0 || took < idle {
		t.Errorf("tasks %v closed=%v after %v, want a close no earlier than the %v idle limit", got, closed, took, idle)
	}
}

func TestFollowerIdleResetsOnNewLine(t *testing.T) {
	const idle = 500 * time.Millisecond
	w, p := startFollower(t, "", idle)
	ch := w.run(t.Context(), ids)
	time.Sleep(300 * time.Millisecond)
	appendFile(t, p, `{"id":"a","prompt":"p"}`+"\n")
	got, closed, took := drain(ch, 5*time.Second)
	if !closed || strings.Join(got, ",") != "a" || took < idle {
		t.Errorf("tasks %v closed=%v after %v, want a, then a close a full idle after it", got, closed, took)
	}
}

func TestFollowerCancelClosesChannel(t *testing.T) {
	w, _ := startFollower(t, "", time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	ch := w.run(ctx, ids)
	cancel()
	if _, closed, _ := drain(ch, 5*time.Second); !closed {
		t.Error("channel stayed open after cancel")
	}
}

func TestFollowerCancelDuringBlockedSend(t *testing.T) {
	w, p := startFollower(t, "", time.Minute)
	appendFile(t, p, `{"id":"a","prompt":"p"}`+"\n")
	ctx, cancel := context.WithCancel(t.Context())
	ch := w.run(ctx, ids) // nobody receives: the first poll blocks sending a
	time.Sleep(100 * time.Millisecond)
	cancel()
	if got, closed, _ := drain(ch, 5*time.Second); !closed || len(got) > 1 {
		t.Errorf("tasks %v closed=%v, want the blocked send released and the channel closed", got, closed)
	}
}

func TestFollowerWaitsForNewlineOfPartialLine(t *testing.T) {
	w, p := startFollower(t, `{"id":"a","prompt":"p"}`+"\n"+`{"id":"b",`, time.Minute)
	if string(w.pending) != `{"id":"b",` || w.n != 1 {
		t.Fatalf("pending %q n %d, want the unterminated tail held back and 1 line consumed", w.pending, w.n)
	}
	lines, _, _ := w.next()
	if len(lines) != 0 {
		t.Fatalf("parsed %d lines before the newline arrived", len(lines))
	}
	appendFile(t, p, `"prompt":"p"}`+"\n")
	lines, _, _ = w.next()
	if len(lines) != 1 || lines[0].Err != nil || lines[0].Task.ID != "b" || lines[0].N != 2 {
		t.Errorf("after the newline: %+v, want line 2 with id b", lines)
	}
}

func TestFollowerPartialLineDoesNotResetIdle(t *testing.T) {
	const idle = 300 * time.Millisecond
	w, p := startFollower(t, `{"id":"a",`, idle)
	ch := w.run(t.Context(), ids)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				appendFile(t, p, " ")
			}
		}
	}()
	got, closed, took := drain(ch, 5*time.Second)
	if !closed || len(got) != 0 || took > 2*time.Second {
		t.Errorf("tasks %v closed=%v after %v: a growing partial line must neither be parsed nor keep the follower alive", got, closed, took)
	}
}

func TestFollowerLineNumbersCountBlankLines(t *testing.T) {
	w, p := startFollower(t, `{"id":"a","prompt":"p"}`+"\n\n", time.Minute)
	appendFile(t, p, `{"id":"b","prompt":"p"}`+"\n\n"+`{"id":"c","prompt":"p"}`+"\n")
	lines, _, _ := w.next()
	if len(lines) != 2 || lines[0].N != 3 || lines[1].N != 5 {
		t.Errorf("lines %+v, want b at physical line 3 and c at 5", lines)
	}
}

func TestRetryForPicksFirstOtherAgent(t *testing.T) {
	bo := batchOpts{Fallback: "none"}
	task := batchTask{Agent: "codex"}
	if retryFor(task, bo, nil) != nil || retryFor(task, bo, []string{"codex"}) != nil {
		t.Error("retryFor returned a Retry for an empty list or only the task's own agent")
	}
	cliEnv(t)
	cliFakes(t)
	task.Req = agent.RunOptions{Prompt: "p", WorkDir: t.TempDir(), Model: "m", Tools: true}
	retry := retryFor(task, bo, []string{"codex", "opencode", "claude"})
	if retry == nil {
		t.Fatal("no Retry for a list holding another agent")
	}
	p, err := retry(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := task.Req
	want.Agent = "opencode"
	if p.Opts.Agent != "opencode" || p.Opts.Prompt != want.Prompt || p.Opts.WorkDir != want.WorkDir || p.Opts.Model != "m" || !p.Opts.Tools {
		t.Errorf("retry opts = %+v, want the task's request on opencode", p.Opts)
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
