package rtkwrap

import "testing"

func TestSessionFlagScopesCodeMode(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv(CodemodeEnv, "")
	if err := SetSessionFlag("s1", true); err != nil {
		t.Fatal(err)
	}
	if got := CodeModeSourceFor("s1"); got != SessionFlagSource {
		t.Errorf("s1 source = %q", got)
	}
	if CodeModeFor("s2") || CodeMode() {
		t.Error("flag leaked to another session or to CodeMode()")
	}
	t.Setenv(CodemodeEnv, "on")
	if err := SetSessionFlag("s1", false); err != nil || CodeModeFor("s1") || !CodeModeFor("s2") {
		t.Errorf("off must beat CODEMODE for s1 only: err %v", err)
	}
	if got, _ := CodeModeStatus("s1"); got != "codemode: off (session flag)" {
		t.Errorf("status = %q", got)
	}
	if _, err := CodeModeStatus("../x"); err == nil {
		t.Error("status accepted a traversal id")
	}
	if _, err := SessionFlag("../x"); err == nil {
		t.Error("traversal id accepted")
	}
}
