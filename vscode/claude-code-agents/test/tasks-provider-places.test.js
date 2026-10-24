'use strict';
// Behavioural guard for "Tasks lives in one place": drives TasksViewProvider against the fake vscode module.
const test = require('node:test');
const assert = require('node:assert/strict');
const { state, fakeView, fakeContext, reset } = require('./fake-vscode');
const { TasksViewProvider } = require('../extension');

function make() {
  reset();
  const provider = new TasksViewProvider({ show() {}, hide() {} }, fakeContext());
  provider.load = async () => {}; // no network in tests
  provider.loadBranch = async () => {};
  provider.doctor = async () => {};
  provider.activatedAt = Date.now() - 10000; // past the startup-restore guard
  provider.result = { items: [], errors: [], totalPages: 1, failedRepos: 0 };
  provider.initialLoad = false;
  return provider;
}

const moved = () => state.commands.filter((c) => c[0] === 'workbench.action.moveEditorToNewWindow').length;

test('left-nav view appearing with Tasks open nowhere pops a window; appearing again while it is open does nothing', async () => {
  const p = make();
  const sidebar = fakeView('claudeCodeTasks.page');
  p.resolveWebviewView(sidebar);
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(state.panels.length, 1, 'one editor panel created');
  assert.equal(moved(), 1, 'moved into a new window');
  assert.equal(p.panelFloating, true);
  sidebar.hide();
  sidebar.show();
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(state.panels.length, 1, 'no second panel');
  assert.equal(moved(), 1, 'no second move');
  assert.doesNotMatch(sidebar.webview.html, /class="row head"/, 'the launcher never renders the page');
  assert.match(sidebar.webview.html, /open in a separate window/);
});

test('left-nav view restored during startup does not pop out; an explicit New Window click always does', async () => {
  const p = make();
  p.activatedAt = Date.now();
  const sidebar = fakeView('claudeCodeTasks.page');
  p.resolveWebviewView(sidebar);
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(state.panels.length, 0, 'startup restore is not a click');
  await p.handleMessage({ type: 'openWindow' });
  assert.equal(state.panels.length, 1);
  assert.equal(moved(), 1);
});

test('New Window from the launcher works even while the Panel view shows Tasks; that view becomes the pointer page', async () => {
  const p = make();
  const panelView = fakeView('claudeCodeTasks.panel', true);
  p.resolveWebviewView(panelView);
  assert.match(panelView.webview.html, /class="row head"|class="empty"/, 'Panel view shows the page');
  p.resolveWebviewView(fakeView('claudeCodeTasks.page'));
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(state.panels.length, 0, 'auto path leaves an open Panel copy alone');
  await p.handleMessage({ type: 'openWindow' });
  assert.equal(state.panels.length, 1, 'the explicit click opens the window');
  assert.match(panelView.webview.html, /Tasks is open in/, 'Panel copy is a pointer, not a second live page');
  assert.equal(state.commands.some((c) => c[0] === 'workbench.action.closePanel'), false, 'the bottom Panel is not collapsed');
  // closing the window brings Tasks back to the Panel view
  state.panels[0].dispose();
  assert.ok(state.commands.some((c) => c[0] === 'claudeCodeTasks.panel.focus'), 're-focuses the Panel view');
  assert.doesNotMatch(panelView.webview.html, /Tasks is open in/);
});

test('Panel from the launcher while a window owns Tasks closes the window and focuses the Panel view, not the pointer page', async () => {
  const p = make();
  p.resolveWebviewView(fakeView('claudeCodeTasks.page'));
  await new Promise((r) => setTimeout(r, 0));
  assert.equal(state.panels.length, 1);
  await p.handleMessage({ type: 'openPanel' });
  assert.equal(p.panel, undefined, 'the window/tab was disposed');
  assert.ok(state.commands.some((c) => c[0] === 'claudeCodeTasks.panel.focus'));
});

test('a source row in the launcher switches the source before opening the window', async () => {
  const p = make();
  p.source = 'jira';
  p.resolveWebviewView(fakeView('claudeCodeTasks.page'));
  await new Promise((r) => setTimeout(r, 0));
  await p.handleMessage({ type: 'openWindow', source: 'github' });
  assert.equal(p.source, 'github');
});

test('popOut twice reveals the existing window instead of moving an empty group', async () => {
  const p = make();
  await p.popOut();
  await p.popOut();
  assert.equal(state.panels.length, 1);
  assert.equal(moved(), 1);
  assert.ok(state.commands.some((c) => c[0] === 'panel.reveal'));
});

test('launcher markup is rebuilt only when its inputs change', () => {
  const p = make();
  const sidebar = fakeView('claudeCodeTasks.page');
  p.activatedAt = Date.now();
  p.resolveWebviewView(sidebar);
  const first = sidebar.webview.html;
  sidebar.webview.html = 'untouched';
  p.render();
  assert.equal(sidebar.webview.html, 'untouched', 'same inputs → no document reload');
  p.status = { ...p.status, jira: { ok: false, message: 'No token' } };
  p.render();
  assert.notEqual(sidebar.webview.html, 'untouched');
  assert.notEqual(sidebar.webview.html, first);
});

test('open: an https url reaches vscode.env.openExternal once; file:// and javascript: are refused', async () => {
  const p = make();
  await p.handleMessage({ type: 'open', url: 'https://example.com/x' });
  assert.deepEqual(state.commands.filter((c) => c[0] === 'openExternal'), [['openExternal', 'https://example.com/x']]);
  await p.handleMessage({ type: 'open', url: 'file:///etc/passwd' });
  await p.handleMessage({ type: 'open', url: 'javascript:alert(1)' });
  assert.equal(state.commands.filter((c) => c[0] === 'openExternal').length, 1);
});
