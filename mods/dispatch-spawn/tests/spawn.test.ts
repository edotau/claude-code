import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import { parseAnswer } from '../hooks/register'

type Reply = { exitCode: number; stdout: string }
type Seen = { argv: readonly string[]; stdin: string }

const SPAWN = { tool_use_id: 't1', prompt: 'p', description: 'sonnet: old', subagentType: 'claude', fork: false } as never

// world stubs the layers beneath the mod; `spawned` records what the spawn finally received.
function world(on: On, reply: Reply) {
  const runs: Seen[] = []
  const spawned: Record<string, unknown>[] = []
  on('session.start', async (_$, e) => ({ cwd: e.cwd }))
  on('env.get', async () => ({ value: '/h' }))
  on('session.id', async () => ({ value: 'S1' }))
  on('session.info', async () => ({ value: { transcriptPath: '/t.jsonl' } as never }))
  on('process.run', async (_$, e) => {
    runs.push({ argv: e.argv, stdin: String(e.init?.stdin) })
    return { value: { ...reply, stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('agent.spawn', async (_$, e) => {
    spawned.push(e as Record<string, unknown>)
    return { model: String(e.model ?? 'inherit') }
  })
  return { runs, spawned }
}

const answer = JSON.stringify({ description: 'opus: new', model: 'opus', prompt: '' })

describe('dispatch-spawn', () => {
  test('session.start tells the hook the session is ready', async ($, on) => {
    const { runs } = world(on, { exitCode: 0, stdout: '{}' })
    await $.session.start({ cwd: '/r', surface: 'terminal', isInteractive: true })
    expect(runs[0]!.argv).toEqual(['/h/.claude/bin/.hookd', 'hook', 'PreToolUse', '--dispatch-effort'])
    expect(JSON.parse(runs[0]!.stdin)).toEqual({ harness_spawn: 'ready', session_id: 'S1' })
  })

  test('an Agent spawn is queried and the answer applied', async ($, on) => {
    const { runs, spawned } = world(on, { exitCode: 0, stdout: answer })
    await $.agent.spawn(SPAWN)
    expect(JSON.parse(runs[0]!.stdin).harness_spawn).toBe('query')
    expect(JSON.parse(runs[0]!.stdin).transcript_path).toBe('/t.jsonl')
    expect(spawned[0]).toMatchObject({ description: 'opus: new', model: 'opus', prompt: 'p' })
  })

  for (const [name, extra] of [
    ['a fork', { fork: true }],
    ['a workflow agent', { workflow: { runId: 'r', agentIndex: 0 } }],
    ['a teammate', { isTeammate: true }],
  ] as const) {
    test(`${name} passes through untouched`, async ($, on) => {
      const { runs, spawned } = world(on, { exitCode: 0, stdout: answer })
      await $.agent.spawn({ ...(SPAWN as object), ...extra } as never)
      expect(runs).toHaveLength(0)
      expect(spawned[0]).toMatchObject({ description: 'sonnet: old' })
    })
  }

  for (const [name, reply] of [
    ['a non-zero exit', { exitCode: 1, stdout: answer }],
    ['unparsable output', { exitCode: 0, stdout: 'nope' }],
  ] as const) {
    test(`${name} passes the call through`, async ($, on) => {
      const { spawned } = world(on, reply)
      await $.agent.spawn(SPAWN)
      expect(spawned[0]).toMatchObject({ description: 'sonnet: old' })
    })
  }

  test('parseAnswer keeps only non-empty strings of the four fields', () => {
    expect(parseAnswer(answer)).toEqual({ description: 'opus: new', model: 'opus' })
    expect(parseAnswer('')).toEqual({})
    expect(parseAnswer('[1]')).toEqual({})
    expect(() => parseAnswer('nope')).toThrow()
  })
})
