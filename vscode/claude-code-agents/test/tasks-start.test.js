'use strict';
// Unit tests for lib/tasks-start.js: prompt templates, taskPrompt/renderPrompt, branch/worktree naming,
// findAttached matching, and shellQuote/launchCommand shell safety. Spec: AGENTS brief for tasks-start.js.
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  PROMPT_TEMPLATES,
  taskPrompt,
  renderPrompt,
  branchNameFor,
  worktreeNameFor,
  findAttached,
  shellQuote,
  launchCommand,
} = require('../lib/tasks-start');

// -- PROMPT_TEMPLATES --

test('PROMPT_TEMPLATES: ids, labels and leading text', () => {
  const byId = Object.fromEntries(PROMPT_TEMPLATES.map((t) => [t.id, t]));
  assert.deepEqual(Object.keys(byId), ['implementation', 'bug', 'test-plan', 'pr-summary', 'plain']);
  assert.equal(byId.implementation.label, 'Start implementation');
  assert.equal(byId.bug.label, 'Investigate bug');
  assert.equal(byId['test-plan'].label, 'Write test plan');
  assert.equal(byId['pr-summary'].label, 'Draft PR summary');
  assert.equal(byId.plain.label, 'Plain prompt');
  assert.ok(byId.implementation.text.startsWith('Implement the following'));
  assert.ok(byId.bug.text.startsWith('Investigate this'));
  assert.ok(byId['test-plan'].text.startsWith('Write a test plan'));
  assert.ok(byId['pr-summary'].text.startsWith('Draft a pull request summary'));
  assert.equal(byId.plain.text, '$task');
  for (const t of PROMPT_TEMPLATES) assert.ok(t.text.includes('$task'), `${t.id} text must contain $task`);
});

// -- taskPrompt --

test('taskPrompt: jira with description appends url then description, each on its own paragraph', () => {
  const item = { source: 'jira', key: 'ABC-1', title: 'Fix the thing', url: 'https://jira/ABC-1', descriptionText: 'Steps to repro.' };
  assert.equal(taskPrompt(item), 'Complete Jira issue ABC-1: Fix the thing\n\nhttps://jira/ABC-1\n\nSteps to repro.');
});

test('taskPrompt: jira with empty description omits the description paragraph', () => {
  const item = { source: 'jira', key: 'ABC-2', title: 'No desc', url: 'https://jira/ABC-2', descriptionText: '   ' };
  assert.equal(taskPrompt(item), 'Complete Jira issue ABC-2: No desc\n\nhttps://jira/ABC-2');
});

test('taskPrompt: github PR', () => {
  const item = { source: 'github', type: 'pr', number: 5, title: 'Add feature', url: 'https://github.com/a/b/pull/5' };
  assert.equal(taskPrompt(item), 'Work on PR #5 "Add feature" (https://github.com/a/b/pull/5)');
});

test('taskPrompt: github issue', () => {
  const item = { source: 'github', type: 'issue', number: 7, title: 'Bug report', url: 'https://github.com/a/b/issues/7' };
  assert.equal(taskPrompt(item), 'Work on issue #7 "Bug report" (https://github.com/a/b/issues/7)');
});

test('taskPrompt: bitbucket pull request', () => {
  const item = { source: 'bitbucket', number: 3, title: 'Refactor', url: 'https://bb/pr/3' };
  assert.equal(taskPrompt(item), 'Work on pull request #3 "Refactor" (https://bb/pr/3)');
});

test('taskPrompt: appends "Branch: <branch>" when item.branch is set, for any source', () => {
  const item = { source: 'github', type: 'pr', number: 5, title: 'Add feature', url: 'u', branch: 'feature/x' };
  assert.equal(taskPrompt(item), 'Work on PR #5 "Add feature" (u)\n\nBranch: feature/x');
});

test('taskPrompt: no Branch line when item.branch is unset', () => {
  const item = { source: 'github', type: 'issue', number: 7, title: 'Bug', url: 'u' };
  assert.ok(!taskPrompt(item).includes('Branch:'));
});

// -- renderPrompt --

test('renderPrompt: substitutes $task and trims the result', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'T', url: 'u' };
  const out = renderPrompt('plain', item);
  assert.equal(out, taskPrompt(item));
});

test('renderPrompt: substitutes $extra and trims extra text', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'T', url: 'u' };
  const out = renderPrompt('implementation', item, '  Focus on edge cases.  ');
  assert.ok(out.includes('Focus on edge cases.'));
  assert.ok(out.startsWith('Implement the following'));
  assert.ok(!out.endsWith(' '), 'result is trimmed');
});

test('renderPrompt: unknown template id falls back to plain', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'T', url: 'u' };
  assert.equal(renderPrompt('does-not-exist', item), taskPrompt(item));
});

test('renderPrompt: default extra is empty when omitted', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'T', url: 'u' };
  const out = renderPrompt('bug', item);
  assert.ok(!out.includes('$extra'));
  assert.ok(!out.includes('undefined'));
});

// -- branchNameFor --

test('branchNameFor: jira matches tasks-jira.branchName (key lower + title slug)', () => {
  const { branchName } = require('../lib/tasks-jira');
  const row = { key: 'ABC-123', title: "Fix the Thing That's Broken!!" };
  const item = { source: 'jira', key: row.key, title: row.title };
  assert.equal(branchNameFor(item), branchName(row));
});

test('branchNameFor: jira slug is lowercase, non-alnum collapsed to -, trimmed, max 52 chars of slug', () => {
  const item = { source: 'jira', key: 'KEY-1', title: '  Weird///Title!! With   Spaces  ' };
  const name = branchNameFor(item);
  assert.ok(name.startsWith('key-1-'));
  assert.doesNotMatch(name, /[A-Z]/);
  assert.doesNotMatch(name, /--/);
  assert.ok(!name.endsWith('-'));
});

test('branchNameFor: github issue -> issue-<n>-<slug>', () => {
  const item = { source: 'github', type: 'issue', number: 42, title: 'Crash on save' };
  assert.equal(branchNameFor(item), 'issue-42-crash-on-save');
});

test('branchNameFor: github PR with item.branch returns it verbatim', () => {
  const item = { source: 'github', type: 'pr', number: 9, title: 'whatever', branch: 'feature/cool-thing' };
  assert.equal(branchNameFor(item), 'feature/cool-thing');
});

test('branchNameFor: github PR without item.branch falls back to pr-<n>-<slug>', () => {
  const item = { source: 'github', type: 'pr', number: 9, title: 'Add widget' };
  assert.equal(branchNameFor(item), 'pr-9-add-widget');
});

test('branchNameFor: bitbucket PR follows the same branch/pr-<n> rule as github', () => {
  const withBranch = { source: 'bitbucket', type: 'pr', number: 2, title: 't', branch: 'feature/y' };
  const withoutBranch = { source: 'bitbucket', type: 'pr', number: 2, title: 'Add widget' };
  assert.equal(branchNameFor(withBranch), 'feature/y');
  assert.equal(branchNameFor(withoutBranch), 'pr-2-add-widget');
});

// -- worktreeNameFor --

test('worktreeNameFor: replaces / with - and caps at 60 chars', () => {
  const item = { source: 'github', type: 'pr', number: 9, title: 'whatever', branch: 'feature/cool-thing' };
  assert.equal(worktreeNameFor(item), 'feature-cool-thing');
  assert.ok(worktreeNameFor(item).length <= 60);
});

test('worktreeNameFor: a long branch name is truncated to 60 chars', () => {
  const item = { source: 'jira', key: 'ABC-1', title: 'a'.repeat(100) };
  assert.ok(worktreeNameFor(item).length <= 60);
});

// -- findAttached --

test('findAttached: exact branch match wins', () => {
  const item = { source: 'jira', key: 'ABC-1', title: 'Fix thing' };
  const branch = branchNameFor(item);
  const worktrees = [{ Path: '/wt/other', Branch: 'unrelated', Exists: true }, { Path: '/wt/mine', Branch: branch, Exists: true }];
  assert.equal(findAttached(worktrees, item), worktrees[1]);
});

test('findAttached: falls back to a jira key mention (case-insensitive) in branch or path basename', () => {
  const item = { source: 'jira', key: 'ABC-1', title: 'Fix thing' };
  const worktrees = [{ Path: '/wt/abc-1-old-name', Branch: 'something-else', Exists: true }];
  assert.equal(findAttached(worktrees, item), worktrees[0]);
});

test('findAttached: github issue falls back to issue-<n> mention', () => {
  const item = { source: 'github', type: 'issue', number: 42, title: 'Crash' };
  const worktrees = [{ Path: '/wt/x', Branch: 'issue-42-renamed', Exists: true }];
  assert.equal(findAttached(worktrees, item), worktrees[0]);
});

test('findAttached: github PR falls back to pr-<n> or -<n>- mention', () => {
  const item = { source: 'github', type: 'pr', number: 9, title: 'Add widget' };
  const byPr = findAttached([{ Path: '/wt/x', Branch: 'pr-9-something', Exists: true }], item);
  assert.ok(byPr);
  const byNum = findAttached([{ Path: '/wt/x', Branch: 'feature-9-something', Exists: true }], item);
  assert.ok(byNum);
});

test('findAttached: Exists === false entries are skipped entirely', () => {
  const item = { source: 'jira', key: 'ABC-1', title: 'Fix thing' };
  const branch = branchNameFor(item);
  const worktrees = [{ Path: '/wt/mine', Branch: branch, Exists: false }];
  assert.equal(findAttached(worktrees, item), null);
});

test('findAttached: no match returns null', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'Nothing' };
  assert.equal(findAttached([{ Path: '/wt/x', Branch: 'unrelated', Exists: true }], item), null);
});

test('findAttached: empty/undefined worktrees list returns null without throwing', () => {
  const item = { source: 'github', type: 'issue', number: 1, title: 'Nothing' };
  assert.equal(findAttached([], item), null);
  assert.equal(findAttached(undefined, item), null);
});

// -- shellQuote / launchCommand --

test('shellQuote: wraps in single quotes and escapes embedded single quotes', () => {
  assert.equal(shellQuote('abc'), "'abc'");
  assert.equal(shellQuote("it's"), "'it'\\''s'");
});

test('launchCommand: cd into cwd, cat the prompt file, pass it to the binary verbatim', () => {
  const cmd = launchCommand({ binary: 'claude-code', cwd: '/work/repo', promptFile: '/tmp/p.md' });
  assert.equal(cmd, "cd '/work/repo' && claude-code claude \"$(cat '/tmp/p.md')\"");
});

test('launchCommand: a cwd with a single quote and a shell-injection payload comes out fully quoted and inert', () => {
  const { execFileSync } = require('node:child_process');
  const evilCwd = "/tmp/it's a $(touch /tmp/claude-start-test-injected-$$) dir";
  const cmd = launchCommand({ binary: 'claude-code', cwd: evilCwd, promptFile: '/tmp/p.md' });
  assert.ok(cmd.includes(shellQuote(evilCwd)));
  // run it for real through bash (the `cd` fails since the dir doesn't exist; the point is nothing inside runs)
  try {
    execFileSync('bash', ['-c', cmd], { stdio: 'ignore' });
  } catch {
    // non-zero exit from the failed `cd` / missing `claude-code` binary is expected and fine
  }
  const fs = require('node:fs');
  const marker = fs.readdirSync('/tmp').some((f) => f.startsWith('claude-start-test-injected-'));
  assert.equal(marker, false, 'the embedded $(...) must never execute');
});

test('launchCommand: the prompt text never appears in the command, only the prompt file path does', () => {
  const promptFile = '/tmp/claude-prompts/some-id.md';
  const cmd = launchCommand({ binary: 'claude-code', cwd: '/work', promptFile });
  assert.ok(cmd.includes(promptFile));
  assert.ok(!cmd.includes('Implement the following'));
});

