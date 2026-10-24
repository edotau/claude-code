'use strict';
// Pure Tasks-panel model: Jira key detection + Bitbucket PR JSON -> tree node shapes.
// No vscode dependency, so node:test can exercise it directly.

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

/** PR rows from lib/bitbucket-client.js are a bare array; an older {prs:[…]} wrapper is still accepted. */
function prRows(json) {
  if (Array.isArray(json)) return json;
  return json && Array.isArray(json.prs) ? json.prs : [];
}

/** Build state (foldBuildStatus folds Jenkins statuses to success|failed|in_progress|unknown) -> glyph. */
function buildSymbol(build) {
  const state = build && (build.state || build.status);
  if (state === 'success' || state === 'SUCCESSFUL') return '✓';
  if (state === 'failed' || state === 'FAILED') return '✗';
  if (state === 'in_progress' || state === 'INPROGRESS') return '…';
  return '';
}

/** "digitalbtd-portal-frontend" from a Data Center PR URL; '' when the URL has no repos segment. */
function repoFromURL(url) {
  const m = /\/repos\/([^/]+)\//.exec(url || '');
  return m ? m[1] : '';
}

/** "n/m" approved/total, or null when there are no reviewers. */
function reviewerSummary(reviewers) {
  const list = reviewers || [];
  if (list.length === 0) return null;
  const approved = list.filter((r) => r.status === 'APPROVED').length;
  return `${approved}/${list.length}`;
}

/** description = "state · build <symbol> · reviewers n/m", omitting parts with no data. */
function prDescription(pr) {
  const parts = [];
  if (pr.state) parts.push(pr.state);
  const symbol = buildSymbol(pr.build);
  if (symbol) parts.push(`build ${symbol}`);
  const reviewers = reviewerSummary(pr.reviewers);
  if (reviewers) parts.push(`reviewers ${reviewers}`);
  return parts.join(' · ');
}

function prTooltip(pr) {
  const lines = [`**#${pr.id}** ${pr.title || ''}`.trim(), ''];
  if (pr.state) lines.push(`State: ${pr.state}  `);
  if (pr.build && (pr.build.state || pr.build.status)) lines.push(`Build: ${buildSymbol(pr.build)} ${pr.build.state || pr.build.status}  `);
  const reviewers = reviewerSummary(pr.reviewers);
  if (reviewers) lines.push(`Reviewers: ${reviewers} approved  `);
  return lines.join('\n');
}

function prChildren(pr, { jiraKey, jiraBaseUrl } = {}) {
  const children = [];
  for (const reviewer of pr.reviewers || []) {
    children.push({ label: `${reviewer.name} — ${reviewer.status}`, description: '', tooltip: '', url: null, icon: 'person', children: [] });
  }
  if (pr.build && (pr.build.state || pr.build.status)) {
    const symbol = buildSymbol(pr.build);
    children.push({
      label: `Build: ${symbol ? `${symbol} ` : ''}${pr.build.state || pr.build.status}`,
      description: '',
      tooltip: '',
      url: pr.build.url || null,
      icon: symbol === '✓' ? 'check' : symbol === '✗' ? 'error' : 'sync',
      children: [],
    });
  }
  if (jiraKey && jiraBaseUrl) {
    children.push({
      label: jiraKey,
      description: '',
      tooltip: `Open ${jiraKey} in Jira`,
      url: `${jiraBaseUrl.replace(/\/$/, '')}/browse/${jiraKey}`,
      icon: 'link',
      children: [],
    });
  }
  return children;
}

/** Tree nodes for the current branch: the branch's PR (with reviewer/build/jira children), or a placeholder. */
function buildBranchNodes(prJson, opts) {
  const { jiraKey, jiraBaseUrl } = opts || {};
  const pr = prRows(prJson)[0] || null;
  if (!pr) {
    const nodes = [{
      label: `No pull request for ${(opts && opts.branch) || (prJson && prJson.branch) || 'this branch'}`,
      description: '',
      tooltip: '',
      url: null,
      icon: 'git-branch',
      children: [],
    }];
    if (jiraKey) {
      nodes.push({
        label: jiraKey,
        description: '',
        tooltip: jiraBaseUrl ? `Open ${jiraKey} in Jira` : '',
        url: jiraBaseUrl ? `${jiraBaseUrl.replace(/\/$/, '')}/browse/${jiraKey}` : null,
        icon: 'link',
        children: [],
      });
    }
    return nodes;
  }
  return [{
    label: `#${pr.id} ${pr.title || ''}`.trim(),
    description: prDescription(pr),
    tooltip: prTooltip(pr),
    url: pr.url || null,
    icon: 'git-pull-request',
    children: prChildren(pr, { jiraKey, jiraBaseUrl }),
  }];
}

/** One node per PR row (mine/reviewing), sorted newest-updated first. */
function buildPrListNodes(rowsJson) {
  const rows = prRows(rowsJson).slice();
  rows.sort((a, b) => Date.parse(b.updated || 0) - Date.parse(a.updated || 0));
  return rows.map((pr) => {
    const parts = [];
    const repo = pr.repo || repoFromURL(pr.url);
    if (repo) parts.push(repo);
    if (pr.state) parts.push(pr.state);
    const symbol = buildSymbol(pr.build);
    if (symbol) parts.push(`build ${symbol}`);
    return {
      label: `#${pr.id} ${pr.title || ''}`.trim(),
      description: parts.join(' · '),
      tooltip: prTooltip(pr),
      url: pr.url || null,
      icon: 'git-pull-request',
      children: [],
    };
  });
}

/** Status bar text for the current branch's PR, or '' when there is neither a PR nor a Jira key. */
function statusBarText(branchModel) {
  const { pr, jiraKey } = branchModel || {};
  if (!pr) return jiraKey ? '$(git-pull-request) no PR' : '';
  const parts = [`#${pr.id}`];
  const symbol = buildSymbol(pr.build);
  if (symbol) parts.push(symbol);
  const reviewers = reviewerSummary(pr.reviewers);
  if (reviewers) parts.push(`· ${reviewers}`);
  return `$(git-pull-request) ${parts.join(' ')}`;
}

module.exports = {
  detectJiraIssueKey,
  buildBranchNodes,
  buildPrListNodes,
  statusBarText,
};
