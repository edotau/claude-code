import { describe, expect, mock, test } from 'claude-code/testing'
import type { On } from 'claude-code'

const leg = { id: 'a1', type: 'claude', description: 'sonnet: fix the parser', status: 'running', files: ['internal/a/x.go', 'internal/b/x.go', 'c.go', 'd.go', 'e.go', 'f.go', 'g.go'] }
const disp = { id: 'd1', agent: 'codex', route: 'databricks:dev', duration: '1m', status: 'running', lastSignal: '2026-11-18T10:00:00Z', task: 'review', note: 'halfway\nthere' }
const tree = (extra: object = {}) => JSON.stringify({ sessions: [{ subagents: [{ ...leg, dispatches: [disp] }] }], ...extra })
const PROPS = { hasSurvey: false, isWorking: false, maxRows: 20, bodyColumns: 80 } as never

type Call = { argv: readonly string[] }

type Reply = { exitCode: number; stdout: string; stderr?: string }

// world stubs the layers beneath the plugin; `reply` answers each tree call, possibly late.
function world(on: On, reply: (n: number) => Reply | Promise<Reply>) {
  const calls: Call[] = []
  on('process.run', async (_$, e) => {
    calls.push({ argv: e.argv })
    const r = await reply(calls.length)

    return { value: { exitCode: r.exitCode, stdout: r.stdout, stderr: r.stderr ?? '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  on('agent.list', async () => ({ value: [] }))
  on('command.register', async () => ({ value: {} as never }))
  on('session.start', async (_$, e) => ({ cwd: e.cwd }))

  return calls
}

const start = ($: any) => $.session.start({ cwd: '/r', surface: 'terminal', isInteractive: true })
const texts = async (ui: any) => (await ui.findAll({ type: 'Text' })).map((t: any) => t.text as string)
const open = async ($: any) => {
  await $.command.run({ command: 'agents-band' })

  return $.ui.mount({ plugin: 'agents-band', surface: 'terminal', component: 'AbovePrompt', props: PROPS })
}

describe('agents-band', () => {
  test('renders held files and a note once a leaf row is expanded', async ($, on) => {
    const calls = world(on, () => ({ exitCode: 0, stdout: tree({ free: ['internal/a/y.go'] }) }))
    const clock = mock.clock(on)
    await start($)
    await clock.settle()
    expect(calls[0]!.argv).toContain('--files')
    const ui = await open($)
    const rows = await ui.findAll({ type: 'Button' })
    expect(rows.map((r: any) => r.props.label as string).some((l: string) => l.includes('7f') && l.includes('✎'))).toBe(true)
    await ui.press({ key: 'leg:d1' })
    const lines = await texts(ui)
    expect(lines.some((l: string) => l.includes('files: a/x.go b/x.go c.go d.go e.go (+2)'))).toBe(true)
    expect(lines.some((l: string) => l.includes('note: halfway there'))).toBe(true)
    await ui.press({ key: 'free' })
    expect((await texts(ui)).some((l: string) => l.includes('y.go'))).toBe(true)
  })

  test('falls back to the plain call when the binary has no --files, without an error', async ($, on) => {
    const plain = JSON.stringify({ sessions: [{ subagents: [{ ...leg, files: undefined }] }] })
    const calls = world(on, n => (n === 1 ? { exitCode: 2, stdout: '', stderr: 'flag provided but not defined: -files' } : { exitCode: 0, stdout: plain }))
    const clock = mock.clock(on)
    await start($)
    await clock.advance(5000)
    expect(calls.map(c => c.argv.includes('--files'))).toEqual([true, false])
    const ui = await open($)
    const lines = (await texts(ui)).join('\n')
    expect(lines).not.toContain('stale')
    expect(lines).toContain('fix the parser')
  })

  test('missing fields draw today\'s rows', async ($, on) => {
    world(on, () => ({ exitCode: 0, stdout: JSON.stringify({ sessions: [{ subagents: [{ type: 'claude', description: 'sonnet: plain', status: 'running' }] }] }) }))
    const clock = mock.clock(on)
    await start($)
    await clock.settle()
    const ui = await open($)
    expect((await texts(ui)).some((l: string) => l.includes('plain'))).toBe(true)
    expect(await ui.findAll({ type: 'Button' })).toHaveLength(0)
  })

  test('a failed call keeps the last good state and marks it stale', async ($, on) => {
    const calls = world(on, n => (n === 1 ? { exitCode: 0, stdout: tree() } : { exitCode: 1, stdout: '' }))
    const clock = mock.clock(on)
    await start($)
    await clock.advance(5000)
    expect(calls).toHaveLength(2)
    const ui = await open($)
    const lines = await texts(ui)
    expect(lines.some((l: string) => l.includes('stale 5s'))).toBe(true)
    expect((await ui.findAll({ type: 'Button' })).length).toBeGreaterThan(0)
  })

  test('idle polls every 30 s and returns to 5 s when a leaf appears', async ($, on) => {
    const idle = JSON.stringify({ sessions: [] })
    let live = false
    const calls = world(on, () => ({ exitCode: 0, stdout: live ? tree() : idle }))
    const clock = mock.clock(on)
    await start($)
    await clock.advance(60000)
    expect(calls).toHaveLength(3)
    live = true
    await clock.advance(30000)
    expect(calls).toHaveLength(4)
    await clock.advance(15000)
    expect(calls).toHaveLength(7)
  })

  test('a tick during a running call does not start another', async ($, on) => {
    let release: () => void = () => {}
    const gate = new Promise<void>(r => (release = r))
    const calls = world(on, async () => {
      await gate

      return { exitCode: 0, stdout: tree() }
    })
    const clock = mock.clock(on)
    void start($)
    await clock.advance(20000)
    expect(calls).toHaveLength(1)
    release()
    await clock.settle()
    await clock.advance(5000)
    expect(calls).toHaveLength(2)
  })
})
