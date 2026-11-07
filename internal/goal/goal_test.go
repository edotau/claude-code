package goal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edotau/claude-code/internal/paths"
)

func goalEnv(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	plan := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(plan, []byte("# P\n\n## Tasks\n\n- [ ] other (task: g-aaaaaa)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return plan
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Save blocks while another holder has the ticket lock, and concurrent Tick/Save never corrupt the ticket.
func TestSaveSerializesOnTicketLock(t *testing.T) {
	goalEnv(t)
	g := Goal{ID: "g-b00001", Text: "x", Status: Open}
	started, done := make(chan struct{}), make(chan error, 1)
	err := paths.WithFileLock(ticketPath(g.ID), func() error {
		go func() { close(started); done <- Save(g) }()
		<-started
		select {
		case <-done:
			t.Error("Save finished while the ticket lock was held")
		case <-time.After(100 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := Tick(g, Open, "", "out"); err != nil {
				t.Error(err)
			}
			if err := Save(g); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if _, err := Load(g.ID); err != nil {
		t.Errorf("ticket not valid JSON after concurrent writes: %v", err)
	}
}

// Add leaves neither a ticket nor a plan line on a bad id, an existing id (old ticket kept) or a link failure.
func TestAddFailuresLeaveNothing(t *testing.T) {
	plan := goalEnv(t)
	before := readFile(t, plan)
	if _, err := Add(Goal{ID: "bad", Text: "x", Plan: plan}); err == nil {
		t.Error("bad id accepted")
	}
	if readFile(t, plan) != before {
		t.Error("bad id touched the plan")
	}
	first, err := Add(Goal{ID: "g-b00002", Text: "first", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(Goal{ID: first.ID, Text: "second", Plan: plan}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("duplicate id: err = %v", err)
	}
	if got, _ := Load(first.ID); got.Text != "first" {
		t.Errorf("old ticket overwritten: %+v", got)
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(Goal{ID: "g-b00003", Text: "x", Plan: filepath.Join(blocker, "plan.md")}); err == nil {
		t.Error("unwritable plan accepted")
	}
	if _, err := os.Stat(ticketPath("g-b00003")); !os.IsNotExist(err) {
		t.Errorf("ticket survived a link failure: %v", err)
	}
}

// Add adopts a preset valid id (an existing plan line) and saves the ticket Open.
func TestAddAdoptsPresetID(t *testing.T) {
	plan := goalEnv(t)
	g, err := Add(Goal{ID: "g-aaaaaa", Text: "other", Plan: plan})
	if err != nil || g.ID != "g-aaaaaa" {
		t.Fatalf("Add = %+v, %v", g, err)
	}
	if got, err := Load(g.ID); err != nil || got.Status != Open {
		t.Errorf("Load = %+v, %v", got, err)
	}
	if strings.Count(readFile(t, plan), "g-aaaaaa") != 1 {
		t.Error("adopting duplicated the plan line")
	}
}

// A Done tick whose plan sync fails errors and leaves the ticket Open; Closed/Open ticks never touch the plan.
func TestTickSyncsPlanBeforeWritingTicket(t *testing.T) {
	plan := goalEnv(t)
	g, err := Add(Goal{ID: "g-b00004", Text: "x", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{Closed, Open} {
		if _, err := Tick(g, st, "", ""); err != nil || strings.Contains(readFile(t, plan), "[x] x") {
			t.Fatalf("%s tick: err=%v, plan touched", st, err)
		}
	}
	// Done goal whose plan lost its task line: Tick fails.
	if err := os.WriteFile(plan, []byte("# P\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Tick(g, Done, "ok", ""); err == nil {
		t.Fatal("Done tick with no plan line succeeded")
	}
	if got, _ := Load(g.ID); got.Status != Open {
		t.Errorf("ticket status = %q after failed sync, want open", got.Status)
	}
}

// insertTask matches only the exact `## Tasks` heading and ends the section at the next `## ` heading.
func TestInsertTask(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"offset0", "## Tasks\n- a\n## B\n", "## Tasks\n- a\nL\n## B\n"},
		{"trailing spaces", "# P\n## Tasks  \n- a\n## B\n", "# P\n## Tasks  \n- a\nL\n## B\n"},
		{"crlf", "## Tasks\r\n- a\r\n## B\r\n", "## Tasks\r\n- a\r\nL\n## B\r\n"},
		{"tasks done only", "## Tasks done\n- a\n", "## Tasks done\n- a\n\n## Tasks\n\nL\n"},
		{"h3 only", "### Tasks\n- a\n", "### Tasks\n- a\n\n## Tasks\n\nL\n"},
		{"no heading", "# P\n", "# P\n\n## Tasks\n\nL\n"},
		{"after decoy", "## Tasks done\n- z\n## Tasks\n- a\n", "## Tasks done\n- z\n## Tasks\n- a\nL\n"},
	}
	for _, c := range cases {
		if got := insertTask(c.doc, "L"); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
