package tools

import (
	"bytes"
	"compress/gzip"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edotau/claude-code/internal/paths"
)

func TestExtractTarGzRejectsCorruptChecksum(t *testing.T) {
	data := buildTarGz(t, "codex", "corrupted executable bytes")
	data[len(data)-8] ^= 0xff

	got, err := extractTarGzSingleFile(data)
	if !errors.Is(err, gzip.ErrChecksum) {
		t.Fatalf("corrupt archive extracted %q with error %v; want gzip checksum failure", got, err)
	}
	if got != nil {
		t.Fatalf("checksum failure returned executable bytes: %q", got)
	}
}

func TestCodexWriteFailureRollsBackCompanions(t *testing.T) {
	setupToolsTestEnv(t)
	dir := paths.BinDir()
	// A valid destination basename whose AtomicWrite temporary basename exceeds NAME_MAX forces the second write to fail.
	codexName := "codex-" + strings.Repeat("x", 244)
	writeToolsFixture(t, filepath.Join(dir, codexName), "release A codex", 0o755)
	writeToolsFixture(t, filepath.Join(dir, "codex-code-mode-host"), "release A host", 0o755)
	all := []stagedBinary{
		{name: "codex-code-mode-host", bin: []byte("release B host")},
		{name: codexName, bin: []byte("release B codex")},
	}
	var out bytes.Buffer
	err := installBinaries(&out, dir, all)
	if err == nil || !strings.Contains(out.String(), "install  "+filepath.Join(dir, codexName)) {
		t.Fatalf("want failure writing second binary after staging companions, got %v; output: %s", err, &out)
	}
	assertInstalledCodexBinary(t, "codex-code-mode-host", "release A host")
	assertInstalledCodexBinary(t, codexName, "release A codex")
}
