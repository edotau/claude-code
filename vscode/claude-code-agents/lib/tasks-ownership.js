'use strict';
// Write gate (user rule, 2026-10-24): only the user's own items take writes from this UI — comments, transitions,
// assignments, merges, board moves and Start. Teammates' work is read-only here even where the server would allow it.

function same(a, b) {
  return !!a && !!b && String(a).toLowerCase() === String(b).toLowerCase();
}

/** True when the item is assigned to or authored by the user; `logins` are what each source's doctor reported. */
function isMine(item, logins) {
  const l = logins || {};
  if (!item) return false;
  if (item.source === 'jira') {
    const a = item.assignee || null;
    return same(a && a.name, l.jira) || same(a && a.displayName, l.jiraDisplay) || (!a && same(item.author, l.jiraDisplay));
  }
  if (item.source === 'github') return same(item.author, l.github) || (item.assignees || []).some((x) => same(x, l.github));
  if (item.source === 'bitbucket') return same(item.author, l.bitbucket);
  return false;
}

/** Login known for the item's source? Without one ownership can't be judged (and writes would fail anyway). */
function loginKnown(item, logins) {
  const l = logins || {};
  if (!item) return false;
  if (item.source === 'jira') return !!(l.jira || l.jiraDisplay);
  if (item.source === 'github') return !!l.github;
  if (item.source === 'bitbucket') return !!l.bitbucket;
  return false;
}

/** `scope` is the `tasksWriteScope` setting: `mine` (default) or `all`. */
function canWrite(item, logins, scope) {
  if (scope === 'all') return true;
  if (!loginKnown(item, logins)) return true;
  return isMine(item, logins);
}

/** The reason shown in the UI when an item is read-only. */
function readOnlyReason(item) {
  if (!item) return 'Read-only';
  if (item.source === 'jira') return item.assignee ? `Read-only: assigned to ${item.assignee.displayName}` : 'Read-only: not assigned to you';
  return `Read-only: ${item.type === 'pr' ? 'pull request' : 'issue'} by ${item.author || 'someone else'}`;
}

module.exports = { isMine, loginKnown, canWrite, readOnlyReason };
