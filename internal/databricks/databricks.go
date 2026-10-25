// Package databricks launches Claude Code on Databricks model serving and runs the endpoint smoke checks.
package databricks

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/helptext"
)

const (
	defaultModel = "databricks-claude-opus-5-5[1m]"
	defaultEnv   = "qa"
	smokePrompt  = "Reply with exactly: OK"
	needlePrompt = "What is the secret word at the very end of this input? Reply with the word only."
	needleWord   = "ZEBRAFINCH"
	needleText   = "\nThe secret word is ZEBRAFINCH.\n"
	filler       = "The quick brown fox jumps over the lazy dog near riverbank.\n"
	fillerBytes  = 1_200_000
	betaOff      = "CLAUDE_CODE_DISABLE_EXPERIMENTAL_BETAS=1"
)

// Plan is one parsed invocation: the provider and model the child launches with, plus pass-through args.
type Plan struct {
	Check    bool
	Big      bool
	Provider string
	Model    string
	Rest     []string
}

// spawn runs bin with argv and env, returning the child's exit code (err only if it could not start).
// A package var so tests never launch the real CLI.
var spawn = func(bin string, argv, env []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(bin, argv...)
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode(), nil
	}
	return 0, err
}

// Run is the `claude-code databricks` verb; args follow the verb name and it returns the exit code.
func Run(args []string) int {
	// Help only as the first arg (or right after check); a later -h belongs to claude, as in the shell wrapper.
	head := args
	if len(head) > 0 && head[0] == "check" {
		head = head[1:]
	}
	if len(head) > 0 && helptext.WantsHelp(head[:1]) {
		usage()
		return 0
	}
	p, err := plan(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: %v\n", err)
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: cannot locate the binary: %v\n", err)
		return 2
	}
	if p.Check {
		return check(self, p)
	}
	return launch(self, p)
}

// plan parses args: optional leading check, optional MODEL, --env, --1m; everything else passes through.
func plan(args []string) (Plan, error) {
	p := Plan{Model: defaultModel}
	if len(args) > 0 && args[0] == "check" {
		p.Check = true
		args = args[1:]
	}
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		p.Model = args[0]
		args = args[1:]
	}
	env := defaultEnv
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--env":
			env = ""
			if i+1 < len(args) {
				env = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--env="):
			env = strings.TrimPrefix(arg, "--env=")
		case arg == "--1m":
			p.Big = true
		default:
			p.Rest = append(p.Rest, arg)
		}
	}
	if !slices.Contains([]string{"dev", "qa", "prod"}, env) {
		return Plan{}, fmt.Errorf("--env must be dev, qa or prod (got %q)", env)
	}
	p.Provider = "databricks-" + env
	if !isClaudeModel(p.Model) {
		p.Provider += "-oai"
	}
	return p, nil
}

// isClaudeModel reports whether model takes the Anthropic passthrough (else it goes via the router).
func isClaudeModel(model string) bool {
	return slices.Contains([]string{"opus", "sonnet", "haiku"}, model) || strings.HasPrefix(model, "databricks-claude-")
}

// childEnv is the parent environment plus the experimental-beta opt-out the passthrough needs.
func childEnv() []string {
	return append(os.Environ(), betaOff)
}

// launch execs this binary as `--provider P --model M rest...` with the parent's stdio.
func launch(self string, p Plan) int {
	argv := append([]string{"--provider", p.Provider, "--model", p.Model}, p.Rest...)
	code, err := spawn(self, argv, childEnv(), os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-code databricks: exec failed: %v\n", err)
		return 2
	}
	return code
}

// capture runs one prompt through the provider and returns combined stdout+stderr (a start error is appended).
func capture(self string, p Plan, prompt string, stdin io.Reader) string {
	var buf bytes.Buffer
	argv := []string{"--provider", p.Provider, "--model", p.Model, "-p", prompt}
	if _, err := spawn(self, argv, childEnv(), stdin, &buf, &buf); err != nil {
		fmt.Fprint(&buf, err)
	}
	return buf.String()
}

// check runs the smoke reply and, with --1m, the needle-at-the-end probe; exit 0 only if every check passes.
func check(self string, p Plan) int {
	rc := 0
	fmt.Printf("== smoke: %s %s\n", p.Provider, p.Model)
	if out := capture(self, p, smokePrompt, nil); lastLine(out) == "OK" {
		fmt.Println("PASS smoke")
	} else {
		fmt.Printf("FAIL smoke: %s\n", tail(out, 5))
		rc = 1
	}
	if !p.Big {
		return rc
	}
	body := needleBody()
	fmt.Printf("== 1m: ~300K tokens, needle at the end (%d bytes)\n", len(body))
	if out := capture(self, p, needlePrompt, strings.NewReader(body)); strings.Contains(out, needleWord) {
		fmt.Println("PASS 1m")
	} else {
		fmt.Printf("FAIL 1m: %s\n", tail(out, 5))
		rc = 1
	}
	return rc
}

// needleBody is fillerBytes of repeated sentence followed by the needle line.
func needleBody() string {
	n := fillerBytes/len(filler) + 1
	return strings.Repeat(filler, n)[:fillerBytes] + needleText
}

// lastLine returns the last non-empty line of s, trimmed.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, " \t\r\n"), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// tail returns the last n lines of s, trailing newlines dropped.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// page is the `-h` text; its FLAGS rows come from the FlagSet built in usage.
var page = helptext.Page{
	Usage: "claude-code databricks [check] [MODEL] [flags] [claude args...]",
	About: "Launch Claude Code on Databricks model serving. MODEL defaults to databricks-claude-opus-5-5[1m]. " +
		"opus, sonnet, haiku and databricks-claude-* use the Anthropic passthrough (databricks-<env>); " +
		"other endpoints go through the router (databricks-<env>-oai). " +
		"check runs a smoke reply; with --1m it also runs a ~1.2 MB needle-at-the-end probe.",
}

// usage prints the verb's help; the FlagSet only documents flags, plan does the parsing.
func usage() {
	fs := helptext.NewFlagSet("claude-code databricks", page)
	fs.String("env", defaultEnv, "Databricks workspace: dev, qa or prod")
	fs.Bool("1m", false, "with check: also run the 1M-context probe")
	fs.Usage()
}
