import type { EngineInterface, Register } from 'claude-code'

// The harness hook that owns the decisions; this mod only asks it and applies the answer at spawn.
const HOOK_ARGS = ['hook', 'PreToolUse', '--dispatch-effort']
const QUERY_TIMEOUT_MS = 5000
const ANSWER_KEYS = ['description', 'prompt', 'subagentType', 'model']

type SpawnAnswer = Readonly<Record<string, string>>

const hookArgv = (home: string): string[] => [`${home}/.claude/bin/.hookd`, ...HOOK_ARGS]

const run = (payload: object) => ({ stdin: JSON.stringify(payload), timeoutMs: QUERY_TIMEOUT_MS })

const text = (value: unknown): string | undefined =>
  typeof value === 'string' && value !== '' ? value : undefined

// Only the four fields a spawn can take, each kept only when it is a non-empty string.
export const parseAnswer = (stdout: string): SpawnAnswer => {
  const raw: unknown = stdout.trim() ? JSON.parse(stdout) : {}
  if (raw === null || typeof raw !== 'object') return {}
  const fields = new Map<string, unknown>(Object.entries(raw))
  const answer: Record<string, string> = {}
  for (const key of ANSWER_KEYS) {
    const value = text(fields.get(key))
    if (value) answer[key] = value
  }
  return answer
}

// A fork inherits everything, and a workflow agent or teammate is not an Agent call the hook judges.
export const isJudged = (e: { fork?: unknown; workflow?: unknown; isTeammate?: unknown; tool_use_id?: unknown }) =>
  !e.fork && !e.workflow && !e.isTeammate && Boolean(e.tool_use_id)

const toolInput = (e: { subagentType?: string; prompt?: string; description?: string; model?: string }) => ({
  subagent_type: e.subagentType,
  prompt: e.prompt,
  description: e.description,
  ...(e.model ? { model: e.model } : {}),
})

const markReady = async ($: EngineInterface, home: string) => {
  const payload = { harness_spawn: 'ready', session_id: await $.session.id() }
  await $.process.run(hookArgv(home), run(payload))
}

const ask = async ($: EngineInterface, home: string, e: Parameters<typeof toolInput>[0] & { permissionMode?: string }) => {
  const payload = {
    harness_spawn: 'query',
    session_id: await $.session.id(),
    transcript_path: (await $.session.info()).transcriptPath,
    permission_mode: e.permissionMode,
    tool_name: 'Agent',
    tool_input: toolInput(e),
  }
  return $.process.run(hookArgv(home), run(payload))
}

export const register: Register = on => {
  // Tell the hook this session applies its rewrites at spawn, so PreToolUse prints notes instead.
  on('session.start', async ($, e, next) => {
    const home = await $.env.get('HOME')
    if (home) await markReady($, home)
    return next(e)
  }).catch(($, e, next) => next(e))

  on('agent.spawn', async ($, e, next) => {
    const home = await $.env.get('HOME')
    if (!home || !isJudged(e)) return next(e)
    const ran = await ask($, home, e)
    return next(ran.exitCode === 0 ? { ...e, ...parseAnswer(ran.stdout) } : e)
  }).catch(($, e, next) => next(e))
}
