package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallRTKConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rtk config is not installed on Windows")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	file := filepath.Join(home, ".config", "rtk", "config.toml")
	if runtime.GOOS == "darwin" {
		file = filepath.Join(home, "Library", "Application Support", "rtk", "config.toml")
	}
	var out bytes.Buffer
	if err := installRTKConfig(&out, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) || !strings.Contains(out.String(), "would write") {
		t.Fatalf("dry run: stat=%v, output=%q", err, out.String())
	}
	if err := installRTKConfig(&out, false); err != nil {
		t.Fatal(err)
	}
	assertRTKConfig(t, file, rtkConfig)
	const userConfig = "[telemetry]\nenabled = true\n"
	if err := os.WriteFile(file, []byte(userConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := installRTKConfig(&out, false); err != nil {
		t.Fatal(err)
	}
	assertRTKConfig(t, file, userConfig)
	if !strings.Contains(out.String(), "kept user file") {
		t.Fatalf("missing user config note: %q", out.String())
	}
}

func assertRTKConfig(t *testing.T, file, want string) {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("config=%q, want %q", data, want)
	}
}
