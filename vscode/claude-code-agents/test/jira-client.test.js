'use strict';
// Unit tests for the Jira REST client (lib/jira-client.js): exact path/method/body/headers for every
// call, non-2xx error shape, baseUrl normalization, and resolveToken's fallback chain.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createClient, resolveToken } = require('../lib/jira-client');

const FIXTURE_DIR = path.join(__dirname, 'fixtures', 'jira');
function fixture(name) {
  return JSON.parse(fs.readFileSync(path.join(FIXTURE_DIR, `${name}.json`), 'utf8'));
}

function jsonResponse(status, body) {
  if (status === 204) return new Response(null, { status: 204 });
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

function fakeFetch(routes) {
  const calls = [];
  const fetchImpl = async (url, init) => {
    calls.push({ url, init });
    for (const [matcher, respond] of routes) {
      const parsed = new URL(url);
      if (matcher(parsed, init)) return respond(parsed, init);
    }
    throw new Error(`fakeFetch: no route for ${url}`);
  };
  return { fetchImpl, calls };
}

const BASE = 'https://jira.corp.example';
const TOKEN = 'T';

function assertCommonHeaders(init) {
  assert.equal(init.headers.Authorization, 'Bearer T');
  assert.equal(init.headers.Accept, 'application/json');
  assert.equal(typeof init.headers['User-Agent'], 'string');
  assert.ok(init.headers['User-Agent'].length > 0);
  assert.equal(init.headers['User-Agent'].includes('Mozilla'), false);
}

// -- baseUrl normalization --

test('createClient: a trailing slash on baseUrl is stripped before building paths', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, fixture('myself'))]]);
  const client = createClient({ baseUrl: `${BASE}/`, token: TOKEN, fetchImpl });
  await client.myself();
  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/myself');
});

// -- search --

test('search: POST /rest/api/2/search with the exact JSON body', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, fixture('search'))]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const fields = ['summary', 'status'];
  await client.search('project = ACME', { startAt: 20, maxResults: 10, fields });

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/search');
  assert.equal(calls[0].init.method, 'POST');
  assertCommonHeaders(calls[0].init);
  assert.deepEqual(JSON.parse(calls[0].init.body), {
    jql: 'project = ACME',
    startAt: 20,
    maxResults: 10,
    fields,
  });
});

test('search: omitted fields falls back to a default field list (not undefined)', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, fixture('search'))]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.search('project = ACME');
  const body = JSON.parse(calls[0].init.body);
  assert.equal(body.startAt, 0);
  assert.ok(Array.isArray(body.fields) && body.fields.length > 0);
});

// -- issue --

test('issue: GET /rest/api/2/issue/KEY with fields + expand=renderedFields', async () => {
  const raw = fixture('search').issues[0];
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, raw)]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.issue('ACME-8139');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139');
  assert.ok(url.searchParams.get('fields').length > 0);
  assert.equal(url.searchParams.get('expand'), 'renderedFields');
  assert.ok(calls[0].init.method === undefined || calls[0].init.method === 'GET');
  assertCommonHeaders(calls[0].init);
});

// -- comments --

test('comments: GET …/comment?orderBy=created&maxResults=50&expand=renderedBody', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, { comments: [] })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.comments('ACME-8139');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139/comment');
  assert.equal(url.searchParams.get('orderBy'), 'created');
  assert.equal(url.searchParams.get('maxResults'), '50');
  assert.equal(url.searchParams.get('expand'), 'renderedBody');
  assertCommonHeaders(calls[0].init);
});

// -- addComment --

test('addComment: POST …/comment with {body: text} as plain text, never ADF', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, { id: '1' })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.addComment('ACME-8139', 'plain text comment');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139/comment');
  assert.equal(calls[0].init.method, 'POST');
  const parsedBody = JSON.parse(calls[0].init.body);
  assert.deepEqual(parsedBody, { body: 'plain text comment' });
  assertCommonHeaders(calls[0].init);
});

// teeth: this would catch a silent switch to ADF `{type:'doc', ...}` or a dropped Bearer header.
test('addComment TEETH: the recorded request body is plain text (not ADF) and carries the Bearer header', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, { id: '1' })]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.addComment('ACME-8139', 'hello');

  const parsedBody = JSON.parse(calls[0].init.body);
  assert.equal(typeof parsedBody.body, 'string');
  assert.equal(parsedBody.body, 'hello');
  assert.equal('type' in parsedBody, false); // no ADF doc wrapper
  assert.equal(calls[0].init.headers.Authorization, 'Bearer T');
});

// -- transitions --

test('transitions: GET …/transitions, unwrapped to the transitions array', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, fixture('transitions'))]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.transitions('ACME-8139');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139/transitions');
  assert.equal(url.searchParams.get('expand'), 'transitions.fields');
  assert.deepEqual(result, fixture('transitions').transitions);
});

// -- transition --

test('transition: POST …/transitions with {transition:{id}}, no comment', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(204, {})]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.transition('ACME-8139', '696880827');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139/transitions');
  assert.equal(calls[0].init.method, 'POST');
  assert.deepEqual(JSON.parse(calls[0].init.body), { transition: { id: '696880827' } });
});

test('transition: with a comment, POST body adds update.comment[0].add.body', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(204, {})]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.transition('ACME-8139', '696880827', 'closing this out');

  assert.deepEqual(JSON.parse(calls[0].init.body), {
    transition: { id: '696880827' },
    update: { comment: [{ add: { body: 'closing this out' } }] },
  });
});

// -- assign --

test('assign: PUT …/assignee with {name}', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(204, {})]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.assign('ACME-8139', 'user.one@example.com');

  const url = new URL(calls[0].url);
  assert.equal(url.pathname, '/rest/api/2/issue/ACME-8139/assignee');
  assert.equal(calls[0].init.method, 'PUT');
  assert.deepEqual(JSON.parse(calls[0].init.body), { name: 'user.one@example.com' });
});

// -- assignable --

test('assignable: GET /rest/api/2/user/assignable/search (or equivalent) carries the auth headers', async () => {
  const { fetchImpl, calls } = fakeFetch([[() => true, () => jsonResponse(200, [fixture('myself')])]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await client.assignable('ACME-8139');

  assertCommonHeaders(calls[0].init);
  assert.equal(calls.length, 1);
});

// -- myself --

test('myself: ok:true with {name, displayName, avatarUrl} on 200', async () => {
  const { fetchImpl } = fakeFetch([[() => true, () => jsonResponse(200, fixture('myself'))]]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.myself();
  assert.deepEqual(result, {
    ok: true,
    user: {
      name: 'user.one@example.com',
      displayName: 'User One',
      avatarUrl: 'https://example.invalid/avatar.png',
    },
  });
});

test('myself: ok:false reason:rejected on a 401', async () => {
  const { fetchImpl } = fakeFetch([[() => true, () => jsonResponse(401, { errorMessages: ['Unauthorized'] })]]);
  const client = createClient({ baseUrl: BASE, token: 'bad', fetchImpl });
  const result = await client.myself();
  assert.deepEqual(result, { ok: false, reason: 'rejected' });
});

test('myself: ok:false reason:unreachable when fetchImpl throws', async () => {
  const fetchImpl = async () => {
    throw new Error('ECONNREFUSED');
  };
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  const result = await client.myself();
  assert.equal(result.ok, false);
  assert.equal(result.reason, 'unreachable');
});

// -- non-2xx error shape --

test('a non-2xx response throws an Error with .status and errorMessages[0] in the message', async () => {
  const { fetchImpl } = fakeFetch([
    [() => true, () => jsonResponse(400, { errorMessages: ['Field summary is required.'] })],
  ]);
  const client = createClient({ baseUrl: BASE, token: TOKEN, fetchImpl });
  await assert.rejects(
    () => client.issue('ACME-0001'),
    (err) => {
      assert.equal(err.status, 400);
      assert.match(err.message, /Field summary is required\./);
      return true;
    }
  );
});

// -- resolveToken --

test('resolveToken: uses `vault get JIRA_TOKEN` via execFileImpl, argv exact, no shell', async () => {
  const calls = [];
  const execFileImpl = (binary, args, cb) => {
    calls.push({ binary, args });
    cb(null, 'JIRA_TOKEN=secret-tok\n');
  };
  const token = await resolveToken({ binary: 'claude-code', env: {}, execFileImpl });
  assert.equal(token, 'secret-tok');
  assert.equal(calls[0].binary, 'claude-code');
  assert.deepEqual(calls[0].args, ['vault', 'get', 'JIRA_TOKEN']);
});

test('resolveToken: falls back to env.JIRA_TOKEN when the binary fails', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const token = await resolveToken({ binary: 'claude-code', env: { JIRA_TOKEN: 'env-tok' }, execFileImpl });
  assert.equal(token, 'env-tok');
});

test('resolveToken: falls back to env.ATLASSIAN_TOKEN when JIRA_TOKEN is unset', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const token = await resolveToken({ binary: 'claude-code', env: { ATLASSIAN_TOKEN: 'atl-tok' }, execFileImpl });
  assert.equal(token, 'atl-tok');
});

test('resolveToken: falls back to the file named by env.JIRA_TOKEN_FILE', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const readFileImpl = (p) => {
    assert.equal(p, '/custom/token-file');
    return 'file-tok\n';
  };
  const token = await resolveToken({
    binary: 'claude-code',
    env: { JIRA_TOKEN_FILE: '/custom/token-file' },
    execFileImpl,
    readFileImpl,
  });
  assert.equal(token, 'file-tok');
});

test('resolveToken: falls back to ~/.ssh/atlassian-ci-pat', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const readFileImpl = (p) => {
    assert.equal(p, path.join('/home/tester', '.ssh', 'atlassian-ci-pat'));
    return 'pat-tok\n';
  };
  const token = await resolveToken({
    binary: 'claude-code',
    env: { HOME: '/home/tester' },
    execFileImpl,
    readFileImpl,
  });
  assert.equal(token, 'pat-tok');
});

test('resolveToken: an empty binary stdout falls through to env, not an empty token', async () => {
  const execFileImpl = (binary, args, cb) => cb(null, '  \n');
  const token = await resolveToken({ binary: 'claude-code', env: { JIRA_TOKEN: 'env-tok' }, execFileImpl });
  assert.equal(token, 'env-tok');
});

test('resolveToken: no source available rejects with "no token"', async () => {
  const execFileImpl = (binary, args, cb) => cb(new Error('not found'));
  const readFileImpl = () => {
    throw new Error('ENOENT');
  };
  await assert.rejects(
    () => resolveToken({ binary: 'claude-code', env: {}, execFileImpl, readFileImpl }),
    /no token/i
  );
});
