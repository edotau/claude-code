'use strict';
// Webview HTML for the Usage popover — no vscode dependency, so node:test can exercise it directly.
const { formatCount } = require('./format');
const { formatResetCountdown, sessionsLabel } = require('./usage-model');

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

function renderProvider(p, show, detailed) {
  const resets = p.resetsAtMs != null ? formatResetCountdown(p.resetsAtMs - Date.now()) : '';
  const body = p.windows.length
    ? detailed
      ? hasMultipleGroups(p.windows)
        ? renderGroupedGrid(p.windows, show)
        : renderUngroupedGrid(p.windows, show)
      : `<div class="windows windows-compact">${p.windows.map((w) => renderWindow(w, show, false)).join('')}</div>`
    : `<div class="provider-note">${esc(p.note || 'no data')}</div>`;
  return `<div class="provider" data-provider="${esc(p.id)}">
    <div class="provider-head">
      ${GLYPHS[p.id] || '<span class="glyph">\u25CF</span>'}
      <span class="provider-name">${esc(p.name)}</span>
      <span class="provider-resets">${esc(resets)}</span>
      <span class="chevron">\u203A</span>
    </div>
    ${body}
  </div>`;
}

/** One agent row: bold name, muted provider/model (or note), then a "tokens · sessions · turns · $cost" line. */
function renderAgentDetailed(a) {
  const label = a.topModel || a.note || '';
  const parts = [];
  if (a.tokens) parts.push(`${formatCount(a.tokens)} tokens`);
  if (a.sessions) parts.push(sessionsLabel(a.sessions));
  if (a.turns) parts.push(`${a.turns} turns`);
  if (a.costUsd != null) parts.push(`$${a.costUsd.toFixed(2)}`);
  return `<div class="agent">
    <div class="agent-name">${esc(a.name)}</div>
    ${label ? `<div class="agent-model muted">${esc(label)}</div>` : ''}
    ${parts.length ? `<div class="agent-stats muted">${esc(parts.join(' · '))}</div>` : ''}
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

function renderAgentsSection(model, detailed, days) {
  const agents = model.agents || [];
  if (!agents.length) return '';
  const windowLabel = `last ${days} day${days === 1 ? '' : 's'}`;
  const body = detailed
    ? agents.map(renderAgentDetailed).join('')
    : agents.map(renderAgentCompact).join('');
  return `<div class="agents-section">
    <div class="agents-head">
      <span class="agents-title">Agents</span>
      <span class="muted">${esc(windowLabel)}</span>
    </div>
    ${body}
  </div>`;
}

function renderApiTable(model) {
  const rows = (model.api || [])
    .map(
      (r) => `<tr>
        <td>${esc(r.provider)}</td>
        <td>${esc(r.model)}</td>
        <td>${esc(formatCount(r.today))}</td>
        <td>${esc(formatCount(r.last30d))}</td>
        <td>${esc(formatCount(r.allTime))}</td>
      </tr>`
    )
    .join('');
  return `<table class="api-table">
    <thead><tr><th>Provider</th><th>Model</th><th>Today</th><th>30d</th><th>All time</th></tr></thead>
    <tbody>${rows || '<tr><td colspan="5" class="muted">No token usage yet</td></tr>'}</tbody>
  </table>`;
}

/** Full webview HTML string for the Usage view; `opts.detailed` picks Detailed vs Compact, `opts.show` used|left. */
function renderUsageHtml(model, opts) {
  const { detailed = true, show = 'used', nonce = '', cspSource = '', days = 1 } = opts || {};
  const agentsSection = renderAgentsSection(model, detailed, days);
  const body =
    model.mode === 'api'
      ? agentsSection + renderApiTable(model)
      : model.providers.map((p) => renderProvider(p, show, detailed)).join('') + agentsSection;
  return `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline' ${cspSource}; script-src 'nonce-${nonce}';">
<style>
  body { font-family: var(--vscode-font-family); font-size: 13px; color: var(--vscode-foreground); background: var(--vscode-sideBar-background); margin: 0; padding: 0; }
  .header { display: flex; align-items: center; justify-content: space-between; padding: 8px 12px; }
  .header h1 { font-size: 13px; font-weight: 600; margin: 0; }
  .header-right { display: flex; align-items: center; gap: 8px; }
  .muted { color: var(--vscode-descriptionForeground); }
  .icon-btn { background: none; border: none; color: var(--vscode-foreground); cursor: pointer; padding: 2px; }
  .segmented { display: flex; margin: 0 12px 8px; border: 1px solid var(--vscode-widget-border); border-radius: 4px; overflow: hidden; }
  .segmented button { flex: 1; background: none; border: none; color: var(--vscode-foreground); padding: 4px 0; cursor: pointer; font-size: 12px; }
  .segmented button.active { background: var(--vscode-button-background); color: var(--vscode-button-foreground); }
  .provider { padding: 8px 12px; border-top: 1px solid var(--vscode-widget-border); cursor: pointer; }
  .provider-head { display: flex; align-items: center; gap: 6px; }
  .provider-name { font-weight: 600; }
  .provider-resets { color: var(--vscode-descriptionForeground); font-size: 12px; }
  .chevron { margin-left: auto; color: var(--vscode-descriptionForeground); font-size: 16px; line-height: 1; }
  .glyph { width: 16px; text-align: center; font-size: 13px; }
  .glyph-claude { color: #d97757; }
  .glyph-agy { color: #4b8bff; }
  .icon-btn { font-size: 14px; line-height: 1; }
  .windows { margin-top: 6px; display: flex; flex-direction: column; gap: 4px; }
  .window-row { display: flex; align-items: center; gap: 6px; font-size: 12px; white-space: nowrap; }
  .window-label { min-width: 32px; color: var(--vscode-descriptionForeground); }
  .window-bar { flex: none; }
  .window-bar { width: 36px; height: 6px; border-radius: 3px; background: var(--vscode-widget-border); overflow: hidden; }
  .window-fill { display: block; height: 100%; }
  .window-pct { width: 32px; text-align: right; }
  .windows-compact { flex-direction: row; flex-wrap: wrap; gap: 10px; }
  .grouped-grid, .ungrouped-grid { margin-top: 6px; display: grid; align-items: center; gap: 4px 6px; font-size: 12px; }
  .ungrouped-grid { grid-template-columns: auto 36px 36px; }
  .grid-group-name, .grid-row-label { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; min-width: 0; color: var(--vscode-descriptionForeground); }
  .grid-col-label { grid-column: span 2; text-align: right; color: var(--vscode-descriptionForeground); }
  .grid-bar { display: inline-block; width: 36px; height: 6px; border-radius: 3px; background: var(--vscode-widget-border); overflow: hidden; }
  .grid-pct { width: 36px; text-align: right; }
  .chip { display: inline-flex; align-items: center; gap: 4px; font-size: 12px; white-space: nowrap; }
  .chip-label { color: var(--vscode-descriptionForeground); }
  .chip-bar { width: 24px; height: 6px; border-radius: 3px; background: var(--vscode-widget-border); overflow: hidden; }
  .chip-fill { display: block; height: 100%; }
  .provider-note { margin-top: 4px; color: var(--vscode-descriptionForeground); font-size: 12px; }
  .agents-section { padding: 8px 12px; border-top: 1px solid var(--vscode-widget-border); }
  .agents-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 6px; }
  .agents-title { font-weight: 600; }
  .agent { margin-bottom: 8px; }
  .agent-name { font-weight: 600; }
  .agent-model, .agent-stats { font-size: 12px; }
  .agent-compact { display: flex; align-items: center; justify-content: space-between; font-size: 12px; padding: 2px 0; }
  .footer-row { display: flex; align-items: center; justify-content: space-between; padding: 8px 12px; border-top: 1px solid var(--vscode-widget-border); cursor: pointer; }
  .api-table { width: 100%; border-collapse: collapse; font-size: 12px; margin: 8px 12px; width: calc(100% - 24px); }
  .api-table th, .api-table td { text-align: left; padding: 2px 6px 2px 0; }
  .api-table th { color: var(--vscode-descriptionForeground); font-weight: 400; }
</style>
</head>
<body>
  <div class="header">
    <h1>Agent Dashboard</h1>
    <div class="header-right">
      <span class="muted">all agents</span>
      <button class="icon-btn" id="refresh" title="Refresh">\u21BB</button>
    </div>
  </div>
  ${
    model.mode === 'api'
      ? ''
      : `<div class="segmented">
    <button id="detailed" class="${detailed ? 'active' : ''}">Detailed</button>
    <button id="compact" class="${!detailed ? 'active' : ''}">Compact</button>
  </div>`
  }
  ${body}
  <div class="footer-row" id="details">
    <span>Usage details &amp; history</span>
    <span class="chevron">\u203A</span>
  </div>
  <div class="footer-row" id="accounts">
    <span>Manage Accounts&hellip;</span>
    <span class="chevron">\u203A</span>
  </div>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    document.getElementById('refresh').addEventListener('click', () => vscode.postMessage({ type: 'refresh' }));
    document.getElementById('details').addEventListener('click', () => vscode.postMessage({ type: 'details' }));
    document.getElementById('accounts').addEventListener('click', () => vscode.postMessage({ type: 'accounts' }));
    const detailedBtn = document.getElementById('detailed');
    const compactBtn = document.getElementById('compact');
    if (detailedBtn) detailedBtn.addEventListener('click', () => vscode.postMessage({ type: 'toggle', detailed: true }));
    if (compactBtn) compactBtn.addEventListener('click', () => vscode.postMessage({ type: 'toggle', detailed: false }));
    document.querySelectorAll('.provider').forEach((el) => {
      el.addEventListener('click', () => vscode.postMessage({ type: 'provider', id: el.getAttribute('data-provider') }));
    });
  </script>
</body>
</html>`;
}

module.exports = { renderUsageHtml, pivotGroupedWindows };
