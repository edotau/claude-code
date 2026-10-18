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

module.exports = { formatCount, formatTokens, formatAge };
