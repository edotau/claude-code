import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { Usage } from '../types'

const active = atom({ plugin: 'slash-usage', key: 'active' } as const, [] as Usage[])

const MAX_ROWS = 2
const MAX_SUGGESTIONS = 6
const MAX_DESCRIPTION = 110
const SLASH_TOKEN = /(?:^|\s)\/([A-Za-z0-9][\w:.-]*)/g
const AGENT_TOKEN = /(?:^|\s)@agent-([\w-]+)/g

// The value of `key:` in a markdown file's frontmatter, unquoted.
function frontmatter(text: string, key: string): string | undefined {
  if (!text.startsWith('---')) return undefined
  const end = text.indexOf('\n---', 3)
  const m = text.slice(0, end < 0 ? undefined : end).match(new RegExp(`^${key}:\\s*(.+)$`, 'm'))
  return m?.[1]?.trim().replace(/^["']|["']$/g, '')
}

async function hintOf($: EngineInterface, path: string): Promise<string | undefined> {
  try {
    const text = await $.fs.read(path)
    return typeof text === 'string' ? frontmatter(text, 'argument-hint') : undefined
  } catch {
    return undefined // unreadable: the band falls back to the description
  }
}

async function namesIn($: EngineInterface, dir: string): Promise<string[]> {
  try {
    return (await $.fs.list(dir)).map(e => e.name)
  } catch {
    return []
  }
}

// `argument-hint` per command (commands/<name>.md) and skill (skills/<name>/SKILL.md).
async function loadHints($: EngineInterface): Promise<Map<string, string>> {
  const root = (await $.env.get('CLAUDE_CONFIG_DIR')) || `${await $.env.get('HOME')}/.claude`
  const files: [string, string][] = [
    ...(await namesIn($, `${root}/commands`))
      .filter(n => n.endsWith('.md'))
      .map((n): [string, string] => [n.slice(0, -3), `${root}/commands/${n}`]),
    ...(await namesIn($, `${root}/skills`)).map((n): [string, string] => [n, `${root}/skills/${n}/SKILL.md`]),
  ]
  const hints = await Promise.all(files.map(async ([name, path]) => [name, await hintOf($, path)] as const))
  return new Map(hints.filter((h): h is readonly [string, string] => h[1] !== undefined))
}

// An agent file's description, first sentence only: the `|` block up to its first <example> or blank line.
function agentSummary(text: string): string | undefined {
  const lines = text.split('\n')
  const i = lines.findIndex(l => l.startsWith('description:'))
  if (i < 0) return undefined
  const inline = (lines[i] ?? '').slice('description:'.length).trim()
  const body = inline && inline !== '|' && inline !== '>' ? [inline] : []
  for (const l of lines.slice(i + 1)) {
    if (!body.length && !l.trim()) continue
    if (!l.startsWith('  ') || l.trim().startsWith('<example>')) break
    body.push(l.trim())
  }
  return body.join(' ').split(/(?<=\.)\s/)[0] || undefined
}

// One-line summary per subagent (agents/<name>.md), keyed by name.
async function loadAgents($: EngineInterface): Promise<Map<string, string>> {
  const root = (await $.env.get('CLAUDE_CONFIG_DIR')) || `${await $.env.get('HOME')}/.claude`
  const names = (await namesIn($, `${root}/agents`)).filter(n => n.endsWith('.md')).map(n => n.slice(0, -3))
  const rows = await Promise.all(
    names.map(async n => {
      try {
        const text = await $.fs.read(`${root}/agents/${n}.md`)
        return [n, typeof text === 'string' ? agentSummary(text) : undefined] as const
      } catch {
        return [n, undefined] as const // unreadable: no band row for it
      }
    }),
  )
  return new Map(rows.filter((r): r is readonly [string, string] => r[1] !== undefined))
}

// Rebuilt by session.start (a reload reruns it).
let descriptions = new Map<string, string>()
let hints = new Map<string, string>()
let agents = new Map<string, string>()

const clip = (s: string): string => (s.length > MAX_DESCRIPTION ? `${s.slice(0, MAX_DESCRIPTION - 1)}…` : s)

function usageLine(name: string): string {
  const hint = hints.get(name)
  if (hint) return `/${name} ${hint}`
  const desc = descriptions.get(name) ?? ''
  return `/${name} — ${clip(desc)}`
}

function commandsIn(text: string): Usage[] {
  const slash = [...new Set([...text.matchAll(SLASH_TOKEN)].map(m => m[1] ?? ''))]
    .filter(n => descriptions.has(n))
    .map(name => ({ name, line: usageLine(name) }))
  const mentioned = [...new Set([...text.matchAll(AGENT_TOKEN)].map(m => m[1] ?? ''))]
    .filter(n => agents.has(n))
    .map(n => ({ name: `@agent-${n}`, line: `@agent-${n} — ${clip(agents.get(n) ?? '')}` }))
  return [...slash, ...mentioned].slice(0, MAX_ROWS)
}

async function refresh($: EngineInterface, text: string): Promise<void> {
  const rows = commandsIn(text)
  const prev = await read($, active)
  if (rows.map(u => u.name).join() !== prev.map(u => u.name).join()) await update($, active, () => rows)
}

// Commands and skills added mid-session show up after the next submitted prompt.
async function reindex($: EngineInterface): Promise<void> {
  descriptions = new Map((await $.command.list()).map(c => [c.name, c.description]))
  hints = await loadHints($)
  agents = await loadAgents($)
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await reindex($)
    return next(e)
  })

  on('prompt.autocomplete', { token: /^\// }, async ($, e, next) => {
    const base = await next(e)
    if (e.text.slice(0, e.start).trim() === '') return base // a leading slash is the engine's own typeahead
    const prefix = e.token.slice(1).toLowerCase()
    const rows = [...descriptions]
      .filter(([name]) => name.toLowerCase().startsWith(prefix))
      .slice(0, MAX_SUGGESTIONS)
      .map(([name, description]) => ({ text: `/${name}`, description }))
    return { suggestions: [...base.suggestions, ...rows] }
  })

  on('prompt.edit', async ($, e, next) => {
    const box = await next(e)
    await refresh($, box.text)
    return box
  })

  on('prompt.change', async ($, e, next) => {
    await refresh($, e.text)
    return next(e)
  })

  on('prompt.submit', async ($, e, next) => {
    await update($, active, () => [])
    void reindex($)
    return next(e)
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const rows = await read($, active)
    if (e.props.hasSurvey || rows.length === 0) return next(e)
    const { Box, Text } = $.ui.resolve(e)
    return (
      <Box flexDirection="column">
        {rows.map(u => (
          <Text key={u.name} dimColor>
            {u.line}
          </Text>
        ))}
      </Box>
    )
  })
}
