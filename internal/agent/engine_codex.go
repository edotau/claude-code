package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
)

type codexEngine struct{ baseEngine }

var _ headlessEngine = codexEngine{}

func (codexEngine) attach(prompt string, paths []string, _ bool) (string, error) {
	_, rest := splitImages(paths)
	return attachInline(prompt, rest)
}

func (codexEngine) before(req RunOptions, t providers.Target) providers.Target {
	if req.Autonomous {
		fmt.Fprintln(os.Stderr, "ask: codex exec has no ask-step to disable; --tools already grants workspace-write")
	}
	return t
}

func (codexEngine) ranModel(t providers.Target) string { return codexRanModel(t) }

func (e codexEngine) exec(ctx context.Context, p vendorPrep, req RunOptions, sink Sink) (Result, error) {
	res, prompt, env, argv := p.res, p.prompt, p.env, p.argv
	bin, err := e.bin()
	if err != nil {
		return res, err
	}
	var extra []string
	if req.SystemPrompt != "" {
		extra = append(extra, "-c", "instructions="+tomlQuote(req.SystemPrompt))
	}
	if req.AppendSystemPrompt != "" {
		extra = append(extra, "-c", "developer_instructions="+tomlQuote(req.AppendSystemPrompt))
	}
	if req.OutputSchema != "" {
		file, err := codexSchemaFile(req.OutputSchema)
		if err != nil {
			return res, &Error{Kind: KindFatal, Agent: e.name, Err: err}
		}
		defer func() { _ = os.Remove(file) }()
		extra = append(extra, "--output-schema", file)
	}
	codexJSON := codexHasJSON()
	if codexJSON {
		extra = append(extra, "--json")
	}
	argv = codexExecArgs(codexImageArgs(argv, req.Attach), extra...)
	var answer strings.Builder
	var banner headBuffer
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, prompt, req.KeepEnv, true)
	cmd.Stderr = io.MultiWriter(cmd.Stderr, &banner)
	if codexJSON {
		return runStream(ctx, e.name, cmd, tail, res, sink, parseCodexStream)
	}
	cmd.Stdout = teeWriter(ctx, io.MultiWriter(sinkWriter(sink), &answer))
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	if err == nil && res.Answer == "" {
		return res, emptyAnswer(e.name)
	}
	return res, childErr(ctx, e.name, err, tail)
}

func (codexEngine) login(_ *providers.Provider, model string, h *Headless) ([]string, []string, error) {
	return codexLogin(model, h)
}

func (codexEngine) wire(_ context.Context, r wireRoute, h *Headless) ([]string, []string, error) {
	return codex(r.Provider, r.Base, r.Model, r.Cred, h)
}

func (e codexEngine) fallbackTarget(reg *providers.Registry) (providers.Target, string, bool) {
	if p, ok := loginProvider(reg, "openai", "codex"); ok && codexChatGPTLogin() == nil {
		// Codex's own config.toml model beats the catalog; the model pin beats both.
		model := p.Models[models.Opus]
		if own := codexOwnModel(); own != "" {
			model = own
		}
		t, _ := ResolveAgentModel(ModelAsk{Agent: e.name, Target: providers.Target{Provider: p, Model: model}})
		return t, "chatgpt (codex login)", true
	}
	return providers.Target{}, "", false
}

func (codexEngine) LoginStatus() (bool, string) {
	if CodexChatGPTLoginPresent() {
		return true, "ChatGPT login (codex login)"
	}
	return false, ""
}

func (codexEngine) checkLogin(ctx context.Context) (providers.AuthVerdict, string) {
	bin, err := paths.VendorBin("codex")
	if err != nil {
		return providers.VerdictTransient, err.Error()
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := codexStatus(ctx, bin)
	var ee *exec.ExitError
	switch {
	case err == nil:
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		line, _, _ = strings.Cut(line, " - ") // drop the account/key tail codex appends
		return providers.VerdictOK, line
	case errors.As(err, &ee):
		return providers.VerdictNeedsLogin, ""
	}
	return providers.VerdictTransient, err.Error()
}

func (codexEngine) render(reg *providers.Registry, sel providers.Selection, _ string, o RenderOptions, out io.Writer) (bool, error) {
	return renderCodex(reg, sel, o, out)
}

func (codexEngine) configPaths(home string) ([]labelPath, error) {
	path, err := CodexConfigPath(home)
	if err != nil {
		return nil, err
	}
	return []labelPath{{"codex", path}}, nil
}

// launchArgs prepends the -f file's codex overrides (after the harness prefix, so the file's keys win).
func (codexEngine) launchArgs(file string, args []string) ([]string, error) {
	if file == "" {
		return args, nil
	}
	overrides, err := codexFileOverrides(file)
	if err != nil {
		return nil, err
	}
	return append(overrides, args...), nil
}

func (codexEngine) profileHint(o LaunchOptions) error { return codexProfileHint(o) }

func (codexEngine) routeDetail() (string, bool) { return codexRouteDetail(), true }
