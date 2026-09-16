package agent

import (
	"context"
	"io"
	"strings"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/vendors"
)

// vendorRunner execs a vendor CLI's headless mode wired by vendors.ConfigureHeadless.
type vendorRunner struct{ name string }

func (v vendorRunner) Name() string { return v.name }

// Caps follow each CLI's verified headless flags; gemini has no effort/session/tool channel wired.
func (v vendorRunner) Caps() Caps {
	if v.name == "gemini" {
		return Caps{Model: true, Provider: true, WorkDir: true}
	}
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true}
}

func (v vendorRunner) Available() error {
	spec, _ := vendors.Lookup(v.name)
	_, err := paths.LookPathReal(spec.Binary)
	return err
}

func (v vendorRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	_, t, err := resolveTarget(req)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: t.Provider.Name, Model: t.Model, SessionID: req.SessionID}
	if len(req.Prompt) > MaxPromptBytes {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: errPromptTooLarge(len(req.Prompt))}
	}
	env, argv, err := vendors.ConfigureHeadless(ctx, v.name, t, vendors.Headless{
		Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools})
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	spec, _ := vendors.Lookup(v.name)
	bin, err := paths.LookPathReal(spec.Binary)
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: err}
	}
	var answer strings.Builder
	var banner headBuffer
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, "")
	cmd.Stdout = io.MultiWriter(stream, &answer)
	cmd.Stderr = io.MultiWriter(tail, &banner)
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	return res, childErr(ctx, v.name, err, tail)
}

// headBuffer keeps the first 4 KiB of a stream: codex prints the session id in its stderr banner.
type headBuffer struct{ buf []byte }

func (h *headBuffer) Write(p []byte) (int, error) {
	if room := 4<<10 - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (h *headBuffer) String() string { return string(h.buf) }

// sessionFromBanner reads codex exec's `session id: <id>` line, so a new run's envelope can be resumed.
func sessionFromBanner(banner string) string {
	for _, line := range strings.Split(banner, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "session id:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// copilotRunner drives the copilot CLI through the SDK with the BYOK block; tools are denied unless asked.
type copilotRunner struct{}

func (copilotRunner) Name() string { return "copilot" }
func (copilotRunner) Caps() Caps {
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, Tools: true}
}
func (copilotRunner) Available() error { _, err := paths.LookPathReal("copilot"); return err }

func (copilotRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	_, t, err := resolveTarget(req)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: t.Provider.Name, Model: t.Model}
	env, _, err := vendors.Configure(ctx, "copilot", t, "")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	bin, err := paths.LookPathReal("copilot")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	out, err := copilotsdk.Ask(ctx, copilotsdk.Options{
		CLIPath: bin, Env: env, ReasoningEffort: req.Effort, WorkingDirectory: req.WorkDir,
		AllowAllTools: req.Tools, UseLoggedInUser: true,
	}, req.Prompt)
	if err != nil {
		return res, err
	}
	io.WriteString(stream, out.Text)
	if out.Model != "" {
		res.Model = out.Model
	}
	res.SessionID, res.Answer = out.SessionID, out.Text
	return res, nil
}
