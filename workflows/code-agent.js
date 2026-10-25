export const meta = {
  name: 'code-agent',
  description: 'One non-Claude leg (codex|gemini|opencode|copilot) as a workflow stage: smoke ask, JSON envelope relay, sonnet fallback',
  whenToUse:
    'Inline from another script: workflow("code-agent", {leg?, tier?, cwd, task, smoke?: true, fallback?: true, tools?: false, timeoutMin?: 3}). No leg: claude-code route --format json picks it at tier (default sonnet). Returns {ok, leg, answer, provider, model, error, skipped?, fallback?}. Budget: smoke ask of at most timeoutMin plus task ask of at most timeoutMin (timeoutMin capped at 3 for codex/opencode/copilot, 8 for gemini) totals at most 6 minutes, inside the Bash tools 10-minute ceiling — the relay leaf never retries either call. gemini: tools default true (agy needs --tools to read files or search), the smoke ask is skipped so only the task ask runs (at most 8 min), keeping smoke+task inside the ceiling — an explicit smoke:true runs it and caps each call at 4 min; at most one call per loop round; no measured 429s on agy — prefer flash-high. copilot: pass tools:true for a commit leg (never falls back — a commit cannot be redone by a different model). opencode: budget 1.5-3 min per call.',
  phases: [{ title: 'Leg', detail: 'haiku leaf runs claude-code ask --format json and relays the envelope; sonnet fallback on failure' }],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a0 = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const LEGS = ['codex', 'gemini', 'opencode', 'copilot']
if (!a0.task) return { error: 'code-agent needs args.task' }
const TIERS = ['opus', 'sonnet', 'haiku']
if (a0.tier && !TIERS.includes(a0.tier)) return { error: 'code-agent: args.tier must be ' + TIERS.join('|') }
const ROUTE_PICK = {
  type: 'object',
  additionalProperties: false,
  required: ['leg', 'claudeState'],
  properties: { leg: { type: 'string' }, claudeState: { type: 'string' } },
}
// No leg: the router's first vendor candidate at the tier (gated on availability, pinned account and quota).
// Same call also reads the Claude account's own pressure state, so the sonnet fallback below can step down.
const routed = a0.leg
  ? null
  : await agent(
      `Run exactly: cd ${a0.cwd || '.'} && claude-code route --format json --fallback vendors --tier ${a0.tier || 'sonnet'}. ` +
        `It prints one JSON object; leg = candidates[0].runner, or leg = "" when candidates is empty. ` +
        `Also run exactly: cd ${a0.cwd || '.'} && claude-code route --format json --agent claude --fallback none --tier sonnet. ` +
        `claudeState = that result's candidates[0].state when candidates is non-empty, else the skipped[] entry whose runner ` +
        `is "claude" and its gate value, else "unknown". Never run anything else.`,
      { label: 'route', phase: 'Leg', schema: ROUTE_PICK, model: 'haiku', effort: 'low' }
    )
const a = routed ? { ...a0, leg: routed.leg } : a0
// Router pressure: low/exhausted Claude account steps the sonnet fallback below down one tier.
const TIER_DOWN = { opus: 'sonnet', sonnet: 'haiku', haiku: 'haiku' }
const PRESSURE = !!(routed && (routed.claudeState === 'low' || routed.claudeState === 'exhausted'))
const routeTier = m => (PRESSURE && TIER_DOWN[m]) ? TIER_DOWN[m] : m
if (PRESSURE) log(`router: claude account ${routed.claudeState} — stepping the sonnet fallback down one tier`)
else if (!routed) log('router: no route read (explicit leg) — no downshift (fail open)')
if (!LEGS.includes(a.leg)) return { error: 'code-agent needs args.leg in ' + LEGS.join('|') + (routed ? ' (route picked none)' : '') }
if (a.task.length > 90 * 1024) return { error: 'code-agent: task over 90 KiB (ask rejects prompts over 96 KiB); pass excerpts, not whole diffs' }
const CWD = a.cwd || '.'
const SMOKE = a.leg === 'gemini' ? a.smoke === true : a.smoke !== false
// gemini's opt-in smoke halves its per-call cap, so smoke + task stay within 8 minutes
const CAP = a.leg === 'gemini' ? (SMOKE ? 4 : 8) : 3
const MIN = Number.isInteger(a.timeoutMin) && a.timeoutMin > 0 && a.timeoutMin <= CAP ? a.timeoutMin : CAP
const TOOLS = a.leg === 'gemini' ? a.tools !== false : a.tools === true
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
  },
}
// One `claude-code ask` relay per call; the heredoc-to-file hop keeps shell quoting away from the task text.
const run = (task, label) =>
  agent(
    `Relay one external-model call; do NOT answer the task yourself. Write the TASK below verbatim to a scratch file with a quoted heredoc (<<'EOF'), ` +
      `then run exactly: cd ${CWD} && claude-code ask --agent ${a.leg} --format json --workdir ${CWD} --timeout ${MIN}m${TOOLS ? ' --tools' : ''} - < <that file>. ` +
      `Set the Bash tool timeout to 600000. It prints ONE JSON line {agent, provider, model, answer, error, …}. ok = exit 0 AND error empty AND answer non-empty. ` +
      `Copy answer verbatim, never summarise. error = the envelope error, or the stderr tail on a non-JSON exit. Never retry.\n\nTASK:\n${task}`,
    { label, phase: 'Leg', schema: ENVELOPE, model: 'haiku', effort: 'low' }
  )

phase('Leg')
const smoke = !SMOKE ? { ok: true, answer: 'READY', error: '' } : await run('Reply READY only.', 'smoke:' + a.leg)
const res =
  smoke && smoke.ok && /READY/i.test(smoke.answer || '')
    ? (await run(a.task, 'ask:' + a.leg)) || { ok: false, answer: '', error: 'relay leaf returned no result' }
    : { ok: false, answer: '', error: (smoke && smoke.error) || 'smoke ask failed' }
if (res.ok) return { ...res, leg: a.leg }
log(`${a.leg} leg failed: ${res.error}`)
if (a.fallback === false || TOOLS) return { ...res, leg: a.leg, skipped: true }
const fb = await agent(`${a.task}\n\n(Work in ${CWD}; prefix every shell command with cd ${CWD} &&. Read-only unless the task says otherwise.)`, {
  label: 'fallback:' + a.leg,
  phase: 'Leg',
  model: routeTier('sonnet'),
  effort: 'medium',
})
return { ok: !!fb, leg: a.leg, answer: typeof fb === 'string' ? fb : JSON.stringify(fb || ''), error: res.error, fallback: true }
