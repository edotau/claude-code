'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  treeArgv,
  dispatchListArgv,
  dispatchStartArgv,
  attachShellArgs,
  attachCommandLine,
} = require('../lib/argv');

test('treeArgv: defaults to --days 1, no --all, --format json', () => {
  assert.deepEqual(
    treeArgv('claude-code', {}),
    ['claude-code', 'agents', 'tree', '--days', '1', '--format', 'json']
  );
});

test('treeArgv: --all flag is inserted before --format', () => {
  assert.deepEqual(
    treeArgv('claude-code', { days: 7, all: true }),
    ['claude-code', 'agents', 'tree', '--days', '7', '--all', '--format', 'json']
  );
});

test('dispatchListArgv: --list --host H --format json', () => {
  assert.deepEqual(
    dispatchListArgv('claude-code', 'myhost'),
    ['claude-code', 'dispatch', '--list', '--host', 'myhost', '--format', 'json']
  );
});

test('dispatchStartArgv: --dir is omitted entirely when empty', () => {
  assert.deepEqual(
    dispatchStartArgv('claude-code', { host: 'h', agent: 'claude', dir: '', task: 'do it' }),
    ['claude-code', 'dispatch', '--host', 'h', '--agent', 'claude', 'do it']
  );
});

test('dispatchStartArgv: --dir included when given', () => {
  assert.deepEqual(
    dispatchStartArgv('claude-code', { host: 'h', agent: 'codex', dir: '/tmp/x', task: 'go' }),
    ['claude-code', 'dispatch', '--host', 'h', '--agent', 'codex', '--dir', '/tmp/x', 'go']
  );
});

test('dispatchStartArgv: a task with quotes, $HOME and newlines stays one argv element', () => {
  const task = `fix the "bug" in $HOME/app\nand rerun the tests`;
  const argv = dispatchStartArgv('claude-code', { host: 'h', agent: 'claude', dir: '', task });
  assert.equal(argv.length, 7);
  assert.equal(argv[argv.length - 1], task);
});

test('dispatchStartArgv: honors any requested agent string verbatim (allowlisting is the CLI\'s job)', () => {
  // argv.js builds argv only; it does not validate the agent name itself.
  const argv = dispatchStartArgv('claude-code', { host: 'h', agent: 'gemini', dir: '', task: 't' });
  assert.equal(argv[argv.indexOf('--agent') + 1], 'gemini');
});

test('attachShellArgs: matches the Go CLI\'s AttachArgs shape minus the leading "ssh"', () => {
  assert.deepEqual(attachShellArgs('myhost', 'cc-claude-abcd'), [
    '-t', 'myhost', 'tmux', 'attach', '-t', 'cc-claude-abcd',
  ]);
});

test('attachCommandLine: full "ssh -t host tmux attach -t session" text', () => {
  assert.equal(
    attachCommandLine('myhost', 'cc-claude-abcd'),
    'ssh -t myhost tmux attach -t cc-claude-abcd'
  );
});
