export const meta = {
  name: 'code-agent',
  description: 'One agent (claude, or external codex|gemini|opencode|copilot) as a workflow stage, optionally on a named profile: smoke ask, JSON envelope relay, sonnet fallback (off when a profile is named)',
  whenToUse:
    'USE: one external-agent call (copilot, codex, gemini, opencode) as a stage of another script; the agent pool and model discovery live here. CALL: workflow("code-agent", {cwd, task}). OPTIONAL: agent (omit = router pick, copilot first), model (relay/fallback Claude model), vendorModel (ask --model: alias, wire id, kimi-k3, discovery), profile (bills it; turns the sonnet fallback off), tools, readOnly (ask --read-only), smoke, fallback, timeoutMin (max 8), agents, claudeState (ok|low|exhausted|unknown, passed by a caller; omit = read it here). agent "claude" runs one in-workflow Claude leaf at model haiku|sonnet|opus (no router, smoke or relay). RETURNS {ok, agent, model, answer, status, fallback?}. Details: skills/subagent-workflows/references/saved-workflows.md.',
  phases: [{ title: 'Agent', detail: 'haiku leaf runs claude-code ask --format json and relays the envelope; sonnet fallback on failure' }],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a0 = args && typeof args === 'object' && !Array.isArray(args) ? args : {}

// ── stepOf: copied verbatim into code-analyzer, code-engineer, green-keeper-loop, code-evaluator ──
// stepOf(steps, name, defaults, {allowed, externalOK, lists}) -> {kind:'claude',model,effort} | {kind:'external',agent,vendorModel,profile}
// An array value (only for stages named in `lists`) returns an array of those; any bad input throws Error naming stage/field.
const stepOf = (steps, name, defaults, opt) => {
  const S_AGENTS = ['claude', 'copilot', 'codex', 'gemini', 'opencode']
  const S_CLAUDE = ['haiku', 'sonnet', 'opus']
  const S_EFFORT = ['low', 'medium', 'high', 'xhigh']
  const S_FIELDS = ['agent', 'model', 'effort', 'profile']
  const allowed = (opt && opt.allowed) || []
  const lists = (opt && opt.lists) || []
  const d = defaults || {}
  if (steps !== undefined && (!steps || typeof steps !== 'object' || Array.isArray(steps))) throw new Error('steps must be an object')
  for (const k of Object.keys(steps || {})) if (!allowed.includes(k)) throw new Error(`steps.${k}: unknown stage (allowed: ${allowed.join('|')})`)
  const one = (v, where) => {
    if (!v || typeof v !== 'object' || Array.isArray(v)) throw new Error(`${where} must be an object`)
    for (const k of Object.keys(v)) if (!S_FIELDS.includes(k)) throw new Error(`${where}.${k}: unknown field (allowed: ${S_FIELDS.join('|')})`)
    const agentName = v.agent !== undefined ? v.agent : d.agent || 'claude'
    if (!S_AGENTS.includes(agentName)) throw new Error(`${where}.agent must be ${S_AGENTS.join('|')}`)
    if (v.effort !== undefined && !S_EFFORT.includes(v.effort)) throw new Error(`${where}.effort must be ${S_EFFORT.join('|')}`)
    if (v.profile !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(v.profile))) throw new Error(`${where}.profile must be a profile name`)
    const effort = v.effort !== undefined ? v.effort : d.effort || 'medium'
    if (agentName === 'claude') {
      const model = v.model !== undefined ? v.model : d.agent && d.agent !== 'claude' ? 'sonnet' : d.model || 'sonnet'
      if (!S_CLAUDE.includes(model)) throw new Error(`${where}.model must be ${S_CLAUDE.join('|')} for agent claude`)
      return { kind: 'claude', model, effort }
    }
    if (!(opt && opt.externalOK)) throw new Error(`${where}.agent: ${agentName} not allowed here (claude only)`)
    const vendorModel = v.model !== undefined ? v.model : v.agent === undefined ? d.model : undefined
    if (vendorModel !== undefined && !/^[A-Za-z0-9._:-]+$/.test(String(vendorModel))) throw new Error(`${where}.model must be a model id or alias`)
    return { kind: 'external', agent: agentName, vendorModel, profile: v.profile !== undefined ? v.profile : d.profile }
  }
  const v = (steps || {})[name]
  if (Array.isArray(v)) {
    if (!lists.includes(name)) throw new Error(`steps.${name}: a list is not allowed for this stage`)
    if (!v.length) throw new Error(`steps.${name}: list is empty`)
    return v.map((x, i) => one(x, `steps.${name}[${i}]`))
  }
  return one(v === undefined ? {} : v, `steps.${name}`)
}
// ── end stepOf ──
if (a0.stepsSelfTest === true) {
  const run1 = (...p) => { try { return { ok: true, got: stepOf(...p) } } catch (e) { return { ok: false, error: e.message } } }
  const O = { allowed: ['refute', 'fix'], externalOK: true, lists: ['refute'] }
  const C = { allowed: ['refute', 'fix'], externalOK: false }
  const cases = [
    ['default', run1(undefined, 'fix', {}, O)],
    ['claude opus', run1({ fix: { model: 'opus', effort: 'high' } }, 'fix', {}, O)],
    ['external', run1({ fix: { agent: 'copilot', model: 'gpt-6.1-sol', profile: 'work' } }, 'fix', {}, O)],
    ['list', run1({ refute: [{ agent: 'copilot' }, { agent: 'codex' }] }, 'refute', {}, O)],
    ['unknown stage', run1({ nope: {} }, 'fix', {}, O)],
    ['unknown field', run1({ fix: { foo: 1 } }, 'fix', {}, O)],
    ['bad claude model', run1({ fix: { model: 'gpt' } }, 'fix', {}, O)],
    ['external not ok', run1({ fix: { agent: 'codex' } }, 'fix', {}, C)],
    ['list not allowed', run1({ fix: [{}] }, 'fix', {}, O)],
    ['bad effort', run1({ fix: { effort: 'max' } }, 'fix', {}, O)],
  ].map(([name, r]) => ({ name, ...r }))
  return { ok: true, cases }
}
const AGENTS = ['codex', 'gemini', 'opencode', 'copilot']
if (!a0.task) return { error: 'code-agent needs args.task' }
if (a0.profile !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a0.profile))) return { error: 'code-agent: args.profile must be a profile name' }
if (a0.agents !== undefined && (typeof a0.agents !== 'string' || !a0.agents.trim() || a0.agents.length > 90 * 1024)) return { error: 'code-agent: args.agents must be a non-empty string under 90 KiB' }
const MODELS = ['opus', 'sonnet', 'haiku']
if (a0.model && !MODELS.includes(a0.model)) return { error: 'code-agent: args.model must be ' + MODELS.join('|') }
if (a0.vendorModel !== undefined && !/^[A-Za-z0-9._:-]+$/.test(String(a0.vendorModel))) return { error: 'code-agent: args.vendorModel must be a model id or alias (e.g. discovery)' }
// A caller-passed claudeState (any string) replaces both Claude pressure reads below; absent = read it here.
const CLAUDE_GIVEN = typeof a0.claudeState === 'string'
const ROUTE_PICK = {
  type: 'object',
  additionalProperties: false,
  required: CLAUDE_GIVEN ? ['agent'] : ['agent', 'claudeState'],
  properties: { agent: { type: 'string' }, claudeState: { type: 'string' } },
}
const ROUTE_STATE = {
  type: 'object',
  additionalProperties: false,
  required: ['claudeState'],
  properties: { claudeState: { type: 'string' } },
}
// No agent: the router's first non-Claude candidate at the model (gated on availability, pinned profile and quota).
// Unless the caller passed claudeState, the same call also reads the Claude pressure state for the sonnet fallback.
const routed = a0.agent
  ? null
  : await agent(
      `Run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --fallback agents --model ${a0.model || 'sonnet'}. ` +
        `It prints one JSON object; agent = candidates[0].agent, or agent = "" when candidates is empty. ` +
        (CLAUDE_GIVEN
          ? ''
          : `Also run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
            `claudeState = that result's candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
            `is "claude" and its gate value, else "unknown". `) +
        `Never run anything else.`,
      { label: 'route', phase: 'Agent', schema: ROUTE_PICK, model: 'haiku', effort: 'low' }
    )
const a = routed ? { ...a0, agent: routed.agent } : a0
const PROFILE = a.profile ? String(a.profile) : ''
const VMODEL_FLAG = a0.vendorModel ? ' --model ' + a0.vendorModel : ''
const FALLBACK_ENABLED = a.fallback !== false && !PROFILE
// Explicit agent, no claudeState passed in: read Claude pressure for the sonnet fallback (none when fallback is off).
const routeState =
  !routed && !CLAUDE_GIVEN && a0.agent && a0.agent !== 'claude' && FALLBACK_ENABLED
    ? await agent(
        `Run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
          `It prints one JSON object; claudeState = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
          `is "claude" and its gate value, else "unknown". Never run anything else.`,
        { label: 'route-state', phase: 'Agent', schema: ROUTE_STATE, model: 'haiku', effort: 'low' }
      ).catch(() => null)
    : null
// Pressure: low/exhausted Claude account steps the sonnet fallback below down one model.
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const claudeState = CLAUDE_GIVEN ? a0.claudeState : routed ? routed.claudeState : routeState ? routeState.claudeState : null
const PRESSURE = claudeState === 'low' || claudeState === 'exhausted'
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${claudeState} — stepping the sonnet fallback down one model`)
else if (!routed && !routeState && !CLAUDE_GIVEN) log(a0.agent && !FALLBACK_ENABLED ? 'pressure: no route read (fallback disabled)' : 'pressure: no route read — no downshift (fail open)')
if (a.agent === 'claude') {
  if (a.task.length > 90 * 1024) return { error: 'code-agent: task over 90 KiB; pass excerpts, not whole diffs' }
  const CLAUDE_MODEL = a.model || 'sonnet'
  const CWD_C = a.cwd || '.'
  const scope = a.tools === false ? ' Read-only: do not edit files.' : ''
  phase('Agent')
  const out = await agent(`${a.task}\n\n(Work in ${CWD_C}; prefix every shell command with cd ${CWD_C} &&.${scope})`, {
    label: 'ask:claude',
    phase: 'Agent',
    agentType: 'code-worker:claude',
    model: routeModel(CLAUDE_MODEL),
    effort: 'medium',
  }).catch(e => ({ failed: String((e && e.message) || e) }))
  if (!out || out.failed) return { ok: false, agent: 'claude', model: CLAUDE_MODEL, answer: '', error: (out && out.failed) || 'claude leaf returned no result', status: 'error', provider: 'anthropic' }
  return { ok: true, agent: 'claude', model: CLAUDE_MODEL, answer: typeof out === 'string' ? out : JSON.stringify(out), status: 'ok', provider: 'anthropic' }
}
if (!AGENTS.includes(a.agent)) return { error: 'code-agent needs args.agent in ' + AGENTS.join('|') + (routed ? ' (route picked none)' : '') }
if (a.task.length > 90 * 1024) return { error: 'code-agent: task over 90 KiB (ask rejects prompts over 96 KiB); pass excerpts, not whole diffs' }
const CWD = a.cwd || '.'
const SMOKE = a.agent === 'gemini' || a.agent === 'codex' ? a.smoke === true : a.smoke !== false
// A smoke ask halves the per-call cap, so smoke + task stay within 8 minutes; the leaf polls short status calls, not one long Bash
const CAP = SMOKE ? 4 : 8
const MIN = Number.isInteger(a.timeoutMin) && a.timeoutMin > 0 && a.timeoutMin <= CAP ? a.timeoutMin : CAP
if (a.timeoutMin !== undefined && a.timeoutMin !== MIN) log(`timeoutMin ${a.timeoutMin} clamped to ${MIN} (cap ${CAP}${SMOKE ? ' with the smoke ask' : ''})`)
const AGENTS_FLAG = a.agents ? ' --agents <agents file>' : ''
const AGENTS_STEP = a.agents ? `Also write the AGENTS text below verbatim to a second scratch file with a quoted heredoc (<<'EOF'), and substitute that file for <agents file>. ` : ''
const AGENTS_TAIL = a.agents ? `\n\nAGENTS:\n${a.agents}` : ''
const TOOLS = a.agent === 'gemini' ? a.tools !== false : a.tools === true
// readOnly asks for ask --read-only (copilot: reads approved, writes refused); it replaces --tools.
const ACCESS_FLAG = a.readOnly === true ? ' --read-only' : TOOLS ? ' --tools' : ''
const ENVELOPE = {
  type: 'object',
  additionalProperties: false,
  required: ['ok', 'answer', 'error'],
  properties: {
    ok: { type: 'boolean' },
    answer: { type: 'string' },
    error: { type: 'string' },
    provider: { type: 'string' },
    model: { type: 'string' },
    status: { type: 'string' },
    transport: { type: 'object' },
  },
}
// One `claude-code ask` relay per call; the heredoc-to-file hop keeps shell quoting away from the task text.
// A thrown or null relay (schema death included) maps to {ok:false}, never a throw.
const run = async (task, label) =>
  (await agent(
    `Agent: ${a.agent}\n` +
      `Relay one external-model call; do NOT answer the task yourself. Write the TASK below verbatim to a scratch file with a quoted heredoc (<<'EOF'), ` +
      `${AGENTS_STEP}Then run exactly: cd ${CWD} && claude-code ask --agent ${a.agent} --format json${PROFILE ? ' --profile ' + PROFILE : ''}${VMODEL_FLAG} --workdir ${CWD} --timeout ${MIN}m${ACCESS_FLAG}${AGENTS_FLAG} --background - < <that file>. ` +
      `It prints ONE JSON line {"id":…,"log":…}; id = the job id. Loop claude-code ${a.agent} status <id> with sleep 5 until done, then claude-code agents result <id> --format json. ` +
      `The result is ONE indented JSON object {agent, status, provider, model, answer, error:{kind,message}|null, transport, …}. ok = exit 0 AND error null AND answer non-empty. ` +
      `Copy status and transport verbatim. Copy answer verbatim, never summarise. error = the envelope error's message, or the stderr tail on a non-JSON exit. Never retry.\n` +
      `Your reply MUST contain ok, answer and error every time, even on failure or timeout. On a failed or timed-out ask reply ` +
      `{"ok":false,"answer":"","error":"<the failure message>"}: the message goes in error, never in answer, and answer stays "". ` +
      `On success error is "".\n\nTASK:\n${task}${AGENTS_TAIL}`,
    { label, phase: 'Agent', schema: ENVELOPE, agentType: 'code-worker:claude', model: 'haiku', effort: 'low' }
  ).catch(() => null)) || { ok: false, answer: '', error: 'relay leaf failed' }

phase('Agent')
const smoke = !SMOKE ? { ok: true, answer: 'READY', error: '' } : await run('Reply READY only.', 'smoke:' + a.agent)
const res =
  smoke.ok && /READY/i.test(smoke.answer || '')
    ? await run(a.task, 'ask:' + a.agent)
    : { ok: false, answer: '', error: smoke.error || 'smoke ask failed' }
const withProfile = o => (PROFILE ? { ...o, profile: PROFILE } : o)
if (res.ok) return withProfile({ ...res, agent: a.agent })
log(`${a.agent} dispatch failed: ${res.error}`)
if (!FALLBACK_ENABLED || TOOLS) return withProfile({ ...res, agent: a.agent, skipped: true })
const fb = await agent(`${a.task}\n\n(Work in ${CWD}; prefix every shell command with cd ${CWD} &&. Read-only unless the task says otherwise.)`, {
  label: 'fallback:' + a.agent,
  phase: 'Agent',
  agentType: 'code-worker:claude',
  model: routeModel(a.model || 'sonnet'),
  effort: 'medium',
})
return { ok: !!fb, agent: 'claude', status: fb ? 'ok' : 'error', answer: typeof fb === 'string' ? fb : JSON.stringify(fb || ''), error: res.error, fallback: a.agent }
