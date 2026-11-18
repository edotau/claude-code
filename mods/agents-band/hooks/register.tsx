import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderChildren } from 'claude-code'

import type { Row, Snapshot } from '../types'
import { age, clip, pack, shorten, strings } from './text'

const snapshot = atom({ plugin: 'agents-band', key: 'snapshot' } as const, null)
const isOpen = atom({ plugin: 'agents-band', key: 'isOpen' } as const, false)
const expanded = atom({ plugin: 'agents-band', key: 'expanded' } as const, [] as string[])
const polledAt = atom({ plugin: 'agents-band', key: 'polledAt' } as const, 0)
const hasFiles = atom({ plugin: 'agents-band', key: 'hasFiles' } as const, true)

const TICK_MS = 5000
const IDLE_MS = 30000
const SLACK_MS = 500
const MAX_ROWS = 12
const HELD_SHOWN = 5
const FREE_SHOWN = 20

type Dispatch = { id?: string; agent?: string; route?: string; duration?: string; lastSignal?: string; status?: string; task?: string; quiet?: boolean; files?: unknown; note?: unknown }
type Leg = { id?: string; type?: string; description?: string; status?: string; dispatches?: Dispatch[]; activity?: { tool?: string }; files?: unknown }
type Phase = { name?: string; agents?: Leg[] }
type Flow = { name?: string; id?: string; status?: string; phases?: Phase[] }
type Session = { subagents?: Leg[]; workflows?: Flow[]; notes?: unknown }
type Tree = { sessions?: Session[]; dispatches?: Dispatch[]; free?: unknown }

type Ui = ReturnType<EngineInterface['ui']['resolve']>

const isIdle = (snap: Snapshot): boolean => snap.running === 0 && snap.lost === 0

const alert = (snap: Snapshot): string => (snap.error !== '' ? '?' : snap.quiet + snap.lost > 0 ? '!' : '')

const footerLabel = (snap: Snapshot, open: boolean): string => `agents ${snap.running}${alert(snap)} ${open ? '▾' : '▸'}`

const bandLabel = (snap: Snapshot, open: boolean): string => (open ? 'hide agents' : `agents ${snap.running}${alert(snap)} ▸`)

const hasFooterOn = (surface: string): boolean => surface === 'terminal' || surface === 'desktop'

// isHidden: collapsed, and either the footer button stands in for the band or there is nothing to show.
const isHidden = (snap: Snapshot, open: boolean, hasFooter: boolean): boolean => !open && (hasFooter || isIdle(snap))

const headLine = (snap: Snapshot, nowMs: number): string =>
  `agents: ${snap.running} running` + (snap.routes ? ` (${snap.routes})` : '') +
  (snap.quiet ? ` · ${snap.quiet} quiet` : '') + (snap.lost ? ` · ${snap.lost} lost` : '') +
  (snap.error !== '' ? ` · stale ${age(nowMs - snap.at)}` : '')

const sinceIso = (iso: string | undefined, nowMs: number): string => {
  const then = iso ? Date.parse(iso) : NaN

  return Number.isNaN(then) ? '' : age(nowMs - then)
}

const routeOf = (d: Dispatch | undefined): string => (d && d.route ? d.route.replace(/^databricks:/, '') : '')

// what names the job: the leg's own description without its model prefix, else a readable form of the task text.
const what = (leg: Leg | undefined, d: Dispatch | undefined): string => {
  const desc = (leg && leg.description ? leg.description : '').replace(/^[a-z0-9.-]+:\s+/i, '')
  if (desc) return desc
  const task = (d && d.task ? d.task : '').replace(/\s+/g, ' ')
  if (task.startsWith('Background update of a finished session')) return 'memory update'

  return task.slice(0, 60)
}

// Raw rows carry full paths in `held`; finish() turns them into details once every path is known.
type Raw = Row & { held: string[]; note: string }

type Tally = { rows: Raw[]; running: number; quiet: number; byRoute: Map<string, number>; seen: Set<string> }

const addLeg = (t: Tally, leg: Leg | undefined, d: Dispatch | undefined, nowMs: number, indent: string): void => {
  const route = routeOf(d) || 'here'
  const isQuiet = d !== undefined && d.quiet === true
  const life = d ? (isQuiet ? `quiet ${sinceIso(d.lastSignal, nowMs)}` : `last=${sinceIso(d.lastSignal, nowMs)}`) : (leg && leg.activity && leg.activity.tool) || ''
  const agent = (d && d.agent) || (leg && leg.type) || '?'
  const held = [...new Set([...strings(leg && leg.files), ...strings(d && d.files)])]
  const note = typeof (d && d.note) === 'string' ? (d!.note as string).replace(/\s+/g, ' ') : ''
  const flags = (held.length ? `${held.length}f ` : '') + (note ? '✎ ' : '')
  const text = `${agent.padEnd(8)}${route.padEnd(6)}${((d && d.duration) || '').padEnd(7)}${life.padEnd(11)}${flags}${what(leg, d)}`
  const id = (d && d.id) || (leg && leg.id) || String(t.rows.length)
  t.rows.push({ key: `leg:${id}`, text: `${indent}${text}`, isQuiet, isHead: false, details: [], held, note })
  t.running += 1
  if (isQuiet) t.quiet += 1
  t.byRoute.set(route, (t.byRoute.get(route) || 0) + 1)
  if (d && d.id) t.seen.add(d.id)
}

const liveDispatch = (leg: Leg): Dispatch | undefined => (leg.dispatches || []).find(d => d.status === 'running')

// finish gives each row its detail lines, then appends the notes and free-file groups.
const finish = (t: Tally, notes: string[], free: string[]): Row[] => {
  const short = shorten([...t.rows.flatMap(r => r.held), ...free])
  const name = (p: string): string => short.get(p) || p
  const rows: Row[] = t.rows.map(({ held, note, ...r }) => ({
    ...r,
    details: [
      ...(held.length ? [`files: ${held.slice(0, HELD_SHOWN).map(name).join(' ')}${held.length > HELD_SHOWN ? ` (+${held.length - HELD_SHOWN})` : ''}`] : []),
      ...(note ? [`note: ${note}`] : []),
    ],
  }))
  if (notes.length) rows.push({ key: 'notes', text: `  notes: ${notes.length} from subagents`, isQuiet: false, isHead: false, details: notes.map(n => `note: ${n.replace(/\s+/g, ' ')}`) })
  if (free.length) {
    const more = free.length > FREE_SHOWN ? [`(+${free.length - FREE_SHOWN} more)`] : []
    rows.push({ key: 'free', text: `  free: ${free.length} dirty, held by no live leaf`, isQuiet: false, isHead: false, details: pack([...free.slice(0, FREE_SHOWN).map(name), ...more]) })
  }

  return rows
}

// summarize turns the tree into the band's snapshot: leaves, then each workflow's legs, then unattached dispatches.
const summarize = (tree: Tree, nowMs: number): Snapshot => {
  const t: Tally = { rows: [], running: 0, quiet: 0, byRoute: new Map(), seen: new Set() }
  const notes: string[] = []
  for (const s of Array.isArray(tree.sessions) ? tree.sessions : []) {
    notes.push(...strings(s.notes))
    for (const leg of s.subagents || []) {
      for (const d of leg.dispatches || []) if (d.id) t.seen.add(d.id)
      if (leg.status === 'running') addLeg(t, leg, liveDispatch(leg), nowMs, '  ')
    }
    for (const w of s.workflows || []) {
      if (w.status !== 'running') continue
      const legs = (w.phases || []).flatMap(p => (p.agents || []).map(a => ({ a, phase: (p.name || '').replace(/^\W+/, '') })))
      const live = legs.filter(l => l.a.status === 'running')
      const done = legs.length - live.length
      t.rows.push({ key: `flow:${w.id || w.name || ''}`, text: `  workflow ${w.name || w.id || ''}: ${live[0] ? live[0].phase : 'between phases'} (${done}/${legs.length} legs done)`, isQuiet: false, isHead: true, details: [], held: [], note: '' })
      for (const l of live) addLeg(t, l.a, liveDispatch(l.a), nowMs, '    ')
    }
  }
  const all = Array.isArray(tree.dispatches) ? tree.dispatches : []
  for (const d of all) if (d.status === 'running' && d.id && !t.seen.has(d.id)) addLeg(t, undefined, d, nowMs, '  ')

  return {
    running: t.running,
    quiet: t.quiet,
    lost: all.filter(d => d.status === 'lost').length,
    routes: [...t.byRoute].map(([r, n]) => `${r} ${n}`).join(', '),
    rows: finish(t, notes, strings(tree.free)),
    more: 0,
    at: nowMs,
    error: '',
  }
}

type Local = { id: string; type: string; description: string; status: string }

const LIVE = ['pending', 'running', 'waiting']

// withLocal adds this session's live agents that the tree did not already list.
const withLocal = (snap: Snapshot, agents: readonly Local[]): Snapshot => {
  const names = (a: Local) => a.description.replace(/^[a-z0-9.-]+:\s+/i, '')
  const extra = agents.filter(a => LIVE.includes(a.status) && !snap.rows.some(r => r.text.includes(names(a))))
  const rows = extra.map(a => ({ key: `local:${a.id}`, text: `  ${a.type.padEnd(8)}${'here'.padEnd(6)}${''.padEnd(7)}${a.status.padEnd(11)}${names(a)}`, isQuiet: false, isHead: false, details: [] }))

  return { ...snap, running: snap.running + extra.length, rows: [...rows, ...snap.rows] }
}

// inFlight keeps polls from overlapping: a tick that finds one running does nothing.
let inFlight = false

const NO_FLAG = /not defined|unknown (flag|option)/i

// fetch runs one tree call; a binary without --files is noted once and this tick yields nothing.
async function fetch($: EngineInterface, nowMs: number): Promise<Snapshot | null> {
  const withFiles = await read($, hasFiles)
  const argv = ['claude-code', 'agents', 'tree', ...(withFiles ? ['--files'] : []), '--format', 'json']
  const out = await $.process.run(argv, { timeoutMs: 10000 })
  if (out.exitCode !== 0 && withFiles && NO_FLAG.test(out.stderr)) {
    await update($, hasFiles, () => false)
    await update($, polledAt, () => -IDLE_MS)

    return null
  }
  if (out.exitCode !== 0) throw new Error(`agents tree exited ${out.exitCode}`)
  const tree = JSON.parse(out.stdout) as Tree
  if (typeof tree !== 'object' || tree === null) throw new Error('agents tree: unexpected output')

  return withLocal(summarize(tree, nowMs), await $.agent.list())
}

// poll reads the tree into the snapshot; a failed call keeps the last good one and marks it stale.
async function poll($: EngineInterface): Promise<void> {
  const nowMs = await $.clock.now()
  await update($, polledAt, () => nowMs)
  try {
    const snap = await fetch($, nowMs)
    if (snap !== null) await update($, snapshot, () => snap)
  } catch (err) {
    const error = String(err).slice(0, 80)
    await update($, snapshot, prev => (prev === null ? null : { ...prev, error }))
  }
}

// tick polls when due: every 5 s while anything is live or the band is open, every 30 s otherwise.
async function tick($: EngineInterface, force: boolean): Promise<void> {
  if (inFlight) return
  inFlight = true
  try {
    const snap = await read($, snapshot)
    const live = (snap !== null && !isIdle(snap)) || (await read($, isOpen)) || (await $.agent.list()).some(a => LIVE.includes(a.status))
    const wait = live ? TICK_MS : IDLE_MS
    if (force || (await $.clock.now()) - (await read($, polledAt)) >= wait - SLACK_MS) await poll($)
  } catch {
    // a failed tick waits for the next one; a handler must never throw
  } finally {
    inFlight = false
  }
}

const toggle = ($: EngineInterface, key: string) =>
  update($, expanded, v => (v.includes(key) ? v.filter(k => k !== key) : [...v, key]))

// rowsOf draws the leaf rows within MAX_ROWS lines; a row with details is a button that shows them.
function rowsOf($: EngineInterface, { Button, Text }: Ui, snap: Snapshot, open: readonly string[]) {
  const out = []
  let used = 0
  let shown = 0
  for (const r of snap.rows) {
    const lines = open.includes(r.key) ? r.details : []
    if (used + 1 + lines.length > MAX_ROWS) break
    used += 1 + lines.length
    shown += 1
    const marker = r.details.length ? (lines.length ? '▾' : '▸') : ' '
    const text = clip(`${marker}${r.text.slice(1)}`)
    out.push(r.details.length
      ? <Button key={r.key} label={text} onPress={() => toggle($, r.key)} />
      : <Text key={r.key} dimColor={!r.isQuiet && !r.isHead} color={r.isQuiet ? 'yellow' : undefined}>{text}</Text>)
    lines.forEach((l, i) => out.push(<Text key={`${r.key}:${i}`} dimColor>{clip(`      ${l}`)}</Text>))
  }
  const more = snap.rows.length - shown

  return more > 0 ? [...out, <Text key="more" dimColor>{'  '}… {more} more (claude-code agents tree --files)</Text>] : out
}

// list draws the expanded band; tail is the collapse button on surfaces without a footer.
async function list($: EngineInterface, ui: Ui, snap: Snapshot, tail: RenderChildren) {
  const { Box, Text } = ui
  const head = <Text dimColor={snap.error !== ''}>{headLine(snap, await $.clock.now())}</Text>

  return <Box flexDirection="column">{head}{rowsOf($, ui, snap, await read($, expanded))}{tail}</Box>
}

const flip = async ($: EngineInterface): Promise<boolean> => {
  const open = await update($, isOpen, v => !v)
  if (open) void tick($, true)

  return open
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    inFlight = false
    await $.command.register({ name: 'agents-band', description: 'Expand or collapse the running-agents list' })
    $.clock.every(TICK_MS, () => tick($, false))
    void tick($, true)

    return next(e)
  })

  on('command.run', { command: 'agents-band' }, async $ => {
    const open = await flip($)

    return { text: open ? 'agents list expanded' : 'agents list collapsed' }
  })

  on('ui.render', { component: 'SessionMode' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    const rest = await next(e)
    if (snap === null || isIdle(snap)) return rest
    const { Box, Button, Text } = $.ui.resolve(e)
    const label = footerLabel(snap, await read($, isOpen))

    return <Box><Button key="agents" label={label} onPress={() => flip($)} /><Text> </Text>{rest}</Box>
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    if (e.props.hasSurvey || snap === null) return next(e)
    const open = await read($, isOpen)
    const hasFooter = hasFooterOn(e.surface)
    if (isHidden(snap, open, hasFooter)) return next(e)
    const ui = $.ui.resolve(e)
    const btn = <ui.Button key="agents" label={bandLabel(snap, open)} onPress={() => flip($)} />

    return open ? list($, ui, snap, hasFooter ? null : btn) : <ui.Box>{btn}</ui.Box>
  })
}
