package gemini

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/proxy"
	"github.com/edotau/claude-code/internal/vault"
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
