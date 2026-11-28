package cli

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/edotau/claude-code/internal/cli/ui/helptext"
	"github.com/edotau/claude-code/internal/routing/jev"
)

// jevOpts are `gateway jev`'s flag values.
type jevOpts struct {
	status, train, attach, stop, options, worker bool // worker: hidden re-exec of --train --bg
	bg, fg, fresh                                bool
	labels, tasks, trainer, out, serve, service  string
	labelsSet, tasksSet                          bool
	format                                       *string
	extra                                        []string
}

func jevFlags() (*flag.FlagSet, *jevOpts) {
	fs := helptext.NewFlagSet("gateway jev", helptext.Page{
		Usage: "claude-code gateway jev [flags] [-- trainer options]",
		About: "Operate the Open-Jev classifier: its training job and the server's state. No flag = --status.\n" +
			"The job's files live in <out>/job; --trainer and --out are remembered after first use.\n" +
			"Options after -- go to the trainer and are kept across restarts of that job.",
	})
	o := &jevOpts{}
	fs.BoolVar(&o.status, "status", false, "Jev server up/down, the job's state, passes, labels, options, latest log lines")
	fs.BoolVar(&o.train, "train", false, "start or continue a job detached, then watch it; Ctrl-C only detaches")
	fs.BoolVar(&o.bg, "bg", false, "with --train: detached without watching")
	fs.BoolVar(&o.fg, "fg", false, "with --train: run inside this terminal; Ctrl-C stops the job")
	fs.BoolVar(&o.fresh, "fresh", false, "with --train: forget the previous job and start over")
	fs.StringVar(&o.labels, "labels", "", "with --train on a new job: the jury label directory")
	fs.StringVar(&o.tasks, "tasks", "", "with --train on a new job: run this one pass instead of tier,agent then tier")
	fs.BoolVar(&o.attach, "attach", false, "follow the running job's output until it ends; Ctrl-C detaches")
	fs.BoolVar(&o.stop, "stop", false, "stop the job's worker and trainer processes; --train continues it")
	fs.BoolVar(&o.options, "options", false, "print the trainer's own option list")
	fs.StringVar(&o.trainer, "trainer", "", "directory holding run-finetune-jev.sh (remembered after first use)")
	fs.StringVar(&o.serve, "serve", "", "serve this checkpoint: it must be a run's base or have passed the gold gate")
	fs.StringVar(&o.service, "service", "", "the server's login item file (remembered after first use)")
	fs.StringVar(&o.out, "out", "", "output directory for checkpoints and reports (remembered after first use)")
	o.format = helptext.FormatFlagWithUsage(fs, "output: text|json (json: --status only)")
	return fs, o
}

// jevSplitArgs separates the flags from the trainer options after the first "--".
func jevSplitArgs(args []string) (flags, extra []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// check rejects stray args and flag combinations; ok is false when code is the exit status.
func (o *jevOpts) check(fs *flag.FlagSet) (code int, ok bool) {
	actions := 0
	for _, b := range []bool{o.status, o.train, o.attach, o.stop, o.options, o.serve != ""} {
		if b {
			actions++
		}
	}
	if fs.NArg() > 0 {
		return helptext.FailUsage("gateway jev: unexpected argument %q (trainer options go after --)", fs.Arg(0)), false
	}
	if msg := o.combo(actions); msg != "" {
		return helptext.FailUsage("gateway jev: %s", msg), false
	}
	return 0, true
}

// trainFlags reports any flag that only --train takes.
func (o *jevOpts) trainFlags() bool {
	return o.bg || o.fg || o.fresh || o.labelsSet || o.tasksSet || len(o.extra) > 0
}

// combo names the first bad flag combination, else "".
func (o *jevOpts) combo(actions int) string {
	only := o.trainFlags()
	switch {
	case o.worker && (actions > 0 || only):
		return "--worker is reserved for detached launches"
	case actions > 1:
		return "at most one of --status, --train, --attach, --stop, --options, --serve"
	case !o.train && only:
		return "--bg, --fg, --fresh, --labels, --tasks and -- options need --train"
	case o.bg && o.fg:
		return "--bg and --fg are two ways to start; pick one"
	case *o.format == helptext.JSON && actions > 0 && !o.status:
		return "--format json applies to --status only"
	}
	return ""
}

// cmdGatewayJev is `gateway jev`: at most one action flag, no flag = --status; stray args are errors.
func cmdGatewayJev(args []string) int {
	flags, extra := jevSplitArgs(args)
	fs, o := jevFlags()
	if len(flags) > 0 && flags[0] == "--worker" { // the detached re-exec of --train --bg, not in --help
		flags, o.worker = flags[1:], true
	}
	if code, done := helptext.Parse(fs, flags); done {
		return code
	}
	o.extra = extra
	fs.Visit(func(f *flag.Flag) {
		o.labelsSet = o.labelsSet || f.Name == "labels"
		o.tasksSet = o.tasksSet || f.Name == "tasks"
	})
	if code, ok := o.check(fs); !ok {
		return code
	}
	j, err := jev.Resolve(o.trainer, o.out, o.service)
	if err != nil {
		return jevFail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return jevRun(ctx, j, o)
}

// jevRun runs the one action the flags name; none is --status.
func jevRun(ctx context.Context, j jev.Job, o *jevOpts) int {
	switch {
	case o.worker:
		return jevFail(j.Worker(ctx, os.Stdout))
	case o.train:
		return jevTrain(ctx, j, o)
	case o.attach:
		return jevFail(j.Attach(ctx, os.Stdout))
	case o.stop:
		return jevFail(j.Stop(os.Stdout))
	case o.options:
		return jevFail(j.Options(ctx, os.Stdout))
	case o.serve != "":
		return jevFail(j.Serve(ctx, o.serve, os.Stdout))
	}
	st := j.Status(ctx)
	if *o.format == helptext.JSON {
		return helptext.WriteJSONOrFail("gateway jev", st)
	}
	st.Text(os.Stdout)
	return 0
}

// jevTrain starts or continues the job detached and then watches it; --bg skips the watch, --fg runs it here.
func jevTrain(ctx context.Context, j jev.Job, o *jevOpts) int {
	err := j.Start(ctx, jev.StartOpts{Fresh: o.fresh, Background: !o.fg, Labels: o.labels,
		Tasks: o.tasks, Extra: o.extra, LabelsSet: o.labelsSet, TasksSet: o.tasksSet}, os.Stdout)
	if err != nil || o.fg || o.bg || j.PID() == 0 { // PID 0: the job already ended, nothing to watch
		return jevFail(err)
	}
	return jevFail(j.Attach(ctx, os.Stdout))
}

// jevFail maps err to an exit status (0 for nil): usage 2, a failed trainer its own code, else 1.
func jevFail(err error) int {
	if err == nil {
		return 0
	}
	if _, ok := errors.AsType[*jev.UsageError](err); ok {
		return helptext.FailUsage("gateway jev: %v", err)
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode()
	}
	return helptext.Fail("gateway jev: %v", err)
}
