'use strict';
// Pure formatting helpers — no vscode dependency, so node:test can exercise them directly.

/** Compact "Nk"/"NM" for large counts, otherwise the plain number. */
function formatCount(n) {
  if (typeof n !== 'number' || !Number.isFinite(n)) return '0';
  if (Math.abs(n) >= 1_000_000) return (n / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (Math.abs(n) >= 1_000) return (n / 1_000).toFixed(1).replace(/\.0$/, '') + 'k';
  return String(n);
}

module.exports = { formatCount };
