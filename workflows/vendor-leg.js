export const meta = {
  name: 'vendor-leg',
  description: 'One non-Claude leg (codex|gemini|opencode|copilot) as a workflow stage: smoke ask, JSON envelope relay, sonnet fallback',
  whenToUse:
    'Inline from another script: workflow("vendor-leg", {leg, cwd, task, smoke?: true, fallback?: true, tools?: false, timeoutMin?: 3}). Returns {ok, leg, answer, provider, model, error, skipped?, fallback?}. Budget: smoke ask of at most timeoutMin plus task ask of at most timeoutMin (timeoutMin capped at 3) totals at most 6 minutes, inside the Bash tools 10-minute ceiling — the relay leaf never retries either call. gemini: at most one call per loop round and never the opus tier (it 429s on this key). copilot: pass tools:true for a commit leg (never falls back — a commit cannot be redone by a different model). opencode: budget 1.5-3 min per call.',
  phases: [{ title: 'Leg', detail: 'haiku leaf runs claude-code ask --format json and relays the envelope; sonnet fallback on failure' }],
}

// ── Args ────────────────────────────────────────────────────────────────────
const a = args && typeof args === 'object' && !Array.isArray(args) ? args : {}
const LEGS = ['codex', 'gemini', 'opencode', 'copilot']
if (!LEGS.includes(a.leg)) return { error: 'vendor-leg needs args.leg in ' + LEGS.join('|') }
if (!a.task) return { error: 'vendor-leg needs args.task' }
if (a.task.length > 90 * 1024) return { error: 'vendor-leg: task over 90 KiB (ask rejects prompts over 96 KiB); pass excerpts, not whole diffs' }
const CWD = a.cwd || '.'
const MIN = Number.isInteger(a.timeoutMin) && a.timeoutMin > 0 && a.timeoutMin <= 3 ? a.timeoutMin : 3
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
      `then run exactly: cd ${CWD} && claude-code ask --agent ${a.leg} --format json --workdir ${CWD} --timeout ${MIN}m${a.tools === true ? ' --tools' : ''} - < <that file>. ` +
      `Set the Bash tool timeout to 600000. It prints ONE JSON line {agent, provider, model, answer, error, …}. ok = exit 0 AND error empty AND answer non-empty. ` +
      `Copy answer verbatim, never summarise. error = the envelope error, or the stderr tail on a non-JSON exit. Never retry.\n\nTASK:\n${task}`,
    { label, phase: 'Leg', schema: ENVELOPE, model: 'haiku', effort: 'low' }
  )

phase('Leg')
const smoke = a.smoke === false ? { ok: true, answer: 'READY', error: '' } : await run('Reply READY only.', 'smoke:' + a.leg)
const res =
  smoke && smoke.ok && /READY/i.test(smoke.answer || '')
    ? (await run(a.task, 'ask:' + a.leg)) || { ok: false, answer: '', error: 'relay leaf returned no result' }
    : { ok: false, answer: '', error: (smoke && smoke.error) || 'smoke ask failed' }
if (res.ok) return { ...res, leg: a.leg }
log(`${a.leg} leg failed: ${res.error}`)
if (a.fallback === false || a.tools === true) return { ...res, leg: a.leg, skipped: true }
const fb = await agent(`${a.task}\n\n(Work in ${CWD}; prefix every shell command with cd ${CWD} &&. Read-only unless the task says otherwise.)`, {
  label: 'fallback:' + a.leg,
  phase: 'Leg',
  model: 'sonnet',
  effort: 'medium',
})
return { ok: !!fb, leg: a.leg, answer: typeof fb === 'string' ? fb : JSON.stringify(fb || ''), error: res.error, fallback: true }
