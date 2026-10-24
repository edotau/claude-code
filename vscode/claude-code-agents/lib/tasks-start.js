'use strict';
// Start / Resume for a task row (Orca's new-workspace composer + jira-prompt-templates): prompt templates, branch and
// worktree names, attached-worktree matching, and a shell-safe launch line. Pure; the extension owns the I/O.

const PROMPT_TEMPLATES = [
  { id: 'implementation', label: 'Start implementation', text: 'Implement the following task. Inspect the existing code, make the changes, and verify them.\n\n$task\n\n$extra' },
  { id: 'bug', label: 'Investigate bug', text: 'Investigate this task. Reproduce the problem, explain the cause, and propose a fix supported by evidence.\n\n$task\n\n$extra' },
  { id: 'test-plan', label: 'Write test plan', text: 'Write a test plan for this task, covering normal behavior, edge cases, and regressions.\n\n$task\n\n$extra' },
  { id: 'pr-summary', label: 'Draft PR summary', text: 'Draft a pull request summary for this task. Describe the problem, changes, validation, and remaining risks. Do not claim unverified work is complete.\n\n$task\n\n$extra' },
  { id: 'plain', label: 'Plain prompt', text: '$task' },
];

function slug(text) {
  return String(text || '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '').slice(0, 52);
}

/** The task as a prompt paragraph: key/number, title, link, description excerpt (Jira), and the branch when known. */
function taskPrompt(item) {
  let head;
  if (item.source === 'jira') {
    head = `Complete Jira issue ${item.key}: ${item.title}\n\n${item.url}`;
    const desc = String(item.descriptionText || '').trim();
    if (desc) head += `\n\n${desc}`;
  } else if (item.source === 'bitbucket') {
    head = `Work on pull request #${item.number} "${item.title}" (${item.url})`;
  } else {
    head = `Work on ${item.type === 'pr' ? 'PR' : 'issue'} #${item.number} "${item.title}" (${item.url})`;
  }
  return item.branch ? `${head}\n\nBranch: ${item.branch}` : head;
}

function renderPrompt(templateId, item, extra) {
  const template = PROMPT_TEMPLATES.find((t) => t.id === templateId) || PROMPT_TEMPLATES[PROMPT_TEMPLATES.length - 1];
  return template.text.replace(/\$(task|extra)\b/g, (_, v) => (v === 'task' ? taskPrompt(item) : String(extra || '').trim())).trim();
}

/** Jira: `key-slug`; GitHub issue: `issue-N-slug`; PRs: the head branch, else `pr-N-slug`. */
function branchNameFor(item) {
  const s = slug(item.title);
  if (item.source === 'jira') return `${String(item.key || '').toLowerCase()}${s ? `-${s}` : ''}`;
  if (item.type === 'pr') return item.branch || `pr-${item.number}${s ? `-${s}` : ''}`;
  return `issue-${item.number}${s ? `-${s}` : ''}`;
}

function worktreeNameFor(item) {
  return branchNameFor(item).replace(/\//g, '-').slice(0, 60);
}

function basename(p) {
  return String(p || '').replace(/[\\/]+$/, '').split(/[\\/]/).pop() || '';
}

/** The `worktree list --format json` entry attached to `item`: exact branch, else a key/number mention in branch or path. */
function findAttached(worktrees, item) {
  const list = (Array.isArray(worktrees) ? worktrees : []).filter((w) => w && w.Exists !== false);
  const branch = branchNameFor(item);
  const exact = list.find((w) => w.Branch === branch);
  if (exact) return exact;
  const needles = item.source === 'jira' ? [String(item.key || '').toLowerCase()] : [`issue-${item.number}`, `pr-${item.number}`, `-${item.number}-`];
  const hit = list.find((w) => {
    const hay = `${w.Branch || ''} ${basename(w.Path)}`.toLowerCase();
    return needles.some((n) => n && hay.includes(n));
  });
  return hit || null;
}

const { isMine } = require('./tasks-ownership');

function shellQuote(s) {
  return `'${String(s).replace(/'/g, "'\\''")}'`;
}

/** `cd '<cwd>' && <binary> claude "$(cat '<promptFile>')"` — the prompt never travels on the command line. */
function launchCommand({ binary, cwd, promptFile }) {
  return `cd ${shellQuote(cwd)} && ${binary} claude "$(cat ${shellQuote(promptFile)})"`;
}

module.exports = { PROMPT_TEMPLATES, taskPrompt, renderPrompt, branchNameFor, worktreeNameFor, findAttached, isMine, shellQuote, launchCommand };
