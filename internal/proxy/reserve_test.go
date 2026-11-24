package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxyteam"
	"github.com/edotau/claude-code/internal/textutil"
)

func TestEnsureTeamStartsOnceThenFinds(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(PIDHeader, strconv.Itoa(os.Getpid()))
		w.Header().Set(HeaderInflight, "0")
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	old := spawnScope
	t.Cleanup(func() { spawnScope = old })
	spawns := 0
	spawnScope = func(team, id string) error {
		spawns++
		start, _ := proc.StartTime(os.Getpid())
		_, err := WriteScope(Scope{Team: team, ID: id, PID: os.Getpid(), Start: start, Port: port,
			Build: textutil.BuildVersion(), Started: time.Now().Add(time.Hour)})
		return err
	}
	team := proxyteam.Team{Name: "general", Host: proxyteam.HostDetached, Reserved: 2}
	a, err := EnsureTeam(context.Background(), team, "d1")
	if err != nil {
		t.Fatal(err)
	}
	b, err := EnsureTeam(context.Background(), team, "d1")
	if err != nil || spawns != 1 || a.Port != port || b.PID != a.PID {
		t.Fatalf("spawns=%d a=%+v b=%+v err=%v", spawns, a, b, err)
	}
}
