package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/edotau/claude-code/internal/agentconfig"
	"github.com/edotau/claude-code/internal/agentid"
	"github.com/edotau/claude-code/internal/catalog"
	"github.com/edotau/claude-code/internal/gateway"
	"github.com/edotau/claude-code/internal/models"
	"github.com/edotau/claude-code/internal/proc"
	"github.com/edotau/claude-code/internal/providers"
	"github.com/edotau/claude-code/internal/router"
	"github.com/edotau/claude-code/internal/vault"
)

// Finding names a failed credential, token, auth, login, served or wire check on a Databricks target.
type Finding struct {
	Check   string   `json:"check"`
	Profile string   `json:"profile"`
	Model   string   `json:"model,omitempty"`
	Detail  string   `json:"detail"`
	Served  []string `json:"served,omitempty"` // READY ids of the model's family the workspace does list
}

func (f Finding) String() string { return f.Check + ": " + f.Detail }

// Preflight is what a dispatch's preflight found and chose; the record and the preflight: lines carry it.
type Preflight struct {
	Requested string    `json:"requested,omitempty"` // <profile>[:<model>] as asked, set by the first failed check
	Chosen    string    `json:"chosen,omitempty"`    // the dispatch the chain starts on
	Failed    []Finding `json:"failed,omitempty"`
	Notes     []string  `json:"notes,omitempty"` // checks skipped or soft: a stale listing
}

// fail records a failed check; the first one names what was requested.
func (pf *Preflight) fail(f Finding) {
	if pf == nil {
		return
	}
	if pf.Requested == "" {
		pf.Requested = strings.TrimSuffix(f.Profile+":"+f.Model, ":")
	}
	pf.Failed = append(pf.Failed, f)
}

// note records a skipped or soft check once.
func (pf *Preflight) note(s string) {
	if pf == nil || s == "" || slices.Contains(pf.Notes, s) {
		return
	}
	pf.Notes = append(pf.Notes, s)
}

// Lines is one line per failed check, then one per note; nil when the preflight found nothing.
func (pf *Preflight) Lines() []string {
	if pf == nil {
		return nil
	}
	var out []string
	for _, f := range pf.Failed {
		chosen := pf.Chosen
		if chosen == "" {
			chosen = "nothing"
		}
		out = append(out, pf.Requested+" failed "+f.Check+" ("+f.Detail+"); chose "+chosen)
	}
	for _, n := range pf.Notes {
		out = append(out, "note: "+n)
	}
	return out
}

// PreflightError is a target the preflight refused with fallback off.
type PreflightError struct{ Finding Finding }

func (e *PreflightError) Error() string {
	return "preflight: " + e.Finding.String() + " (no fallback for this dispatch: --fallback or " + router.EnvFallback + ")"
}

// refreshListing starts one detached re-list of a workspace's listing, deduped per profile; a seam for tests.
var refreshListing = func(profile string) { catalog.SpawnDiscover(profile, false) }

// StubRefreshListingForTest turns the detached listing refresh off for a whole test binary (call from TestMain).
func StubRefreshListingForTest() { refreshListing = func(string) {} }

// CheckWorkspace checks local credentials, token lifetime and cached or live workspace authentication.
func CheckWorkspace(p *providers.Provider, st router.State, now time.Time, timeout ...time.Duration) []Finding {
	budget := DefaultToolsTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		budget = timeout[0]
	}
	return checkWorkspace(p, st, now, budget, nil)
}

func checkWorkspace(p *providers.Provider, st router.State, now time.Time, budget time.Duration, pf *Preflight) []Finding {
	var out []Finding
	if !providers.CredentialPresent(p) {
		out = append(out, Finding{Check: "credential", Profile: p.Name, Detail: "no stored credential for " + p.Name})
	}
	if reason, marked := st.AuthMarked(router.AccountOf(p), now); marked {
		out = append(out, Finding{Check: "auth", Profile: p.Name, Detail: p.Name + " is auth-marked: " + reason})
	}
	if f, dead := deadLoginFinding(p); dead {
		out = append(out, f)
	}
	if len(out) > 0 {
		return out
	}
	if f := tokenFinding(p, now, budget); f != nil {
		if pf == nil || !p.CommandAuth() {
			return []Finding{*f}
		}
		pf.note(p.Name + ": " + f.Detail + "; credential command will refresh during the run")
	}
	if f := workspaceAuth(p, st, now); f != nil {
		out = append(out, *f)
	}
	return out
}

// CredentialFingerprint hashes vault set-times; absent metadata or command credentials disable reuse.
func CredentialFingerprint(p *providers.Provider) string {
	if p.Auth.Command != "" {
		return ""
	}
	meta, err := vault.LoadMeta()
	if err != nil {
		return ""
	}
	name := providers.AuthSecretName(p)
	if name == "" {
		name = p.Auth.Env
	}
	at := meta[name].SetAt
	spAt := time.Time{}
	if p.Auth.SPClientID != "" {
		spAt = meta[providers.SecretName(p.Provider, p.Name, providers.AuthM2M)].SetAt
	}
	if at.IsZero() && spAt.IsZero() {
		return ""
	}
	stamp := strings.Join([]string{at.UTC().Format(time.RFC3339Nano), spAt.UTC().Format(time.RFC3339Nano),
		p.Identity, p.Auth.Type, p.Host, p.BaseURL}, "|")
	return fmt.Sprintf("%x", sha256.Sum256([]byte(stamp)))
}

type runTimeoutKey struct{}

func launchWorkspaceFindings(ctx context.Context, agentName string, p *providers.Provider, st router.State, now time.Time, pf *Preflight) []Finding {
	budget := DefaultToolsTimeout
	if ctx != nil {
		if timeout, ok := ctx.Value(runTimeoutKey{}).(time.Duration); ok && timeout > 0 {
			budget = timeout
		}
	}
	if !agentid.Routing(agentName).ResponsesRoute {
		return CheckWorkspace(p, st, now, budget)
	}
	if pf == nil {
		pf = &Preflight{}
	}
	return checkWorkspace(p, st, now, budget, pf)
}

func tokenFinding(p *providers.Provider, now time.Time, budget time.Duration) *Finding {
	exp, _, ok := providers.TokenExpiryMethodFor(p.Name, p.Identity)
	if !ok || exp.Sub(now) >= budget+2*time.Minute {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := providers.Keepalive(ctx, p)
	exp, _, ok = providers.TokenExpiryMethodFor(p.Name, p.Identity)
	if ok && time.Until(exp) >= budget+2*time.Minute {
		return nil
	}
	detail := "token cannot cover the run timeout plus 2m"
	if err != nil {
		detail += "; refresh failed"
	}
	return &Finding{Check: "token", Profile: p.Name, Detail: detail + ": run `" + providers.LoginCommand(p.Name) + "`"}
}

func workspaceAuth(p *providers.Provider, st router.State, now time.Time) *Finding {
	fp := CredentialFingerprint(p)
	account := router.AccountOf(p)
	key := router.VerdictKey(p.Name, account, "", "")
	if st.VerifiedOK(account, key, fp, now) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, _ := providers.ProbeCredential(ctx, p)
	verdict := "unknown"
	switch status {
	case providers.ProbeVerified:
		verdict = "ok"
	case providers.ProbeRejected:
		verdict = "auth"
	}
	// A refresh during the probe invalidates its result rather than blessing an untested credential.
	if CredentialFingerprint(p) == fp {
		router.StoreVerdict(account, key, router.Verdict{CredFP: fp, Status: verdict, Source: "listing", At: time.Now()})
	}
	if verdict != "auth" {
		return nil
	}
	return &Finding{Check: "auth", Profile: p.Name, Detail: "credential rejected: run `" + providers.LoginCommand(p.Name) + "`"}
}

// deadLoginFinding is the failure for a remembered dead user login that no paired service principal covers.
func deadLoginFinding(p *providers.Provider) (Finding, bool) {
	login, dead := providers.LoginStatus(p)
	if !dead || p.Identity == providers.IdentitySP || (p.Identity != providers.IdentityUser && providers.SPPresent(p)) {
		return Finding{}, false
	}
	return Finding{Check: "login", Profile: p.Name, Detail: fmt.Sprintf("%s login needs renewal (%s since %s): run `%s`",
		p.Name, login.State, login.Since.Local().Format("15:04"), providers.LoginCommand(p.Name))}, true
}

// checkModel is model as p serves it to agentName; failed names the check that refused it, note a check it skipped.
func checkModel(agentName string, p *providers.Provider, model string, now time.Time) (wire string, failed *Finding, note string) {
	base, effort, ok := geminiEffort(agentName, model) // agy takes <id>-<effort>; the endpoint is <id>
	if !ok {
		return checkServed(agentName, p, model, now)
	}
	if wire, failed, note = checkServed(agentName, p, base, now); failed != nil {
		failed.Model = model
		return "", failed, note
	}
	return wire + "-" + effort, nil, note
}

// checkServed is checkModel for a model with no effort suffix.
func checkServed(agentName string, p *providers.Provider, model string, now time.Time) (wire string, failed *Finding, note string) {
	if model == "" || model == router.DiscoveryModel || slices.Contains(router.Aliases, models.Strip1M(model)) {
		return model, nil, ""
	}
	fail := func(check, detail string) (string, *Finding, string) {
		f := &Finding{Check: check, Profile: p.Name, Model: model, Detail: detail}
		if check == "served" {
			f.Served = servedAlternatives(p, model, now)
		}
		return "", f, ""
	}
	served := servedSpelling(agentName, p, model, now)
	wire, why := preflightModelWire(agentName, p, served)
	spelled := cmp.Or(wire, providers.SpellPin(p, served)) // a Responses agent's wire check runs before Serves' wording
	switch {
	case unlisted(p, spelled, now): // a fresh listing without it beats the wire wording: the endpoint does not exist
		return fail("served", p.Name+" lists no endpoint "+spelled)
	case preflightResponsesMismatch(agentName, p, spelled):
		return fail("wire", p.Name+" serves no "+model+" over Responses")
	case why != "":
		return fail("served", why)
	case p.Preset != providers.PresetDatabricks:
		return wire, nil, ""
	}
	note, why = preflightEndpoint(p, wire, now)
	if why != "" {
		return fail("served", why)
	}
	return wire, nil, note
}

// geminiEffort cuts gemini's trailing native effort off model; ok is false for any other agent or no suffix.
func geminiEffort(agentName, model string) (base, effort string, ok bool) {
	base, effort, cut := strings.CutLast(model, "-")
	return base, effort, agentid.Routing(agentName).EffortSuffix && cut && slices.Contains(agentid.NativeEfforts(agentid.Gemini), effort)
}

// servedSpelling is id as agentName asks p for it: databricks-<id> where served; claude never takes GPT/Gemini.
func servedSpelling(agentName string, p *providers.Provider, id string, now time.Time) string {
	if fam := models.FamilyKind(id); agentid.Routing(agentName).ModelFamily == string(models.FamilyClaude) && (fam == models.FamilyGPT || fam == models.FamilyGemini) {
		return providers.SpellPin(p, id)
	}
	return router.SpellServed(p, id, now)
}

// launchWire is the model a launch requests from p for id: servedSpelling, else p's plain wire spelling.
func launchWire(agentName string, p *providers.Provider, id string) string {
	id = models.Strip1M(id)
	return cmp.Or(servedSpelling(agentName, p, id, time.Now()), p.WireModel(id))
}

// unlisted reports a Databricks row whose fresh listing has no endpoint wire.
func unlisted(p *providers.Provider, wire string, now time.Time) bool {
	if p.Preset != providers.PresetDatabricks || wire == "" {
		return false
	}
	listed, _, fresh := router.ServedListed(p, wire, now)
	return fresh && !listed
}

// preflightResponsesMismatch preserves the Responses wire refusal before the served-model refusal.
func preflightResponsesMismatch(agentName string, p *providers.Provider, wire string) bool {
	return speaksResponses(agentName) && p.Preset == providers.PresetDatabricks &&
		wire != "" && !providers.ResponsesServable(p, wire)
}

// preflightModelWire permits agy's gateway transport without requiring a custom-base-URL dialect.
func preflightModelWire(agentName string, p *providers.Provider, model string) (wire, why string) {
	if agentid.Routing(agentName).AnthropicRoute || (agentid.Routing(agentName).GoogleOrDatabricks && GeminiGatewayRow(p)) {
		if wire = providers.SpellPin(p, model); wire == "" {
			why = p.Name + " serves no " + model
		}
	} else {
		wire, why = ProfileServes(agentName, p, model)
	}
	return wire, why
}

// preflightEndpoint checks the cached workspace listing without making a model call.
func preflightEndpoint(p *providers.Provider, wire string, now time.Time) (note, why string) {
	listed, ready, fresh := router.ServedListed(p, wire, now)
	switch {
	case !fresh:
		return "no fresh listing for " + p.Name + ": endpoint check skipped", ""
	case !listed:
		return "", p.Name + " lists no endpoint " + wire
	case !ready:
		return "", wire + " is not a READY endpoint on " + p.Name
	}
	return "", ""
}

// maxAlternatives caps the served ids a refusal names.
const maxAlternatives = 5

// servedAlternatives is up to maxAlternatives READY ids of model's family in p's fresh listing; nil without one.
func servedAlternatives(p *providers.Provider, model string, now time.Time) []string {
	s, ok := router.FreshServed(p.Name, now)
	if !ok {
		return nil
	}
	fam := familyKey(model)
	var out []string
	for _, m := range s.Models {
		if len(out) == maxAlternatives {
			break
		}
		if m.IsReady() && familyKey(m.ID) == fam && m.ID != model {
			out = append(out, m.ID)
		}
	}
	return out
}

// familyKey is id's model family: Claude, GPT or Gemini, else its vendor word (kimi-k3-1-pro -> kimi).
func familyKey(id string) string {
	if fam := models.FamilyKind(id); fam == models.FamilyClaude || fam == models.FamilyGPT || fam == models.FamilyGemini {
		return string(fam)
	}
	vendor, _, _ := strings.Cut(catalog.ID(id), "-")
	return "other:" + vendor
}

// checkRouteServed marks r Served on a Databricks row; none turns an unlisted or not-READY model into ErrModelRoute.
func checkRouteServed(reg *providers.Registry, r ModelRoute, none bool, now time.Time) (ModelRoute, error) {
	p := reg.Row(r.Profile)
	if p == nil || p.Preset != providers.PresetDatabricks || r.Dialect == "own-login" {
		return r, nil
	}
	if _, _, fresh := router.ServedListed(p, "", now); !fresh {
		r.Served = ServedUnverified
		return r, nil
	}
	f := FreshModelFinding(r.Agent, p, r.Model, now)
	if f == nil {
		return r, nil
	}
	detail := f.Detail
	if len(f.Served) > 0 {
		detail += "; " + p.Name + " serves " + strings.Join(f.Served, ", ")
	}
	if none {
		return r, fmt.Errorf("%w: %s (no fallback)", ErrModelRoute, detail)
	}
	r.Served, r.Note = ServedNo, joinNotes(r.Note, detail)
	return r, nil
}

// CheckModelNoted is checkModel whose skipped-check note starts one listing refresh and lands in pf (nil: stderr).
func CheckModelNoted(agentName string, p *providers.Provider, model string, now time.Time, pf *Preflight) (string, *Finding) {
	wire, failed, note := checkModel(agentName, p, model, now)
	if note == "" {
		return wire, failed
	}
	refreshListing(p.Name)
	if pf != nil {
		pf.note(note)
	} else {
		fmt.Fprintln(os.Stderr, "preflight: note: "+note)
	}
	return wire, failed
}

// FreshModelFinding is checkModel's refusal judged on p's fresh cached listing, with no refresh; nil without one.
func FreshModelFinding(agentName string, p *providers.Provider, model string, now time.Time) *Finding {
	if _, _, fresh := router.ServedListed(p, "", now); !fresh {
		return nil
	}
	if _, auto := SplitAuto(model); auto { // auto picks a served model at run time
		return nil
	}
	_, failed, _ := checkModel(agentName, p, model, now)
	return failed
}

// armFinding is the first failed local check of the overlay databricks:<p> workspace, recorded in rc.pf; nil passes.
func (rc routeCtx) armFinding() *Finding {
	ctx := rc.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	p := rc.reg.Row(router.ArmProfileContext(ctx))
	if p == nil || p.Preset != providers.PresetDatabricks {
		return nil
	}
	resolved, err := agentconfig.CommandProvider(ctx, p)
	if err != nil {
		return &Finding{Check: "credential", Profile: p.Name, Detail: "invalid tokenCommand overlay"}
	}
	fs := launchWorkspaceFindings(ctx, rc.want, resolved, loadState(), time.Now(), rc.pf)
	if len(fs) == 0 {
		return nil
	}
	rc.pf.fail(fs[0])
	return &fs[0]
}

// databricksDispatch reports d billing a Databricks workspace of reg.
func databricksDispatch(reg *providers.Registry, d Dispatch) bool {
	_, profile, _ := strings.Cut(d.Account, "·")
	p := reg.Row(profile)
	return p != nil && p.Preset == providers.PresetDatabricks
}

// proxyNote is the note for a proxy that is not running; the launch starts it, so it is never a failure.
func proxyNote() string {
	if st, err := gateway.ReadState(); err == nil && st.PID > 0 && proc.Alive(st.PID) {
		return ""
	}
	return "proxy not running: the launch starts it (claude-code gateway proxy --status)"
}

// explicitFinding is the first failed preflight check of an explicit Databricks backend, nil when it passes.
func explicitFinding(ctx context.Context, q RouteReq, p *providers.Provider, pf *Preflight) *Finding {
	if p.Preset != providers.PresetDatabricks {
		return nil
	}
	resolved, err := agentconfig.CommandProvider(ctx, p)
	if err != nil {
		return &Finding{Check: "credential", Profile: p.Name, Detail: "invalid tokenCommand overlay"}
	}
	p = resolved
	now := time.Now()
	if fs := launchWorkspaceFindings(ctx, q.Agent, p, loadState(), now, pf); len(fs) > 0 {
		return &fs[0]
	}
	_, failed := CheckModelNoted(q.Agent, p, routedWire(q, p, now), now, pf)
	return failed
}

// routedWire is q.Model as ResolveModel spells it on p, so preflight judges the wire id that runs, not the typed one.
func routedWire(q RouteReq, p *providers.Provider, now time.Time) string {
	typed := strings.TrimPrefix(q.Model, p.Name+":")
	reg, err := providers.Load()
	if q.Model == "" || err != nil {
		return typed
	}
	r, err := ResolveModel(reg, ModelQuery{Agent: q.Agent, Model: q.Model, Profile: p.Name}, now)
	if err != nil {
		return typed
	}
	return r.Model
}

// modelFinding is explicitFinding for a --model <profile>:<model> with no --profile; nil when it names no row.
func modelFinding(ctx context.Context, q RouteReq, pf *Preflight) *Finding {
	name, _, ok := strings.Cut(q.Model, ":")
	if !ok {
		return nil
	}
	reg, err := providers.Load()
	if err != nil || reg.Providers[name] == nil {
		return nil
	}
	return explicitFinding(ctx, q, reg.Providers[name], pf)
}

// previewServes refuses an explicit --model that is no alias, no catalog id and no READY endpoint of profiles.
func previewServes(tb *router.RouteTable, model string, profiles []*providers.Provider) error {
	if model == "" || model == router.DiscoveryModel || tb.AliasOf(model) != "" {
		return nil
	}
	if id, _, ok := models.Resolve(model); ok {
		if models.FamilyKind(id) == models.FamilyClaude {
			return fmt.Errorf("--model %q names no alias or version the table routes", model)
		}
		return nil
	}
	now := time.Now()
	for _, p := range profiles {
		if _, ready, fresh := router.ServedListed(p, router.SpellServed(p, strings.TrimPrefix(model, p.Name+":"), now), now); fresh && ready {
			return nil
		}
	}
	return notServedErr(model, profiles)
}

// notServedErr is the one-line refusal of a model no preview profile serves, naming the listing that shows what is.
func notServedErr(model string, profiles []*providers.Provider) error {
	if len(profiles) == 0 {
		return fmt.Errorf("--model %s: no catalog id, and no Databricks profile on this route serves it; "+
			"claude-code models --live --profile <p> lists what <p> serves", model)
	}
	return fmt.Errorf("--model %s: not a READY endpoint on %s; claude-code models --live --profile %s lists what it serves",
		model, profiles[0].Name, profiles[0].Name)
}

// firstName is the first profile's name, "" for none.
func firstName(profiles []*providers.Provider) string {
	if len(profiles) == 0 {
		return ""
	}
	return profiles[0].Name
}

func previewProfilesContext(ctx context.Context, q RouteReq, lead string) []*providers.Provider {
	reg := loadRegistryWarn()
	typed, _, _ := strings.Cut(q.Model, ":")
	names := []string{typed, router.ArmProfileContext(ctx), q.Profile, lead}
	if pins, err := AgentPinsContext(ctx); err == nil {
		names = append(names, pins[q.Agent].Profile)
	}
	var out []*providers.Provider
	for _, n := range names {
		if p := reg.Row(n); p != nil && p.Preset == providers.PresetDatabricks && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

// sameModel reports a dispatch model ([profile:]wire) naming want, with or without the databricks- prefix.
func sameModel(dispatched, want string) bool {
	if _, after, ok := strings.Cut(dispatched, ":"); ok {
		dispatched = after
	}
	if _, after, ok := strings.Cut(want, ":"); ok {
		want = after
	}
	return models.SameModel(dispatched, want)
}

// ProfileServes is model as p spells it when a fresh listing does not mark it NOT_READY and agentName Serves it there.
func ProfileServes(agentName string, p *providers.Provider, model string) (wire, why string) {
	if wire = providers.SpellPin(p, model); wire == "" {
		return "", "serves no " + model
	}
	if ready, fresh := router.ServedReady(p, wire, time.Now()); fresh && !ready {
		return "", "serves no " + wire + " (not a READY endpoint on " + p.Name + ")"
	}
	if ok, why := Serves(agentName, p, wire); !ok {
		return "", why
	}
	return wire, ""
}

// previewRoute is the pick through ask's chain, each candidate on the model it would run; later drops go to Skipped.
func previewRoute(ctx context.Context, q RouteReq, lead string) (Chain, router.PickResult, error) {
	tb, err := router.Load()
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	profiles := previewProfilesContext(ctx, q, lead)
	if err := previewServes(tb, q.Model, profiles); err != nil {
		return Chain{}, router.PickResult{}, err
	}
	chain, pick, err := chainFor(ctx, q, tb.PreviewAlias(q.Model, q.Task), lead)
	if err != nil {
		return Chain{}, router.PickResult{}, err
	}
	if _, _, exact := router.ModelRow(q.Model); exact != "" && !slices.ContainsFunc(chain.Dispatches, func(d Dispatch) bool {
		return sameModel(d.Model, exact)
	}) {
		return Chain{}, router.PickResult{}, fmt.Errorf("--model %s: no %s candidate runs it; claude-code models --live "+
			"--profile %s lists what is served", q.Model, cmp.Or(q.Agent, "routed"), cmp.Or(firstName(profiles), "<p>"))
	}
	return chain, previewOutcome(chain, pick), nil
}

func previewOutcome(chain Chain, pick router.PickResult) router.PickResult {
	res := pick
	res.Candidates = nil
	for _, d := range chain.Dispatches {
		if d.rung < 1 || d.rung > len(pick.Candidates) {
			continue
		}
		c := pick.Candidates[d.rung-1]
		model := strings.TrimPrefix(d.Model, c.Profile+":")
		if !VaultAgentIsClaude(c.Agent) || c.Profile != "" && !slices.Contains(models.AliasNames, model) {
			c.Model, c.Effort = d.Model, d.Effort
		}
		res.Candidates = append(res.Candidates, c)
	}
	res.Skipped = slices.Clone(pick.Skipped)
	for _, s := range chain.Skipped {
		if slices.ContainsFunc(pick.Skipped, func(k router.Skip) bool { return s == k.Agent+": "+k.Reason }) {
			continue
		}
		who, why, _ := strings.Cut(s, ": ")
		res.Skipped = append(res.Skipped, router.Skip{Agent: who, Gate: "model", Reason: why})
	}
	return res
}
