package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/proc/inflight"
)

// BuildAskRecord constructs the dispatch record for req, reusing jobRec's identity when resuming a job.
func BuildAskRecord(ctx context.Context, name string, req Request, jobRec *Record, workdir string, background bool) Record {
	if jobRec != nil {
		return *jobRec // same id: a job run continues the record it was spawned from
	}
	rec := ClaimRecord(name, req, time.Now())
	cwd := workdir
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	rec.Repo = RepoRoot(ctx, cwd)
	rec.Owner = inflight.SessionOwner()
	rec.Log = rec.LogPath()
	if !background {
		rec.PID = os.Getpid() // foreground run: alive while this process runs, lost only if it dies
	}
	return rec
}

// ResolveOutputSchema reads --output-schema as inline JSON when it starts with "{", else as a file, and checks it parses.
func ResolveOutputSchema(arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	body := strings.TrimSpace(arg)
	if !strings.HasPrefix(body, "{") {
		data, err := os.ReadFile(arg)
		if err != nil {
			return "", fmt.Errorf("--output-schema: %w", err)
		}
		body = strings.TrimSpace(string(data))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(body), &probe); err != nil {
		return "", fmt.Errorf("--output-schema: not a JSON object: %w", err)
	}
	return body, nil
}

// RecordEnvelope is the result event's envelope, or one rebuilt from rec when no result event can be read.
func RecordEnvelope(rec Record, errw io.Writer) Envelope {
	if f, err := os.Open(rec.EventsPath()); err == nil {
		defer f.Close()
		var env *Envelope
		sc := bufio.NewScanner(f)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var ev Event
			if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Type == EventResult && ev.Envelope != nil {
				env = ev.Envelope
			}
		}
		if err := sc.Err(); err != nil {
			fmt.Fprintf(errw, "agents result: %s: events unreadable, envelope may be incomplete: %v\n", rec.ID, err)
		}
		if env != nil {
			return *env
		}
	}
	var runErr error
	if rec.Status == "error" {
		runErr = fmt.Errorf("%s", rec.Error)
	}
	res := Result{Agent: rec.Agent, Provider: rec.Provider, Model: rec.Model, Invoked: rec.Invoked,
		SessionID: rec.SessionID, DurationMs: rec.DurationMs, Fallback: rec.Fallback, FallbackReason: rec.FallbackReason}
	if log, err := os.ReadFile(rec.LogPath()); err == nil {
		res.Answer = string(log)
	}
	return NewEnvelope(res, runErr, rec.ID)
}
