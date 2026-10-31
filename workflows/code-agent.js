export const meta = {
  name: 'code-agent',
  description: 'One non-Claude agent (codex|gemini|opencode|copilot) as a workflow stage, optionally on a named profile: smoke ask, JSON envelope relay, sonnet fallback (off when a profile is named)',
  whenToUse:
    'Inline from another script: workflow("code-agent", {agent?, model?, vendorModel?, profile?, cwd, task, smoke?: true, fallback?: true, tools?: false, timeoutMin?: 3, agents?: string}). No agent: claude-code gateway router --format json picks it at the model (default sonnet). model is the relay/fallback Claude model; vendorModel is passed to ask as --model (e.g. byok for the curated Databricks pool). profile names a vault profile: both asks get --profile <p> (ask resolves host, provider and auth) and the sonnet fallback is off, since it would run on the session route. Returns {ok, agent, profile?, answer, provider, model, error, status?, transport?, skipped?, fallback?} — status is the envelope\'s ok|error|partial|quota|stalled; fallback is the original agent\'s name when the in-workflow sonnet fallback ran as agent claude. Budget: smoke ask of at most timeoutMin plus task ask of at most timeoutMin (timeoutMin capped at 3 for codex/opencode/copilot, 8 for gemini) totals at most 6 minutes, inside the Bash tools 10-minute ceiling — the relay leaf never retries either call. gemini: tools default true (agy needs --tools to read files or search), the smoke ask is skipped so only the task ask runs (at most 8 min), keeping smoke+task inside the ceiling — an explicit smoke:true runs it and caps each call at 4 min; at most one call per loop round; no measured 429s on agy — prefer flash-high. copilot: pass tools:true for a commit dispatch (never falls back — a commit cannot be redone by a different model). opencode: budget 1.5-3 min per call. agents is the text of an agents file (JSON), written through the same heredoc hop as the task and passed to ask as --agents <file> on both asks.',
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
const SMOKE = a.agent === 'gemini' ? a.smoke === true : a.smoke !== false
// gemini's opt-in smoke halves its per-call cap, so smoke + task stay within 8 minutes
const CAP = a.agent === 'gemini' ? (SMOKE ? 4 : 8) : 3
const MIN = Number.isInteger(a.timeoutMin) && a.timeoutMin > 0 && a.timeoutMin <= CAP ? a.timeoutMin : CAP
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
const run = (task, label) =>
  agent(
    `Agent: ${a.agent}\n` +
      `Relay one external-model call; do NOT answer the task yourself. Write the TASK below verbatim to a scratch file with a quoted heredoc (<<'EOF'), ` +
      `${AGENTS_STEP}Then run exactly: cd ${CWD} && claude-code ask --agent ${a.agent} --format json${PROFILE ? ' --profile ' + PROFILE : ''}${VMODEL_FLAG} --workdir ${CWD} --timeout ${MIN}m${TOOLS ? ' --tools' : ''}${AGENTS_FLAG} - < <that file>. ` +
      `Set the Bash tool timeout to 600000. It prints ONE JSON line {agent, status, provider, model, answer, error, transport, …}. ok = exit 0 AND error empty AND answer non-empty. ` +
      `Copy status and transport verbatim. Copy answer verbatim, never summarise. error = the envelope error, or the stderr tail on a non-JSON exit. Never retry.\n\nTASK:\n${task}${AGENTS_TAIL}`,
    { label, phase: 'Agent', schema: ENVELOPE, agentType: 'code-worker', model: 'haiku', effort: 'low' }
  )

phase('Agent')
const smoke = !SMOKE ? { ok: true, answer: 'READY', error: '' } : await run('Reply READY only.', 'smoke:' + a.agent)
const res =
  smoke && smoke.ok && /READY/i.test(smoke.answer || '')
    ? (await run(a.task, 'ask:' + a.agent)) || { ok: false, answer: '', error: 'relay leaf returned no result' }
    : { ok: false, answer: '', error: (smoke && smoke.error) || 'smoke ask failed' }
const withProfile = o => (PROFILE ? { ...o, profile: PROFILE } : o)
if (res.ok) return withProfile({ ...res, agent: a.agent })
log(`${a.agent} dispatch failed: ${res.error}`)
if (!FALLBACK_ENABLED || TOOLS) return withProfile({ ...res, agent: a.agent, skipped: true })
const fb = await agent(`${a.task}\n\n(Work in ${CWD}; prefix every shell command with cd ${CWD} &&. Read-only unless the task says otherwise.)`, {
  label: 'fallback:' + a.agent,
  phase: 'Agent',
  agentType: 'code-worker',
  model: routeModel('sonnet'),
  effort: 'medium',
})
return { ok: !!fb, agent: 'claude', status: fb ? 'ok' : 'error', answer: typeof fb === 'string' ? fb : JSON.stringify(fb || ''), error: res.error, fallback: a.agent }
