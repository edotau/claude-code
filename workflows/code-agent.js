export const meta = {
  name: 'code-agent',
  description: 'One non-Claude agent (codex|gemini|opencode|copilot) as a workflow stage, optionally on a named profile: smoke ask, JSON envelope relay, sonnet fallback (off when a profile is named)',
  whenToUse:
    'USE: one external-agent call (copilot, codex, gemini, opencode) as a stage of another script; the agent pool and model discovery live here. CALL: workflow("code-agent", {cwd, task}). OPTIONAL: agent (omit = router pick, copilot first), model (relay/fallback Claude model), vendorModel (ask --model: alias, wire id, kimi-k3, byok), profile (bills it; turns the sonnet fallback off), tools, smoke, fallback, timeoutMin (max 8), agents. RETURNS {ok, agent, model, answer, status, fallback?}. PLANNED: agent "claude" (plans/2026-11-03-workflow-steps.md). Details: skills/subagent-workflows/references/saved-workflows.md.',
  phases: [{ title: 'Agent', detail: 'haiku leaf runs claude-code ask --format json and relays the envelope; sonnet fallback on failure' }],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a0 = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const AGENTS = ['codex', 'gemini', 'opencode', 'copilot']
if (!a0.task) return { error: 'code-agent needs args.task' }
if (a0.profile !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a0.profile))) return { error: 'code-agent: args.profile must be a profile name' }
if (a0.agents !== undefined && (typeof a0.agents !== 'string' || !a0.agents.trim() || a0.agents.length > 90 * 1024)) return { error: 'code-agent: args.agents must be a non-empty string under 90 KiB' }
const MODELS = ['opus', 'sonnet', 'haiku']
if (a0.model && !MODELS.includes(a0.model)) return { error: 'code-agent: args.model must be ' + MODELS.join('|') }
if (a0.vendorModel !== undefined && !/^[A-Za-z0-9._-]+$/.test(String(a0.vendorModel))) return { error: 'code-agent: args.vendorModel must be a model id or alias (e.g. byok)' }
const ROUTE_PICK = {
  type: 'object',
  additionalProperties: false,
  required: ['agent', 'claudeState'],
  properties: { agent: { type: 'string' }, claudeState: { type: 'string' } },
}
const ROUTE_STATE = {
  type: 'object',
  additionalProperties: false,
  required: ['claudeState'],
  properties: { claudeState: { type: 'string' } },
}
// No agent: the router's first non-Claude candidate at the model (gated on availability, pinned profile and quota).
// Same call also reads the Claude account's own pressure state, so the sonnet fallback below can step down.
const routed = a0.agent
  ? null
  : await agent(
      `Run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --fallback agents --model ${a0.model || 'sonnet'}. ` +
        `It prints one JSON object; agent = candidates[0].agent, or agent = "" when candidates is empty. ` +
        `Also run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
        `claudeState = that result's candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
        `is "claude" and its gate value, else "unknown". Never run anything else.`,
      { label: 'route', phase: 'Agent', schema: ROUTE_PICK, model: 'haiku', effort: 'low' }
    )
const a = routed ? { ...a0, agent: routed.agent } : a0
const PROFILE = a.profile ? String(a.profile) : ''
const VMODEL_FLAG = a0.vendorModel ? ' --model ' + a0.vendorModel : ''
const FALLBACK_ENABLED = a.fallback !== false && !PROFILE
// An explicit agent skips the agent-pick call above, but the sonnet fallback still needs the Claude pressure state
// — read it alone, same haiku pattern as code-engineer.js's route-state agent. No read when fallback is off.
const routeState =
  !routed && a0.agent && FALLBACK_ENABLED
    ? await agent(
        `Run exactly: cd ${a0.cwd || '.'} && claude-code gateway router --format json --agent claude --fallback none --model sonnet. ` +
          `It prints one JSON object; claudeState = candidates[0].state when candidates is non-empty, else the skipped[] entry whose agent ` +
          `is "claude" and its gate value, else "unknown". Never run anything else.`,
        { label: 'route-state', phase: 'Agent', schema: ROUTE_STATE, model: 'haiku', effort: 'low' }
      ).catch(() => null)
    : null
// Pressure: low/exhausted Claude account steps the sonnet fallback below down one model.
const MODEL_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const claudeState = routed ? routed.claudeState : routeState ? routeState.claudeState : null
const PRESSURE = claudeState === 'low' || claudeState === 'exhausted'
const routeModel = m => (PRESSURE && MODEL_DOWN[m]) ? MODEL_DOWN[m] : m
if (PRESSURE) log(`pressure: claude account ${claudeState} — stepping the sonnet fallback down one model`)
else if (!routed && !routeState) log(a0.agent && !FALLBACK_ENABLED ? 'pressure: no route read (fallback disabled)' : 'pressure: no route read — no downshift (fail open)')
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
      `${AGENTS_STEP}Then run exactly: cd ${CWD} && claude-code ask --agent ${a.agent} --format json${PROFILE ? ' --profile ' + PROFILE : ''}${VMODEL_FLAG} --workdir ${CWD} --timeout ${MIN}m${TOOLS ? ' --tools' : ''}${AGENTS_FLAG} --background - < <that file>. ` +
      `It prints ONE JSON line {"id":…,"log":…}; id = the job id. Loop claude-code ${a.agent} status <id> with sleep 5 until done, then claude-code agents result <id> --format json. ` +
      `The result is ONE indented JSON object {agent, status, provider, model, answer, error:{kind,message}|null, transport, …}. ok = exit 0 AND error null AND answer non-empty. ` +
      `Copy status and transport verbatim. Copy answer verbatim, never summarise. error = the envelope error's message, or the stderr tail on a non-JSON exit. Never retry.\n` +
      `Your reply MUST contain ok, answer and error every time, even on failure or timeout. On a failed or timed-out ask reply ` +
      `{"ok":false,"answer":"","error":"<the failure message>"}: the message goes in error, never in answer, and answer stays "". ` +
      `On success error is "".\n\nTASK:\n${task}${AGENTS_TAIL}`,
    { label, phase: 'Agent', schema: ENVELOPE, agentType: 'code-worker', model: 'haiku', effort: 'low' }
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
  agentType: 'code-worker',
  model: routeModel(a.model || 'sonnet'),
  effort: 'medium',
})
return { ok: !!fb, agent: 'claude', status: fb ? 'ok' : 'error', answer: typeof fb === 'string' ? fb : JSON.stringify(fb || ''), error: res.error, fallback: a.agent }
