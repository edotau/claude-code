import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderChildren } from 'claude-code'

import type { Row, Snapshot } from '../types'

const snapshot = atom({ plugin: 'agents-band', key: 'snapshot' } as const, null)
const isOpen = atom({ plugin: 'agents-band', key: 'isOpen' } as const, false)

const PERIOD_MS = 5000
const MAX_ROWS = 12

type Dispatch = { id?: string; agent?: string; route?: string; duration?: string; lastSignal?: string; status?: string; task?: string; quiet?: boolean }
type Leg = { type?: string; description?: string; status?: string; dispatches?: Dispatch[]; activity?: { tool?: string } }
type Phase = { name?: string; agents?: Leg[] }
type Flow = { name?: string; id?: string; status?: string; phases?: Phase[] }
type Session = { subagents?: Leg[]; workflows?: Flow[] }
type Tree = { sessions?: Session[]; dispatches?: Dispatch[] }

type Ui = ReturnType<EngineInterface['ui']['resolve']>

const isIdle = (snap: Snapshot): boolean => snap.running === 0 && snap.lost === 0 && snap.error === ''

const alert = (snap: Snapshot): string => (snap.quiet + snap.lost > 0 ? '!' : '')

// footerLabel is the status-line button's text: the running count, with "!" for quiet or lost legs or an error.
const footerLabel = (snap: Snapshot, open: boolean): string =>
  `agents ${snap.error !== '' ? '!' : `${snap.running}${alert(snap)}`} ${open ? '▾' : '▸'}`

const bandLabel = (snap: Snapshot, open: boolean): string => (open ? 'hide agents' : `agents ${snap.running}${alert(snap)} ▸`)

const hasFooterOn = (surface: string): boolean => surface === 'terminal' || surface === 'desktop'

// isHidden: collapsed, and either the footer button stands in for the band or there is nothing to show.
const isHidden = (snap: Snapshot, open: boolean, hasFooter: boolean): boolean => !open && (hasFooter || isIdle(snap))

const headLine = (snap: Snapshot): string =>
  `agents: ${snap.running} running` + (snap.routes ? ` (${snap.routes})` : '') +
  (snap.quiet ? ` · ${snap.quiet} quiet` : '') + (snap.lost ? ` · ${snap.lost} lost` : '')

// age renders the seconds since an RFC 3339 time, or "" when it is absent or unreadable.
const age = (iso: string | undefined, nowMs: number): string => {
  const then = iso ? Date.parse(iso) : NaN
  if (Number.isNaN(then)) return ''
  const s = Math.max(0, Math.round((nowMs - then) / 1000))

  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m${s % 60}s`
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

type Tally = { rows: Row[]; running: number; quiet: number; byRoute: Map<string, number>; seen: Set<string> }

// addLeg appends one running leg as a row and counts it.
const addLeg = (t: Tally, leg: Leg | undefined, d: Dispatch | undefined, nowMs: number, indent: string): void => {
  const route = routeOf(d) || 'here'
  const isQuiet = d !== undefined && d.quiet === true
  const life = d ? (isQuiet ? `quiet ${age(d.lastSignal, nowMs)}` : `last=${age(d.lastSignal, nowMs)}`) : (leg && leg.activity && leg.activity.tool) || ''
  const agent = (d && d.agent) || (leg && leg.type) || '?'
  t.rows.push({ text: `${indent}${agent.padEnd(9)}${route.padEnd(6)}${((d && d.duration) || '').padEnd(8)}${life.padEnd(12)}${what(leg, d)}`, isQuiet, isHead: false })
  t.running += 1
  if (isQuiet) t.quiet += 1
  t.byRoute.set(route, (t.byRoute.get(route) || 0) + 1)
  if (d && d.id) t.seen.add(d.id)
}

const liveDispatch = (leg: Leg): Dispatch | undefined => (leg.dispatches || []).find(d => d.status === 'running')

// summarize turns the tree into the band's snapshot: leaves, then each workflow's legs, then unattached dispatches.
const summarize = (tree: Tree, nowMs: number): Snapshot => {
  const t: Tally = { rows: [], running: 0, quiet: 0, byRoute: new Map(), seen: new Set() }
  for (const s of tree.sessions || []) {
    for (const leg of s.subagents || []) {
      for (const d of leg.dispatches || []) if (d.id) t.seen.add(d.id)
      if (leg.status === 'running') addLeg(t, leg, liveDispatch(leg), nowMs, '  ')
    }
    for (const w of s.workflows || []) {
      if (w.status !== 'running') continue
      const legs = (w.phases || []).flatMap(p => (p.agents || []).map(a => ({ a, phase: (p.name || '').replace(/^\W+/, '') })))
      const live = legs.filter(l => l.a.status === 'running')
      const done = legs.length - live.length
      t.rows.push({ text: `  workflow ${w.name || w.id || ''}: ${live[0] ? live[0].phase : 'between phases'} (${done}/${legs.length} legs done)`, isQuiet: false, isHead: true })
      for (const l of live) addLeg(t, l.a, liveDispatch(l.a), nowMs, '    ')
    }
  }
  const all = tree.dispatches || []
  for (const d of all) if (d.status === 'running' && d.id && !t.seen.has(d.id)) addLeg(t, undefined, d, nowMs, '  ')
  const rows = t.rows.slice(0, MAX_ROWS)

  return {
    running: t.running,
    quiet: t.quiet,
    lost: all.filter(d => d.status === 'lost').length,
    routes: [...t.byRoute].map(([r, n]) => `${r} ${n}`).join(', '),
    rows,
    more: t.rows.length - rows.length,
    error: '',
  }
}

type Local = { type: string; description: string; status: string }

const LIVE = ['pending', 'running', 'waiting']

// withLocal adds this session's live agents that the tree did not already list.
const withLocal = (snap: Snapshot, agents: readonly Local[]): Snapshot => {
  const names = (a: Local) => a.description.replace(/^[a-z0-9.-]+:\s+/i, '')
  const extra = agents.filter(a => LIVE.includes(a.status) && !snap.rows.some(r => r.text.includes(names(a))))
  const shown = extra.slice(0, Math.max(0, MAX_ROWS - snap.rows.length))
  const rows = shown.map(a => ({ text: `  ${a.type.padEnd(9)}${'here'.padEnd(6)}${''.padEnd(8)}${a.status.padEnd(12)}${names(a)}`, isQuiet: false, isHead: false }))

  return { ...snap, running: snap.running + extra.length, rows: [...snap.rows, ...rows], more: snap.more + extra.length - shown.length }
}

// poll reads the agent tree and this session's agents into the snapshot atom.
async function poll($: EngineInterface): Promise<void> {
  const out = await $.process.run(['claude-code', 'agents', 'tree', '--format', 'json'])
  const nowMs = await $.clock.now()
  let snap: Snapshot
  try {
    if (out.exitCode !== 0) throw new Error(`agents tree exited ${out.exitCode}`)
    snap = withLocal(summarize(JSON.parse(out.stdout) as Tree, nowMs), await $.agent.list())
  } catch (err) {
    snap = { running: 0, quiet: 0, lost: 0, routes: '', rows: [], more: 0, error: String(err).slice(0, 80) }
  }
  await update($, snapshot, () => snap)
}

// rowsOf draws one line per running leg, plus the overflow note.
function rowsOf({ Text }: Ui, snap: Snapshot) {
  const rows = snap.rows.map((r, i) => (
    <Text key={String(i)} dimColor={!r.isQuiet && !r.isHead} color={r.isQuiet ? 'yellow' : undefined}>{r.text}</Text>
  ))

  return snap.more > 0 ? [...rows, <Text key="more" dimColor>{'  '}… {snap.more} more (claude-code agents tree)</Text>] : rows
}

// list draws the expanded band; tail is the collapse button on surfaces without a footer.
function list(ui: Ui, snap: Snapshot, tail: RenderChildren) {
  const { Box, Text } = ui
  if (snap.error !== '') return <Box><Text dimColor>agents: {snap.error}</Text></Box>

  return <Box flexDirection="column"><Text>{headLine(snap)}</Text>{rowsOf(ui, snap)}{tail}</Box>
}

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({ name: 'agents-band', description: 'Expand or collapse the running-agents list' })
    $.clock.every(PERIOD_MS, () => poll($))
    void poll($)

    return next(e)
  })

  on('command.run', { command: 'agents-band' }, async $ => {
    const open = await update($, isOpen, v => !v)

    return { text: open ? 'agents list expanded' : 'agents list collapsed' }
  })

  on('ui.render', { component: 'SessionMode' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    const rest = await next(e)
    if (snap === null || isIdle(snap)) return rest
    const { Box, Button, Text } = $.ui.resolve(e)
    const label = footerLabel(snap, await read($, isOpen))

    return <Box><Button key="agents" label={label} onPress={() => update($, isOpen, v => !v)} /><Text> </Text>{rest}</Box>
  })

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    const snap = await read($, snapshot)
    if (e.props.hasSurvey || snap === null) return next(e)
    const open = await read($, isOpen)
    const hasFooter = hasFooterOn(e.surface)
    if (isHidden(snap, open, hasFooter)) return next(e)
    const ui = $.ui.resolve(e)
    const toggle = <ui.Button key="agents" label={bandLabel(snap, open)} onPress={() => update($, isOpen, v => !v)} />

    return open ? list(ui, snap, hasFooter ? null : toggle) : <ui.Box>{toggle}</ui.Box>
  })
}
