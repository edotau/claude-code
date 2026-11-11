package plans

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/hosttest"
)

func TestUnreadableMirrorRecordPreventsTick(t *testing.T) {
	for _, scope := range []string{"repo", "top-level"} {
		t.Run(scope, func(t *testing.T) {
			config := hermetic(t)
			repo, base := tickRangeRepo(t)
			home := Home(repo)
			if scope == "top-level" {
				home = filepath.Join(config, "plans")
			}
			plan := filepath.Join(home, "2026-11-09-draft.md")
			body := "- [ ] 1.1 Draft owns: internal/a/a.go\n"
			writeFile(t, plan, body)
			if err := os.MkdirAll(DestRecordPath("draft"), 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := os.ReadFile(DestRecordPath("draft")); err == nil {
				t.Fatal("fixture record must be unreadable")
			}
			drafts := MirrorDests()
			if !slices.Contains(drafts, plan) {
				t.Fatalf("unreadable record left readable draft out of exclusions: %v", drafts)
			}
			if err := TickRange(context.Background(), repo, "", base+"..HEAD", "", drafts, false, io.Discard); err != nil {
				t.Fatal(err)
			}
			wantBytes(t, plan, "unapproved draft", body)
		})
	}
}

func TestClearClaimOwnedRejectsEmptyOwner(t *testing.T) {
	plan := userTickPlan(t, "- [ ] 1.1 Task owns: a.go <!-- working: alice -->\n")
	before := readFile(t, plan)
	if err := ClearClaimOwned(plan, "1.1", ""); err != nil {
		t.Fatal(err)
	}
	wantBytes(t, plan, "alice's claim with empty caller", before)
}

func TestTickRangePropagatesOutputErrors(t *testing.T) {
	cases := []struct {
		name, owns, explicit, output string
		plans                        int
	}{
		{"no owner", "other.go", "", "no plan owns", 1},
		{"ambiguous", "internal/a/a.go", "", "ambiguous", 2},
		{"nothing ticked", "other.go", "2026-11-09-0.md", "nothing to tick in", 1},
		{"ticked", "internal/a/a.go", "2026-11-09-0.md", "would tick", 1},
		{"partial", "internal/a/a.go, missing.go", "2026-11-09-0.md", "partly landed", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hermetic(t)
			repo, base := tickRangeRepo(t)
			home := Home(repo)
			for i := range tc.plans {
				name := "2026-11-09-" + strconv.Itoa(i) + ".md"
				writeFile(t, filepath.Join(home, name), "- [ ] 1.1 Task owns: "+tc.owns+"\n")
			}
			plan := ""
			if tc.explicit != "" {
				plan = filepath.Join(home, tc.explicit)
			}
			rng := base + ".." + strings.TrimSpace(hosttest.In(t, repo, "rev-parse", "HEAD~2"))
			var output bytes.Buffer
			if err := TickRange(context.Background(), repo, "", rng, plan, nil, true, &output); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), tc.output) {
				t.Fatalf("wrong output branch: %q, want %q", output.String(), tc.output)
			}
			reader, writer := io.Pipe()
			defer writer.Close()
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if err := TickRange(context.Background(), repo, "", rng, plan, nil, true, writer); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("TickRange = %v, want closed pipe error", err)
			}
		})
	}
}
