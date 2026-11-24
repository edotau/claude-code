import type { Register } from 'claude-code'

const WORDS = ['on', 'off', 'status']
const TIMEOUT_MS = 5000

// The first word is the whole argument; none means status, a bad word is the verb's to refuse (exit 2).
export const wordOf = (args: string): string => args.trim().split(/\s+/)[0] || 'status'

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    try {
      await $.command.register({
        name: 'codemode',
        description: 'Code mode for this session: raw Bash output, no rtk rewrite',
        argumentHint: `[${WORDS.join('|')}]`,
        immediate: true,
      })
    } catch {
      // without the command the session keeps its launch-time mode
    }
    return next(e)
  })

  on('command.run', { command: 'codemode' }, async ($, e) => {
    try {
      const home = await $.env.get('HOME')
      if (!home) return { text: 'codemode: HOME is not set' }
      const argv = [`${home}/.claude/bin/claude-code`, 'agents', 'codemode', wordOf(e.args), '--session', await $.session.id()]
      const ran = await $.process.run(argv, { timeoutMs: TIMEOUT_MS })
      const out = (ran.exitCode === 0 ? ran.stdout : ran.stderr || ran.stdout).trim()
      return { text: out || `codemode: exit ${ran.exitCode}` }
    } catch (err) {
      return { text: `codemode: could not run the verb (${err instanceof Error ? err.message : String(err)})` }
    }
  })
}
