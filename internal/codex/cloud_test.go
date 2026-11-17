package codex

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths/pathstest"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/ui/helptext"
	"github.com/edotau/claude-code/internal/vault/vaulttest"
)

func TestCloudArgv(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    []string
		wantErr string
	}{
		{"exec", []string{"exec", "fix", "it", "--env", "e1", "--attempts", "2", "--branch", "b"},
			[]string{"cloud", "exec", "--attempts", "2", "--branch", "b", "--env", "e1", "--", "fix it"}, ""},
		{"exec without env", []string{"exec", "x"}, nil, "--env ID is required"},
		{"status", []string{"status", "t1"}, []string{"cloud", "status", "--", "t1"}, ""},
		{"list", []string{"list", "--env", "e", "--limit", "5"}, []string{"cloud", "list", "--env", "e", "--limit", "5"}, ""},
		{"diff", []string{"diff", "t1", "--attempt", "2"}, []string{"cloud", "diff", "--attempt", "2", "--", "t1"}, ""},
		{"apply", []string{"apply", "t1"}, []string{"cloud", "apply", "--", "t1"}, ""},
		{"foreign flag", []string{"status", "t1", "--env", "e"}, nil, "flag provided but not defined"},
		{"dash value", []string{"list", "--env", "-x"}, nil, "must not start"},
		{"missing task", []string{"diff"}, nil, "wrong number"},
	}
	for _, tc := range tests {
		fs, _ := cloudFlags(tc.args[0])
		pos, err := helptext.ParseArgs(fs, tc.args[1:])
		var got []string
		if err == nil {
			got, err = cloudArgv(tc.args[0], fs, pos)
		}
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err=%v, want %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: argv=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}
}

// Cloud runs the vendor only past its gate: exec needs overlay cloud, the read-back verbs and a dispatch's exec differ.
func TestCloudGateRuns(t *testing.T) {
	vaulttest.Use(t)
	log := filepath.Join(t.TempDir(), "argv")
	pathstest.FakeVendor(t, "codex", `echo "$*" >> `+log)
	ran := func() string { b, _ := os.ReadFile(log); return strings.TrimSpace(string(b)) }
	open := `{"cloud":true}`
	tests := []struct {
		name   string
		args   []string
		nested bool
		wantRC int
		want   string
	}{
		{"exec without cloud", []string{"exec", "x", "--env", "e1"}, false, 1, ""},
		{"exec with cloud", []string{"exec", "x", "--env", "e1", "--overlay", open}, false, 0, "cloud exec --env e1 -- x"},
		{"exec nested", []string{"exec", "x", "--env", "e1", "--overlay", open}, true, 1, ""},
		{"exec databricks", []string{"exec", "x", "--env", "e1", "--overlay", `{"cloud":true,"provider":"databricks"}`}, false, 1, ""},
		{"status without cloud", []string{"status", "t1"}, false, 0, "cloud status -- t1"},
		{"list without cloud", []string{"list"}, false, 0, "cloud list"},
		{"diff nested", []string{"diff", "t1"}, true, 0, "cloud diff -- t1"},
		{"status databricks", []string{"status", "t1", "--overlay", `{"provider":"databricks"}`}, false, 1, ""},
	}
	for _, tc := range tests {
		os.Remove(log)
		id := ""
		if tc.nested {
			id = "d1"
		}
		t.Setenv(proc.DispatchIDEnv, id)
		if rc := Cloud(tc.args); rc != tc.wantRC || ran() != tc.want {
			t.Errorf("%s: rc=%d ran=%q, want rc=%d ran=%q", tc.name, rc, ran(), tc.wantRC, tc.want)
		}
	}
}
