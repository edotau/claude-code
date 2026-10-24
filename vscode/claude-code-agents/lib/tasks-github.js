'use strict';
// GitHub read path for the Tasks page over the `gh` CLI (execFile, no shell) — ported from claude-orcastra's
// src/main/github/client/list. Pure argv builders + mappers; `execFileImpl` is the injectable seam for tests.

const PR_LIST_FIELDS =
  'number,title,state,url,labels,updatedAt,author,isDraft,headRefName,baseRefName,headRefOid,reviewRequests,latestReviews,reviewDecision,additions,deletions,changedFiles,statusCheckRollup,mergeable,mergeStateStatus,assignees';
// Why: issue numbers follow creation order, so a created sort aligns gh's PR rows with numbered Search API pages.
const NUMBER_SORT = 'sort:created-desc';
const PER_REPO_FETCH_LIMIT = 36;
const CROSS_REPO_DISPLAY_LIMIT = 100;

/** `{owner, name}` for a github.com remote (ssh, scp-like or https), else null. */
function repoFromRemote(remoteUrl, host) {
  const trimmed = (remoteUrl || '').trim();
  const wanted = (host || 'github.com').toLowerCase();
  let hostname;
  let pathname;
  const scpLike = trimmed.match(/^(?:[^@/]+@)?([^:/]+):(?!\/\/)([^\s]+)$/);
  if (scpLike && !trimmed.includes('://')) {
    hostname = scpLike[1];
    pathname = scpLike[2];
  } else {
    try {
      const url = new URL(trimmed);
      hostname = url.hostname;
      pathname = url.pathname;
    } catch {
      return null;
    }
  }
  if (hostname.toLowerCase() !== wanted) return null;
  const parts = pathname.replace(/\.git$/i, '').split('/').filter(Boolean);
  if (parts.length !== 2) return null;
  return { owner: parts[0], name: parts[1] };
}

/** `owner/name` → `{owner, name}`; null for anything else. */
function parseRepoSlug(slug) {
  const m = /^([A-Za-z0-9_.-]+)\/([A-Za-z0-9_.-]+)$/.exec((slug || '').trim());
  return m ? { owner: m[1], name: m[2] } : null;
}

function quoteSearchValue(value) {
  return /[\s"]/.test(value) ? `"${value.replaceAll('\\', '\\\\').replaceAll('"', '\\"')}"` : value;
}

/** GitHub search qualifiers shared by the issue and PR requests; `kind` picks the PR-only ones. */
function searchParts(kind, repo, query) {
  const parts = [];
  if (kind === 'issue') parts.push(`repo:${repo.owner}/${repo.name}`);
  parts.push(kind === 'issue' ? 'is:issue' : 'is:pr');
  if (query.state === 'open') parts.push('is:open');
  else if (query.state === 'closed') {
    parts.push('is:closed');
    if (kind === 'pr') parts.push('-is:merged'); // "Closed" means closed-without-merge
  } else if (query.state === 'merged') parts.push('is:merged');
  if (kind === 'pr' && query.draft) parts.push('draft:true');
  if (query.assignee) parts.push(`assignee:${quoteSearchValue(query.assignee)}`);
  if (query.author) parts.push(`author:${quoteSearchValue(query.author)}`);
  for (const label of query.labels || []) parts.push(`label:${quoteSearchValue(label)}`);
  if (kind === 'pr' && query.reviewRequested) parts.push(`review-requested:${quoteSearchValue(query.reviewRequested)}`);
  if (kind === 'pr' && query.reviewedBy) parts.push(`reviewed-by:${quoteSearchValue(query.reviewedBy)}`);
  if (query.freeText) parts.push(query.freeText);
  return parts;
}

/** `gh api search/issues …` argv for one page of issues (REST search carries page= natively). */
function issueSearchArgs({ repo, query, page, limit }) {
  const q = searchParts('issue', repo, query).join(' ');
  return ['api', '--cache', '120s', `search/issues?q=${encodeURIComponent(q)}&sort=created&order=desc&per_page=${limit}&page=${page}`, '--jq', '.items'];
}

/** `gh pr list …` argv; search/issues lacks the PR columns, so gh's rich list is fetched to page×limit and sliced. */
function prListArgs({ repo, query, page, limit }) {
  const search = [...searchParts('pr', repo, query), NUMBER_SORT].join(' ');
  return { args: ['pr', 'list', '--repo', `${repo.owner}/${repo.name}`, '--state', 'all', '--limit', String(Math.min(page * limit, 1000)), '--search', search, '--json', PR_LIST_FIELDS], offset: (page - 1) * limit };
}

/** `gh api search/issues?…per_page=1 --jq .total_count` argv for the page count of one kind. */
function countArgs({ repo, query, kind }) {
  const parts = searchParts(kind, repo, query);
  if (kind === 'pr') parts.unshift(`repo:${repo.owner}/${repo.name}`);
  return ['api', '--cache', '120s', `search/issues?q=${encodeURIComponent(parts.join(' '))}&per_page=1`, '--jq', '.total_count'];
}

/** Folds `statusCheckRollup` to one state: failed > in_progress > pending > success > none. */
function summarizeChecks(rollup) {
  const list = Array.isArray(rollup) ? rollup : [];
  const counts = { success: 0, failed: 0, pending: 0, skipped: 0 };
  let url;
  for (const c of list) {
    const status = String(c.status || '').toUpperCase();
    const conclusion = String(c.conclusion || c.state || '').toUpperCase();
    if (status && status !== 'COMPLETED') {
      counts.pending++;
      continue;
    }
    if (conclusion === 'SUCCESS' || conclusion === 'NEUTRAL') counts.success++;
    else if (conclusion === 'SKIPPED') counts.skipped++;
    else if (conclusion === 'FAILURE' || conclusion === 'ERROR' || conclusion === 'TIMED_OUT' || conclusion === 'CANCELLED' || conclusion === 'ACTION_REQUIRED' || conclusion === 'STARTUP_FAILURE') {
      counts.failed++;
      if (!url) url = c.detailsUrl || c.targetUrl;
    } else if (conclusion === 'PENDING' || conclusion === 'EXPECTED' || conclusion === '') counts.pending++;
    else counts.success++;
  }
  const ran = counts.success + counts.failed + counts.pending;
  let state = 'none';
  let label = 'No checks';
  if (counts.failed > 0) {
    state = 'failed';
    label = `${counts.failed} failing`;
  } else if (counts.pending > 0) {
    state = 'in_progress';
    label = `${counts.pending} running`;
  } else if (ran > 0) {
    state = 'success';
    label = `${counts.success} passed`;
  }
  return { state, label, counts, ...(url ? { url } : {}) };
}

/** Merge readiness from gh's `mergeable` + `mergeStateStatus`, or the terminal state for merged/closed PRs. */
function mergeState(item, state) {
  if (state === 'merged') return { state: 'merged', label: 'Merged' };
  if (state === 'closed') return { state: 'closed', label: 'Closed' };
  if (state === 'draft') return { state: 'blocked', label: 'Draft' };
  const mergeable = String(item.mergeable || '').toUpperCase();
  const status = String(item.mergeStateStatus || '').toUpperCase();
  if (mergeable === 'CONFLICTING' || status === 'DIRTY') return { state: 'conflict', label: 'Conflicts' };
  if (status === 'BLOCKED' || status === 'BEHIND') return { state: 'blocked', label: status === 'BEHIND' ? 'Behind' : 'Blocked' };
  if (status === 'UNSTABLE') return { state: 'blocked', label: 'Checks failing' };
  if (status === 'CLEAN' || status === 'HAS_HOOKS') return { state: 'ready', label: 'Ready' };
  if (mergeable === 'MERGEABLE') return { state: 'ready', label: 'Ready' };
  return { state: 'unknown', label: 'Unknown' };
}

function loginOf(user) {
  if (!user) return '';
  if (typeof user === 'string') return user;
  return user.login || user.name || '';
}

function labelNames(labels) {
  return (Array.isArray(labels) ? labels : []).map((l) => (l && typeof l === 'object' ? l.name : l)).filter(Boolean).map(String);
}

/** Requested reviewers (PENDING) + the latest review per author, as the row contract's `reviewers`. */
function reviewersOf(item) {
  const out = [];
  for (const r of item.latestReviews || []) {
    const name = loginOf(r.author);
    if (name) out.push({ name, status: String(r.state || 'COMMENTED').toUpperCase() });
  }
  for (const u of item.reviewRequests || []) {
    const name = loginOf(u);
    if (name && !out.some((r) => r.name === name)) out.push({ name, status: 'PENDING' });
  }
  return out;
}

function repoLabel(repo) {
  return `${repo.owner}/${repo.name}`;
}

/** `gh pr list --json` item → row contract (`source: 'github'`, `type: 'pr'`). */
function mapPullRequest(item, repo) {
  const raw = String(item.state || '').toLowerCase();
  const state = raw === 'merged' || item.mergedAt ? 'merged' : raw === 'closed' ? 'closed' : item.isDraft ? 'draft' : 'open';
  return {
    source: 'github',
    id: `github:${repoLabel(repo)}:pr:${item.number}`,
    type: 'pr',
    number: Number(item.number),
    title: String(item.title || ''),
    state,
    url: String(item.url || ''),
    repo: { owner: repo.owner, name: repo.name, label: repoLabel(repo) },
    author: loginOf(item.author) || null,
    updatedAt: String(item.updatedAt || ''),
    labels: labelNames(item.labels),
    branch: String(item.headRefName || ''),
    baseBranch: String(item.baseRefName || ''),
    headSha: item.headRefOid || undefined,
    additions: typeof item.additions === 'number' ? item.additions : undefined,
    deletions: typeof item.deletions === 'number' ? item.deletions : undefined,
    changedFiles: typeof item.changedFiles === 'number' ? item.changedFiles : undefined,
    reviewDecision: item.reviewDecision ? String(item.reviewDecision).toUpperCase() : null,
    reviewers: reviewersOf(item),
    assignees: (item.assignees || []).map(loginOf).filter(Boolean),
    checks: summarizeChecks(item.statusCheckRollup),
    merge: mergeState(item, state),
    jiraKey: null,
  };
}

/** `search/issues` REST item → row contract (`type: 'issue'`). */
function mapIssue(item, repo) {
  return {
    source: 'github',
    id: `github:${repoLabel(repo)}:issue:${item.number}`,
    type: 'issue',
    number: Number(item.number),
    title: String(item.title || ''),
    state: String(item.state || 'open').toLowerCase() === 'closed' ? 'closed' : 'open',
    stateReason: item.state_reason || null,
    url: String(item.html_url || item.url || ''),
    repo: { owner: repo.owner, name: repo.name, label: repoLabel(repo) },
    author: loginOf(item.user || item.author) || null,
    updatedAt: String(item.updated_at || item.updatedAt || ''),
    labels: labelNames(item.labels),
    assignees: (item.assignees || []).map(loginOf).filter(Boolean),
    reviewers: [],
    checks: null,
    merge: null,
    jiraKey: null,
  };
}

/** Human message for a gh failure; keeps the class visible (auth, rate limit, missing repo) and trims gh's noise. */
function classifyGhError(err) {
  const text = String((err && (err.stderr || err.message)) || err || '').trim();
  if (/rate limit/i.test(text)) return 'GitHub rate limit reached — try again in a minute';
  if (/not logged in|gh auth login|authentication required|HTTP 401/i.test(text)) return 'Not signed in to GitHub — run `gh auth login`';
  if (/Could not resolve to a Repository|HTTP 404|Not Found/i.test(text)) return 'Repository not found or no access';
  if (/HTTP 403/i.test(text)) return 'GitHub denied the request (403)';
  if (err && err.code === 'ENOENT') return 'The gh CLI is not installed';
  return text.split('\n')[0] || 'GitHub request failed';
}

function ghExec(execFileImpl, args, options) {
  return new Promise((resolve, reject) => {
    execFileImpl('gh', args, { maxBuffer: 32 * 1024 * 1024, ...(options || {}) }, (err, stdout, stderr) => {
      if (err) {
        err.stderr = stderr;
        return reject(err);
      }
      resolve(String(stdout || ''));
    });
  });
}

function parseJsonArray(text) {
  const trimmed = text.trim();
  if (!trimmed) return [];
  const parsed = JSON.parse(trimmed);
  return Array.isArray(parsed) ? parsed : [];
}

async function settledInt(promise) {
  try {
    const n = parseInt((await promise).trim(), 10);
    return Number.isFinite(n) ? n : null;
  } catch {
    return null;
  }
}

/** One repo, one page: issues + PRs by scope, merged newest-number first; per-kind failures become `errors`. */
async function listRepoPage({ repo, query, page, limit, execFileImpl }) {
  const wantIssues = query.scope !== 'pr';
  const wantPrs = query.scope !== 'issue';
  const pr = wantPrs ? prListArgs({ repo, query, page, limit }) : null;
  const [issuesRes, prsRes, issueCount, prCount] = await Promise.all([
    wantIssues ? ghExec(execFileImpl, issueSearchArgs({ repo, query, page, limit })).then(parseJsonArray, (e) => ({ error: e })) : [],
    pr ? ghExec(execFileImpl, pr.args).then((out) => parseJsonArray(out).slice(pr.offset, pr.offset + limit), (e) => ({ error: e })) : [],
    wantIssues ? settledInt(ghExec(execFileImpl, countArgs({ repo, query, kind: 'issue' }))) : 0,
    wantPrs ? settledInt(ghExec(execFileImpl, countArgs({ repo, query, kind: 'pr' }))) : 0,
  ]);
  const errors = [];
  const items = [];
  if (Array.isArray(issuesRes)) items.push(...issuesRes.map((i) => mapIssue(i, repo)));
  else errors.push({ repo: repoLabel(repo), kind: 'issues', message: classifyGhError(issuesRes.error) });
  if (Array.isArray(prsRes)) items.push(...prsRes.map((p) => mapPullRequest(p, repo)));
  else errors.push({ repo: repoLabel(repo), kind: 'pull requests', message: classifyGhError(prsRes.error) });
  items.sort((a, b) => b.number - a.number);
  const pages = (n, got) => (n == null ? (got.length >= limit ? page + 1 : page) : Math.max(1, Math.ceil(n / limit)));
  const totalPages = Math.max(wantIssues ? pages(issueCount, Array.isArray(issuesRes) ? issuesRes : []) : 1, wantPrs ? pages(prCount, Array.isArray(prsRes) ? prsRes : []) : 1);
  return { items, errors, totalPages };
}

/** Fans one page out across `repos`; cross-repo rows sort by updatedAt and cap at CROSS_REPO_DISPLAY_LIMIT. */
async function listWorkItems({ repos, query, page = 1, limit = PER_REPO_FETCH_LIMIT, execFileImpl }) {
  const run = execFileImpl || require('node:child_process').execFile;
  const results = await Promise.all(repos.map((repo) => listRepoPage({ repo, query, page, limit, execFileImpl: run })));
  const items = results.flatMap((r) => r.items);
  if (repos.length > 1) items.sort((a, b) => Date.parse(b.updatedAt || 0) - Date.parse(a.updatedAt || 0));
  return {
    items: items.slice(0, CROSS_REPO_DISPLAY_LIMIT),
    errors: results.flatMap((r) => r.errors),
    totalPages: Math.max(1, ...results.map((r) => r.totalPages)),
    failedRepos: results.filter((r) => r.errors.length && !r.items.length).length,
  };
}

/** `{ok, login}` from `gh api user`; `{ok:false, message}` when gh is missing or signed out. */
async function authStatus(execFileImpl) {
  const run = execFileImpl || require('node:child_process').execFile;
  try {
    const login = (await ghExec(run, ['api', 'user', '--jq', '.login'])).trim();
    return login ? { ok: true, login } : { ok: false, message: 'gh returned no user' };
  } catch (err) {
    return { ok: false, message: classifyGhError(err) };
  }
}

module.exports = {
  PER_REPO_FETCH_LIMIT,
  CROSS_REPO_DISPLAY_LIMIT,
  PR_LIST_FIELDS,
  repoFromRemote,
  parseRepoSlug,
  issueSearchArgs,
  prListArgs,
  countArgs,
  summarizeChecks,
  mergeState,
  mapPullRequest,
  mapIssue,
  classifyGhError,
  listWorkItems,
  authStatus,
};
