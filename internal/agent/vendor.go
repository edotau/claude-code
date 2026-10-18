package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/edotau/claude-code/internal/copilotsdk"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/vendors"
)

// ownLoginProvider is the Result.Provider for a vendor run on the credentials its own CLI stores.
const ownLoginProvider = "own-login"

// vendorTarget mirrors the shim (launch.Vendor): the session target when it can serve the vendor, else the
// vendor's own login or first keyed provider. Only --provider (or a provider:model) is the caller's provider
// choice; a slot name or model id is re-applied to the fallback provider. own is true when the vendor runs
// on its own stored login and no harness route is injected.
func vendorTarget(req Request, name string) (t providers.Target, own bool, err error) {
	reg, t, err := resolveTarget(req)
	if err != nil || req.Provider != "" || namesProvider(reg, req.Model) {
		return t, false, err
	}
	sel, err := providers.Select(reg, "")
	if err != nil {
		return t, false, err
	}
	ft, note, err := vendors.ResolveTarget(reg, name, sel, false)
	if note != "" {
		fmt.Fprintln(os.Stderr, note)
	}
	if errors.Is(err, vendors.ErrOwnLogin) {
		return ft, true, nil
	}
	if err != nil || ft.Provider == t.Provider {
		return t, false, err // no fallback: the caller's slot or model id stands
	}
	switch {
	case req.Model == "":
	case slices.Contains(models.Slots, req.Model):
		if m := ft.Provider.Models[req.Model]; m != "" {
			ft.Model = m
		}
	default:
		ft.Model = req.Model
	}
	return ft, false, nil
}

// namesProvider is true for the provider:model form, where the prefix is a registered provider.
func namesProvider(reg *providers.Registry, spec string) bool {
	if reg == nil {
		return false
	}
	name, _, ok := strings.Cut(spec, ":")
	_, found := reg.Providers[name]
	return ok && found
}

// vendorRunner execs a vendor CLI's headless mode wired by vendors.ConfigureHeadless.
type vendorRunner struct{ name string }

func (v vendorRunner) Name() string { return v.name }

// Caps follow each CLI's verified headless flags; gemini has no effort flag.
func (v vendorRunner) Caps() Caps {
	if v.name == "gemini" {
		return Caps{Model: true, Provider: true, WorkDir: true, SessionID: true, Tools: true}
	}
	return Caps{Model: true, Provider: true, Effort: true, WorkDir: true, SessionID: true, Tools: true}
}

func (v vendorRunner) Available() error {
	spec, _ := vendors.Lookup(v.name)
	_, err := paths.LookPathReal(spec.Binary)
	return err
}

func (v vendorRunner) Run(ctx context.Context, req Request, stream io.Writer) (Result, error) {
	t, own, err := vendorTarget(req, v.name)
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: ownLoginProvider, Model: t.Model, SessionID: req.SessionID}
	if !own {
		res.Provider = t.Provider.Name
	}
	if len(req.Prompt) > MaxPromptBytes {
		return res, &Error{Kind: KindFatal, Agent: v.name, Err: errPromptTooLarge(len(req.Prompt))}
	}
	h := vendors.Headless{Prompt: req.Prompt, Effort: req.Effort, SessionID: req.SessionID, Tools: req.Tools,
		Writable: gitCommonDirOutside(req)}
	var env, argv []string
	if own {
		env, argv, err = vendors.OwnLoginHeadless(v.name, h)
	} else {
		env, argv, err = vendors.ConfigureHeadless(ctx, v.name, t, h)
	}
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
	stdin := ""
	if v.name == "codex" {
		stdin = req.Prompt // codex exec reads the prompt from stdin behind its `-` operand
	}
	cmd, tail := childCmd(ctx, bin, argv, env, req.WorkDir, stdin)
	cmd.Stderr = io.MultiWriter(tail, &banner)
	switch v.name {
	case "opencode":
		return v.runStream(ctx, cmd, tail, res, stream, parseOpenCodeStream)
	case "gemini":
		return v.runStream(ctx, cmd, tail, res, stream, parseGeminiStream)
	}
	cmd.Stdout = io.MultiWriter(stream, &answer)
	err = cmd.Run()
	res.Answer = strings.TrimSpace(answer.String())
	if id := sessionFromBanner(banner.String()); id != "" {
		res.SessionID = id
	}
	if err == nil && res.Answer == "" {
		return res, emptyAnswer(v.name)
	}
	return res, childErr(ctx, v.name, err, tail)
}

// emptyAnswer: a clean exit with no text is a failure the caller must see, not a blank success.
func emptyAnswer(name string) error {
	return &Error{Kind: KindFatal, Agent: name, Err: errors.New("the CLI exited 0 with an empty answer (model returned no text)")}
}

// runStream reads a CLI's JSON event stream: the answer, session id, usage and errors ride in it.
func (v vendorRunner) runStream(ctx context.Context, cmd *exec.Cmd, tail *TailBuffer, res Result, stream io.Writer,
	parse func(io.Reader, io.Writer) (Result, error)) (Result, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return res, err
	}
	if err := cmd.Start(); err != nil {
		return res, childErr(ctx, v.name, err, tail)
	}
	out, perr := parse(stdout, stream)
	_, _ = io.Copy(io.Discard, stdout)
	werr := cmd.Wait()
	res.Answer, res.Usage = out.Answer, out.Usage
	if out.SessionID != "" {
		res.SessionID = out.SessionID
	}
	if werr != nil || ctx.Err() != nil {
		return res, childErr(ctx, v.name, errors.Join(werr, perr), tail)
	}
	if perr == nil && res.Answer == "" {
		return res, emptyAnswer(v.name)
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
	t, _, err := vendorTarget(req, "copilot")
	if err != nil {
		return Result{}, err
	}
	res := Result{Provider: t.Provider.Name, Model: models.Strip1M(t.Model)}
	env, _, err := vendors.Configure(ctx, "copilot", t, "")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	bin, err := paths.LookPathReal("copilot")
	if err != nil {
		return res, &Error{Kind: KindFatal, Agent: "copilot", Err: err}
	}
	var model string
	if t.Provider.Auth.Login != "" {
		model = models.Strip1M(t.Model) // no BYOK block, so the session takes the catalog id itself ([1m] is ours)
	}
	out, err := copilotsdk.Ask(ctx, copilotsdk.Options{
		CLIPath: bin, Env: env, Model: model, ReasoningEffort: req.Effort, WorkingDirectory: req.WorkDir,
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

type geminiEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	Error     struct {
		Message string `json:"message"`
	} `json:"error"`
	Stats struct {
		Total int `json:"total_tokens"`
		Input int `json:"input_tokens"`
	} `json:"stats"`
}

// parseGeminiStream reads `gemini --output-format stream-json`: assistant deltas, then one result event.
func parseGeminiStream(r io.Reader, w io.Writer) (Result, error) {
	var res Result
	var answer strings.Builder
	dec := json.NewDecoder(r)
	for {
		var ev geminiEvent
		if err := dec.Decode(&ev); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return res, fmt.Errorf("decode gemini stream-json: %w", err)
		}
		switch ev.Type {
		case "init":
			res.SessionID = ev.SessionID
		case "message":
			if ev.Role != "assistant" {
				continue
			}
			if _, err := io.WriteString(w, ev.Content); err != nil {
				return res, err
			}
			answer.WriteString(ev.Content)
		case "result":
			// output_tokens omits thinking; total minus input counts it, as opencode's reasoning does.
			res.Usage = Usage{InputTokens: ev.Stats.Input, OutputTokens: max(ev.Stats.Total-ev.Stats.Input, 0)}
			if ev.Status != "success" {
				res.Answer = strings.TrimSpace(answer.String())
				return res, errors.New(cmp.Or(ev.Error.Message, "gemini run "+cmp.Or(ev.Status, "failed")))
			}
		}
	}
	res.Answer = strings.TrimSpace(answer.String())
	return res, nil
}

// gitCommonDirOutside is the workdir's git common dir when a tools run's workdir is a linked worktree: its index,
// objects and refs live in the main repo's .git, outside a workspace sandbox, so `git commit` there would fail.
func gitCommonDirOutside(req Request) []string {
	if !req.Tools || req.WorkDir == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", req.WorkDir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return nil
	}
	dir := strings.TrimSpace(string(out))
	if rel, err := filepath.Rel(req.WorkDir, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return nil // an ordinary checkout: .git is inside the workspace already
	}
	return []string{dir}
}
