package agent

import (
	"context"
	"fmt"
	"io"
	"os"
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
	s, _ := LookupSchema(e.name)
	bin, err := paths.VendorBin(s.CLI.Binary)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: e.name, Err: err}
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
		t, _ := providers.ResolveAgentModel(providers.ModelAsk{Agent: e.name, Target: providers.Target{Provider: p, Model: model}})
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
