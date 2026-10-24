'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const {
  dispatchStartArgv,
  attachShellArgs,
  attachCommandLine,
  vscodeServeArgv,
  AGENTS,
} = require('../lib/argv');

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

test('dispatchStartArgv: rejects an agent outside the allowlist', () => {
  assert.throws(() => dispatchStartArgv('claude-code', { host: 'h', agent: 'not-a-real-agent', dir: '', task: 't' }), /must be one of/);
});

test('dispatchStartArgv: the allowlist matches the quick-pick list exported as AGENTS', () => {
  for (const agent of AGENTS) {
    assert.doesNotThrow(() => dispatchStartArgv('claude-code', { host: 'h', agent, dir: '', task: 't' }));
  }
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

test('vscodeServeArgv: "<binary> vscode serve"', () => {
  assert.deepEqual(vscodeServeArgv('claude-code'), ['claude-code', 'vscode', 'serve']);
});

test('vscodeServeArgv: {stop: true} appends --stop', () => {
  assert.deepEqual(
    vscodeServeArgv('claude-code', { stop: true }),
    ['claude-code', 'vscode', 'serve', '--stop']
  );
});

test('vscodeServeArgv: {stop: false} omits --stop entirely', () => {
  assert.deepEqual(
    vscodeServeArgv('claude-code', { stop: false }),
    ['claude-code', 'vscode', 'serve']
  );
});
