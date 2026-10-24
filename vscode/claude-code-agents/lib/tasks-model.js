'use strict';
// Jira issue-key detection for the Tasks page (branch, then title). No vscode dependency.

// Jira-shaped tokens that are standards or encodings, never issue keys (ported from
// claude-orcastra's src/shared/jira-issue-key-detection.ts).
const NON_ISSUE_PROJECTS = new Set(['UTF', 'ISO', 'ES', 'SHA', 'MD', 'RFC', 'CVE', 'HTTP', 'TLS', 'SSL', 'IPV', 'X', 'V']);

// Key-shaped tokens bounded by start/end or a separator; the project must start with a letter.
const KEY_IN_TEXT = /(?:^|[^A-Za-z0-9])([A-Za-z][A-Za-z0-9_]*-\d+)(?=$|[^A-Za-z0-9])/g;

function firstIssueKey(text, known) {
  if (!text) return null;
  for (const match of text.matchAll(KEY_IN_TEXT)) {
    const key = match[1].toUpperCase();
    const project = key.slice(0, key.lastIndexOf('-'));
    if (NON_ISSUE_PROJECTS.has(project)) continue;
    if (known && !known.has(project)) continue;
    return key;
  }
  return null;
}

/** The Jira issue a branch (else a PR/review title) names, or null. Branch wins over title. */
function detectJiraIssueKey(input) {
  const { branch, title, knownProjectKeys } = input || {};
  const known = knownProjectKeys && knownProjectKeys.length
    ? new Set(knownProjectKeys.map((key) => key.toUpperCase()))
    : null;
  const stripped = branch ? branch.replace(/^refs\/heads\//, '') : null;
  return firstIssueKey(stripped, known) ?? firstIssueKey(title, known);
}

module.exports = { detectJiraIssueKey };
