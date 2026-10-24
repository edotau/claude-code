'use strict';
// Task query language, ported 1:1 from claude-orcastra's src/shared/task-query.ts. Pure; node:test exercises it.

/** Splits on whitespace outside quotes; `value` has quotes stripped, `raw` keeps them. */
function tokenizeWithRaw(rawQuery) {
  const tokens = [];
  let value = '';
  let raw = '';
  let quote = null;
  const flush = () => {
    if (value || raw) {
      tokens.push({ value, raw });
      value = '';
      raw = '';
    }
  };
  for (const char of String(rawQuery || '')) {
    if (/\s/.test(char) && quote === null) {
      flush();
      continue;
    }
    raw += char;
    if ((char === '"' || char === "'") && quote === null) {
      quote = char;
      continue;
    }
    if (char === quote) {
      quote = null;
      continue;
    }
    value += char;
  }
  flush();
  return tokens;
}

function tokenizeSearchQuery(rawQuery) {
  return tokenizeWithRaw(rawQuery).map((t) => t.value);
}

function hasExplicitTaskScope(rawQuery) {
  return tokenizeWithRaw(rawQuery).some(({ value, raw }) => /^is:/i.test(raw) && /^is:(?:issue|pr|pull-request)$/i.test(value));
}

function emptyQuery() {
  return { scope: 'all', state: null, draft: false, assignee: null, author: null, reviewRequested: null, reviewedBy: null, labels: [], freeText: '' };
}

/** `is:` / `state:` / `assignee:` / `author:` / `review-requested:` / `reviewed-by:` / `label:`; the rest is free text. */
function parseTaskQuery(rawQuery) {
  const query = emptyQuery();
  const freeText = [];
  let sawIssue = false;
  let sawPr = false;
  for (const { value: token, raw } of tokenizeWithRaw(String(rawQuery || '').trim())) {
    if (raw.startsWith('"') || raw.startsWith("'")) {
      freeText.push(raw);
      continue;
    }
    const normalized = token.toLowerCase();
    if (normalized === 'is:issue') {
      sawIssue = true;
      query.scope = sawPr ? 'all' : 'issue';
      continue;
    }
    if (normalized === 'is:pr' || normalized === 'is:pull-request') {
      sawPr = true;
      query.scope = sawIssue ? 'all' : 'pr';
      continue;
    }
    if (normalized === 'is:open' || normalized === 'is:closed' || normalized === 'is:merged') {
      query.state = normalized.slice(3);
      continue;
    }
    if (normalized === 'is:draft') {
      query.scope = 'pr';
      query.state = 'open';
      query.draft = true;
      continue;
    }
    const [rawKey, ...rest] = token.split(':');
    const value = rest.join(':').trim();
    const key = rawKey.toLowerCase();
    if (!value) {
      freeText.push(raw);
      continue;
    }
    if (key === 'assignee') query.assignee = value;
    else if (key === 'author') query.author = value;
    else if (key === 'review-requested') {
      query.scope = 'pr';
      query.reviewRequested = value;
    } else if (key === 'reviewed-by') {
      query.scope = 'pr';
      query.reviewedBy = value;
    } else if (key === 'label') query.labels.push(value);
    else if (key === 'state' && ['open', 'closed', 'merged', 'all'].includes(value.toLowerCase())) query.state = value.toLowerCase();
    else freeText.push(raw); // unknown qualifiers pass through to the backend untouched
  }
  if (query.draft) {
    query.scope = 'pr';
    query.state = 'open';
  } else if (query.state === 'merged' || query.reviewRequested !== null || query.reviewedBy !== null) {
    query.scope = 'pr';
  }
  query.freeText = freeText.join(' ').trim();
  return query;
}

function quoteIfNeeded(value) {
  return /\s/.test(value) ? `"${value.replaceAll('"', '\\"')}"` : value;
}

/** Inverse of parseTaskQuery for the qualifiers it understands; free text goes last. */
function serializeTaskQuery(q) {
  const parts = [];
  if (q.scope === 'pr') parts.push('is:pr');
  else if (q.scope === 'issue') parts.push('is:issue');
  if (q.state === 'open' || q.state === 'closed' || q.state === 'merged') parts.push(`is:${q.state}`);
  else if (q.state === 'all') parts.push('state:all');
  if (q.draft) parts.push('is:draft');
  if (q.author) parts.push(`author:${quoteIfNeeded(q.author)}`);
  if (q.assignee) parts.push(`assignee:${quoteIfNeeded(q.assignee)}`);
  if (q.reviewRequested) parts.push(`review-requested:${quoteIfNeeded(q.reviewRequested)}`);
  if (q.reviewedBy) parts.push(`reviewed-by:${quoteIfNeeded(q.reviewedBy)}`);
  for (const label of q.labels) parts.push(`label:${quoteIfNeeded(label)}`);
  if (q.freeText) parts.push(q.freeText);
  return parts.join(' ');
}

/** Sets one qualifier on a raw query (null clears; `labels` takes the full next array) and reserializes. */
function withQualifier(rawQuery, key, value) {
  const parsed = parseTaskQuery(rawQuery);
  const str = typeof value === 'string' ? value : null;
  switch (key) {
    case 'author':
      parsed.author = str;
      break;
    case 'assignee':
      parsed.assignee = str;
      break;
    case 'reviewRequested':
      parsed.reviewRequested = str;
      if (str) parsed.scope = 'pr';
      break;
    case 'reviewedBy':
      parsed.reviewedBy = str;
      if (str) parsed.scope = 'pr';
      break;
    case 'labels':
      parsed.labels = Array.isArray(value) ? value : [];
      break;
    case 'state':
      parsed.state = ['open', 'closed', 'merged', 'all'].includes(value) ? value : null;
      if (parsed.state === 'merged') parsed.scope = 'pr';
      if (parsed.state !== 'open') parsed.draft = false;
      break;
    case 'draft':
      parsed.draft = value === 'true';
      if (parsed.draft) {
        parsed.scope = 'pr';
        parsed.state = 'open';
      }
      break;
    default:
      break;
  }
  return serializeTaskQuery(parsed);
}

/** Drops `repo:` tokens before a per-repo fan-out; tokens with whitespace are re-quoted so `label:"a b"` round-trips. */
function stripRepoQualifiers(rawQuery) {
  const kept = [];
  for (const token of tokenizeSearchQuery(String(rawQuery || '').trim())) {
    if (/^repo:[^\s]+$/i.test(token)) continue;
    if (/\s/.test(token)) {
      const [rawKey, ...rest] = token.split(':');
      kept.push(rest.length > 0 ? `${rawKey}:"${rest.join(':')}"` : `"${token}"`);
    } else {
      kept.push(token);
    }
  }
  return kept.join(' ');
}

module.exports = { tokenizeSearchQuery, hasExplicitTaskScope, parseTaskQuery, serializeTaskQuery, withQualifier, stripRepoQualifiers };
