'use strict';
// Unit tests for the Bitbucket Data Center client: parseRepoRef, createClient's HTTP surface,
// mapPullRequest, foldBuildStatus and resolveToken, against test/fixtures/bitbucket/ (real
// Data Center shapes).
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {
  parseRepoRef,
  createClient,
  mapPullRequest,
  foldBuildStatus,
  resolveToken,
  repoRefFromDir,
} = require('../lib/bitbucket-client');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'bitbucket');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

const HOST = 'bitbucket.biscrum.com';

// -- parseRepoRef --

test('parseRepoRef: ssh URL with an explicit port', () => {
  assert.deepEqual(
    parseRepoRef('ssh://git@bitbucket.biscrum.com:7999/digitalbtd/digitalbtd-portal-frontend.git', HOST),
    { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' }
  );
});

test('parseRepoRef: https /scm/ clone URL with .git suffix', () => {
  assert.deepEqual(
    parseRepoRef('https://bitbucket.biscrum.com/scm/digitalbtd/digitalbtd-portal-frontend.git', HOST),
    { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' }
  );
});

test('parseRepoRef: scp-like git@host:port/project/repo.git (no scheme)', () => {
  assert.deepEqual(
    parseRepoRef('git@bitbucket.biscrum.com:7999/digitalbtd/digitalbtd-portal-frontend.git', HOST),
    { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' }
  );
});

test('parseRepoRef: scp-like git@host:project/repo.git (no port, no scheme)', () => {
  assert.deepEqual(
    parseRepoRef('git@bitbucket.biscrum.com:digitalbtd/digitalbtd-portal-frontend.git', HOST),
    { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' }
  );
});

test('parseRepoRef: http (no s) /scm/ clone URL requires /scm/ too', () => {
  assert.equal(
    parseRepoRef('http://bitbucket.biscrum.com/digitalbtd/digitalbtd-portal-frontend.git', HOST),
    null
  );
});

test('parseRepoRef: a GitHub remote does not match the Bitbucket host', () => {
  assert.equal(parseRepoRef('https://github.com/edotau/claude-code.git', HOST), null);
});

// -- mapPullRequest --

test('mapPullRequest: an open PR maps id/title/state/branches/reviewers/updated, build defaults to unknown', () => {
  const raw = fixture('pr-by-id-open');
  const row = mapPullRequest(raw, null);
  assert.equal(row.id, 1662);
  assert.equal(row.title, 'add excel for gdb mart dl');
  assert.equal(row.state, 'open');
  assert.equal(row.from_branch, 'DIGITALBTD-10051/frontend');
  assert.equal(row.to_branch, 'master');
  assert.equal(row.author, 'Dev 1');
  assert.deepEqual(row.reviewers, [
    { name: 'Dev 2', status: 'APPROVED' },
    { name: 'Dev 3', status: 'UNAPPROVED' },
    { name: 'Dev 4', status: 'UNAPPROVED' },
  ]);
  assert.equal(row.updated, new Date(raw.updatedDate).toISOString());
  assert.deepEqual(row.build, { state: 'unknown', successful: 0, failed: 0, in_progress: 0 });
});

test('mapPullRequest: a merged PR maps state to "merged"', () => {
  const raw = fixture('pr-by-id-merged');
  const row = mapPullRequest(raw, null);
  assert.equal(row.state, 'merged');
  assert.equal(row.from_branch, raw.fromRef.displayId);
  assert.equal(row.to_branch, raw.toRef.displayId);
});

test('mapPullRequest: a passed build (already folded) is attached as-is', () => {
  const raw = fixture('pr-by-id-open');
  const build = { state: 'success', successful: 1, failed: 0, in_progress: 0 };
  const row = mapPullRequest(raw, build);
  assert.deepEqual(row.build, build);
});

// -- foldBuildStatus --

test('foldBuildStatus: a single SUCCESSFUL status folds to success with no url', () => {
  const { values } = fixture('build-status-open');
  const folded = foldBuildStatus(values);
  assert.equal(folded.state, 'success');
  assert.equal(folded.successful, 1);
  assert.equal('url' in folded, false);
});

test('foldBuildStatus: any FAILED status folds to failed, reporting the first non-success url', () => {
  const values = [
    { state: 'SUCCESSFUL', url: 'https://jenkins.example.com/job/a/1/' },
    { state: 'FAILED', url: 'https://jenkins.example.com/job/b/1/' },
  ];
  const folded = foldBuildStatus(values);
  assert.equal(folded.state, 'failed');
  assert.equal(folded.url, 'https://jenkins.example.com/job/b/1/');
});

test('foldBuildStatus: an INPROGRESS status with no failures folds to in_progress', () => {
  const values = [
    { state: 'SUCCESSFUL', url: 'https://jenkins.example.com/job/a/1/' },
    { state: 'INPROGRESS', url: 'https://jenkins.example.com/job/c/1/' },
  ];
  const folded = foldBuildStatus(values);
  assert.equal(folded.state, 'in_progress');
  assert.equal(folded.url, 'https://jenkins.example.com/job/c/1/');
});

test('foldBuildStatus: no values folds to unknown with no url', () => {
  assert.equal(foldBuildStatus([]).state, 'unknown');
  assert.equal(foldBuildStatus(undefined).state, 'unknown');
  assert.equal('url' in foldBuildStatus([]), false);
});

// -- createClient: HTTP surface --

function fakeFetch(routes) {
  const calls = [];
  const fetchImpl = async (url, opts) => {
    calls.push({ url, opts });
    const parsed = new URL(url);
    for (const [matcher, respond] of routes) {
      if (matcher(parsed)) return respond(parsed);
    }
    throw new Error(`fakeFetch: no route for ${url}`);
  };
  return { fetchImpl, calls };
}

function jsonResponse(status, body, headers) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: (name) => (headers || {})[name.toLowerCase()] ?? null },
    json: async () => body,
  };
}

const OPEN_SHA = '0fbb34a3eddde66e6fb4db0f2b2f0709f07b95c7';
const MERGED_SHA = '8b60f37cfd97c6d34342e1b9bca67014e4ad907b';

test('createClient.pullRequestForBranch: exact path + query on the pull-requests endpoint, Bearer auth', async () => {
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname.endsWith('/pull-requests'), () => jsonResponse(200, fixture('pr-for-branch'))],
    [(u) => u.pathname.includes(`/commits/${OPEN_SHA}`), () => jsonResponse(200, fixture('build-status-open'))],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const ref = { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' };
  const pr = await client.pullRequestForBranch(ref, 'DIGITALBTD-10051/frontend');

  const prCall = new URL(calls[0].url);
  assert.equal(prCall.pathname, '/rest/api/1.0/projects/DIGITALBTD/repos/digitalbtd-portal-frontend/pull-requests');
  assert.equal(prCall.searchParams.get('at'), 'refs/heads/DIGITALBTD-10051/frontend');
  assert.equal(prCall.searchParams.get('direction'), 'OUTGOING');
  assert.equal(prCall.searchParams.get('state'), 'ALL');
  assert.equal(calls[0].opts.headers.Authorization, 'Bearer tok');

  assert.equal(pr.id, 1662);
  assert.equal(pr.state, 'open');
  assert.equal(pr.build.state, 'success');
});

test('createClient.pullRequestForBranch: an empty page returns null without a build-status fetch', async () => {
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname.endsWith('/pull-requests'), () => jsonResponse(200, fixture('pr-for-branch-none'))],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const pr = await client.pullRequestForBranch({ host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' }, 'no-such-branch');
  assert.equal(pr, null);
  assert.equal(calls.length, 1);
});

test('createClient.buildStatus: exact path on /rest/build-status/1.0/commits/<sha>, folded', async () => {
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname.includes('commits'), () => jsonResponse(200, fixture('build-status-open'))],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const build = await client.buildStatus(OPEN_SHA);
  const url = new URL(calls[0].url);
  assert.equal(url.pathname, `/rest/build-status/1.0/commits/${OPEN_SHA}`);
  assert.equal(build.state, 'success');
});

test('createClient.doctor: ok=true with the username off X-AUSERNAME on a 200', async () => {
  const { fetchImpl } = fakeFetch([
    [() => true, () => jsonResponse(200, {}, { 'x-ausername': 'alice' })],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const result = await client.doctor();
  assert.deepEqual(result, { ok: true, user: { username: 'alice' } });
});

test('createClient.doctor: ok=false reason=rejected on a 401 (no throw)', async () => {
  const { fetchImpl } = fakeFetch([
    [() => true, () => jsonResponse(401, { message: 'Unauthorized' })],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'bad', fetchImpl });
  const result = await client.doctor();
  assert.deepEqual(result, { ok: false, reason: 'rejected' });
});

test('createClient.listWithBuilds: maps raw PRs and attaches each one\'s own build status by its from-branch head', async () => {
  const rawRows = [fixture('pr-by-id-open'), fixture('pr-by-id-merged')];
  const { fetchImpl } = fakeFetch([
    [(u) => u.pathname.includes(`/commits/${OPEN_SHA}`), () => jsonResponse(200, fixture('build-status-open'))],
    [(u) => u.pathname.includes(`/commits/${MERGED_SHA}`), () => jsonResponse(200, { values: [{ state: 'FAILED', url: 'https://x/2/' }] })],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const out = await client.listWithBuilds(rawRows);
  assert.equal(out[0].id, 1662);
  assert.equal(out[0].build.state, 'success');
  assert.equal(out[1].id, 1654);
  assert.equal(out[1].build.state, 'failed');
});

test('createClient.pullRequests: GET {repoApiPath}/pull-requests with state=OPEN (default) + limit=50, returns raw page.values', async () => {
  const rawOpen = fixture('pr-by-id-open');
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname.endsWith('/pull-requests'), () => jsonResponse(200, { values: [rawOpen], size: 1 })],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  const ref = { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' };
  const rows = await client.pullRequests(ref);

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/1.0/projects/DIGITALBTD/repos/digitalbtd-portal-frontend/pull-requests');
  assert.equal(url.searchParams.get('state'), 'OPEN');
  assert.equal(url.searchParams.get('limit'), '50');
  assert.equal(calls[0].opts.headers.Authorization, 'Bearer ' + 'tok');
  assert.deepEqual(rows, [rawOpen]);
});

test('createClient.dashboard: an explicit state is passed through; omitted state defaults to OPEN', async () => {
  const { fetchImpl, calls } = fakeFetch([
    [(u) => u.pathname.endsWith('/dashboard/pull-requests'), () => jsonResponse(200, { values: [] })],
  ]);
  const client = createClient({ baseUrl: `https://${HOST}`, token: 'tok', fetchImpl });
  await client.dashboard('AUTHOR', 'MERGED');
  assert.equal(new URL(calls[0].url).searchParams.get('state'), 'MERGED');
  await client.dashboard('REVIEWER');
  assert.equal(new URL(calls[1].url).searchParams.get('state'), 'OPEN');
});

// -- resolveToken (execFileImpl is Node callback-style: (binary, args, cb(err, stdout))) --

test('resolveToken: trims the binary\'s stdout', async () => {
  const execFileImpl = (binary, args, cb) => {
    assert.deepEqual(args, ['vault', 'get', 'BITBUCKET_TOKEN']);
    cb(null, 'BITBUCKET_TOKEN=obviously-fake-token\n');
  };
  const token = await resolveToken({ binary: 'claude-code', env: {}, execFileImpl });
  assert.equal(token.replace('obviously-fake-token', '[masked]'), '[masked]');
});

test('resolveToken: falls back to the env var when the binary fails', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const token = await resolveToken({ binary: 'claude-code', env: { BITBUCKET_TOKEN: 'env-tok' }, execFileImpl });
  assert.equal(token, 'env-tok');
});

test('resolveToken: neither the binary nor the env var rejects with "no token"', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  await assert.rejects(
    () => resolveToken({
      binary: 'claude-code', env: {}, execFileImpl,
      readFileImpl: () => { throw new Error('missing fixture file'); },
    }),
    /no token/i
  );
});

test('resolveToken: an empty binary stdout falls through to the env var, not an empty token', async () => {
  const execFileImpl = (binary, args, cb) => cb(null, '  \n');
  const token = await resolveToken({ binary: 'claude-code', env: { BITBUCKET_TOKEN: 'env-tok' }, execFileImpl });
  assert.equal(token, 'env-tok');
});

// -- repoRefFromDir --

test('repoRefFromDir: resolves the ref from `git remote get-url origin`\'s stdout', async () => {
  const execFileImpl = (bin, args, opts, cb) => cb(null, 'https://bitbucket.biscrum.com/scm/digitalbtd/digitalbtd-portal-frontend.git\n');
  const ref = await repoRefFromDir('/some/dir', HOST, execFileImpl);
  assert.deepEqual(ref, { host: HOST, project: 'DIGITALBTD', repo: 'digitalbtd-portal-frontend' });
});

test('repoRefFromDir: a failing git command resolves null, not a rejection', async () => {
  const execFileImpl = (bin, args, opts, cb) => cb(new Error('not a git repo'));
  const ref = await repoRefFromDir('/some/dir', HOST, execFileImpl);
  assert.equal(ref, null);
});
