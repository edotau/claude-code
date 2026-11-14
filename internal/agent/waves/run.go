package waves

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/memory"
	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// Options is the dispatch --waves library contract.
type Options struct {
	Plan                        string
	From, To                    int
	DryRun, Status, Fresh, JSON bool
	Gate                        string
	Route                       []string
	Provider                    string
	Profiles                    []string
	Timeout                     time.Duration
	Dir                         string
	Executable                  string
	Out, Err                    io.Writer
}

type runHooks struct {
	launch func(context.Context, string, []Spec, LaunchOptions) ([]Envelope, error)
	check  CheckEnv
}

type runner struct {
	o                        Options
	h                        runHooks
	state                    State
	path, dir, slug, reports string
}

// Run prints the wave report and returns the dispatch exit code.
func Run(ctx context.Context, o Options) int { return runWith(ctx, o, runHooks{launch: Launch}) }

func runWith(ctx context.Context, o Options, h runHooks) int {
	if o.Out == nil {
		o.Out = os.Stdout
	}
	if o.Err == nil {
		o.Err = os.Stderr
	}
	r := runner{o: o, h: h}
	code, err := r.run(ctx)
	if err != nil {
		fmt.Fprintln(o.Err, err)
		if code == 0 {
			code = 1
		}
	}
	if err := render(o.Out, r.state, o.JSON); err != nil {
		fmt.Fprintln(o.Err, err)
		return 1
	}
	return code
}

func (r *runner) run(ctx context.Context) (int, error) {
	if err := r.prepare(); err != nil {
		return 2, err
	}
	if r.o.Status {
		s, err := StateLoad(r.path)
		r.state = s
		if err != nil {
			return 1, err
		}
		return 0, nil
	}
	parse := plans.ParseWaves
	if filepath.Ext(r.o.Plan) == ".jsonl" {
		parse = plans.ParseManifest
	}
	p, err := parse(r.o.Plan)
	if err != nil {
		return 2, err
	}
	if err = r.validate(p); err != nil {
		return 2, err
	}
	if r.o.DryRun {
		r.preview(p)
		return 0, nil
	}
	if ctx.Err() != nil {
		r.state.Next = "stopped, same command resumes"
		return 4, ctx.Err()
	}
	if err = r.load(); err != nil {
		return 1, err
	}
	return r.execute(ctx, p)
}

func (r *runner) execute(ctx context.Context, p plans.WavePlan) (int, error) {
	for _, w := range p.Waves {
		if !r.selected(w.Number) {
			continue
		}
		w.Tasks = append(append([]plans.WaveTask(nil), w.Tasks...), p.Manual...)
		code, err := r.wave(ctx, w)
		if code != 0 || err != nil {
			return code, err
		}
	}
	r.state.Next = ""
	return 0, r.state.Save(r.path)
}

func (r *runner) prepare() error {
	var err error
	r.o.Dir, err = filepath.Abs(r.o.Dir)
	if err != nil {
		return err
	}
	r.o.Plan, err = filepath.Abs(r.o.Plan)
	if err != nil {
		return err
	}
	r.slug = paths.Slug(strings.TrimSuffix(filepath.Base(r.o.Plan), filepath.Ext(r.o.Plan)), "._-", 50, "plan")
	store, err := memory.RepoDir(r.o.Dir)
	if err != nil {
		return err
	}
	r.dir = filepath.Join(store, "waves", r.slug)
	r.path, r.reports = filepath.Join(r.dir, "state.json"), memory.ReportsDir(store)
	r.state.Plan = r.o.Plan
	return nil
}

func (r *runner) selected(n int) bool {
	return (r.o.From == 0 || n >= r.o.From) && (r.o.To == 0 || n <= r.o.To)
}

func (r *runner) validate(p plans.WavePlan) error {
	if r.o.From < 0 || r.o.To < 0 || (r.o.To != 0 && r.o.From > r.o.To) || r.o.Timeout < 0 {
		return errors.New("invalid wave range or timeout")
	}
	found := false
	for _, w := range p.Waves {
		if r.selected(w.Number) {
			found = true
		}
		if err := GateRefusal(r.gate(w)); err != nil {
			return err
		}
	}
	if !found {
		return errors.New("no waves in requested range")
	}
	return nil
}

func (r *runner) gate(w plans.Wave) string {
	if r.o.Gate != "" {
		return r.o.Gate
	}
	return w.Gate
}

func (r *runner) load() error {
	if _, err := memory.EnsureRepoDir(r.o.Dir); err != nil {
		return err
	}
	if !r.o.Fresh {
		s, err := StateLoad(r.path)
		if err == nil {
			r.state = s
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	base, dirty, err := StateGitHead(r.o.Dir)
	if err != nil {
		return err
	}
	r.state, err = StateStart(base, dirty, FingerprintGit(r.o.Dir))
	r.state.Plan = r.o.Plan
	if err != nil {
		return err
	}
	return r.state.Save(r.path)
}

func (r *runner) specs(w plans.Wave) ([]Spec, []string) {
	var specs []Spec
	var manual []string
	for _, t := range w.Tasks {
		if t.Manual {
			manual = append(manual, t.ID)
			continue
		}
		owns := make([]string, len(t.Owns))
		for i, own := range t.Owns {
			owns[i] = filepath.Join(r.o.Dir, own)
		}
		specs = append(specs, Spec{ID: strings.TrimPrefix(t.ID, "Phase "), Agent: t.Agent, Profile: t.Profile,
			Task: t.Task, Owns: owns, Workdir: r.o.Dir, Verify: t.Verify})
	}
	return specs, manual
}

func (r *runner) wave(ctx context.Context, w plans.Wave) (int, error) {
	specs, ws, launch, skip, err := r.planWave(w)
	if err != nil {
		return 1, err
	}
	if skip {
		return 0, nil
	}
	envelopes, launchErr := r.launch(ctx, launch, specs)
	ws, err = r.record(ws, envelopes)
	if err != nil {
		return 1, err
	}
	if ctx.Err() != nil || errors.Is(launchErr, context.DeadlineExceeded) || hasRunning(ws) {
		ws.Status, ws.Reason = "stopped", "stopped, same command resumes"
		return 4, r.finish(ws)
	}
	code, err := r.verify(ctx, w, ws, specs, launchErr)
	if err != nil {
		ws, _ = r.state.Wave(w.Number)
		ws.Status, ws.Reason = WaveStateFailed, err.Error()
		if ctx.Err() != nil {
			code, ws.Status = 4, "stopped"
		}
		return code, errors.Join(err, r.finish(ws))
	}
	return code, nil
}

func (r *runner) save(w WaveState) error {
	r.state = r.state.WithWave(w)
	return r.state.Save(r.path)
}

func (r *runner) finish(w WaveState) error {
	if w.Status != WaveStatePassed {
		r.state.Next = r.rerun(w.N)
	}
	return r.save(w)
}

func (r *runner) launch(ctx context.Context, specs, wave []Spec) ([]Envelope, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return r.h.launch(ctx, r.o.Executable, specs, LaunchOptions{Slug: r.slug, ReportsDir: r.reports,
		Provider: r.o.Provider, Profiles: r.o.Profiles, Route: r.o.Route, Timeout: r.o.Timeout, Wave: wave,
		Fingerprint: r.fingerprint})
}

func (r *runner) planWave(w plans.Wave) ([]Spec, WaveState, []Spec, bool, error) {
	specs, manual := r.specs(w)
	ids := make([]string, len(specs))
	for i, s := range specs {
		ids[i] = s.ID
	}
	decision, err := Decide(r.state, w.Number, ids, false, r.fingerprint)
	if err != nil {
		return nil, WaveState{}, nil, false, err
	}
	if decision.Wave == DecideSkip {
		return nil, WaveState{}, nil, true, nil
	}
	ws, _ := r.state.Wave(w.Number)
	ws.N, ws.Status, ws.Manual = w.Number, WaveStateRunning, manual
	ws.Reason, ws.Outside, ws.Unattributed, ws.Gates, ws.Verifier, ws.Violations = "", nil, nil, nil, nil, nil
	var launch []Spec
	for i, d := range decision.Leaves {
		if d.Action == DecideKeep {
			continue
		}
		if specs[i].Profile == "" && len(r.o.Profiles) > 0 {
			specs[i].Profile = r.o.Profiles[len(launch)%len(r.o.Profiles)]
		}
		launch = append(launch, specs[i])
		ws = ws.WithLeaf(LeafState{ID: specs[i].ID, Agent: specs[i].Agent, Profile: specs[i].Profile,
			Owns: specs[i].Owns, Status: WaveStateRunning})
	}
	if err = r.save(ws); err != nil {
		return nil, WaveState{}, nil, false, err
	}
	return specs, ws, launch, false, nil
}

func (r *runner) rerun(n int) string {
	args := []string{"claude-code", "agents", "dispatch", "--waves", r.o.Plan, "--wave", strconv.Itoa(n)}
	for _, pair := range [][2]string{{"--gate", r.o.Gate}, {"--provider", r.o.Provider}, {"--profiles", strings.Join(r.o.Profiles, ",")}} {
		if pair[1] != "" {
			args = append(args, pair[:]...)
		}
	}
	args = append(args, r.o.Route...)
	for i, arg := range args {
		args[i] = "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
	}
	return strings.Join(args, " ")
}
