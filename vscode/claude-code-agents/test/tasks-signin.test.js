'use strict';
// Behavioural tests for the Tasks 'signIn' handler: password box → `vault set` on stdin, never the shared terminal.
const test = require('node:test');
const assert = require('node:assert/strict');
const { vscode, state, fakeContext, reset } = require('./fake-vscode');
const { TasksViewProvider } = require('../extension');

function make(inputValue) {
  reset();
  const provider = new TasksViewProvider({ show() {}, hide() {} }, fakeContext());
  const stored = [];
  const loads = [];
  provider.storeSecret = async (binary, name, value) => stored.push({ binary, name, value });
  provider.load = async (opts) => loads.push(opts);
  provider.tokenPromise = Promise.resolve('stale');
  provider.jiraTokenPromise = Promise.resolve('stale');
  const prompts = [];
  vscode.window.showInputBox = async (opts) => {
    prompts.push(opts);
    return inputValue;
  };
  return { provider, stored, loads, prompts };
}

for (const [source, name] of [['jira', 'JIRA_TOKEN'], ['bitbucket', 'BITBUCKET_TOKEN']]) {
  test(`signIn ${source}: stores the trimmed value as ${name}, clears token caches, reloads`, async () => {
    const { provider, stored, loads, prompts } = make('  tok-123 \n');
    await provider.handleMessage({ type: 'signIn', source });
    assert.equal(prompts.length, 1);
    assert.equal(prompts[0].password, true);
    assert.deepEqual(stored, [{ binary: 'claude-code', name, value: 'tok-123' }]);
    assert.equal(provider.tokenPromise, null);
    assert.equal(provider.jiraTokenPromise, null);
    assert.deepEqual(loads, [{ force: true }]);
    assert.equal(vscode.window.terminals.length, 0);
  });
}

test('signIn: cancelled or blank input stores nothing and keeps the caches', async () => {
  for (const input of [undefined, '   ']) {
    const { provider, stored, loads } = make(input);
    await provider.handleMessage({ type: 'signIn', source: 'jira' });
    assert.deepEqual(stored, []);
    assert.deepEqual(loads, []);
    assert.notEqual(provider.jiraTokenPromise, null);
  }
});

test('signIn: a failed vault set surfaces an error and does not reload', async () => {
  const { provider, loads } = make('tok');
  provider.storeSecret = async () => {
    throw new Error('vault locked');
  };
  await provider.handleMessage({ type: 'signIn', source: 'bitbucket' });
  assert.ok(state.messages.some(([kind, m]) => kind === 'error' && m.includes('BITBUCKET_TOKEN') && m.includes('vault locked')));
  assert.deepEqual(loads, []);
});
