'use strict';
// Jira Data Center REST client (/rest/api/2, Bearer PAT), read/write. Plain JS, no dependencies —
// fetchImpl/execFileImpl/readFileImpl are injectable seams for tests. Mirrors lib/bitbucket-client.js's shape.

const DEFAULT_FIELDS = ['summary', 'status', 'issuetype', 'priority', 'assignee', 'reporter', 'labels', 'updated', 'created', 'project'];
const REQUEST_TIMEOUT_MS = 15000;

function serverRoot(baseUrl) {
  return (baseUrl || '').replace(/\/+$/, '');
}

/** Non-2xx → Error with `.status` set, message built from the body's errorMessages[]/errors{} when JSON parses. */
async function throwForStatus(response) {
  let body = null;
  try {
    body = await response.json();
  } catch {
    // non-JSON error body: fall through to the generic message
  }
  const messages = [];
  if (body && Array.isArray(body.errorMessages)) messages.push(...body.errorMessages);
  if (body && body.errors && typeof body.errors === 'object') messages.push(...Object.values(body.errors));
  const err = new Error(messages.length ? messages.join('; ') : `Jira HTTP ${response.status}`);
  err.status = response.status;
  throw err;
}

/** Creates a Jira Data Center REST client bound to `baseUrl`/`token`; `fetchImpl` defaults to global fetch. */
function createClient(opts) {
  const { baseUrl, token, fetchImpl } = opts || {};
  const doFetch = fetchImpl || fetch;

  async function request(path, { method = 'GET', body } = {}) {
    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
    const headers = { Authorization: `Bearer ${token}`, Accept: 'application/json', 'User-Agent': 'claude-code-agents' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    try {
      const response = await doFetch(`${serverRoot(baseUrl)}${path}`, {
        method,
        headers,
        body: body !== undefined ? JSON.stringify(body) : undefined,
        signal: controller.signal,
      });
      if (response.status === 204) return { ok: true };
      if (!response.ok) await throwForStatus(response);
      return response.json();
    } finally {
      clearTimeout(timer);
    }
  }

  function search(jql, { startAt = 0, maxResults = 36, fields } = {}) {
    return request('/rest/api/2/search', { method: 'POST', body: { jql, startAt, maxResults, fields: fields || DEFAULT_FIELDS } });
  }

  function issue(key) {
    const fields = 'summary,description,status,issuetype,priority,assignee,reporter,labels,updated,created,project';
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}?fields=${fields}&expand=renderedFields`);
  }

  function comments(key, { maxResults = 50 } = {}) {
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}/comment?orderBy=created&maxResults=${maxResults}&expand=renderedBody`);
  }

  function addComment(key, text) {
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}/comment`, { method: 'POST', body: { body: text } });
  }

  /** Transitions with their screen fields (`expand=transitions.fields`) so required resolution/comment can be collected. */
  function transitions(key) {
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}/transitions?expand=transitions.fields`).then((body) => body.transitions || []);
  }

  /** `fields` (e.g. `{resolution: {id}}`) ride in the transition body; a comment goes through `update.comment`. */
  function transition(key, transitionId, comment, fields) {
    const body = { transition: { id: String(transitionId) } };
    if (fields && Object.keys(fields).length) body.fields = fields;
    if (typeof comment === 'string' && comment) body.update = { comment: [{ add: { body: comment } }] };
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}/transitions`, { method: 'POST', body });
  }

  /** `PUT /issue/{key}` with a raw `{fields, update}` body (summary, labels, priority…). */
  function update(key, body) {
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}`, { method: 'PUT', body });
  }

  function priorities() {
    return request('/rest/api/2/priority');
  }

  function assign(key, name) {
    return request(`/rest/api/2/issue/${encodeURIComponent(key)}/assignee`, { method: 'PUT', body: { name } });
  }

  function assignable(key, query) {
    return request(`/rest/api/2/user/assignable/search?issueKey=${encodeURIComponent(key)}&username=${encodeURIComponent(query)}&maxResults=20`);
  }

  async function myself() {
    try {
      const controller = new AbortController();
      const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
      let response;
      try {
        response = await doFetch(`${serverRoot(baseUrl)}/rest/api/2/myself`, {
          headers: { Authorization: `Bearer ${token}`, Accept: 'application/json', 'User-Agent': 'claude-code-agents' },
          signal: controller.signal,
        });
      } finally {
        clearTimeout(timer);
      }
      if (response.status === 401 || response.status === 403) return { ok: false, reason: 'rejected' };
      if (!response.ok) return { ok: false, reason: 'unreachable' };
      const user = await response.json();
      return { ok: true, user: { name: user.name, displayName: user.displayName, avatarUrl: (user.avatarUrls && user.avatarUrls['24x24']) || null } };
    } catch (err) {
      return { ok: false, reason: 'unreachable', message: err.message };
    }
  }

  return { search, issue, comments, addComment, transitions, transition, update, priorities, assign, assignable, myself };
}

/** First non-empty token file among the skill's conventional locations ($JIRA_TOKEN_FILE, ~/.ssh/atlassian-ci-pat, ~/.ssh/jira-ci-pat). */
function tokenFromFiles(env, readFileImpl) {
  const readFile = readFileImpl || ((p) => require('node:fs').readFileSync(p, 'utf8'));
  const home = env.HOME || require('node:os').homedir();
  const path = require('node:path');
  const candidates = [env.JIRA_TOKEN_FILE, path.join(home, '.ssh', 'atlassian-ci-pat'), path.join(home, '.ssh', 'jira-ci-pat')].filter(Boolean);
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

/** Resolves JIRA_TOKEN: `<binary> secrets get`, then env (JIRA_TOKEN, ATLASSIAN_TOKEN), then the token files. */
function resolveToken(opts) {
  const { binary, env, execFileImpl, readFileImpl } = opts || {};
  const runExecFile = execFileImpl || require('node:child_process').execFile;
  const environment = env || process.env;
  return new Promise((resolve, reject) => {
    runExecFile(binary, ['secrets', 'get', 'JIRA_TOKEN'], (err, stdout) => {
      const fromBinary = err ? null : stdout.toString().trim();
      if (fromBinary) return resolve(fromBinary);
      if (environment.JIRA_TOKEN) return resolve(environment.JIRA_TOKEN);
      if (environment.ATLASSIAN_TOKEN) return resolve(environment.ATLASSIAN_TOKEN);
      const fromFile = tokenFromFiles(environment, readFileImpl);
      if (fromFile) return resolve(fromFile);
      reject(new Error('no token: JIRA_TOKEN is not set'));
    });
  });
}

module.exports = { createClient, resolveToken };
