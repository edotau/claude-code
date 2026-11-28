package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agent"
	"github.com/edotau/claude-code/internal/platform/paths"
	"github.com/edotau/claude-code/internal/platform/proc"
	"github.com/edotau/claude-code/internal/platform/vault"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/routing/providers"
)

// agyExecArg rebinds headless/live children; interactive launches already exec in the minting process.
const agyExecArg = "--harness-agy-exec"

// init follows the private worker entry pattern (hooks/docs, hooks/lane); no CLI initialization is needed here.
func init() {
	if len(os.Args) < 3 || os.Args[1] != agyExecArg {
		return
	}
	if err := execAgy(os.Args[2], os.Args[3:]); err != nil {
		fmt.Fprintf(os.Stderr, "gemini: gateway session: %v\n", err)
		os.Exit(70)
	}
}

var errNoSamePIDExec = errors.New("gateway sessions require same-PID exec (unsupported on native Windows)")

// execAgyImage is the exec seam for checking the final vendor environment without replacing the test process.
var execAgyImage = proc.Exec

// agyEnvKey is the adopted-key lookup seam; a launcher must not rely on the scrubbed process environment.
var agyEnvKey = vault.EnvKey

func execAgy(bin string, argv []string) error {
	if runtime.GOOS == "windows" {
		return errNoSamePIDExec
	}
	secret, err := proxy.ClientSecret()
	if err != nil {
		return err
	}
	tok, err := rebindAgyToken(secret, os.Getenv("AGY_LLM_GATEWAY_API_KEY"), os.Getpid(), time.Now())
	if err != nil {
		return err
	}
	env := proc.ValuesFromEnv(os.Environ())
	env["AGY_LLM_GATEWAY_API_KEY"] = tok
	delete(env, "HARNESS_VAULT_KEY") // only the harness trampoline may unlock the vault
	return execAgyImage(bin, argv, env.Env())
}

// RebindGatewayToken re-signs env's agy gateway token to pid, a process that outlives this one.
func RebindGatewayToken(env map[string]string, pid int) error {
	secret, err := proxy.ClientSecret()
	if err != nil {
		return err
	}
	tok, err := rebindAgyToken(secret, env["AGY_LLM_GATEWAY_API_KEY"], pid, time.Now())
	if err != nil {
		return err
	}
	env["AGY_LLM_GATEWAY_API_KEY"] = tok
	return nil
}

// rebindAgyToken copies signed scope/fence/failover only; the standard signer renews TTL, still bounded by agy's PID.
func rebindAgyToken(secret, tok string, pid int, now time.Time) (string, error) {
	if err := proxy.CheckSessionToken(secret, tok, "", now, proc.StartTime); err != nil {
		return "", fmt.Errorf("launch token: %w", err)
	}
	claims, err := proxy.ParseSessionToken(secret, tok)
	if err != nil {
		return "", err
	}
	claims.PID = pid
	if claims.Start, err = proc.StartTime(pid); err != nil {
		return "", err
	}
	return proxy.SignSessionToken(secret, claims, now)
}

// signedShape reports tok has the signed-token shape; the empty secret reaches only the secret-free shape check.
func signedShape(tok string) bool {
	_, err := proxy.ParseSessionToken("", tok)
	return !errors.Is(err, proxy.ErrSessionMalformed)
}

// bindAgyChild leaves login launches alone and keeps the gateway token out of argv and files.
func bindAgyChild(cmd *exec.Cmd) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	cur := proc.ValuesFromEnv(cmd.Env)
	if cur["AGY_LLM_GATEWAY_URL"] == "" || !signedShape(cur["AGY_LLM_GATEWAY_API_KEY"]) {
		return nil // no route, or a plain key: nothing to rebind
	}
	if runtime.GOOS == "windows" {
		return errNoSamePIDExec
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd.Args = append([]string{self, agyExecArg, cmd.Path}, cmd.Args[1:]...)
	cmd.Path = self
	if key := agyEnvKey(); key != "" {
		env := proc.ValuesFromEnv(cmd.Env)
		env["HARNESS_VAULT_KEY"] = key // restored only until execAgy strips it before the vendor exec
		cmd.Env = env.Env()
	}
	return nil
}

// geminiLogin runs agy on its own cached Google sign-in (AgyLoginMarker); without one it errors with the sign-in hint.
func geminiLogin(model string, h *agent.Headless) ([]string, []string, error) {
	present, perr := geminiLoginProbe()
	if perr != nil {
		return nil, nil, fmt.Errorf("gemini: agy login marker unreadable: %w", perr)
	}
	if !present {
		s, _ := agent.LookupSchema("gemini")
		return nil, nil, errors.New("gemini: agy is not signed in; " + s.CLI.ConfigureHint)
	}
	if h != nil {
		return nil, nil, nil // headless argv is runAgy's (prompt over stdin stream-json)
	}
	return nil, []string{"--model", model}, nil
}

// geminiLoginProbe reports whether agy's cached Google login is on disk, plus the first real read failure.
func geminiLoginProbe() (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	for _, p := range agyLoginMarkers(home) {
		fi, serr := os.Stat(p)
		if serr == nil && fi.Size() > 0 {
			return true, nil
		}
		if serr != nil && !errors.Is(serr, fs.ErrNotExist) && err == nil {
			err = serr
		}
	}
	return false, err
}

// AgyLoginMarker is agy's cached Google OAuth token path under home, the marker a login run leaves behind.
func AgyLoginMarker(home string) string {
	return agyLoginMarkers(home)[0]
}

// agyLoginMarkers lists every path agy has used for its cached Google OAuth token, newest first.
func agyLoginMarkers(home string) []string {
	return []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token"),
		filepath.Join(home, ".gemini", "jetski-standalone-oauth-token"),
	}
}

// agyModelsTTL is how long one `agy models` read is reused; a refresh ignores it.
const agyModelsTTL = 24 * time.Hour

// ParseAgyModels is the ids in `agy models` output: a header line, then one "<id>\t<label>" row per model.
func ParseAgyModels(out []byte) []string {
	var ids []string
	for line := range strings.SplitSeq(string(out), "\n") {
		id, _, tab := strings.Cut(line, "\t")
		if id = strings.TrimSpace(id); tab && id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// AgyModels lists the models agy serves: a Databricks gateway profile's served Gemini list, else `agy models`.
func AgyModels(ctx context.Context, p *providers.Provider, refresh bool) ([]string, error) {
	if p != nil && p.Preset == providers.PresetDatabricks {
		return agyServedGemini(p, time.Now()), nil
	}
	ttl := agyModelsTTL
	if refresh {
		ttl = 0
	}
	valid := func(ids []string) ([]byte, error) { return json.Marshal(ids) }
	cache := paths.DiskTTL[[]string]{
		Name: "agy-models", Path: filepath.Join(paths.StateDir(), "agy-models.json"), TTL: ttl,
		Decode: func(b []byte) (ids []string, err error) { return ids, json.Unmarshal(b, &ids) }, Encode: valid,
	}
	return cache.Get(ctx, runAgyModels)
}

// agyModelsTimeout bounds one `agy models` run; a var so a test can shrink it.
var agyModelsTimeout = 15 * time.Second

func runAgyModels(ctx context.Context) ([]string, error) {
	bin, err := paths.AgentBin("agy")
	if err != nil {
		return nil, err
	}
	return execAgyModels(ctx, bin)
}

// execAgyModels runs `bin models` under agyModelsTimeout in its own process group, so a hung child tree dies too.
func execAgyModels(ctx context.Context, bin string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, agyModelsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "models")
	proc.OwnGroup(cmd)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("agy models timed out")
	}
	if err != nil {
		return nil, err
	}
	return ParseAgyModels(out), nil
}
