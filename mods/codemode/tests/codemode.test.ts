import { describe, expect, test } from 'claude-code/testing'
import type { On } from 'claude-code'

import { wordOf } from '../hooks/register'

type Reply = { exitCode: number; stdout: string; stderr: string }

function world(on: On, reply: Reply) {
  const argvs: (readonly string[])[] = []
  on('env.get', async () => ({ value: '/h' }))
  on('session.id', async () => ({ value: 'S1' }))
  on('process.run', async (_$, e) => {
    argvs.push(e.argv)
    return { value: { ...reply, isStdoutTruncated: false, isStderrTruncated: false } }
  })
  return argvs
}

describe('codemode', () => {
  test('wordOf defaults to status', () => {
    expect([wordOf(''), wordOf('  on  '), wordOf('off now')]).toEqual(['status', 'on', 'off'])
  })

  test('/codemode on runs the verb with this session and shows its line', async ($, on) => {
    const argvs = world(on, { exitCode: 0, stdout: 'codemode: on (session)\n', stderr: '' })
    const out = await $.command.run({ command: 'codemode', args: 'on' } as never)
    expect(argvs[0]).toEqual(['/h/.claude/bin/claude-code', 'agents', 'codemode', 'on', '--session', 'S1'])
    expect(out.text).toBe('codemode: on (session)')
  })

  test('a refused word shows the verb stderr', async ($, on) => {
    world(on, { exitCode: 2, stdout: '', stderr: 'usage: codemode on|off|status\n' })
    const out = await $.command.run({ command: 'codemode', args: 'bogus' } as never)
    expect(out.text).toBe('usage: codemode on|off|status')
  })
})

describe('codemode failure', () => {
  test('a rejected spawn answers with a codemode line', async ($, on) => {
    on('env.get', async () => ({ value: '/h' }))
    on('session.id', async () => ({ value: 'S1' }))
    on('process.run', async () => {
      throw new Error('spawn ENOENT')
    })
    const out = await $.command.run({ command: 'codemode', args: '' } as never)
    expect(out.text).toContain('codemode: could not run the verb')
  })
})
