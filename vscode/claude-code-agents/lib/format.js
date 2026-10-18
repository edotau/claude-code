'use strict';
// Pure formatting helpers — no vscode dependency, so node:test can exercise them directly.

/** Compact "Nk"/"NM" for large counts, otherwise the plain number. */
function formatCount(n) {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '0';
  if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (Math.abs(n) >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

/** "49.5k in / 309 out" style summary; missing fields read as 0. */
function formatTokens(tokens) {
  const t = tokens || {};
  return `${formatCount(t.in || 0)} in / ${formatCount(t.out || 0)} out`;
}

/** Relative age ("just now", "5m", "3h", "2d") from an ISO timestamp to `now` (ms epoch, defaults to Date.now()). */
function formatAge(iso, now) {
  if (!iso) return 'unknown';
  const then = Date.parse(iso);
  if (Number.isNaN(then)) return 'unknown';
  const nowMs = typeof now === 'number' ? now : Date.now();
  const deltaSec = Math.max(0, Math.round((nowMs - then) / 1000));
  if (deltaSec < 60) return 'just now';
  const deltaMin = Math.round(deltaSec / 60);
  if (deltaMin < 60) return `${deltaMin}m`;
  const deltaHour = Math.round(deltaMin / 60);
  if (deltaHour < 24) return `${deltaHour}h`;
  const deltaDay = Math.round(deltaHour / 24);
  return `${deltaDay}d`;
}

/** "tool arg" from an Activity{tool,arg,text}, falling back to its text blurb; '' when absent. */
function formatActivity(activity) {
  if (!activity) return '';
  if (activity.tool) return activity.arg ? `${activity.tool} ${activity.arg}` : activity.tool;
  return activity.text || '';
}

/** "↳ codex gpt-6-astra running 2m14s [⚠ fallback]", mirroring internal/agenttree/render.go's formatLeg. */
function formatLeg(leg) {
  const name = leg.fallback ? `${leg.fallback}→${leg.agent}` : leg.agent;
  let line = `↳ ${name} ${leg.model || ''} ${leg.status || ''} ${leg.duration || ''}`.replace(/\s+/g, ' ').trim();
  if (leg.fallback) line += ' ⚠ fallback';
  return line;
}

/** "HH:MM:SS tool Read path…" / "HH:MM:SS text "…"" / "HH:MM:SS result 12 B", one Snapshot.Events entry. */
function formatEventLine(e) {
  const ts = e.at ? new Date(e.at).toLocaleTimeString() : '';
  if (e.kind === 'tool') return `${ts} tool ${e.tool || ''} ${e.arg || ''}`.trim();
  if (e.kind === 'text') return `${ts} text "${e.text || ''}"`;
  if (e.kind === 'result') return `${ts} result ${e.size || 0} B`;
  return ts;
}

/** Plain-text rendering of a `web view --format json` Snapshot, for the output-channel detail pane. */
function formatSnapshotText(snap) {
  const lines = [`${snap.id}  ${snap.label || ''}`];
  if (snap.workflow) lines.push(`workflow ${snap.workflow}  phase ${snap.phase || ''}`);
  const model = snap.effort ? `${snap.model || ''} effort=${snap.effort}` : snap.model || '';
  lines.push(`status ${snap.status}  model ${model}  ${formatTokens(snap.tokens)}`);
  const activity = formatActivity(snap.activity);
  if (activity) lines.push(`last activity: ${activity}`);
  for (const f of snap.filesEdited || []) lines.push(`  edited ${f}`);
  for (const l of snap.legs || []) lines.push(`  ${formatLeg(l)}`);
  for (const e of snap.events || []) lines.push(formatEventLine(e));
  if (snap.result) lines.push(`result: ${snap.result}`);
  return lines.join('\n');
}

module.exports = { formatCount, formatTokens, formatAge, formatActivity, formatLeg, formatEventLine, formatSnapshotText };
