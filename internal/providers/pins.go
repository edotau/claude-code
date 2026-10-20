package providers

import (
	"bufio"
	"cmp"
	"github.com/edotau/claude-code/internal/proc"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/edotau/claude-code/internal/audit"
	"github.com/edotau/claude-code/internal/keyring"
	"github.com/edotau/claude-code/internal/paths"
)

// Pin keys: process env beats env.d/provider.env. A model pin may be "provider:model" to cross providers.
const (
	PinProvider = "HARNESS_PROVIDER"
	PinModel    = "HARNESS_MODEL" // fills every slot
	// PinGitHubUser names the gh account copilot runs as (`gh auth token --user`); unset = gh's active one.
	PinGitHubUser = "COPILOT_GITHUB_USER"
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
		if err := paths.AtomicWrite(file, []byte(b.String()), 0o600); err != nil {
			return err
		}
		for k, v := range kv {
			switch {
			case k == PinProvider:
				audit.Log(audit.ProviderSwitch, v, pinCaller())
			case k == PinModel || (strings.HasPrefix(k, "HARNESS_") && strings.HasSuffix(k, "_MODEL")):
				audit.Log(audit.ModelPin, k, cmp.Or(v, "unpinned"))
			}
		}
		return nil
	})
}

// PinDiff previews SetPins(kv): one "KEY: old → new" line per key whose value would change, sorted.
func PinDiff(kv map[string]string) []string {
	cur := ReadEnvFile(PinFile())
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		if cur[k] == kv[k] {
			continue
		}
		out = append(out, k+": "+cmp.Or(cur[k], "(unset)")+" → "+cmp.Or(kv[k], "(unset)"))
	}
	return out
}

// OAuthTokenEnv is the env var Claude Code reads a long-lived Max-plan token from (`claude setup-token` mints one).
const OAuthTokenEnv = "CLAUDE_CODE_OAUTH_TOKEN"

// HeadlessOAuthToken is the stored setup-token for unattended `claude -p` runs: the process env first, then
// the OS keyring, then env.d/secrets.env. "" when none has one — the run then depends on the interactive /login session.
func HeadlessOAuthToken() string {
	if tok := strings.TrimSpace(os.Getenv(OAuthTokenEnv)); tok != "" {
		return tok
	}
	return StoredSecret(OAuthTokenEnv)
}

// keyringGet is the OS keyring lookup; a seam so tests never touch the real keychain.
var keyringGet = keyring.Get

// StoredSecret is name from the managed stores: the OS keyring first, then env.d/secrets.env; "" when neither.
func StoredSecret(name string) string {
	if v, ok := keyringGet(name); ok {
		return v
	}
	return strings.TrimSpace(ReadEnvFile(SecretsFile())[name])
}

// EffectiveSecret is the order a launch actually uses: process env, then the OS keyring, then
// env.d/secrets.env. source names which plane answered ("env"|"keyring"|"file"), "" when none do.
// secrets.ladder labels the same order — keep them in step.
func EffectiveSecret(name string) (value, source string) {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v, "env"
	}
	if v, ok := keyringGet(name); ok {
		return v, "keyring"
	}
	if v := strings.TrimSpace(ReadEnvFile(SecretsFile())[name]); v != "" {
		return v, "file"
	}
	return "", ""
}

// pinCaller names who wrote a pin — the verb and the parent process — so an unexpected switch can be traced.
func pinCaller() string {
	verb := ""
	if len(os.Args) > 1 {
		verb = os.Args[1]
	}
	return "via " + cmp.Or(verb, "?") + " (parent " + cmp.Or(proc.Comm(os.Getppid()), "?") + ")"
}
