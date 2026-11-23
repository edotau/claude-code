package waves

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/paths"
	"github.com/edotau/claude-code/internal/plans"
)

// Options is the ask --waves library contract.
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
	notify                      func(w WaveState, report string) // a background job's per-wave notice hook
}

type runHooks struct {
	launch func(context.Context, string, []Spec, LaunchOptions) ([]Envelope, error)
	check  CheckEnv
}

type runner struct {
	o                        Options
	h                        runHooks
	state                    State
	plan                     plans.WavePlan // read once; Blocked by looks up ticks and waves across it
	path, dir, slug, reports string
	day                      time.Time              // fixed at start so report names agree across midnight
	launched                 map[string]bool        // leaf ids this run launches, in any wave; their kept dependents relaunch
	checked                  map[string]checkResult // verify results taken at release, reused by verifyLeaf
}

type checkResult struct {
	passed bool
	result *stepResult
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
	r.plan = p
	if r.o.DryRun {
		r.preview(p)
		return 0, nil
	}
	if ctx.Err() != nil {
		r.state.Next = "stopped, same command resumes"
		return 4, ctx.Err()
	}
	return r.locked(ctx, p)
}

// locked loads the resume state and runs the plan under its run lock.
func (r *runner) locked(ctx context.Context, p plans.WavePlan) (int, error) {
	unlock, err := r.lock()
	if err != nil {
		return 1, err
	}
	defer unlock()
	if err = r.load(); err != nil {
		return 1, err
	}
	return r.execute(ctx, p)
}

// lock holds the plan's run lock, so a second run of one plan cannot launch duplicate writers.
func (r *runner) lock() (func(), error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "run.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if !paths.TryLock(f) {
		_ = f.Close()
		return nil, fmt.Errorf("another run of %s holds %s", r.o.Plan, f.Name())
	}
	return func() { _ = f.Close() }, nil
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
	r.slug = planSlug(r.o.Plan)
	r.day = time.Now()
	// keyed by the full path too: two repos' same-named plans never share resume state
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(r.o.Plan)))[:8]
	r.dir, r.reports = filepath.Join(paths.StateDir(), "waves", r.slug+"-"+key), paths.MemorySubDir("reports")
	r.path = filepath.Join(r.dir, "state.json")
	r.state.Plan = r.o.Plan
	r.adoptLegacy(filepath.Join(paths.StateDir(), "waves", r.slug, "state.json"))
	return nil
}

// adoptLegacy moves this plan's pre-hash <slug>/state.json to the keyed path, so an in-flight run still resumes.
func (r *runner) adoptLegacy(old string) {
	if _, err := os.Stat(r.path); !errors.Is(err, os.ErrNotExist) {
		return
	}
	if s, err := StateLoad(old); err != nil || s.Plan != r.o.Plan {
		return
	}
	if os.MkdirAll(r.dir, 0o700) == nil && os.Rename(old, r.path) == nil {
		return
	}
	r.path = old // the move failed: resume from the old file in place
}

// planSlug names a plan's state dir, reports and notices.
func planSlug(plan string) string {
	return paths.Slug(strings.TrimSuffix(filepath.Base(plan), filepath.Ext(plan)), "._-", 50, "plan")
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
	if !r.o.Fresh {
		s, err := StateLoad(r.path)
		if err == nil && s.Plan != r.o.Plan {
			return fmt.Errorf("%s belongs to plan %s, not %s; --fresh starts over", r.path, s.Plan, r.o.Plan)
		}
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
		if t.Done { // ticked or struck: the work is settled, so no leaf runs it again
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
	defer r.notify(w.Number)
	_, launchErr := r.launchRounds(ctx, launch, specs, r.waits(w), func(envelopes []Envelope) error {
		ws, err = r.record(ws, envelopes)
		return err
	})
	if err != nil {
		return 1, err
	}
	if ctx.Err() != nil {
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

// notify writes wave n's report and hands it to the notice hook; foreground runs have no hook.
func (r *runner) notify(n int) {
	ws, ok := r.state.Wave(n)
	if r.o.notify == nil || !ok {
		return
	}
	var buf bytes.Buffer
	_ = renderWave(&buf, ws, r.state.Base)
	report := filepath.Join(r.reports, dateFirst(r.slug+"-wave-"+strconv.Itoa(n), r.day)+".md")
	err := os.MkdirAll(r.reports, 0o700)
	if err == nil {
		err = os.WriteFile(report, buf.Bytes(), 0o600)
	}
	if err != nil { // the state file still holds the whole wave
		report = r.path
	}
	r.o.notify(ws, report)
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
	return r.h.launch(ctx, r.o.Executable, specs, LaunchOptions{Slug: r.slug, ReportsDir: r.reports, Day: r.day,
		Provider: r.o.Provider, Profiles: r.o.Profiles, Route: r.o.Route, Timeout: r.o.Timeout, Wave: wave,
		Fingerprint: r.fingerprint})
}

func (r *runner) planWave(w plans.Wave) ([]Spec, WaveState, []Spec, bool, error) {
	specs, manual := r.specs(w)
	ids := make([]string, len(specs))
	for i, s := range specs {
		ids[i] = s.ID
	}
	decision, err := Decide(r.state, w.Number, ids, r.fingerprint)
	if err != nil {
		return nil, WaveState{}, nil, false, err
	}
	if decision.Wave == DecideSkip {
		return nil, WaveState{}, nil, true, nil
	}
	ws, _ := r.state.Wave(w.Number)
	ws.N, ws.Status, ws.Manual = w.Number, WaveStateRunning, manual
	ws.Reason, ws.Outside, ws.Unattributed, ws.Gates, ws.Verifier, ws.Violations = "", nil, nil, nil, nil, nil
	ws.Fallback = nil
	ws.NoVerify = nil
	var launch []Spec
	for i, d := range r.staleKeeps(w, decision.Leaves) {
		if specs[i].Verify == "" {
			ws.NoVerify = append(ws.NoVerify, specs[i].ID)
		}
		if d.Action == DecideKeep {
			continue
		}
		if specs[i].Profile == "" && len(r.o.Profiles) > 0 {
			specs[i].Profile = r.o.Profiles[i%len(r.o.Profiles)] // by plan position: a resume keeps each leaf's profile
		}
		launch = append(launch, specs[i])
		ws = ws.WithLeaf(LeafState{ID: specs[i].ID, Agent: specs[i].Agent, Profile: specs[i].Profile,
			Owns: specs[i].Owns, Status: WaveStateRunning})
	}
	if err = r.snapshotDirty(len(launch) > 0, ws); err != nil {
		return nil, WaveState{}, nil, false, err
	}
	return specs, ws, launch, false, nil
}

// snapshotDirty adds the files dirty before a launch to the snapshot, so a leaf answers only for its own run's writes.
func (r *runner) snapshotDirty(launching bool, ws WaveState) error {
	if !launching {
		return r.save(ws)
	}
	_, dirty, err := StateGitHead(r.o.Dir)
	if err != nil {
		return err
	}
	now, err := FingerprintAll(FingerprintGit(r.o.Dir), dirty)
	if err != nil {
		return err
	}
	merged := maps.Clone(r.state.Dirty)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, now)
	r.state.Dirty = merged
	return r.save(ws)
}

// staleKeeps relaunches a kept leaf whose blocker launches in this run, transitively, so it reruns on new work.
func (r *runner) staleKeeps(w plans.Wave, leaves []DecideLeaf) []DecideLeaf {
	if r.launched == nil {
		r.launched = map[string]bool{}
	}
	blockedBy := map[string][]string{}
	for _, t := range w.Tasks {
		for _, b := range t.BlockedBy {
			blockedBy[specID(t.ID)] = append(blockedBy[specID(t.ID)], specID(b))
		}
	}
	out := slices.Clone(leaves)
	for _, d := range out { // seeded first: a dependent may precede its blocker in the wave
		if d.Action != DecideKeep {
			r.launched[d.ID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for i, d := range out {
			if d.Action != DecideKeep {
				continue
			}
			if j := slices.IndexFunc(blockedBy[d.ID], func(id string) bool { return r.launched[id] }); j >= 0 {
				out[i] = DecideLeaf{ID: d.ID, Action: DecideRelaunch, Why: "blocker relaunched: " + blockedBy[d.ID][j]}
				r.launched[d.ID], changed = true, true
			}
		}
	}
	return out
}

func (r *runner) rerun(n int) string {
	args := []string{"claude-code", "agents", "ask", "--waves", r.o.Plan, "--wave", strconv.Itoa(n)}
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
