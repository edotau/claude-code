package agent

import (
	"os"
	"testing"
	"time"
)

func TestRecoverableRequiresPreviousBootDeadBackgroundJob(t *testing.T) {
	boot := time.Now().Add(-time.Minute)
	base := NewRecord("codex", RunOptions{Prompt: "saved request"}, boot.Add(-time.Hour))
	base.Background, base.PID, base.FallbackPolicy = true, adapterDeadPID(t), "all"
	setConfigDir(t)
	fixtureRecord(t, base)
	for _, tc := range []struct {
		name string
		edit func(*Record)
		want bool
	}{
		{"dead", func(r *Record) {}, true},
		{"reaped", func(r *Record) { r.Status, r.Error = "lost", LostError }, true},
		{"current boot", func(r *Record) { r.Time = boot.Add(time.Second) }, false},
		{"no timestamp", func(r *Record) { r.Time = time.Time{} }, false},
		{"foreground", func(r *Record) { r.Background = false }, false},
		{"live runner", func(r *Record) { r.PID = os.Getpid() }, false},
		{"no runner", func(r *Record) { r.PID = 0 }, false},
		{"live session", func(r *Record) { r.Live = true }, false},
		{"remote", func(r *Record) { r.Host = "fixture" }, false},
		{"waves", func(r *Record) { r.Agent = "waves" }, false},
		{"parented", func(r *Record) { r.Parent = "parent" }, false},
		{"review", func(r *Record) { r.Request.Prompt = "codex review split into 3 parts" }, false},
		{"carried", func(r *Record) { r.Request.Carry = true }, false},
		{"legacy no fallback policy", func(r *Record) { r.FallbackPolicy = "" }, false},
		{"too old", func(r *Record) { r.Time = boot.Add(-recoverMaxAge - time.Second) }, false},
		{"relaunched once", func(r *Record) { _ = markRecovery(*r) }, false},
		{"no request", func(r *Record) { r.Request.Prompt = "" }, false},
		{"success", func(r *Record) { r.Status = "ok" }, false},
		{"cancelled", func(r *Record) { r.Status, r.Error = "error", "cancelled" }, false},
		{"unrelated lost", func(r *Record) { r.Status = "lost" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base
			tc.edit(&r)
			if got := r.recoverable(boot); got != tc.want {
				t.Fatalf("recoverable = %t, want %t", got, tc.want)
			}
		})
	}
	if base.recoverable(time.Time{}) {
		t.Fatal("unknown boot time must not replay a job")
	}
}
