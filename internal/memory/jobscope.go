package memory

import (
	"context"
	"io"
	"slices"
)

// The memory job kinds JobTools knows; update also covers init and catch-up, which run through it.
const (
	JobUpdate = "update"
	JobDream  = "dream"
)

// JobTools reports whether a memory job of kind holds tools; an unknown kind gets none.
func JobTools(kind string) bool { return kind == JobUpdate || kind == JobDream }

// JobKind names the job ctx's child env marks: dream carries its own child marker, anything else is an update.
func JobKind(ctx context.Context) string { return jobKind(ChildEnv(ctx)) }

func jobKind(env []string) string {
	if slices.Contains(env, dreamChildEnv+"=1") {
		return JobDream
	}
	return JobUpdate
}

// Failure classes a runner reports; auth and budget need the owner, so no automatic retry repeats them.
const (
	JobClassAuth   = "auth"
	JobClassBudget = "budget"
	JobClassQuota  = "quota"
	JobClassFailed = "failed"
)

// JobResult is what the injected runner says about its run: the route that answered, a failure's class and fix.
type JobResult struct {
	Agent, Profile, Model, Class, Fix string
	Local                             bool // a local-model run: lower trust, same caps and validation
}

type jobReportKey struct{}

// WithJobReport gives the runner a report slot; the job reads it after o.Run returns.
func WithJobReport(ctx context.Context) (context.Context, *JobResult) {
	r := &JobResult{}
	return context.WithValue(ctx, jobReportKey{}, r), r
}

// ReportJob fills ctx's report slot; a ctx without one (a test runner) drops it.
func ReportJob(ctx context.Context, r JobResult) {
	if p, ok := ctx.Value(jobReportKey{}).(*JobResult); ok {
		*p = r
	}
}

type jobOutputKey struct{}

// WithJobOutput routes the runner's agent output, receipts and notes to w (the job's log tee).
func WithJobOutput(ctx context.Context, w io.Writer) context.Context {
	return context.WithValue(ctx, jobOutputKey{}, w)
}

// JobOutput is the writer set by WithJobOutput, else fallback (the runner's own stdout or stderr).
func JobOutput(ctx context.Context, fallback io.Writer) io.Writer {
	if w, ok := ctx.Value(jobOutputKey{}).(io.Writer); ok && w != nil {
		return w
	}
	return fallback
}

type jobAnswerKey struct{}

type jobAnswer struct {
	schema string
	text   *string
}

// WithJobAnswer asks the runner for one answer matching schema; the job reads *text after o.Run returns.
func WithJobAnswer(ctx context.Context, schema string) (context.Context, *string) {
	text := new(string)
	return context.WithValue(ctx, jobAnswerKey{}, jobAnswer{schema, text}), text
}

// JobAnswerSchema is the schema ctx asks for; "" means a tools-on run with no structured answer.
func JobAnswerSchema(ctx context.Context) string {
	a, _ := ctx.Value(jobAnswerKey{}).(jobAnswer)
	return a.schema
}

// AnswerJob fills ctx's answer slot; a ctx without one (a test runner) drops it.
func AnswerJob(ctx context.Context, text string) {
	if a, ok := ctx.Value(jobAnswerKey{}).(jobAnswer); ok {
		*a.text = text
	}
}
