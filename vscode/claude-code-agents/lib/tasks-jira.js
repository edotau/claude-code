'use strict';
// Jira Data Center adapter for the Tasks page: turns a parsed task query into JQL, then shapes
// lib/jira-client.js search results into the row contract lib/tasks-github.js/tasks-bitbucket.js produce.

/** Quotes a JQL string literal, escaping embedded `"` and `\`. */
function quoteValue(value) {
  return `"${String(value).replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
}

/** Splits freeText on whitespace outside double quotes, keeping quoted segments intact. */
function tokenizeFreeText(freeText) {
  return (String(freeText || '').match(/"[^"]*"|\S+/g) || []).map((t) => t);
}

function stripQuotes(value) {
  return value.length >= 2 && value.startsWith('"') && value.endsWith('"') ? value.slice(1, -1) : value;
}

/** Parses the query's free text into `project:`/`status:`/bare-key clauses, leftovers join into `text ~`. */
function freeTextClauses(freeText) {
  const clauses = [];
  const textWords = [];
  for (const token of tokenizeFreeText(freeText)) {
    const projectMatch = /^project:(.+)$/i.exec(token);
    const statusMatch = /^status:(.+)$/i.exec(token);
    if (projectMatch) {
      clauses.push({ kind: 'project', jql: `project = ${stripQuotes(projectMatch[1]).toUpperCase()}` });
      continue;
    }
    if (statusMatch) {
      clauses.push({ kind: 'status', jql: `status = ${quoteValue(stripQuotes(statusMatch[1]))}` });
      continue;
    }
    if (/^[A-Z][A-Z0-9_]+-\d+$/.test(stripQuotes(token))) {
      clauses.push({ kind: 'key', jql: `key = ${stripQuotes(token)}` });
      continue;
    }
    textWords.push(stripQuotes(token));
  }
  if (textWords.length) clauses.push({ kind: 'text', jql: `text ~ ${quoteValue(textWords.join(' '))}` });
  return clauses;
}

/** Builds JQL from the `parseTaskQuery` shape (lib/task-query.js); always ends `ORDER BY updated DESC`. */
function toJql(parsedQuery, opts) {
  const query = parsedQuery || {};
  const clauses = [];
  if (query.assignee) clauses.push(query.assignee === '@me' ? 'assignee = currentUser()' : `assignee = ${quoteValue(query.assignee)}`);
  if (query.author) clauses.push(query.author === '@me' ? 'reporter = currentUser()' : `reporter = ${quoteValue(query.author)}`);
  if (query.state === 'closed') clauses.push('statusCategory = Done');
  else if (query.state !== 'all' && query.state !== 'merged') clauses.push('statusCategory != Done');
  if ((query.labels || []).length) clauses.push(`labels in (${query.labels.map(quoteValue).join(', ')})`);
  const textClauses = freeTextClauses(query.freeText);
  const hasProjectClause = textClauses.some((c) => c.kind === 'project');
  clauses.push(...textClauses.map((c) => c.jql));
  const projects = (opts && opts.projects) || [];
  if (!hasProjectClause && projects.length) clauses.push(`project in (${projects.join(', ')})`);
  return clauses.length ? `${clauses.join(' AND ')} ORDER BY updated DESC` : 'ORDER BY updated DESC';
}

/** Jira issue (search/issue response) → row contract (`source: 'jira'`, `type: 'issue'`); tolerates missing nested fields. */
function mapIssue(issue, opts) {
  const { baseUrl } = opts || {};
  const fields = issue.fields || {};
  const key = issue.key;
  const url = `${(baseUrl || '').replace(/\/$/, '')}/browse/${key}`;
  const status = fields.status || {};
  const statusCategory = status.statusCategory || {};
  const project = fields.project || {};
  const assignee = fields.assignee || null;
  return {
    source: 'jira',
    id: `jira:${key}`,
    type: 'issue',
    number: key,
    key,
    title: fields.summary || '',
    state: statusCategory.key === 'done' ? 'closed' : 'open',
    status: { name: status.name || '', category: statusCategory.key || '' },
    url,
    repo: { owner: project.key || '', name: project.key || '', label: project.key || '' },
    author: (fields.reporter && fields.reporter.displayName) || null,
    assignees: assignee ? [assignee.displayName] : [],
    assignee: assignee
      ? { name: assignee.name, displayName: assignee.displayName, avatarUrl: (assignee.avatarUrls && assignee.avatarUrls['24x24']) || null }
      : null,
    priority: fields.priority ? { name: fields.priority.name, iconUrl: fields.priority.iconUrl || null } : null,
    issueType: fields.issuetype
      ? { name: fields.issuetype.name, iconUrl: fields.issuetype.iconUrl || null, subtask: !!fields.issuetype.subtask }
      : null,
    labels: fields.labels || [],
    updatedAt: fields.updated || '',
    createdAt: fields.created || '',
    reviewers: [],
    checks: null,
    merge: null,
    jiraKey: key,
    jiraUrl: url,
  };
}

/** One page of Jira rows for `query`: builds JQL, searches, shapes to the row contract. */
async function list({ client, query, projects, page = 1, limit = 36, baseUrl }) {
  const jql = toJql(query, { projects });
  const result = await client.search(jql, { startAt: (page - 1) * limit, maxResults: limit });
  const total = result.total || 0;
  return {
    items: (result.issues || []).map((issue) => mapIssue(issue, { baseUrl })),
    errors: [],
    totalPages: Math.max(1, Math.ceil(total / limit)),
    failedRepos: 0,
    total,
  };
}

module.exports = { toJql, mapIssue, list };
