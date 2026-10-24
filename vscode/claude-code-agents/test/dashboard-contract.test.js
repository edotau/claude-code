'use strict';
// Contract: the extension is a single webview dashboard — the old tree view (claudeCodeAgents.tree) and
// its item commands are gone; only the webview view and the (non-tree) dispatch command remain.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const pkg = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'package.json'), 'utf8'));

function findCommand(id) {
  return (pkg.contributes.commands || []).find((c) => c.command === id);
}

test('package.json: the activitybar container title is "Agents Dashboard"', () => {
  const containers = pkg.contributes.viewsContainers.activitybar;
  const claudeCodeAgents = containers.find((c) => c.id === 'claudeCodeAgents');
  assert.ok(claudeCodeAgents, 'claudeCodeAgents activitybar container missing');
  assert.equal(claudeCodeAgents.title, 'Agents Dashboard');
});

test('package.json: the claudeCodeAgents.tree view no longer exists', () => {
  const views = pkg.contributes.views.claudeCodeAgents || [];
  assert.ok(!views.some((v) => v.id === 'claudeCodeAgents.tree'), 'claudeCodeAgents.tree view should be gone');
});

test('package.json: the tree-item commands (viewAgent/attach/copyAttachCommand/toggleShowAll) are gone', () => {
  for (const id of ['viewAgent', 'attach', 'copyAttachCommand', 'toggleShowAll']) {
    assert.equal(findCommand(`claudeCodeAgents.${id}`), undefined, `claudeCodeAgents.${id} should no longer be declared`);
  }
});

test('package.json: claudeCodeAgents.dispatch still exists', () => {
  const cmd = findCommand('claudeCodeAgents.dispatch');
  assert.ok(cmd, 'claudeCodeAgents.dispatch should remain');
  assert.equal(cmd.title, 'Dispatch…');
});
