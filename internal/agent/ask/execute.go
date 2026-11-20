package ask

import (
	"errors"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/edotau/claude-code/internal/agent"
)

// execute starts a live session or runs the one task.
func (r *askRun) execute() int {
	f := r.f
	if f.live {
		return startLive(r.ctx, r.state, r.perms, f.name, f.as, f.format)
	}
	if r.jobRec != nil && r.jobRec.Live {
		return agent.RunHolder(r.jobRec, newClient().Launcher)
	}
	framed, prompt, err := r.readPrompt()
	if err != nil {
		return failAsk(f.format, f.name, err)
	}
	if strings.TrimSpace(prompt) == "" {
		r.fs.Usage()
		printAskFailure(f.format, f.name, errors.New("no task: give a prompt, or \"-\" to read stdin"))
		return 2
	}
	join := func() string { return prompt }
	if framed == nil && !f.dryRun {
		join = r.startRecall(prompt) // overlaps the setup below; joined before request reads the brief
	}
	ctx, stop := signal.NotifyContext(r.ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP) // a kill finalizes the record
	defer stop()
	if framed != nil { // the remote end of `ask --host`: stdin closing means the local side cancelled or vanished
		go func() { _, _ = io.Copy(io.Discard, framed); stop() }()
	}
	f.timeout = askTimeout(r.fs, f.timeout, f.retry, f.job, f.tools)
	warnAskMemory(r.ag, f.enableMemory)
	f.debugFile = absDebugFile(f.debugFile)
	prompt = join()
	req, run := r.request(prompt), r.armRun()
	if run != nil {
		req = run.probe(req)
	}
	return askPrepareAndRun(r.dryCtx(ctx, prompt), r.fs, req, r.jobRec, r.ov, f.env, f.keepEnv, f.as, f.format, f.save, f.name, f.background, f.native, run, r.out)
}
