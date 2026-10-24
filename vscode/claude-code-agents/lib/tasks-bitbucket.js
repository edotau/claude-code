'use strict';
// Bitbucket Data Center adapter for the Tasks page: picks the DC endpoint a parsed query maps to, then shapes
// lib/bitbucket-client.js rows into the same row contract lib/tasks-github.js produces. Pure except `list`.
const { detectJiraIssueKey } = require('./tasks-model');

/** The query's state as a DC `state` param; DC has no "merged" vs "closed" split beyond MERGED/DECLINED. */
function dcState(query) {
  switch (query.state) {
    case 'merged':
      return 'MERGED';
    case 'closed':
      return 'DECLINED';
    case 'all':
      return 'ALL';
    default:
      return 'OPEN';
  }
}

/** `review-requested:@me` → dashboard REVIEWER; `author:@me` → dashboard AUTHOR; anything else → the repo's PRs. */
function endpointFor(query) {
  if (query.reviewRequested === '@me') return { kind: 'dashboard', role: 'REVIEWER' };
  if (query.author === '@me') return { kind: 'dashboard', role: 'AUTHOR' };
  return { kind: 'repo' };
}

/** `{project, repo}` from a DC PR URL (`/projects/P/repos/r/pull-requests/1`), or null. */
function repoFromUrl(url) {
  const m = /\/projects\/([^/]+)\/repos\/([^/]+)\//.exec(url || '');
  return m ? { project: decodeURIComponent(m[1]).toUpperCase(), repo: decodeURIComponent(m[2]) } : null;
}

function checksOf(build) {
  const b = build || { state: 'unknown' };
  const state = b.state === 'success' ? 'success' : b.state === 'failed' ? 'failed' : b.state === 'in_progress' ? 'in_progress' : 'none';
  const label = state === 'success' ? `${b.successful || 0} passed` : state === 'failed' ? `${b.failed || 0} failing` : state === 'in_progress' ? `${b.in_progress || 0} running` : 'No builds';
  return { state, label, counts: { success: b.successful || 0, failed: b.failed || 0, pending: b.in_progress || 0, skipped: 0 }, ...(b.url ? { url: b.url } : {}) };
}

/** bitbucket-client row → row contract (`source: 'bitbucket'`, `type: 'pr'`); DC exposes no mergeability, so Merge is a link. */
function mapRow(row, opts) {
  const { jiraBaseUrl } = opts || {};
  const repo = repoFromUrl(row.url) || { project: '', repo: '' };
  const state = row.state === 'merged' ? 'merged' : row.state === 'declined' ? 'closed' : 'open';
  const jiraKey = detectJiraIssueKey({ branch: row.from_branch, title: row.title });
  return {
    source: 'bitbucket',
    id: `bitbucket:${repo.project}/${repo.repo}:pr:${row.id}`,
    type: 'pr',
    number: row.id,
    title: row.title || '',
    state,
    url: row.url || '',
    repo: { owner: repo.project, name: repo.repo, label: `${repo.project}/${repo.repo}` },
    author: row.author || null,
    updatedAt: row.updated || '',
    labels: [],
    branch: row.from_branch || '',
    baseBranch: row.to_branch || '',
    reviewers: (row.reviewers || []).map((r) => ({ name: r.name, status: r.status === 'APPROVED' ? 'APPROVED' : r.status === 'NEEDS_WORK' ? 'CHANGES_REQUESTED' : 'PENDING' })),
    assignees: [],
    checks: checksOf(row.build),
    merge: state === 'merged' ? { state: 'merged', label: 'Merged' } : state === 'closed' ? { state: 'closed', label: 'Declined' } : { state: 'unknown', label: 'Open in Bitbucket' },
    jiraKey,
    jiraUrl: jiraKey && jiraBaseUrl ? `${jiraBaseUrl.replace(/\/$/, '')}/browse/${jiraKey}` : null,
  };
}

function includesCi(haystack, needle) {
  return String(haystack || '').toLowerCase().includes(needle.toLowerCase());
}

/** Client-side narrowing DC can't do server-side: non-@me author, free text over title/branch/number/Jira key. */
function filterRows(rows, query) {
  return rows.filter((row) => {
    if (query.author && query.author !== '@me' && !includesCi(row.author, query.author)) return false;
    if (query.reviewRequested && query.reviewRequested !== '@me' && !row.reviewers.some((r) => includesCi(r.name, query.reviewRequested))) return false;
    if (query.freeText) {
      const words = query.freeText.replace(/["']/g, '').split(/\s+/).filter(Boolean);
      const hay = `${row.title} ${row.branch} #${row.number} ${row.jiraKey || ''} ${row.author || ''}`;
      if (!words.every((w) => includesCi(hay, w))) return false;
    }
    return true;
  });
}

/** One page of DC rows for `query`: the dashboard (mine / reviewing) or every `repos` entry's PR list. */
async function list({ client, query, repos, page = 1, limit = 36, jiraBaseUrl }) {
  const endpoint = endpointFor(query);
  const state = dcState(query);
  const errors = [];
  let raw = [];
  if (endpoint.kind === 'dashboard') {
    raw = await client.dashboard(endpoint.role, state);
  } else {
    const settled = await Promise.allSettled((repos || []).map((ref) => client.pullRequests(ref, state)));
    settled.forEach((s, i) => {
      if (s.status === 'fulfilled') raw.push(...s.value);
      else errors.push({ repo: `${repos[i].project}/${repos[i].repo}`, kind: 'pull requests', message: s.reason && s.reason.message ? s.reason.message : String(s.reason) });
    });
  }
  const mapped = (await client.listWithBuilds(raw)).map((row) => mapRow(row, { jiraBaseUrl }));
  const rows = filterRows(mapped, query).sort((a, b) => Date.parse(b.updatedAt || 0) - Date.parse(a.updatedAt || 0));
  const totalPages = Math.max(1, Math.ceil(rows.length / limit));
  return { items: rows.slice((page - 1) * limit, page * limit), errors, totalPages, failedRepos: errors.length };
}

module.exports = { dcState, endpointFor, repoFromUrl, mapRow, filterRows, list };
