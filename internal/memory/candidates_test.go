package memory

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

const candidateRows = `{"id":"fc-a","evidence":["feedback/feedback-a.md","notes/note-c.md"],"repeats":1,"change":"Add a hook.","target":"unknown","kind":"hook","size":"small"}` + "\n" +
	`{"id":"fc-b","evidence":["feedback/feedback-b.md"],"repeats":3,"change":"Add a flag.","target":"internal/foo/a.go","kind":"verb","size":"medium"}`

func TestCandidateApproveAndClose(t *testing.T) {
	cfg, _ := isolatedConfig(t)
	t.Setenv(dreamChildEnv, "")
	for _, n := range []string{"feedback/feedback-a.md", "feedback/feedback-b.md", "notes/note-c.md"} {
		write(t, filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(n)), "x\n")
	}
	write(t, filepath.Join(cfg, "rules", "x.md"), "one\ntwo\n")
	path := filepath.Join(paths.MemoryStoreDir(), filepath.FromSlash(candidatesRel))
	write(t, path, "# C\n\n```jsonl\n"+candidateRows+"\n```\n")
	if err := EnsureBank(""); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if got := openCandidatesNotice(); got != "2 open feedback candidates" {
		t.Errorf("notice = %q", got)
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-b", Enforced: "rules/x.md:2"}); err == nil {
		t.Error("enforced before approval was accepted")
	}
	for _, o := range []ReviewOptions{{Candidate: "fc-b", Approve: true}, {Candidate: "fc-b", Enforced: "rules/x.md:2"}} {
		if err := candidateAction(&out, o); err != nil {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-a", Enforced: "rules/x.md:9"}); err == nil {
		t.Error("a line past the end of the file was accepted")
	}
	got := mustRead(t, path)
	if !strings.Contains(got, `"status":"enforced","enforced_by":"rules/x.md:2"`) || !strings.Contains(got, strings.SplitN(candidateRows, "\n", 2)[0]) {
		t.Errorf("record not rewritten in place:\n%s", got)
	}
	if p := candidateProblems(); len(p) != 0 || openCandidatesNotice() != "1 open feedback candidate" {
		t.Errorf("problems %v, notice %q", p, openCandidatesNotice())
	}
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-zz"}); err != errNoCandidate {
		t.Errorf("unknown id: %v", err)
	}
	t.Setenv(dreamChildEnv, "1")
	if err := candidateAction(&out, ReviewOptions{Candidate: "fc-a", Approve: true}); err == nil {
		t.Error("a dream job approved a candidate")
	}
}

// A target with shell metacharacters fails validation, and the printed gate is a quoted clean package path.
func TestCandidateTargetNeverReachesShell(t *testing.T) {
	isolatedConfig(t)
	for _, target := range []string{"internal/x/$(touch /tmp/pwned).go", "internal/x;id", "internal/a b/c.go", "internal/x'y/z.go",
		"internal/`id`/z.go", "-internal/x.go", "internal/-rf/x.go", "internal/../x.go", "internal/x\ny/z.go"} {
		if problem := targetProblem(target); problem == "" || strings.Contains(problem, "touch") {
			t.Errorf("target %q: problem %q", target, problem)
		}
		if g := candidateGate(target); g != "" {
			t.Errorf("target %q: gate %q", target, g)
		}
	}
	if g := candidateGate("internal/foo/a.go"); targetProblem("internal/foo/a.go") != "" || !strings.HasSuffix(g, "go test ./internal/foo/...") {
		t.Errorf("plain target: gate %q", g)
	}
}
