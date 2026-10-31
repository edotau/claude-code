'use strict';
// Webview HTML for the Usage popover — no vscode dependency, so node:test can exercise it directly.
const { formatCount } = require('./format');
const { formatResetCountdown, formatResetDuration, sessionsLabel } = require('./usage-model');

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

// Inline glyphs stand in for provider logos; the codicon font is not available inside a webview.
const GLYPHS = { anthropic: '<span class="glyph glyph-claude">\u273B</span>', google: '<span class="glyph glyph-agy">\u25B3</span>', github: '<span class="glyph">\u2B24</span>', openai: '<span class="glyph">\u273A</span>' };

function fillColor(state) {
  if (state === 'warn') return 'var(--vscode-editorWarning-foreground)';
  if (state === 'exceeded') return 'var(--vscode-editorError-foreground)';
  return 'var(--vscode-foreground)';
}

/** One window row, detailed (full-width bar) or compact (inline chip) layout. */
function renderWindow(w, show, detailed) {
  const pct = Math.round(show === 'left' ? w.leftPct : w.usedPct);
  const color = fillColor(w.state);
  if (detailed) {
    return `<div class="window-row" data-pct="${pct}">
      <span class="window-label">${esc(w.label)}</span>
      <span class="window-bar"><span class="window-fill" style="width:${pct}%;background:${color}"></span></span>
      <span class="window-pct">${pct}%</span>
    </div>`;
  }
  return `<span class="chip">
    <span class="chip-label">${esc(w.label)}</span>
    <span class="chip-bar"><span class="chip-fill" style="width:${pct}%;background:${color}"></span></span>
    <span class="chip-pct">${pct}%</span>
  </span>`;
}

/** Window label as it appears in a pivoted grid column: the part after the group prefix, or the whole label. */
function windowColumnLabel(w) {
  if (!w.group) return w.label;
  const idx = w.label.lastIndexOf(' · ');
  return idx === -1 ? w.label : w.label.slice(idx + 3);
}

/** Pure pivot: provider windows carrying `group` → one grid row per group, one column per distinct window label
 * (in order of first appearance). Exported so tests can cover it without rendering HTML. */
function pivotGroupedWindows(windows) {
  const columns = [];
  const groupOrder = [];
  const byGroup = new Map();
  for (const w of windows) {
    const g = w.group || '';
    const col = windowColumnLabel(w);
    if (!columns.includes(col)) columns.push(col);
    if (!byGroup.has(g)) {
      byGroup.set(g, {});
      groupOrder.push(g);
    }
    byGroup.get(g)[col] = w;
  }
  const rows = groupOrder.map((g) => {
    const cells = {};
    for (const col of columns) cells[col] = byGroup.get(g)[col] || null;
    return { group: g, cells };
  });
  return { columns, rows };
}

/** Two or more distinct groups among a provider's windows calls for the pivoted grid; one or zero stays a row list. */
function hasMultipleGroups(windows) {
  return new Set(windows.map((w) => w.group).filter(Boolean)).size >= 2;
}

/** One grid cell pair (bar + pct), or a muted dash spanning the pair when the window is absent for this group. */
function renderGridCell(w, show) {
  if (!w) return '<span class="grid-bar"></span><span class="grid-pct muted">—</span>';
  const pct = Math.round(show === 'left' ? w.leftPct : w.usedPct);
  const color = fillColor(w.state);
  return `<span class="grid-bar"><span class="window-fill" style="width:${pct}%;background:${color}"></span></span><span class="grid-pct">${pct}%</span>`;
}

/** Aligned CSS grid for a grouped provider (Antigravity): header row of window labels, one row per group. */
function renderGroupedGrid(windows, show) {
  const { columns, rows } = pivotGroupedWindows(windows);
  const templateCols = `minmax(0,1fr) ${columns.map(() => '36px 36px').join(' ')}`;
  const header =
    '<span class="grid-cell"></span>' +
    columns.map((c) => `<span class="grid-col-label muted">${esc(c)}</span>`).join('');
  const body = rows
    .map(
      (r) =>
        `<span class="grid-group-name">${esc(r.group)}</span>` +
        columns.map((c) => renderGridCell(r.cells[c], show)).join('')
    )
    .join('');
  return `<div class="grouped-grid" style="grid-template-columns:${templateCols}">${header}${body}</div>`;
}

/** Aligned CSS grid for an ungrouped provider (Claude, GitHub Copilot): same fixed-width bar/pct cells, one row each. */
function renderUngroupedGrid(windows, show) {
  const rows = windows
    .map((w) => {
      const pct = Math.round(show === 'left' ? w.leftPct : w.usedPct);
      const color = fillColor(w.state);
      return (
        `<span class="grid-row-label muted" title="${esc(w.label)}">${esc(w.label)}</span>` +
        `<span class="grid-bar"><span class="window-fill" style="width:${pct}%;background:${color}"></span></span>` +
        `<span class="grid-pct">${pct}%</span>`
      );
    })
    .join('');
  return `<div class="ungrouped-grid">${rows}</div>`;
}

/** "5219 / 7000 credits" (used) or "1781 of 7000 credits left" \u2014 the CLI's DETAIL column for a credits-metered
 * window (`used_credits`/`limit_credits`); `null` when the window carries no credits figures. */
function creditsLabel(w, show) {
  if (w.limitCredits == null) return null;
  const used = w.usedCredits || 0;
  if (show === 'left') return `${Math.round(w.limitCredits - used)} of ${Math.round(w.limitCredits)} credits left`;
  return `${Math.round(used)} / ${Math.round(w.limitCredits)} credits`;
}

/** One muted line per window carrying credits figures, below the bars \u2014 the CLI's per-row DETAIL text. */
function renderCreditsNotes(windows, show) {
  const lines = windows.map((w) => creditsLabel(w, show)).filter(Boolean);
  return lines.map((l) => `<div class="credits-note muted">${esc(l)}</div>`).join('');
}

/** "$100.60 used this month": a pay-per-use window's total spend, no percent, bar or limit. */
function spendLabel(w) {
  const amount = w.unit === 'USD' ? `$${(w.usedCredits || 0).toFixed(2)}` : `${w.usedCredits || 0} ${w.unit || ''}`.trim();
  return `${amount} used this ${w.window}`;
}

/** Neutral rows for API-rate billing, plus (Remaining only) the auth token's time left; no bar, no red. */
function renderSpendRows(p, spend, show) {
  const rows = spend.map((w) => `<div class="spend-row">${esc(spendLabel(w))}</div>`);
  if (show === 'left' && p.tokenExpiresAtMs != null) {
    const ms = p.tokenExpiresAtMs - Date.now();
    const left = ms > 0 ? `${formatResetDuration(ms)} left` : 'expired';
    rows.push(`<div class="spend-row token-row"><span class="muted">auth token</span>  ${esc(left)}</div>`);
  }
  return rows.join('');
}

function renderProvider(p, show, detailed) {
  const resets = p.resetsAtMs != null ? formatResetCountdown(p.resetsAtMs - Date.now()) : '';
  const spend = p.windows.filter((w) => w.spend);
  if (spend.length) p = { ...p, windows: p.windows.filter((w) => !w.spend) };
  const spendBody = spend.length ? renderSpendRows(p, spend, show) : '';
  const body = p.windows.length
    ? detailed
      ? hasMultipleGroups(p.windows)
        ? renderGroupedGrid(p.windows, show)
        : renderUngroupedGrid(p.windows, show)
      : `<div class="windows windows-compact">${p.windows.map((w) => renderWindow(w, show, false)).join('')}</div>`
    : spend.length
    ? ''
    : `<div class="provider-note">${esc(p.note || 'no data')}</div>`;
  const credits = p.windows.length ? renderCreditsNotes(p.windows, show) : '';
  const muted = p.windows.length || spend.length ? '' : ' provider-card-muted';
  const name = p.account ? `${esc(p.name)} <span class="provider-account muted">\u00B7 ${esc(p.account)}</span>` : esc(p.name);
  return `<div class="provider-card${muted}">
    <div class="provider-head card-head">
      ${GLYPHS[p.id] || '<span class="glyph">\u25CF</span>'}
      <span class="provider-name">${name}</span>
      <span class="provider-resets">${esc(resets)}</span>
    </div>
    <div class="card-body">${body}${credits}${spendBody}</div>
  </div>`;
}

/** One "<provider>/<model>  <tokens> · <turns> turns[ · $cost]" line — the CLI's indented per-model AGENTS row. */
function renderAgentModelRow(m) {
  const parts = [`${formatCount(m.tokens)}`];
  if (m.turns) parts.push(`${m.turns} turn${m.turns === 1 ? '' : 's'}`);
  if (m.costUsd != null) parts.push(`$${m.costUsd.toFixed(2)}`);
  const label = m.provider && m.model ? `${m.provider}/${m.model}` : m.model || m.provider || '';
  return `<div class="agent-model-row">
    <span class="agent-model-name muted" title="${esc(label)}">${esc(label)}</span>
    <span class="agent-model-stats muted">${esc(parts.join(' · '))}</span>
  </div>`;
}

/** One agent card: bold name, a "tokens · sessions · turns · $cost" total line, every `models[]` row beneath it
 * (provider/model · tokens · turns · cost, mirroring the CLI's indented AGENTS rows), and a share bar of this
 * agent's tokens against `totalTokens`. A note-only agent (no sessions in window) renders muted, no bar. */
function renderAgentCard(a, totalTokens) {
  if (!a.tokens && !a.sessions) {
    return `<div class="agent-card agent-card-muted fixed-card">
      <div class="agent-name card-head muted">${esc(a.name)}</div>
      <div class="card-body"><div class="agent-note muted">${esc(a.note || 'no sessions in window')}</div></div>
    </div>`;
  }
  const parts = [];
  if (a.tokens) parts.push(`${formatCount(a.tokens)} tokens`);
  if (a.sessions) parts.push(sessionsLabel(a.sessions));
  if (a.turns) parts.push(`${a.turns} turns`);
  if (a.costUsd != null) parts.push(`$${a.costUsd.toFixed(2)}`);
  const statsText = parts.join(' · ');
  const share = totalTokens > 0 ? Math.min(100, (a.tokens / totalTokens) * 100) : 0;
  // Models with neither tokens nor turns are configured-but-unused noise; the card lists only what ran.
  const used = (a.models || []).filter((m) => m.tokens || m.turns);
  const models = used.length ? `<div class="agent-models">${used.map(renderAgentModelRow).join('')}</div>` : '';
  return `<div class="agent-card fixed-card">
    <div class="agent-name card-head">${esc(a.name)}</div>
    <div class="card-body">
    <div class="agent-stats muted" title="${esc(statsText)}">${esc(statsText)}</div>
    ${models}
    </div>
    <div class="agent-share"><span class="agent-share-fill" style="width:${share}%"></span></div>
  </div>`;
}

/** Compact: one line per agent, "<Name>  <tokens> · <N> sessions". */
function renderAgentCompact(a) {
  const parts = [];
  if (a.tokens) parts.push(formatCount(a.tokens));
  if (a.sessions) parts.push(sessionsLabel(a.sessions));
  return `<div class="agent-compact">
    <span class="agent-name">${esc(a.name)}</span>
    <span class="muted">${esc(parts.join(' · '))}</span>
  </div>`;
}

/** "last N days" fragment for the AGENTS section header; `stats.windowDays` (the query window) wins over the
 * webview's own `days` option when both are known. */
function agentsWindowLabel(model, days) {
  const n = model.stats && model.stats.windowDays != null ? model.stats.windowDays : days;
  return `last ${n} day${n === 1 ? '' : 's'}`;
}

/** Agent cards/rows only — no head; the AGENTS section header carries the title + window label. */
function renderAgentsBody(model, detailed) {
  const agents = model.agents || [];
  if (!agents.length) return '';
  const totalTokens = agents.reduce((sum, a) => sum + (a.tokens || 0), 0);
  const body = detailed
    ? `<div class="agents-grid">${agents.map((a) => renderAgentCard(a, totalTokens)).join('')}</div>`
    : agents.map(renderAgentCompact).join('');
  return `<div class="agents-section">${body}</div>`;
}

/** The CLI's "API KEYS (per-token, accumulated via harness)" ledger as one card per provider/profile — printed in
 * every billing mode, never conditioned on `model.mode`, so a per-token key beside a subscription still shows. */
function renderApiCards(model) {
  const groups = new Map();
  for (const r of model.api || []) {
    const key = r.profile ? `${r.provider}/${r.profile}` : r.provider;
    if (!groups.has(key)) groups.set(key, { profile: r.profile, provider: r.provider, rows: [] });
    groups.get(key).rows.push(r);
  }
  if (!groups.size) return '<div class="cards-grid"><div class="muted">No token usage yet</div></div>';
  const cards = [...groups.values()].map((g) => {
    const rows = [...g.rows].sort((a, b) => b.last30d - a.last30d);
    const total = rows.reduce((sum, r) => sum + r.last30d, 0);
    const title = g.profile ? `${esc(g.profile)} <span class="muted">· ${esc(g.provider)}</span>` : esc(g.provider);
    const body = rows
      .map(
        (r) => `<tr>
        <td class="api-model" title="${esc(r.model)}">${esc(r.model)}</td>
        <td>${esc(formatCount(r.today))}</td>
        <td>${esc(formatCount(r.last30d))}</td>
        <td>${esc(formatCount(r.allTime))}</td>
      </tr>`
      )
      .join('');
    return `<div class="api-card fixed-card">
      <div class="card-head api-head"><span class="provider-name">${title}</span><span class="muted">${esc(formatCount(total))} · 30d</span></div>
      <div class="card-body"><table class="api-table">
        <thead><tr><th>Model</th><th>Today</th><th>30d</th><th>All</th></tr></thead>
        <tbody>${body}</tbody>
      </table></div>
    </div>`;
  });
  return `<div class="cards-grid">${cards.join('')}</div>`;
}

/** MODELS: one card per provider whose credential resolves, its tier → model map; the session profile is marked. */
function renderProviderModels(model) {
  const providers = model.providerModels || [];
  if (!providers.length) return '';
  const cards = providers.map((p) => {
    const rows = p.models
      .map((m) => `<div class="agent-model-row"><span class="muted">${esc(m.tier)}</span><span class="agent-model-name" title="${esc(m.id)}">${esc(m.id)}</span></div>`)
      .join('');
    const badge = p.session ? ' <span class="badge">session</span>' : '';
    return `<div class="model-card">
      <div class="card-head api-head"><span class="provider-name">${esc(p.name)}${badge}</span><span class="muted">${esc(p.group || '')} · ${esc(p.source)}</span></div>
      <div class="card-body"><div class="agent-models">${rows || '<span class="muted">no models</span>'}</div></div>
    </div>`;
  });
  return `<div class="cards-grid">${cards.join('')}</div>`;
}

/** "Oct 22" from a "YYYY-MM-DD" date string, parsed as UTC midnight so the local timezone can't shift it a day. */
function formatDayLabel(dateStr) {
  const d = new Date(`${dateStr}T00:00:00Z`);
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
}

/** 0 (no tokens) .. 4 (top quartile of `max`) — same 5-step scale the heatmap cells and the legend use. */
function heatLevel(tokens, max) {
  if (!tokens || max <= 0) return 0;
  const ratio = tokens / max;
  if (ratio > 0.75) return 4;
  if (ratio > 0.5) return 3;
  if (ratio > 0.25) return 2;
  return 1;
}

const HEAT_OPACITY = [1, 0.3, 0.5, 0.75, 1]; // level 0 paints the track tint at full opacity

/** One tile: a muted label over a bold value; `opts.html` lets the value carry inline markup (the cost partial suffix). */
function renderStatTile(label, value, opts) {
  const html = (opts && opts.html) || esc(value);
  return `<div class="stat-tile"><div class="stat-tile-label muted">${esc(label)}</div><div class="stat-tile-value">${html}</div></div>`;
}

/** Total tokens / Est. cost / Sessions / Active days, in a `repeat(auto-fit, minmax(140px, 1fr))` tile row. */
function renderStatsTiles(stats) {
  const costValue =
    stats.costUsd == null
      ? 'n/a'
      : `$${stats.costUsd.toFixed(2)}${stats.costPartial ? ' <span class="muted">(partial)</span>' : ''}`;
  const tiles = [
    renderStatTile('Total tokens', formatCount(stats.totalTokens)),
    renderStatTile('Est. cost', null, { html: costValue }),
    renderStatTile('Sessions', String(stats.sessions)),
    renderStatTile('Active days', String(stats.activeDays)),
  ];
  return `<div class="stats-tiles">${tiles.join('')}</div>`;
}

/** GitHub-style heatmap: one 10px square per day in `stats.daily`, 7 rows (Sun..Sat) × N week columns. */
function renderHeatmap(stats) {
  const days = stats.daily || [];
  if (!days.length) return '<div class="muted">No activity yet</div>';
  const firstDow = new Date(`${days[0].date}T00:00:00Z`).getUTCDay();
  const max = Math.max(1, ...days.map((d) => d.tokens));
  const cells = [];
  let col = 0;
  let row = firstDow;
  for (const d of days) {
    const level = heatLevel(d.tokens, max);
    cells.push(
      `<span class="heat-cell" style="grid-column:${col + 1};grid-row:${row + 1};opacity:${HEAT_OPACITY[level]}${level === 0 ? ';background:var(--track)' : ''}" title="${esc(
        formatDayLabel(d.date)
      )}: ${esc(formatCount(d.tokens))} tokens"></span>`
    );
    row++;
    if (row > 6) {
      row = 0;
      col++;
    }
  }
  const columns = col + 1;
  return `<div class="heatmap-wrap"><div class="heatmap" style="grid-template-columns:repeat(${columns},var(--heat-cell,10px));grid-template-rows:repeat(7,var(--heat-cell,10px))">${cells.join('')}</div></div>`;
}

/** "Less [□▢▦▩■] More" legend, same 5-step opacity scale as the heatmap cells. */
function renderHeatLegend() {
  const squares = HEAT_OPACITY.map((o, i) => `<span class="heat-cell heat-legend-cell" style="opacity:${o}${i === 0 ? ';background:var(--track)' : ''}"></span>`).join('');
  return `<div class="heat-legend muted">Less ${squares} More</div>`;
}

const MIX_SEGMENTS = [
  { key: 'input', label: 'Input', color: 'var(--vscode-charts-blue)' },
  { key: 'output', label: 'Output', color: 'var(--vscode-charts-green)' },
  { key: 'cacheRead', label: 'Cache read', color: 'var(--vscode-charts-yellow)' },
  { key: 'cacheWrite', label: 'Cache write', color: 'var(--vscode-charts-orange)' },
];

/** One stacked bar of input/output/cache-read/cache-write token counts, with a swatch legend below it. */
function renderTokenMix(stats) {
  const mix = stats.mix || { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };
  const total = mix.input + mix.output + mix.cacheRead + mix.cacheWrite;
  const bar =
    total > 0
      ? MIX_SEGMENTS.map((s) => `<span class="mix-seg" style="width:${(mix[s.key] / total) * 100}%;background:${s.color}"></span>`).join('')
      : '';
  const legend = MIX_SEGMENTS.map(
    (s) => `<span class="mix-legend-item"><span class="mix-swatch" style="background:${s.color}"></span>${esc(s.label)} ${esc(formatCount(mix[s.key]))}</span>`
  ).join('');
  return `<div class="token-mix">
    <div class="mix-bar">${bar || '<span class="muted">No token data yet</span>'}</div>
    <div class="mix-legend">${legend}</div>
  </div>`;
}

/** Stats layer: tile row, then a two-column (one on narrow) row of the daily-intensity heatmap and token mix. */
function renderStatsSection(stats) {
  if (!stats) return '';
  const bestDay = stats.bestDay && stats.bestDay.tokens > 0 ? formatDayLabel(stats.bestDay.date) : null;
  return `<div class="stats-section">
    ${renderStatsTiles(stats)}
    <div class="stats-cols">
      <div class="stats-col">
        <div class="stats-col-head"><span class="stats-col-title">Daily intensity</span>${bestDay ? `<span class="muted">Best: ${esc(bestDay)}</span>` : ''}</div>
        ${renderHeatmap(stats)}
        ${renderHeatLegend()}
      </div>
      <div class="stats-col">
        <div class="stats-col-head"><span class="stats-col-title">Token mix</span></div>
        ${renderTokenMix(stats)}
      </div>
    </div>
  </div>`;
}

/** Grey placeholder block; the shimmer lives in the page CSS and stops under prefers-reduced-motion. */
function skel(extra) {
  return `<div class="skel" style="${extra || ''}"></div>`;
}

/** Placeholder provider cards, shaped like renderProvider's output, with no numbers. */
function renderSkeletonProviders(n) {
  const card = `<div class="provider-card"><div class="card-head">${skel('width:45%')}</div><div class="card-body">${skel()}${skel()}${skel('width:70%')}</div></div>`;
  return card.repeat(n);
}

/** Placeholder OVERVIEW: four stat tiles plus the heatmap and token-mix blocks. */
function renderSkeletonStats() {
  const tile = `<div class="stat-tile">${skel('width:60%')}${skel('width:40%;height:18px')}</div>`;
  const col = `<div class="stats-col">${skel('width:35%')}${skel('height:84px')}</div>`;
  return `<div class="stats-section"><div class="stats-tiles">${tile.repeat(4)}</div><div class="stats-cols">${col}${col}</div></div>`;
}

/** Placeholder card grid (API keys, agents): n cards of a head and three lines. */
function renderSkeletonCards(n) {
  const card = `<div class="agent-card"><div class="card-head">${skel('width:50%')}</div><div class="card-body">${skel()}${skel('width:80%')}${skel('width:60%')}</div></div>`;
  return `<div class="cards-grid">${card.repeat(n)}</div>`;
}

/** Full webview HTML string for the Usage view; `opts.detailed` picks Detailed vs Compact, `opts.show` used|left. */
function renderUsageHtml(model, opts) {
  const { detailed = true, show = 'used', nonce = '', cspSource = '', days = 1, skeleton = false, quotasPending = false } = opts || {};
  const overviewBody = skeleton ? renderSkeletonStats() : detailed ? renderStatsSection(model.stats) : '';
  // The CLI prints PROVIDER QUOTAS and API KEYS as two always-present sections — never either/or on billing mode.
  const pending = skeleton || (quotasPending && !model.providers.length);
  const quotasBody = pending ? renderSkeletonProviders(3) : model.providers.map((p) => renderProvider(p, show, detailed)).join('');
  const apiKeysBody = skeleton ? renderSkeletonCards(1) : renderApiCards(model);
  const modelsBody = skeleton ? '' : renderProviderModels(model);
  const agentsBody = skeleton ? `<div class="agents-section">${renderSkeletonCards(3)}</div>` : renderAgentsBody(model, detailed);
  const agentsHeader = `AGENTS · ${agentsWindowLabel(model, days)}`;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>
  body { --track: color-mix(in srgb, var(--vscode-foreground) 14%, transparent); --card-border: color-mix(in srgb, var(--vscode-foreground) 12%, transparent); --card-tint: color-mix(in srgb, var(--vscode-foreground) 3%, transparent); --card-head-tint: color-mix(in srgb, var(--vscode-foreground) 5%, transparent); --heat-cell: 10px; font-family: var(--vscode-font-family); font-size: 13px; color: var(--vscode-foreground); background: var(--vscode-sideBar-background); margin: 0; padding: 0; }
  .header { display: flex; align-items: center; justify-content: space-between; gap: 8px; padding: 8px 12px; flex-wrap: nowrap; }
  .header h1 { font-size: 13px; font-weight: 600; margin: 0; min-width: 0; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .header-right { display: flex; align-items: center; gap: 8px; flex: none; white-space: nowrap; }
  .muted { color: var(--vscode-descriptionForeground); }
  .section-header-row { display: flex; align-items: center; justify-content: space-between; }
  .show-toggle { display: inline-flex; border: 1px solid var(--vscode-widget-border); border-radius: 4px; overflow: hidden; text-transform: none; letter-spacing: 0; }
  .show-toggle button { background: none; border: none; color: var(--vscode-descriptionForeground); padding: 4px 8px; min-height: 24px; cursor: pointer; font-size: 11px; }
  .show-toggle button.active { background: var(--vscode-button-background); color: var(--vscode-button-foreground); }

  .icon-btn { background: none; border: none; color: var(--vscode-foreground); cursor: pointer; padding: 2px; min-width: 24px; min-height: 24px; display: inline-flex; align-items: center; justify-content: center; }
  .segmented { display: flex; margin: 0 12px 8px; border: 1px solid var(--vscode-widget-border); border-radius: 4px; overflow: hidden; }
  .segmented button { flex: 1; background: none; border: none; color: var(--vscode-foreground); padding: 5px 0; min-height: 24px; cursor: pointer; font-size: 12px; }
  .segmented button.active { background: var(--vscode-button-background); color: var(--vscode-button-foreground); }
  /* Section headers: 16px of space + a 1px rule above, 8px below the rule to the uppercase label. */
  .section-header { margin: 16px 12px 8px; padding-top: 8px; border-top: 1px solid var(--vscode-widget-border); font-size: 11px; font-weight: 600; letter-spacing: 0.08em; text-transform: uppercase; color: var(--vscode-descriptionForeground); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .quotas-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; padding: 0 12px; }
  .provider-card, .agent-card, .api-card, .model-card { border: 1px solid var(--card-border); border-radius: 8px; background: var(--card-tint); padding: 0; min-width: 0; overflow: hidden; }
  .card-head { padding: 8px 10px; background: var(--card-head-tint); border-bottom: 1px solid var(--card-border); }
  .card-body { padding: 8px 10px; }
  .provider-card-muted { opacity: 0.7; }
  .provider-head { display: flex; align-items: center; gap: 6px; min-width: 0; flex-wrap: nowrap; }
  .provider-name { font-weight: 600; flex: none; white-space: nowrap; }
  .provider-resets { color: var(--vscode-descriptionForeground); font-size: 12px; flex: 1 1 auto; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .glyph { width: 16px; text-align: center; font-size: 13px; flex: none; }
  .glyph-claude { color: #d97757; }
  .glyph-agy { color: #4b8bff; }
  .icon-btn { font-size: 14px; line-height: 1; }
  .windows { margin-top: 0; display: flex; flex-direction: column; gap: 6px; }
  .window-row { display: flex; align-items: center; gap: 6px; font-size: 12px; white-space: nowrap; }
  .window-label { min-width: 32px; color: var(--vscode-descriptionForeground); }
  .window-bar { flex: none; }
  .window-bar { width: 36px; height: 6px; border-radius: 3px; background: var(--track); overflow: hidden; }
  .window-fill { display: block; height: 100%; }
  .window-pct { width: 32px; text-align: right; }
  .windows-compact { flex-direction: row; flex-wrap: wrap; gap: 10px; }
  .grouped-grid, .ungrouped-grid { margin-top: 0; display: grid; align-items: center; gap: 6px 6px; font-size: 12px; }
  .ungrouped-grid { grid-template-columns: minmax(0,1fr) 36px 36px; }
  .grid-group-name, .grid-row-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; color: var(--vscode-descriptionForeground); }
  .grid-col-label { grid-column: span 2; text-align: right; color: var(--vscode-descriptionForeground); }
  .grid-bar { display: inline-block; width: 36px; height: 6px; border-radius: 3px; background: var(--track); overflow: hidden; }
  .grid-pct { width: 36px; text-align: right; }
  .chip { display: inline-flex; align-items: center; gap: 4px; font-size: 12px; white-space: nowrap; }
  .chip-label { color: var(--vscode-descriptionForeground); }
  .chip-bar { width: 24px; height: 6px; border-radius: 3px; background: var(--track); overflow: hidden; }
  .chip-fill { display: block; height: 100%; }
  .provider-note { margin-top: 0; color: var(--vscode-descriptionForeground); font-size: 12px; }
  .provider-account { font-weight: 400; }
  .credits-note { margin-top: 4px; font-size: 11px; }
  .agents-section { padding: 0 12px; }
  .agents-grid, .cards-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(220px, 1fr)); gap: 12px; }
  .cards-grid { padding: 0 12px; }
  /* Equal-height cards: the body scrolls on overflow so one long card never stretches its row. */
  .fixed-card { height: 220px; display: flex; flex-direction: column; }
  .fixed-card .card-body { flex: 1 1 auto; min-height: 0; overflow-y: auto; }
  .fixed-card .agent-share { margin: 0 10px 8px; flex: none; }
  .api-head { display: flex; align-items: baseline; justify-content: space-between; gap: 6px; min-width: 0; }
  .api-head .muted { font-size: 11px; white-space: nowrap; }
  .badge { font-size: 10px; font-weight: 400; padding: 0 4px; border-radius: 3px; background: var(--vscode-badge-background); color: var(--vscode-badge-foreground); }
  .agent-card-muted { opacity: 0.7; }
  .agent-name { font-weight: 600; }
  .agent-model, .agent-stats, .agent-note { font-size: 12px; }
  .agent-stats { margin-top: 2px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .agent-models { margin-top: 4px; display: flex; flex-direction: column; gap: 2px; }
  .agent-model-row { display: flex; align-items: baseline; justify-content: space-between; gap: 6px; font-size: 11px; }
  .agent-model-name { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .agent-model-stats { flex: none; white-space: nowrap; }
  .agent-share { margin-top: 6px; height: 4px; border-radius: 2px; background: var(--track); overflow: hidden; }
  .agent-share-fill { display: block; height: 100%; background: var(--vscode-charts-blue, var(--vscode-foreground)); }
  .agent-compact { display: flex; align-items: center; justify-content: space-between; gap: 6px; font-size: 12px; padding: 2px 0; }
  .api-table { width: 100%; border-collapse: collapse; font-size: 11px; table-layout: fixed; }
  .api-table th, .api-table td { text-align: right; padding: 1px 0 1px 6px; white-space: nowrap; }
  .api-table th:first-child, .api-table td:first-child { text-align: left; padding-left: 0; width: 55%; }
  .api-model { overflow: hidden; text-overflow: ellipsis; }
  .api-table th { color: var(--vscode-descriptionForeground); font-weight: 400; }
  .stats-section { padding: 0 12px; }
  .stats-tiles { display: grid; grid-template-columns: repeat(auto-fit, minmax(120px, 1fr)); gap: 8px; margin-bottom: 10px; }
  .stat-tile { border: 1px solid var(--vscode-widget-border); border-radius: 4px; padding: 8px; min-width: 0; }
  .stat-tile-label { font-size: 12px; margin-bottom: 2px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .stat-tile-value { font-size: clamp(13px, 4vw, 16px); font-weight: 600; font-variant-numeric: tabular-nums; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .stats-cols { display: grid; grid-template-columns: 1fr 1fr; gap: 12px; }
  .stats-col { border: 1px solid var(--vscode-widget-border); border-radius: 4px; padding: 8px; min-width: 0; }
  .stats-col-head { display: flex; align-items: center; justify-content: space-between; gap: 6px; margin-bottom: 6px; }
  .stats-col-title { font-weight: 600; font-size: 12px; min-width: 0; flex: 1 1 auto; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .stats-col-head .muted { flex: none; white-space: nowrap; }
  .heatmap-wrap { overflow-x: auto; }
  .heatmap { display: grid; gap: 2px; }
  .heat-cell { width: var(--heat-cell); height: var(--heat-cell); border-radius: 2px; background: var(--vscode-charts-blue, var(--vscode-foreground)); }
  .heat-legend { display: flex; align-items: center; gap: 3px; margin-top: 6px; font-size: 11px; }
  .heat-legend-cell { display: inline-block; }
  .token-mix { display: flex; flex-direction: column; gap: 8px; }
  .mix-bar { display: flex; height: 10px; border-radius: 3px; overflow: hidden; background: var(--vscode-widget-border); }
  .mix-seg { display: block; height: 100%; }
  .mix-legend { display: flex; flex-wrap: wrap; gap: 8px; font-size: 11px; }
  .mix-legend-item { display: inline-flex; align-items: center; gap: 4px; color: var(--vscode-descriptionForeground); }
  .mix-swatch { width: 8px; height: 8px; border-radius: 2px; display: inline-block; }
  .skel { height: 12px; margin: 6px 0; border-radius: 4px; background: linear-gradient(90deg, var(--track) 25%, var(--card-head-tint) 50%, var(--track) 75%); background-size: 200% 100%; animation: skel-shimmer 1.4s linear infinite; }
  @keyframes skel-shimmer { from { background-position: 200% 0; } to { background-position: -200% 0; } }
  @media (prefers-reduced-motion: reduce) { .skel { animation: none; } }
  @media (max-width: 480px) {
    .stats-cols { grid-template-columns: 1fr; }
  }
  /* Short Panel: the heatmap/token-mix columns hide so the sections below the overview stay above the fold. */
  @media (max-height: 420px) {
    .stats-cols { display: none; }
    .stats-section { padding: 0 8px; }
    body { --heat-cell: 8px; }
  }
</style>
</head>
<body>
  <div class="header">
    <h1>Agents Dashboard</h1>
    <div class="header-right">
      <span class="muted">all agents</span>
      <button class="icon-btn" id="refresh" title="Refresh">\u21BB</button>
    </div>
  </div>
  <div class="segmented">
    <button id="detailed" class="${detailed ? 'active' : ''}">Detailed</button>
    <button id="compact" class="${!detailed ? 'active' : ''}">Compact</button>
  </div>
  <div class="section-header">OVERVIEW</div>
  ${overviewBody}
  <div class="section-header section-header-row"><span>QUOTAS</span>
    <span class="show-toggle" role="group" aria-label="Percentages">
      <button id="show-used" class="${show === 'used' ? 'active' : ''}" title="Show percentage used">Usage</button>
      <button id="show-left" class="${show === 'left' ? 'active' : ''}" title="Show percentage remaining">Remaining</button>
    </span></div>
  <div class="quotas-grid">${quotasBody}</div>
  <div class="section-header">API KEYS</div>
  ${apiKeysBody}
  <div class="section-header">${esc(agentsHeader)}</div>
  ${agentsBody}
  ${modelsBody ? `<div class="section-header">MODELS</div>${modelsBody}` : ''}
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.getElementById('refresh').addEventListener('click', () => vscode.postMessage({ type: 'refresh' }));
    for (const mode of ['used', 'left']) {
      const btn = document.getElementById('show-' + mode);
      if (btn) btn.addEventListener('click', () => vscode.postMessage({ type: 'show', show: mode }));
    }
    const detailedBtn = document.getElementById('detailed');
    const compactBtn = document.getElementById('compact');
    if (detailedBtn) detailedBtn.addEventListener('click', () => vscode.postMessage({ type: 'toggle', detailed: true }));
    if (compactBtn) compactBtn.addEventListener('click', () => vscode.postMessage({ type: 'toggle', detailed: false }));
  </script>
</body>
</html>`;
}

module.exports = { renderUsageHtml, pivotGroupedWindows };
