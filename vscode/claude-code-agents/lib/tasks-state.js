'use strict';
// Tasks page state helpers: presets per source, page numbering, relative time, and a single-flight page cache.

const PRESETS = {
  github: [
    { id: 'assigned', label: 'Assigned to me', query: 'is:issue is:open assignee:@me' },
    { id: 'created', label: 'Created by me', query: 'is:open author:@me' },
    { id: 'review', label: 'Review requested', query: 'is:pr is:open review-requested:@me' },
    { id: 'myprs', label: 'My PRs', query: 'is:pr is:open author:@me' },
  ],
  bitbucket: [
    { id: 'review', label: 'Review requests', query: 'is:pr is:open review-requested:@me' },
    { id: 'myprs', label: 'My pull requests', query: 'is:pr is:open author:@me' },
    { id: 'repo', label: 'This repo', query: 'is:pr is:open' },
  ],
};

const DEFAULT_QUERY = { github: 'is:open', bitbucket: 'is:pr is:open review-requested:@me' };

function presetsFor(source) {
  return PRESETS[source] || [];
}

/** Orca's page strip: all pages up to 9, else first/last + a ±2 window around `current` with ellipses (0-based). */
function getPageNumbers(current, total) {
  if (total <= 9) return Array.from({ length: total }, (_, i) => i);
  const pages = new Set([0, total - 1]);
  for (let i = Math.max(0, current - 2); i <= Math.min(total - 1, current + 2); i++) pages.add(i);
  const sorted = [...pages].sort((a, b) => a - b);
  const out = [];
  sorted.forEach((p, i) => {
    if (i > 0 && p - sorted[i - 1] > 1) out.push('ellipsis');
    out.push(p);
  });
  return out;
}

/** "just now" · "5m ago" · "3h ago" · "2d ago" · "Oct 3" (same year) · "Oct 3, 2025"; '' for an unparseable input. */
function formatRelativeTime(input, nowMs) {
  const t = Date.parse(input);
  if (!Number.isFinite(t)) return '';
  const now = nowMs == null ? Date.now() : nowMs;
  const diff = Math.max(0, now - t);
  const min = Math.floor(diff / 60000);
  if (min < 1) return 'just now';
  if (min < 60) return `${min}m ago`;
  const hours = Math.floor(min / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  if (days < 7) return `${days}d ago`;
  const d = new Date(t);
  const sameYear = d.getUTCFullYear() === new Date(now).getUTCFullYear();
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC', ...(sameYear ? {} : { year: 'numeric' }) });
}

function cacheKey(parts) {
  return JSON.stringify([parts.source, parts.repos, parts.query, parts.page]);
}

/** Page cache with TTL + single-flight: concurrent loads of one key share the in-flight promise; `invalidate` drops all. */
class PageCache {
  constructor(opts) {
    this.ttlMs = (opts && opts.ttlMs) || 120000;
    this.entries = new Map();
    this.inflight = new Map();
  }

  get(parts, nowMs) {
    const hit = this.entries.get(cacheKey(parts));
    if (!hit) return null;
    if ((nowMs == null ? Date.now() : nowMs) - hit.at > this.ttlMs) return null;
    return hit.value;
  }

  async load(parts, loader, opts) {
    const key = cacheKey(parts);
    const now = opts && opts.nowMs != null ? opts.nowMs : Date.now();
    if (!(opts && opts.force)) {
      const cached = this.get(parts, now);
      if (cached) return cached;
    }
    if (this.inflight.has(key)) return this.inflight.get(key);
    const p = Promise.resolve()
      .then(loader)
      .then((value) => {
        this.entries.set(key, { at: now, value });
        return value;
      })
      .finally(() => this.inflight.delete(key));
    this.inflight.set(key, p);
    return p;
  }

  invalidate() {
    this.entries.clear();
  }
}

module.exports = { PRESETS, DEFAULT_QUERY, presetsFor, getPageNumbers, formatRelativeTime, cacheKey, PageCache };
