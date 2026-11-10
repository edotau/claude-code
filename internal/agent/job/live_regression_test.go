package job

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/agent/retry"
)

func TestLiveFinishReturnsSessionFailure(t *testing.T) {
	setConfigDir(t)
	rec := ClaimRecord("claude", agent.RunOptions{}, time.Now())
	s := liveSession{rec: &rec, name: "claude", profile: "test", session: "session-1"}
	want := errors.New("live child stream broke")
	if err := s.finish(want); !errors.Is(err, want) {
		t.Fatalf("finish = %v, want session failure %v", err, want)
	}
	got, err := LoadRecord(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.Error != want.Error() || got.SessionID != "session-1" {
		t.Fatalf("saved live failure = %+v", got)
	}
}

func TestLiveStartFailureDoesNotPublishSocket(t *testing.T) {
	liveEnv(t)
	rec := ClaimRecord("claude", agent.RunOptions{}, time.Now())
	sock := LiveSocket(rec.ID)
	const marker = "socket must not be touched before child starts"
	if err := os.WriteFile(sock, []byte(marker), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cmd := exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing-agent"))
	err := serveChild(ctx, cancel, &rec, "claude", nil, cmd, &retry.TailBuffer{}, "")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("serveChild = %v, want failed child start", err)
	}
	got, err := os.ReadFile(sock)
	if err != nil || string(got) != marker {
		t.Fatalf("failed start touched socket path: content=%q err=%v", got, err)
	}
}
