'use strict';
// Bitbucket Data Center client, read-only. Ported from claude-orcastra's src/main/bitbucket
// (datacenter-repository-ref.ts, datacenter-client.ts, datacenter-pull-request-mappers.ts), plain JS,
// no dependencies — fetchImpl/execFileImpl are injectable seams for tests.

const MAX_CONCURRENT_BUILD_FETCHES = 6;

/** "ACME/acme-portal-frontend" style ref from an ssh/scp/https Data Center remote on `host`. */
function parseRepoRef(remoteUrl, host) {
  const trimmed = (remoteUrl || '').trim();
  const hostLower = (host || '').toLowerCase();
  const scpLike = trimmed.match(/^(?:[^@/]+@)?([^:/]+):(?!\/\/)([^\s]+)$/);
  if (scpLike && !trimmed.includes('://')) {
    if (scpLike[1].toLowerCase() !== hostLower) return null;
    return parseProjectRepoPath(scpLike[2], false, hostLower);
  }
  let url;
  try {
    url = new URL(trimmed);
  } catch {
    return null;
  }
  if (url.hostname.toLowerCase() !== hostLower) return null;
  const requireScm = url.protocol === 'http:' || url.protocol === 'https:';
  return parseProjectRepoPath(url.pathname, requireScm, hostLower);
}

function decodeSegment(value) {
  try {
    return decodeURIComponent(value);
  } catch {
    return value;
  }
}

/** Data Center paths are `{project}/{repo}`, behind `/scm/` over HTTPS; the REST API wants the project KEY uppercase. */
function parseProjectRepoPath(pathname, requireScm, host) {
  const parts = pathname
    .replace(/\/+$/, '')
    .replace(/\.git$/i, '')
    .split('/')
    .map((part) => part.trim())
    .filter(Boolean);
  // scp-like `git@host:7999/PROJ/repo.git` carries a leading port segment; drop it before matching.
  if (/^\d+$/.test(parts[0] || '')) parts.shift();
  const hasScm = (parts[0] || '').toLowerCase() === 'scm';
  if (requireScm && !hasScm) return null;
  const tail = hasScm ? parts.slice(1) : parts;
  if (tail.length !== 2) return null;
  const [project, repo] = tail.map(decodeSegment);
  return project && repo ? { host, project: project.toUpperCase(), repo } : null;
}

function mapPullRequestState(state) {
  switch ((state || '').trim().toUpperCase()) {
    case 'MERGED':
      return 'merged';
    case 'DECLINED':
    case 'SUPERSEDED':
      return 'declined';
    case 'OPEN':
    default:
      return 'open';
  }
}

/** Folds Jenkins/build-status values to the row contract's build.state: failed > in_progress > success > unknown. */
function foldBuildStatus(values) {
  const list = values || [];
  const states = list.map((v) => (v.state || '').trim().toUpperCase());
  const url = list.find((v) => (v.state || '').trim().toUpperCase() !== 'SUCCESSFUL')?.url || list[0]?.url;
  const counts = {
    successful: states.filter((s) => s === 'SUCCESSFUL').length,
    failed: states.filter((s) => s === 'FAILED' || s === 'STOPPED' || s === 'ERROR').length,
    in_progress: states.filter((s) => s === 'INPROGRESS' || s === 'PENDING').length,
  };
  let state = 'unknown';
  if (states.length === 0) state = 'unknown';
  else if (counts.failed > 0) state = 'failed';
  else if (counts.in_progress > 0) state = 'in_progress';
  else if (states.every((s) => s === 'SUCCESSFUL')) state = 'success';
  return {
    state,
    successful: counts.successful,
    failed: counts.failed,
    in_progress: counts.in_progress,
    ...(state !== 'success' && url ? { url } : {}),
  };
}

/** Maps a raw Data Center pull-request + its folded build status to the tasks-model row contract. */
function mapPullRequest(raw, build) {
  const reviewers = (raw.reviewers || []).map((r) => ({
    name: (r.user && (r.user.displayName || r.user.name)) || '',
    status: r.status || (r.approved ? 'APPROVED' : 'UNAPPROVED'),
  }));
  return {
    id: raw.id,
    title: raw.title || '',
    state: mapPullRequestState(raw.state),
    author: (raw.author && raw.author.user && (raw.author.user.displayName || raw.author.user.name)) || '',
    from_branch: (raw.fromRef && raw.fromRef.displayId) || '',
    to_branch: (raw.toRef && raw.toRef.displayId) || '',
    url: (raw.links && raw.links.self && raw.links.self[0] && raw.links.self[0].href) || '',
    reviewers,
    updated: typeof raw.updatedDate === 'number' ? new Date(raw.updatedDate).toISOString() : '',
    build: build || { state: 'unknown', successful: 0, failed: 0, in_progress: 0 },
  };
}

/** Data Center lists newest first; an open PR outranks any newer merged/declined one. */
function pickBranchPullRequest(values) {
  const list = values || [];
  return list.find((raw) => mapPullRequestState(raw.state) === 'open') || list[0] || null;
}

function headSha(raw) {
  return raw && raw.fromRef && raw.fromRef.latestCommit ? String(raw.fromRef.latestCommit).trim() : undefined;
}

/** Runs async `tasks` with at most `limit` in flight at once, preserving input order in the result. */
async function mapWithConcurrency(items, limit, task) {
  const results = new Array(items.length);
  let next = 0;
  async function worker() {
    while (next < items.length) {
      const i = next++;
      results[i] = await task(items[i], i);
    }
  }
  const workers = Array.from({ length: Math.min(limit, items.length) }, () => worker());
  await Promise.all(workers);
  return results;
}

function repoApiPath(ref) {
  return `/rest/api/1.0/projects/${encodeURIComponent(ref.project)}/repos/${encodeURIComponent(ref.repo)}`;
}

/** Server root that `/rest/...` paths hang off: tolerate a baseUrl that already carries `/rest/api/1.0`. */
function serverRoot(baseUrl) {
  return (baseUrl || '').replace(/\/+$/, '').replace(/\/rest(?:\/api\/[^/]+)?$/i, '');
}

function buildUrl(baseUrl, path, searchParams) {
  const url = new URL(`${serverRoot(baseUrl)}${path}`);
  for (const [key, value] of Object.entries(searchParams || {})) {
    url.searchParams.set(key, value);
  }
  return url.toString();
}

/** Creates a Data Center REST client bound to `baseUrl`/`token`; `fetchImpl` defaults to global fetch. */
function createClient(opts) {
  const { baseUrl, token, fetchImpl } = opts || {};
  const doFetch = fetchImpl || fetch;

  async function requestJson(path, searchParams) {
    const response = await doFetch(buildUrl(baseUrl, path, searchParams), {
      headers: { Accept: 'application/json', Authorization: `Bearer ${token}` },
    });
    if (!response.ok) {
      const err = new Error(`Bitbucket request failed: HTTP ${response.status}`);
      err.status = response.status;
      throw err;
    }
    return response.json();
  }

  async function buildStatus(sha) {
    if (!sha) return foldBuildStatus([]);
    const page = await requestJson(`/rest/build-status/1.0/commits/${encodeURIComponent(sha)}`, { limit: '100' });
    return foldBuildStatus((page && page.values) || []);
  }

  async function pullRequestForBranch(ref, branch) {
    const page = await requestJson(`${repoApiPath(ref)}/pull-requests`, {
      at: `refs/heads/${branch}`,
      direction: 'OUTGOING',
      state: 'ALL',
      limit: '25',
    });
    const raw = pickBranchPullRequest((page && page.values) || []);
    if (!raw) return null;
    return mapPullRequest(raw, await buildStatus(headSha(raw)));
  }

  /** Raw (unmapped) dashboard PRs — pass through `listWithBuilds` to get row-contract shapes with builds. */
  async function dashboard(role, state) {
    const page = await requestJson('/rest/api/1.0/dashboard/pull-requests', {
      role,
      state: state || 'OPEN',
      limit: '50',
    });
    return (page && page.values) || [];
  }

  /** Raw (unmapped) PRs of one repo, newest first; `state` is OPEN|MERGED|DECLINED|ALL (default OPEN). */
  async function pullRequests(ref, state) {
    const page = await requestJson(`${repoApiPath(ref)}/pull-requests`, { state: state || 'OPEN', limit: '50' });
    return (page && page.values) || [];
  }

  /** Maps raw PRs to row-contract shapes, fetching each one's build status (by its from-branch head) concurrently. */
  async function listWithBuilds(rawRows) {
    return mapWithConcurrency(rawRows, MAX_CONCURRENT_BUILD_FETCHES, async (raw) =>
      mapPullRequest(raw, await buildStatus(headSha(raw)))
    );
  }

  async function doctor() {
    let response;
    try {
      response = await doFetch(buildUrl(baseUrl, '/rest/api/1.0/projects', { limit: '1' }), {
        headers: { Accept: 'application/json', Authorization: `Bearer ${token}` },
      });
    } catch (err) {
      return { ok: false, reason: 'unreachable', message: err.message };
    }
    if (response.status === 401 || response.status === 403) {
      return { ok: false, reason: 'rejected' };
    }
    if (!response.ok) {
      return { ok: false, reason: 'unreachable' };
    }
    const username = (response.headers.get('x-ausername') || '').trim();
    return username ? { ok: true, user: { username } } : { ok: false, reason: 'rejected' };
  }

  return { pullRequestForBranch, dashboard, pullRequests, buildStatus, listWithBuilds, doctor };
}

/** First non-empty token file among the skill's conventional locations ($BITBUCKET_TOKEN_FILE, ~/.ssh/bitbucket-ci-pat). */
function tokenFromFiles(env, readFileImpl) {
  const readFile = readFileImpl || ((p) => require('node:fs').readFileSync(p, 'utf8'));
  const home = env.HOME || require('node:os').homedir();
  const candidates = [env.BITBUCKET_TOKEN_FILE, require('node:path').join(home, '.ssh', 'bitbucket-ci-pat')].filter(Boolean);
  for (const p of candidates) {
    try {
      const v = String(readFile(p)).trim();
      if (v) return v;
    } catch {
      // absent file: try the next location
    }
  }
  return null;
}

/** Resolves BITBUCKET_TOKEN: `<binary> vault get` (NAME=value), then env, then the token files. The caller caches/invalidates it. */
function resolveToken(opts) {
  const { binary, env, execFileImpl, readFileImpl } = opts || {};
  const runExecFile = execFileImpl || require('node:child_process').execFile;
  const environment = env || process.env;
  return new Promise((resolve, reject) => {
    runExecFile(binary, ['vault', 'get', 'BITBUCKET_TOKEN'], (err, stdout) => {
      const fromBinary = err ? null : stdout.toString().trim().replace(/^BITBUCKET_TOKEN=/, '');
      if (fromBinary) return resolve(fromBinary);
      if (environment.BITBUCKET_TOKEN) return resolve(environment.BITBUCKET_TOKEN);
      const fromFile = tokenFromFiles(environment, readFileImpl);
      if (fromFile) return resolve(fromFile);
      reject(new Error('no token: BITBUCKET_TOKEN is not set'));
    });
  });
}

/** Resolves the Data Center repo ref for `dir`'s `origin` remote against `host`, or null off that host. */
function repoRefFromDir(dir, host, execFileImpl) {
  const runExecFile = execFileImpl || require('node:child_process').execFile;
  return new Promise((resolve) => {
    runExecFile('git', ['remote', 'get-url', 'origin'], { cwd: dir }, (err, stdout) => {
      if (err) return resolve(null);
      resolve(parseRepoRef(stdout.toString().trim(), host));
    });
  });
}

module.exports = {
  parseRepoRef,
  mapPullRequestState,
  foldBuildStatus,
  mapPullRequest,
  pickBranchPullRequest,
  createClient,
  resolveToken,
  repoRefFromDir,
};
