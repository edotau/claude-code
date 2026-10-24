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

const SORT_FIELDS = { key: 'key', title: 'summary', status: 'status', priority: 'priority', assignee: 'assignee', updated: 'updated', created: 'created' };

function orderBy(sort) {
  const field = sort && SORT_FIELDS[sort.field] ? SORT_FIELDS[sort.field] : 'updated';
  const dir = sort && String(sort.dir).toLowerCase() === 'asc' ? 'ASC' : 'DESC';
  return `ORDER BY ${field} ${dir}`;
}

/** Quick-filter + sprint clauses shared by the query builder and raw JQL. */
function extraClauses(opts) {
  const out = [];
  const statuses = (opts && opts.statuses) || [];
  if (statuses.length) out.push(`status in (${statuses.map(quoteValue).join(', ')})`);
  if (opts && opts.currentSprint) out.push('sprint in openSprints()');
  return out;
}

/** Orca's mayBeJql: `jql:` prefix or operator shapes mean the text is JQL, not words to match. */
function isRawJql(text) {
  const t = String(text || '').trim();
  if (!t) return false;
  if (/^jql:/i.test(t)) return true;
  return /( = | != | in \(| ~ | is empty| is null|\border by\b)/i.test(t);
}

/** Raw JQL as typed, with the quick filters AND-ed in and the sort applied (or `ORDER BY updated DESC` added). */
function rawJql(text, opts) {
  let body = String(text || '').trim().replace(/^jql:\s*/i, '');
  const m = /\border by\b/i.exec(body);
  let order = '';
  if (m) {
    order = body.slice(m.index).trim();
    body = body.slice(0, m.index).trim();
  }
  const extras = extraClauses(opts);
  if (extras.length) body = body ? `(${body}) AND ${extras.join(' AND ')}` : extras.join(' AND ');
  const finalOrder = opts && opts.sort ? orderBy(opts.sort) : order || 'ORDER BY updated DESC';
  return `${body ? `${body} ` : ''}${finalOrder}`;
}

/** Builds JQL from the `parseTaskQuery` shape (lib/task-query.js); the sort defaults to `ORDER BY updated DESC`. */
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
  clauses.push(...extraClauses(opts));
  const order = orderBy(opts && opts.sort);
  return clauses.length ? `${clauses.join(' AND ')} ${order}` : order;
}

const CATEGORY_RANK = { new: 0, indeterminate: 1, done: 2 };

/** Statuses present in the loaded rows (Orca's "statuses come from loaded issues"), category order then name. */
function statusCatalog(rows) {
  const seen = new Map();
  for (const r of rows || []) {
    const st = r.status || {};
    if (!st.name) continue;
    const e = seen.get(st.name) || { name: st.name, category: st.category || '', count: 0 };
    e.count += 1;
    seen.set(st.name, e);
  }
  return [...seen.values()].sort((a, b) => (CATEGORY_RANK[a.category] ?? 9) - (CATEGORY_RANK[b.category] ?? 9) || a.name.localeCompare(b.name));
}

function groupByStatus(rows, order) {
  return require('./tasks-board').jiraColumns(rows || [], order);
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
async function list({ client, query, projects, page = 1, limit = 36, baseUrl, sort, statuses, currentSprint, rawQuery }) {
  const opts = { projects, sort, statuses, currentSprint };
  const jql = isRawJql(rawQuery) ? rawJql(rawQuery, opts) : toJql(query, opts);
  const result = await client.search(jql, { startAt: (page - 1) * limit, maxResults: limit });
  const total = result.total || 0;
  return {
    items: (result.issues || []).map((issue) => mapIssue(issue, { baseUrl })),
    errors: [],
    totalPages: Math.max(1, Math.ceil(total / limit)),
    failedRepos: 0,
    total,
    jql,
  };
}

// --- detail view (Orca JiraIssueWorkspace) ---
const { sanitizeHtml, textOf } = require('./sanitize-html');

function escapeText(text) {
  return String(text == null ? '' : text).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

/** DC transition `fields` is an object keyed by field key; normalise to Orca's `JiraCreateField[]` shape. */
function transitionFields(transition) {
  const raw = transition.fields || {};
  if (Array.isArray(raw)) return raw;
  return Object.entries(raw).map(([key, f]) => ({
    key,
    name: (f && f.name) || key,
    required: !!(f && f.required),
    schema: (f && f.schema) || {},
    allowedValues: (f && f.allowedValues) || [],
  }));
}

function allowedValueKey(v) {
  return (v.id && String(v.id).trim()) || (v.value && String(v.value).trim()) || (v.name && String(v.name).trim()) || '';
}

/** Orca's classifyTransitionRequirements: none · form (resolution select / required comment) · unsupported fields. */
function classifyTransition(transition) {
  const required = transitionFields(transition).filter((f) => f.required);
  if (!required.length) return { kind: 'none' };
  let resolution = null;
  let commentRequired = false;
  const unsupported = [];
  for (const field of required) {
    const custom = field.schema && field.schema.custom;
    if (required.filter((o) => o.key === field.key).length > 1) unsupported.push(field);
    else if (field.key === 'resolution' && !custom && (field.allowedValues || []).some(allowedValueKey)) resolution = field;
    else if (field.key === 'comment' && !custom) commentRequired = true;
    else unsupported.push(field);
  }
  if (unsupported.length) return { kind: 'unsupported', fields: unsupported };
  return { kind: 'form', resolution, commentRequired };
}

function mapTransition(t) {
  const to = t.to || {};
  const cat = (to.statusCategory && to.statusCategory.key) || '';
  return { id: String(t.id), name: t.name || '', to: { name: to.name || '', category: cat }, requirement: classifyTransition(t) };
}

/** Comment → `{id, author, avatarUrl, created, bodyHtml}`; `renderedBody` (DC HTML) sanitised, raw `body` escaped as fallback. */
function mapComment(c, opts) {
  const { baseUrl } = opts || {};
  const author = c.author || {};
  const bodyHtml = c.renderedBody ? sanitizeHtml(c.renderedBody, { baseUrl }) : `<p>${escapeText(c.body || '').replace(/\n/g, '<br>')}</p>`;
  return {
    id: String(c.id || ''),
    author: author.displayName || author.name || 'unknown',
    avatarUrl: (author.avatarUrls && author.avatarUrls['24x24']) || null,
    created: c.created || '',
    bodyHtml,
  };
}

/** Row + description/comments/transitions for the inline detail view. */
function mapDetail({ issue, comments, transitions }, opts) {
  const { baseUrl } = opts || {};
  const row = mapIssue(issue, { baseUrl });
  const rendered = (issue.renderedFields && issue.renderedFields.description) || '';
  const raw = (issue.fields && issue.fields.description) || '';
  const descriptionHtml = rendered ? sanitizeHtml(rendered, { baseUrl }) : raw ? `<p>${escapeText(raw).replace(/\n/g, '<br>')}</p>` : '';
  return {
    ...row,
    descriptionHtml,
    descriptionText: textOf(descriptionHtml, 2000),
    comments: ((comments && comments.comments) || comments || []).map((c) => mapComment(c, { baseUrl })),
    transitions: (transitions || []).map(mapTransition),
  };
}

/** Orca's buildJiraBranchName: lower-case key + slug of the title (≤ 52 chars). */
function branchName(row) {
  const slug = String(row.title || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 52);
  return `${String(row.key).toLowerCase()}${slug ? `-${slug}` : ''}`;
}

/** Orca's buildJiraIssuePrompt. */
function prompt(row) {
  const desc = String(row.descriptionText || '').trim();
  return `Complete Jira issue ${row.key}: ${row.title}\n\n${row.url}${desc ? `\n\n${desc}` : ''}`;
}

module.exports = { toJql, isRawJql, rawJql, statusCatalog, groupByStatus, SORT_FIELDS, mapIssue, list, classifyTransition, mapTransition, mapComment, mapDetail, branchName, prompt };
