import { atom, read, update } from 'claude-code'
import type { EngineInterface, Hook, MatchedHook, PromptDecoration, Register } from 'claude-code'

import type { Part, Usage } from '../types'

const active = atom({ plugin: 'slash-usage', key: 'active' } as const, [] as Usage[])

const MAX_ROWS = 2
const MAX_SUGGESTIONS = 6
const MAX_DESCRIPTION = 110
const SLASH_TOKEN = /(?:^|\s)\/([A-Za-z0-9][\w:.-]*)/g
const AGENT_TOKEN = /(?:^|\s)@agent-([\w-]+)/g
const PARAM = /\[?(--[\w-]+)(?:[ =](<[^>]+>|[^\s[\]|-][^\s[\]]*))?\]?|\[<[^>]+>\]|<[^>]+>(?!\])/g
const WORD = /'[^']*'?|"[^"]*"?|\S+/g
// Alternatives naming a kind of value (<json|file>) are a placeholder, not choices to complete.
const KINDS = new Set(['file', 'path', 'dir', 'host', 'url', 'name', 'id'])

// One parameter of an argument-hint: `[--flag <value>]`, `--flag a|b`, `[--switch]` or a positional `<task>`.
type Param = { raw: string; flag?: string; value?: string; choices: string[]; optional: boolean }

// Where the cursor stands in a command's arguments: what is filled, what is awaited, what is being typed.
type Context = {
  name: string
  start: number
  params: Param[]
  used: ReadonlySet<string>
  expecting?: Param
  partial: string
  hasPositional: boolean
  isFresh: boolean
}

function choicesOf(value: string | undefined): string[] {
  const alts = (value ?? '').replace(/^<|>$/g, '').split('|')
  const isLiteral = alts.length > 1 && alts.every(a => /^[a-z0-9][\w.:-]*$/.test(a) && !KINDS.has(a))
  return isLiteral ? alts : []
}

function paramsOf(hint: string): Param[] {
  return [...hint.matchAll(PARAM)].map(m => {
    const optional = m[0].startsWith('[')
    const raw = !optional && m[0].endsWith(']') ? m[0].slice(0, -1) : m[0] // a `]` closing an outer group is not ours
    return { raw, flag: m[1], value: m[2], choices: choicesOf(m[2]), optional }
  })
}

// The value of `key:` in a markdown file's frontmatter, unquoted; [ \t] keeps an empty value from eating the next line.
function frontmatter(text: string, key: string): string | undefined {
  if (!text.startsWith('---')) return undefined
  const end = text.indexOf('\n---', 3)
  const m = text.slice(0, end < 0 ? undefined : end).match(new RegExp(`^${key}:[ \\t]*(\\S.*)$`, 'm'))
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
let parsed = new Map<string, Param[]>()
let last: { text: string; cursor: number; context: Context | undefined } | undefined

// A command's parameters, parsed from its hint once per reindex.
function paramsFor(name: string): Param[] {
  const known = parsed.get(name)
  if (known) return known
  const params = paramsOf(hints.get(name) ?? '')
  parsed.set(name, params)
  return params
}

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

// The last known command before the cursor and how far its arguments are filled; undefined outside one.
function contextAt(text: string, cursor: number): Context | undefined {
  if (last && last.text === text && last.cursor === cursor) return last.context // one parse per keystroke
  const context = parseContext(text, cursor)
  last = { text, cursor, context }
  return context
}

function parseContext(text: string, cursor: number): Context | undefined {
  const cmd = [...text.slice(0, cursor).matchAll(SLASH_TOKEN)].filter(m => hints.has(m[1] ?? '')).pop()
  const name = cmd?.[1]
  if (!cmd || !name) return undefined
  const params = paramsFor(name)
  const args = text.slice(cmd.index + cmd[0].length, cursor)
  const typed = args.match(WORD) ?? []
  const partial = /\S$/.test(args) ? (typed.pop() ?? '') : ''
  const used = new Set<string>()
  let expecting: Param | undefined
  let hasPositional = false
  for (const w of typed) {
    const awaited = expecting
    expecting = undefined
    if (awaited) continue // this word is the awaited flag's value
    const p = w.startsWith('--') ? params.find(q => q.flag === w.split('=')[0]) : undefined
    if (p?.flag) used.add(p.flag)
    if (p?.value && !w.includes('=')) expecting = p
    if (!w.startsWith('-')) hasPositional = true
  }
  const start = cmd.index + cmd[0].length - name.length - 1
  return { name, start, params, used, expecting, partial, hasPositional, isFresh: args === '' }
}

const isFilled = (c: Context, p: Param): boolean => (p.flag ? c.used.has(p.flag) : c.hasPositional)

// The usage row for the command at the cursor: filled parameters struck, the next one to give in bold.
function partsOf(c: Context): Part[] {
  const open = c.params.filter(p => !isFilled(c, p))
  const now = c.expecting ?? open.find(p => p.flag && !p.optional) ?? open.find(p => !p.flag)
  const state = (p: Param): Part['state'] => (p === now ? 'now' : isFilled(c, p) ? 'done' : 'todo')
  return [{ text: `/${c.name}`, state: 'todo' }, ...c.params.map(p => ({ text: p.raw, state: state(p) }))]
}

// What is still to give, drawn dim at the cursor; nothing while a word is being typed.
function ghostOf(c: Context): string {
  if (c.partial) return ''
  if (c.expecting) return c.expecting.value ?? ''
  const rest = c.params.filter(p => !isFilled(c, p)).map(p => p.raw).join(' ')
  return rest && c.isFresh ? ` ${rest}` : rest
}

// Bold on the command's name, and the remaining usage as virtual text when the cursor ends its line.
function decorationsOf(text: string, cursor: number): PromptDecoration[] {
  const c = contextAt(text, cursor)
  if (!c) return []
  const name: PromptDecoration = { start: c.start, end: c.start + c.name.length + 1, bold: true }
  const ghost = /^[ \t]*(\n|$)/.test(text.slice(cursor)) ? ghostOf(c) : ''
  return ghost ? [name, { start: cursor, end: cursor, virtualText: ghost, dimColor: true }] : [name]
}

// Typeahead rows for the word at the cursor: an awaited flag's choices, else the command's unused flags.
function completions(c: Context, token: string): { text: string; description: string }[] {
  if (c.expecting) {
    const flag = c.expecting.flag ?? ''
    return c.expecting.choices.filter(v => v.startsWith(token)).map(v => ({ text: `${v} `, description: flag }))
  }
  if (!token.startsWith('-')) return []
  return c.params
    .filter(p => p.flag?.startsWith(token) && !c.used.has(p.flag))
    .map(p => ({ text: `${p.flag} `, description: p.raw }))
}

function rowsFor(text: string, cursor: number): Usage[] {
  const c = contextAt(text, cursor)
  const rows = commandsIn(text)
  if (!c || c.params.length === 0) return rows
  const here: Usage = { name: c.name, line: usageLine(c.name), parts: partsOf(c) }
  return [here, ...rows.filter(u => u.name !== c.name)].slice(0, MAX_ROWS)
}

async function refresh($: EngineInterface, text: string, cursor: number): Promise<void> {
  const rows = rowsFor(text, cursor)
  const prev = await read($, active)
  if (JSON.stringify(rows) !== JSON.stringify(prev)) await update($, active, () => rows)
}

// Commands and skills added mid-session show up after the next submitted prompt.
async function reindex($: EngineInterface): Promise<void> {
  descriptions = new Map((await $.command.list()).map(c => [c.name, c.description]))
  hints = await loadHints($)
  agents = await loadAgents($)
  parsed = new Map()
  last = undefined
}

const onStart: Hook<'session.start'> = async ($, e, next) => {
  await reindex($)
  return next(e)
}

const onSlashComplete: MatchedHook<'prompt.autocomplete', { token: RegExp }> = async ($, e, next) => {
  const base = await next(e)
  if (e.text.slice(0, e.start).trim() === '') return base // a leading slash is the engine's own typeahead
  const prefix = e.token.slice(1).toLowerCase()
  const rows = [...descriptions]
    .filter(([name]) => name.toLowerCase().startsWith(prefix))
    .slice(0, MAX_SUGGESTIONS)
    .map(([name, description]) => ({ text: `/${name}`, description }))
  return { suggestions: [...base.suggestions, ...rows] }
}

const onComplete: Hook<'prompt.autocomplete'> = async ($, e, next) => {
  const base = await next(e)
  const c = e.token.startsWith('/') ? undefined : contextAt(e.text, e.cursor)
  return c ? { suggestions: [...base.suggestions, ...completions(c, e.token)] } : base
}

const onEdit: Hook<'prompt.edit'> = async ($, e, next) => {
  const box = await next(e)
  try {
    await refresh($, box.text, box.cursor)
    const mine = decorationsOf(box.text, box.cursor)
    return mine.length ? { ...box, decorations: [...(box.decorations ?? []), ...mine] } : box
  } catch {
    return box // a failed hint must never cost the person their keystroke
  }
}

const onChange: Hook<'prompt.change'> = async ($, e, next) => {
  await refresh($, e.text, e.cursor).catch(() => undefined) // a failed hint must never cost the person their keystroke
  return next(e)
}

const onSubmit: Hook<'prompt.submit'> = async ($, e, next) => {
  await update($, active, () => []).catch(() => undefined) // a failed clear must not block the submit
  reindex($).catch(() => undefined) // a failed refresh must not escape the hook; the next submit retries
  return next(e)
}

const onRender: MatchedHook<'ui.render', { component: 'AbovePrompt' }> = async ($, e, next) => {
  const rows = await read($, active)
  if (e.props.hasSurvey || rows.length === 0) return next(e)
  const { Box, Text } = $.ui.resolve(e)
  return (
    <Box flexDirection="column">
      {rows.map(u =>
        u.parts ? (
          <Box key={u.name} flexDirection="row" gap={1}>
            {u.parts.map(p => (
              <Text
                key={p.text}
                bold={p.state === 'now'}
                dimColor={p.state !== 'now'}
                strikethrough={p.state === 'done'}
              >
                {p.text}
              </Text>
            ))}
          </Box>
        ) : (
          <Text key={u.name} dimColor>
            {u.line}
          </Text>
        ),
      )}
    </Box>
  )
}

export const register: Register = on => {
  on('session.start', onStart)
  on('prompt.autocomplete', { token: /^\// }, onSlashComplete)
  on('prompt.autocomplete', onComplete)
  on('prompt.edit', onEdit)
  on('prompt.change', onChange)
  on('prompt.submit', onSubmit)
  on('ui.render', { component: 'AbovePrompt' }, onRender)
}
