// ── stepOf: copied verbatim into code-analyzer, code-looper, code-evaluator, code-workers, code-engineer ──
// stepOf(steps, name, defaults, {allowed, external, profile, provider}) -> stage; claude in-context, others via `agents ask`.
const COWORKERS = []
// ranOnReworded: a relay-reworded ran_on that still names the agent and a route (account·profile, profile:model or model id).
const ranOnReworded = (ranOn) => /^(?:ran on )?(?:claude|codex|copilot|gemini|opencode)\b/.test(ranOn) && (/\S[·:][A-Za-z0-9]/.test(ranOn) || /\b[A-Za-z]+-[\w.-]*\d/.test(ranOn))
// ranOnVerdict reads a leg's result: ranOn as relayed, whether it is the envelope's line (canon, wire.RanOn) and whether the leg ran.
const ranOnVerdict = (r) => {
  const ranOn = (r && r.ran_on) || ''
  const canon = /^ran on \S+ · .+ · fallback (?:none|from )/.test(ranOn)
  const failed = /\b(?:unavailable|failed|blocked|no execution|not run)\b/i.test(ranOn.split(' · fallback ')[0])
  return { ranOn, canon, ok: !!(r && r.coworker === 'ok' && (canon || ranOnReworded(ranOn)) && !failed) }
}
// Unknown stage/field backstop only; value and route checks run in the dispatch-effort hook (workflowlint.go).
const S_FIELDS = ['agent', 'model', 'effort', 'profile', 'provider']
const stepObj = (v) => !!v && typeof v === 'object' && !Array.isArray(v)
const stepUnknown = (obj, allowed, msg) => { for (const k of Object.keys(obj)) if (!allowed.includes(k)) throw new Error(msg(k)) }
const stepCheck = (steps, name, allowed) => {
  if (steps !== undefined && !stepObj(steps)) throw new Error('steps must be an object')
  const all = steps || {}, al = allowed || []
  stepUnknown(all, al, (k) => `steps.${k}: unknown stage (allowed: ${al.join('|')})`)
  const where = `steps.${name}`
  const v = all[name] === undefined ? {} : all[name]
  if (!stepObj(v)) throw new Error(`${where} must be an object`)
  stepUnknown(v, S_FIELDS, (k) => `${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
  return v
}
// The route unit comes whole from the step if it names profile or provider, else from the run-wide args; keys never mix.
const stepFields = (v, d, o) => {
  const own = v.profile !== undefined || v.provider !== undefined, src = own ? v : o
  return { who: v.agent !== undefined ? v.agent : 'claude', effort: v.effort !== undefined ? v.effort : d.effort || 'medium',
    profile: src.profile || '', provider: src.provider || '' }
}
const stepSame = (_label, r) => r
const claudeStage = (v, d, effort) => ({ agent: 'claude', ext: false, model: v.model !== undefined ? v.model : d.model || 'sonnet', effort,
  type: d.agentType ? { agentType: d.agentType } : {}, task: (p) => p, schema: (s) => s, done: stepSame, text: stepSame })
const ST_EXT_RETRY = ' Set the Bash timeout to 600000 ms and pass --timeout 9m to agents ask. If a tool call is refused because auto mode cannot judge it right now (its classifier is unavailable), retry that same call up to 3 times before reporting BLOCKED.'
const ST_EXT_DONE = ' Copy the envelope dispatch id into dispatch_id and run `claude-code agents diff --verify <id>` with that id; verification failure means BLOCKED. The external agent does the whole task; never do it yourself or grade/answer in-context after a probe call. Copy the envelope\'s ran_on verbatim into ran_on, the length of its hops array in hops, and set coworker "ok". If every hop failed'
const ST_EXT_FAIL = ' (429, auth, no route), set coworker "BLOCKED", coworker_error to the exact error, and the other fields to their empty values.\n\nTASK:\n'
const EXT_F = { dispatch_id: { type: 'string' }, ran_on: { type: 'string' }, hops: { type: 'integer' }, coworker: { type: 'string', enum: ['ok', 'BLOCKED'] }, coworker_error: { type: 'string' } }
const EXT_R = ['dispatch_id', 'ran_on', 'coworker']
// Agents whose relay subagent is retired: a claude leaf runs their stage from a `Run:` line, so the ran-on gate holds.
const RELAY_RETIRED = ['gemini']
// ANSI-C quotes decode task escapes; hex apostrophes also pass the hook's literal-quote check.
const stepTaskArg = (p) => "$'" + p.replace(/[\\'\x00-\x1f\x7f]/g, (c) => '\\x' + c.charCodeAt(0).toString(16).padStart(2, '0')) + "'"
const stepRecord = (name, who, label, r) => {
  const { ranOn, canon, ok } = ranOnVerdict(r)
  if (ok && !canon) log(`coworkers ${label}: ran_on reworded by the relay: ${ranOn}`)
  const why = ok ? '' : (r && r.coworker_error) || 'no ran_on in the result'
  COWORKERS.push({ stage: name, label, agent: who, status: ok ? 'ok' : 'BLOCKED', ran_on: ok ? r.ran_on : '', hops: (r && r.hops) || 0, error: why })
  log(`coworkers ${label}: ${ok ? (/^ran on /.test(r.ran_on) ? '' : 'ran on ') + r.ran_on + ' (hop ' + ((r && r.hops) || 0) + ')' : who + ' BLOCKED: ' + why}`)
  return ok
}
const stepExt = (name, v, d, f) => {
  const { who, effort, profile, provider } = f
  // no profile/provider: Go routes the agent's pin; a named one leads, ladder after.
  const edit = d.tools === 'edit'
  const mode = edit || d.tools === 'run' ? '--tools' : who === 'copilot' ? '--read-only' : '--tools'
  // a named provider goes whole as --overlay; a bare profile stays sugar and ask fills the provider from the registry.
  const route = provider ? ` --overlay '${JSON.stringify({ provider, profile: profile || 'auto', ...(v.model ? { model: v.model } : {}) })}'`
    : (profile ? ` --profile ${profile}` : '') + (v.model ? ` --model ${v.model}` : '')
  const ask = `claude-code agents ask --agent ${who} --fallback agents --format json ${mode} --effort ${effort}` + route
  const retired = RELAY_RETIRED.includes(who)
  const how = retired ? 'Run the `Run:` line above exactly; its quoted argument is the complete task. Never do it in-context.'
    : `Run the task below with \`${ask} --attach <file>\`: Write the task text to a NEW file from \`mktemp "${'$'}{TMPDIR:-/tmp}/${who}-task.XXXXXX"\` (never a fixed name: parallel legs overwrite each other), never in the repo; Read that empty file before you Write it (Write refuses an existing file it has not read), attach that exact path, and never do it in-context.`
  return {
    agent: who, ext: true, model: 'sonnet', effort: 'low', type: { agentType: retired ? 'claude' : who }, profile, provider,
    task: (p) => (retired ? `Run: ${ask} -- ${stepTaskArg(p)}\n` : '') + `Agent: ${who}\n${how}` +
      (edit ? '' : ' The task is read-only: tell the agent to edit nothing.') + ST_EXT_RETRY + ST_EXT_DONE + ST_EXT_FAIL + p,
    schema: (s) => s ? { ...s, required: [...(s.required || []), ...EXT_R], properties: { ...s.properties, ...EXT_F } }
      : { type: 'object', additionalProperties: false, required: ['answer', ...EXT_R], properties: { answer: { type: 'string' }, ...EXT_F } },
    done: (label, r) => (stepRecord(name, who, label, r) ? r : null),
    text: (label, r) => (stepRecord(name, who, label, r) ? r.answer : null),
  }
}
const stepOf = (steps, name, defaults, opt) => {
  const o = opt || {}, d = defaults || {}
  const v = stepCheck(steps, name, o.allowed)
  const f = stepFields(v, d, o)
  if (f.who === 'claude' && !f.profile && !f.provider) return claudeStage(v, d, f.effort)
  if (!o.external) throw new Error(`steps.${name}.agent: ${f.who} is not wired in this workflow yet (claude only)`)
  return stepExt(name, v, d, f)
}
// ── end stepOf ──
