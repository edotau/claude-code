package hosttest

import (
	"os"
	"path/filepath"
	"testing"
)

var hermeticKeys = []string{"CLAUDE_CONFIG_DIR", "HOME", "XDG_DATA_HOME", "CLAUDE_PROFILE", "ANTHROPIC_API_KEY"}

func snapshot() map[string]string {
	m := map[string]string{}
	for _, k := range hermeticKeys {
		m[k] = os.Getenv(k)
	}
	return m
}

func TestHermeticEnvGivesIndependentDirsAndClearsOverrides(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "outer")
	t.Setenv("ANTHROPIC_API_KEY", "sk-outer")
	var envs []map[string]string
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			HermeticEnv(t)
			envs = append(envs, snapshot())
		})
	}
	for _, e := range envs {
		cfg := e["CLAUDE_CONFIG_DIR"]
		want := map[string]string{"CLAUDE_CONFIG_DIR": cfg, "HOME": filepath.Join(cfg, "home"),
			"XDG_DATA_HOME": filepath.Join(cfg, "home", ".local", "share"), "CLAUDE_PROFILE": "", "ANTHROPIC_API_KEY": ""}
		for k, v := range want {
			if e[k] != v {
				t.Errorf("%s = %q, want %q", k, e[k], v)
			}
		}
	}
	if len(envs) != 2 || envs[0]["CLAUDE_CONFIG_DIR"] == envs[1]["CLAUDE_CONFIG_DIR"] {
		t.Errorf("config dirs not independent: %v", envs)
	}
}

func TestHermeticEnvRestoresOnCleanup(t *testing.T) {
	t.Setenv("CLAUDE_PROFILE", "outer")
	t.Setenv("ANTHROPIC_API_KEY", "sk-outer")
	before := snapshot()
	t.Run("inner", func(t *testing.T) { HermeticEnv(t) })
	for k, v := range snapshot() {
		if before[k] != v {
			t.Errorf("%s = %q after cleanup, want %q", k, v, before[k])
		}
	}
}
