'use strict';
// Behavioural tests for TasksViewProvider.start(id, {fresh}): resume vs ask vs tasksStartDefault,
// the worktree-create seam, and the prompt-file/launch-command plumbing. Drives the fake vscode module.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const { vscode, state, fakeContext, reset } = require('./fake-vscode');
const { TasksViewProvider } = require('../extension');
const tasksStart = require('../lib/tasks-start');

function make() {
  reset();
  const provider = new TasksViewProvider({ show() {}, hide() {} }, fakeContext());
  provider.result = { items: [], errors: [], totalPages: 1, failedRepos: 0 };
  provider.initialLoad = false;
  return provider;
}

function setWorkspace(dir) {
  vscode.workspace.workspaceFolders = [{ uri: { fsPath: dir } }];
}

/** Stubs provider.exec to answer `worktree list --format json` from `listsByCall` (one array per call, last repeats). */
function stubExec(provider, listsByCall) {
  const calls = [];
  provider.exec = async (binary, argv, cwd) => {
    calls.push({ binary, argv, cwd });
    if (argv[0] === 'worktree' && argv[1] === 'list') {
      const n = calls.filter((c) => c.argv[0] === 'worktree' && c.argv[1] === 'list').length - 1;
      const list = listsByCall[Math.min(n, listsByCall.length - 1)] || [];
      return JSON.stringify(list);
    }
    return '';
  };
  return calls;
}

/** Stubs showQuickPick to return `picks` in order, one per call. */
function stubQuickPicks(picks) {
  let i = 0;
  const seen = [];
  vscode.window.showQuickPick = async (items, opts) => {
    seen.push({ items, opts });
    const p = picks[i++];
    return p;
  };
  return seen;
}

const githubIssue = { id: 'github:acme/widgets:issue:3', source: 'github', type: 'issue', number: 3, title: 'Report bug', url: 'https://github.com/acme/widgets/issues/3' };

test('start(): attached + not fresh resumes — no "where" QuickPick, terminal cwd is the attached worktree', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  const branch = tasksStart.branchNameFor(githubIssue);
  const attachedPath = '/worktrees/acme-widgets/' + branch;
  stubExec(p, [[{ Path: attachedPath, Branch: branch, Exists: true }]]);
  const seen = stubQuickPicks([{ id: 'implementation' }]);

  await p.start(githubIssue.id, {});

  assert.equal(seen.length, 1, 'only the template QuickPick runs, not the where-to-start one');
  assert.equal(vscode.window.terminals.length, 1);
  const term = vscode.window.terminals[0];
  assert.equal(term.cwd, attachedPath);
  assert.equal(term.sent.length, 1);
  assert.match(term.sent[0], new RegExp(`cd '${attachedPath.replace(/\//g, '\\/')}'`));
  const promptFile = term.sent[0].match(/cat '([^']+)'/)[1];
  assert.equal(fs.readFileSync(promptFile, 'utf8').trim(), tasksStart.renderPrompt('implementation', githubIssue));
});

test('start(): fresh=true ignores an attached worktree and asks where to start', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  const branch = tasksStart.branchNameFor(githubIssue);
  stubExec(p, [[{ Path: '/worktrees/acme-widgets/' + branch, Branch: branch, Exists: true }]]);
  const seen = stubQuickPicks([{ id: 'here' }, { id: 'implementation' }]);

  await p.start(githubIssue.id, { fresh: true });

  assert.equal(seen.length, 2, 'fresh start always asks where, then the template');
  assert.equal(vscode.window.terminals[0].cwd, '/work/repo');
});

test('start(): tasksStartDefault "here" skips the where-to-start QuickPick and uses the workspace dir', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  stubExec(p, [[]]);
  const origCfg = p.cfg.bind(p);
  p.cfg = () => ({ ...origCfg(), startDefault: 'here' });
  const seen = stubQuickPicks([{ id: 'implementation' }]);

  await p.start(githubIssue.id, {});

  assert.equal(seen.length, 1, 'only the template QuickPick runs');
  assert.equal(vscode.window.terminals[0].cwd, '/work/repo');
});

test('start(): the worktree path calls `worktree create <name>` then re-lists, and uses the matching entry\'s Path', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  const name = tasksStart.worktreeNameFor(githubIssue);
  const newPath = '/worktrees/acme-widgets/' + name;
  const calls = stubExec(p, [[], [{ Path: newPath, Branch: name, Exists: true }]]);
  const seen = stubQuickPicks([{ id: 'worktree' }, { id: 'implementation' }]);

  await p.start(githubIssue.id, {});

  const listCalls = calls.filter((c) => c.argv[0] === 'worktree' && c.argv[1] === 'list');
  const createCalls = calls.filter((c) => c.argv[0] === 'worktree' && c.argv[1] === 'create');
  assert.equal(createCalls.length, 1);
  assert.deepEqual(createCalls[0].argv, ['worktree', 'create', name]);
  assert.equal(listCalls.length, 2, 'lists before and after the create');
  assert.equal(seen.length, 2);
  assert.equal(vscode.window.terminals[0].cwd, newPath);
});

test('start(): tasksStartDefault "worktree" skips the picker and goes straight to worktree creation', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  const name = tasksStart.worktreeNameFor(githubIssue);
  const newPath = '/worktrees/acme-widgets/' + name;
  const calls = stubExec(p, [[], [{ Path: newPath, Branch: name, Exists: true }]]);
  const origCfg = p.cfg.bind(p);
  p.cfg = () => ({ ...origCfg(), startDefault: 'worktree' });
  const seen = stubQuickPicks([{ id: 'implementation' }]);

  await p.start(githubIssue.id, {});

  assert.equal(seen.length, 1, 'no where-to-start pick, only the template');
  assert.ok(calls.some((c) => c.argv[0] === 'worktree' && c.argv[1] === 'create'));
  assert.equal(vscode.window.terminals[0].cwd, newPath);
});

test('start(): no attached worktree and no workspace folder warns instead of launching', async () => {
  const p = make();
  vscode.workspace.workspaceFolders = [];
  p.result.items = [githubIssue];
  stubExec(p, [[]]);
  const origCfg = p.cfg.bind(p);
  p.cfg = () => ({ ...origCfg(), startDefault: 'here' });
  stubQuickPicks([{ id: 'implementation' }]);

  await p.start(githubIssue.id, {});

  assert.equal(vscode.window.terminals.length, 0);
  assert.ok(state.messages.some((m) => m[0] === 'warn'));
});

test('start(): dismissing the where-to-start QuickPick aborts before the template pick or any terminal', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  stubExec(p, [[]]);
  const seen = stubQuickPicks([undefined]);

  await p.start(githubIssue.id, {});

  assert.equal(seen.length, 1);
  assert.equal(vscode.window.terminals.length, 0);
});

test('start(): dismissing the template QuickPick aborts before any terminal is created', async () => {
  const p = make();
  setWorkspace('/work/repo');
  p.result.items = [githubIssue];
  stubExec(p, [[]]);
  stubQuickPicks([{ id: 'here' }, undefined]);

  await p.start(githubIssue.id, {});

  assert.equal(vscode.window.terminals.length, 0);
});
