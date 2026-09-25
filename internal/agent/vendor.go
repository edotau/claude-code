package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	cmd.Stderr = io.MultiWriter(tail, &banner)
	if v.name == "opencode" {
		return v.runOpenCode(ctx, cmd, tail, res, stream)
	}
	cmd.Stdout = io.MultiWriter(stream, &answer)
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	return res, childErr(ctx, v.name, err, tail)
}

// runOpenCode reads `opencode run --format json` events: the answer, session id and usage ride in them.
func (v vendorRunner) runOpenCode(ctx context.Context, cmd *exec.Cmd, tail *TailBuffer, res Result, stream io.Writer) (Result, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, v.name, err, tail)
	}
	out, perr := parseOpenCodeStream(stdout, stream)
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, v.name, errors.Join(werr, perr), tail)
	}
	return res, perr
}

type openCodeEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionID"`
	Part      struct {
		Text   string `json:"text"`
		Tokens struct {
			Input, Output, Reasoning int
			Cache                    struct{ Read, Write int }
		} `json:"tokens"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// parseOpenCodeStream streams the root session's text parts (opencode filters subagents); error events go to stdout.
func parseOpenCodeStream(r io.Reader, w io.Writer) (Result, error) {
	var res Result
	var texts, errs []string
	dec := json.NewDecoder(r)
	for {
		var ev openCodeEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode opencode json: %w", err)
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Type {
		case "text":
			if ev.Part.Text == "" {
				continue
			}
			if len(texts) > 0 {
				io.WriteString(w, "\n")
			}
			if _, err := io.WriteString(w, ev.Part.Text); err != nil {
				return res, err
			}
			texts = append(texts, ev.Part.Text)
		case "step_finish":
			t := ev.Part.Tokens
			res.Usage.InputTokens += t.Input + t.Cache.Read + t.Cache.Write
			res.Usage.OutputTokens += t.Output + t.Reasoning
		case "error":
			msg := cmp.Or(ev.Error.Data.Message, ev.Error.Name, "unknown error")
			errs = append(errs, msg)
		}
	}
	res.Answer = strings.TrimSpace(strings.Join(texts, "\n"))
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
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
