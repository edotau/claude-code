package launch

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexConfigCheck(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "config.toml")
	if err := os.WriteFile(file, []byte("PRIVATE-CONFIG-CONTENTS\nnot valid TOML"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		managed bool
		ok      bool
		detail  string
	}{
		{"absent", filepath.Join(root, "missing.toml"), false, true, ": absent"},
		{"user", file, false, true, ": present; CLI and machine settings may override it"},
		{"machine", file, true, true, ": present; machine policy may override CLI settings; effective route unverified"},
		{"nonregular", root, false, false, ": unavailable; effective route unknown"},
		{"unavailable", filepath.Join(file, "config.toml"), true, false, ": unavailable; effective route unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := codexConfigCheck("config check", tt.path, tt.managed)
			want := Check{Name: "config check", OK: tt.ok, Detail: tt.path + tt.detail}
			if got != want {
				t.Errorf("codexConfigCheck() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestCodexConfigChecksPaths(t *testing.T) {
	root := t.TempDir()
	machine := filepath.Join(root, "managed.toml")
	requirements := filepath.Join(root, "requirements.toml")
	if err := os.WriteFile(machine, []byte("PRIVATE-MACHINE-CONTENTS"), 0o600); err != nil {
		t.Fatal(err)
	}
	previous := codexManagedPaths
	codexManagedPaths = func() []string { return []string{machine, requirements} }
	t.Cleanup(func() { codexManagedPaths = previous })
	userHome := filepath.Join(root, "user-home")
	t.Setenv("HOME", userHome)
	t.Setenv("USERPROFILE", userHome)
	for _, override := range []string{"", filepath.Join(root, "custom-codex-home")} {
		name := "default home"
		if override != "" {
			name = "CODEX_HOME"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", override)
			home := override
			if home == "" {
				home = filepath.Join(userHome, ".codex")
			}
			config := filepath.Join(home, "config.toml")
			if err := os.MkdirAll(home, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(config, []byte("PRIVATE-USER-CONTENTS"), 0o600); err != nil {
				t.Fatal(err)
			}
			got := codexConfigChecks()
			want := []Check{
				{Name: "codex machine config", OK: true, Detail: machine + ": present; machine policy may override CLI settings; effective route unverified"},
				{Name: "codex machine config", OK: true, Detail: requirements + ": absent"},
				{Name: "codex user config", OK: true, Detail: config + ": present; CLI and machine settings may override it"},
			}
			if len(got) != len(want) {
				t.Fatalf("got %d checks, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Errorf("check %d = %+v, want %+v", i, got[i], want[i])
				}
			}
		})
	}
}
