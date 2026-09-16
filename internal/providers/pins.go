package providers

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/paths"
)

// Pin keys: process env beats env.d/provider.env. A model pin may be "provider:model" to cross providers.
const (
	PinProvider = "HARNESS_PROVIDER"
	PinModel    = "HARNESS_MODEL" // fills every slot
)

// SlotPin is the per-slot pin key, e.g. HARNESS_OPUS_MODEL; it beats PinModel.
func SlotPin(slot string) string { return "HARNESS_" + strings.ToUpper(slot) + "_MODEL" }

// PinFile is where `claude-code use` / `models --pin` persist choices.
func PinFile() string { return filepath.Join(paths.EnvDir(), "provider.env") }

// SecretsFile holds KEY=VALUE credentials (0600, gitignored) consulted after the process env.
func SecretsFile() string { return filepath.Join(paths.EnvDir(), "secrets.env") }

// ReadEnvFile parses KEY=VALUE lines (# comments, optional export, optional quotes); a missing file is empty.
func ReadEnvFile(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		out[strings.TrimSpace(k)] = v
	}
	return out
}

// Pin reads key from the process env, then the pin file.
func Pin(key string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return ReadEnvFile(PinFile())[key]
}

// SetPins writes (value != "") or clears (value == "") keys in the pin file under its lock.
func SetPins(kv map[string]string) error {
	file := PinFile()
	return paths.WithFileLock(file, func() error {
		cur := ReadEnvFile(file)
		for k, v := range kv {
			if v == "" {
				delete(cur, k)
			} else {
				cur[k] = v
			}
		}
		keys := make([]string, 0, len(cur))
		for k := range cur {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString("# Managed by `claude-code use` / `claude-code models --pin`.\n")
		for _, k := range keys {
			b.WriteString(k + "=" + cur[k] + "\n")
		}
		return paths.AtomicWrite(file, []byte(b.String()), 0o600)
	})
}
