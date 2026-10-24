package vendoragent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/secrets/shapes"
)

// Row is one background-job line for `claude-code <v> status`.
type Row struct {
	ID      string
	Status  string // running | lost | ok | error (lost = running record, dead pid)
	Elapsed string
	Preview string
	Session string
}

// Status lists rows for name's jobs in cwd's repo (all repos when all is set), newest first; ref narrows via Resolve.
func Status(ctx context.Context, name, cwd, ref string, all bool) ([]Row, error) {
	if _, ok := Lookup(name); !ok {
		return nil, errUnknownVendor(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), all)
	if ref != "" {
		rec, err := Resolve(recs, ref, nil)
		if err != nil {
			return nil, err
		}
		recs = []agent.Record{rec}
	}
	rows := make([]Row, len(recs))
	for i, r := range recs {
		rows[i] = toRow(r)
	}
	return rows, nil
}

// Result returns ref's record (default: newest finished) and its log text; a running match errors.
func Result(ctx context.Context, name, cwd, ref string) (agent.Record, string, error) {
	if _, ok := Lookup(name); !ok {
		return agent.Record{}, "", errUnknownVendor(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), false)
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return r.Status != "running" })
	if err != nil {
		return agent.Record{}, "", err
	}
	if rec.Status == "running" {
		return agent.Record{}, "", fmt.Errorf("job %s is still running", rec.ID)
	}
	log, _ := os.ReadFile(rec.Log) // best-effort: a missing log still returns the record
	return rec, string(log), nil
}

// CancelJob interrupts ref's job, default: the single running job for name in cwd's repo.
func CancelJob(ctx context.Context, name, cwd, ref string) (agent.Record, error) {
	if _, ok := Lookup(name); !ok {
		return agent.Record{}, errUnknownVendor(name)
	}
	recs := List(name, RepoRoot(ctx, cwd), false)
	rec, err := Resolve(recs, ref, func(r agent.Record) bool { return Alive(r) })
	if err != nil {
		return agent.Record{}, err
	}
	if !Alive(rec) {
		return agent.Record{}, fmt.Errorf("job %s is not running", rec.ID)
	}
	if err := Cancel(&rec); err != nil {
		return agent.Record{}, err
	}
	return rec, nil
}

// toRow derives a status Row from a dispatch record; redaction matches printDispatchLog (internal/cli/agent.go).
func toRow(r agent.Record) Row {
	status := r.Status
	if status == "running" && !Alive(r) {
		status = "lost"
	}
	elapsed := r.Duration
	if status == "running" || status == "lost" {
		elapsed = time.Since(r.Time).Round(time.Second).String()
	}
	preview := strings.Join(strings.Fields(r.Request.Prompt), " ")
	for w := range strings.FieldsSeq(preview) {
		if shapes.Looks(w) {
			preview = strings.ReplaceAll(preview, w, "REDACTED")
		}
	}
	if len([]rune(preview)) > 60 {
		runes := []rune(preview)
		preview = string(runes[:60]) + "…"
	}
	return Row{ID: r.ID, Status: status, Elapsed: elapsed, Preview: preview, Session: r.SessionID}
}
