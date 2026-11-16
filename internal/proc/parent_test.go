package proc

import (
	"os"
	"os/exec"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestParseStatPPID(t *testing.T) {
	stat := "12345 (weird ) ( comm) S 6789 12345 12345 0 -1 4194304 0 0 0 0"
	if got := parseStatPPID(stat); got != 6789 {
		t.Errorf("parseStatPPID(%q) = %d, want 6789", stat, got)
	}
}

// darwin's Descendants parses `ps -A -o pid=,ppid=` (right-aligned, padded) and walks grandchildren breadth first.
func TestParseChildrenWalksDescendants(t *testing.T) {
	ps := "    1     0\n  100     1\n  200   100\n  201   100\n  300   200\n  bad line here\n  400     1\n"
	children := parseChildren(ps)
	if _, ok := children[0]; ok {
		t.Errorf("pid 1 recorded as a child of 0: %v", children)
	}
	if got, want := walkChildren(children, 100), []int{200, 201, 300}; !slices.Equal(got, want) {
		t.Errorf("walkChildren(100) = %v, want %v", got, want)
	}
	if got := walkChildren(children, 300); len(got) != 0 {
		t.Errorf("walkChildren(leaf 300) = %v, want empty", got)
	}
}

// Descendants reads the live process table: /proc on linux, ps on darwin.
func TestDescendants(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Descendants reads /proc or ps")
	}
	cmd := exec.Command("sh", "-c", "sleep 30 & sleep 30 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var got []int
	killAll := func(pids []int) {
		for _, pid := range pids {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	}
	t.Cleanup(func() {
		killAll(got)
		killAll(Descendants(cmd.Process.Pid))
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got = Descendants(cmd.Process.Pid)
		if len(got) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 2 {
		t.Fatalf("Descendants(%d) = %v, want 2 sleep PIDs", cmd.Process.Pid, got)
	}
	for _, pid := range got {
		if len(Descendants(pid)) != 0 {
			t.Errorf("Descendants(leaf %d) = %v, want empty", pid, Descendants(pid))
		}
	}
}

// A version-named binary is an agent only through its exec path; any other version-named process is not.
func TestAgentName(t *testing.T) {
	names := map[string]bool{"claude": true, "codex": true}
	cases := []struct{ comm, exe, want string }{
		{"codex", "", "codex"},
		{"2.1.325", "/Users/u/.local/bin/claude", "claude"},
		{"2.1.325", "/Users/u/.local/share/claude/versions/2.1.325", "claude"},
		{"2.1.325", "/opt/tool/2.1.325", ""},
		{"1.2.3", "/opt/foo/versions/1.2.3", ""},
		{"2.1.325", "/opt/claude/bin/2.1.325", ""},
		{"2.1.325", "", ""},
		{"vim", "/usr/bin/claude", ""},
	}
	for _, c := range cases {
		read := false
		got := AgentName(c.comm, func() string { read = true; return c.exe }, names)
		if got != c.want {
			t.Errorf("AgentName(%q, %q) = %q, want %q", c.comm, c.exe, got, c.want)
		}
		if read && !versionLike(c.comm) {
			t.Errorf("AgentName(%q) read the exec path for a non-version name", c.comm)
		}
	}
}
