'use strict';
// Contract: the Tasks panel's declared surface — container, three views, commands, settings.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const pkg = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'package.json'), 'utf8'));

function findCommand(id) {
  return (pkg.contributes.commands || []).find((c) => c.command === id);
}

function findSetting(id) {
  return pkg.contributes.configuration.properties[id];
}

test('package.json: a claudeCodeTasks activitybar container exists', () => {
  const containers = pkg.contributes.viewsContainers.activitybar;
  const tasks = containers.find((c) => c.id === 'claudeCodeTasks');
  assert.ok(tasks, 'claudeCodeTasks activitybar container missing');
});

test('package.json: claudeCodeTasks declares the branch/mine/reviewing views', () => {
  const views = pkg.contributes.views.claudeCodeTasks || [];
  const ids = views.map((v) => v.id);
  assert.ok(ids.includes('claudeCodeTasks.branch'), 'claudeCodeTasks.branch view missing');
  assert.ok(ids.includes('claudeCodeTasks.mine'), 'claudeCodeTasks.mine view missing');
  assert.ok(ids.includes('claudeCodeTasks.reviewing'), 'claudeCodeTasks.reviewing view missing');
});

test('package.json: claudeCodeTasks.refresh/open/startAgent commands exist', () => {
  assert.ok(findCommand('claudeCodeTasks.refresh'), 'claudeCodeTasks.refresh command missing');
  assert.ok(findCommand('claudeCodeTasks.open'), 'claudeCodeTasks.open command missing');
  assert.ok(findCommand('claudeCodeTasks.startAgent'), 'claudeCodeTasks.startAgent command missing');
});

test('package.json: tasksRefreshSeconds/tasksJiraBaseUrl/tasksStatusBar settings exist', () => {
  const refresh = findSetting('claudeCodeAgents.tasksRefreshSeconds');
  assert.ok(refresh, 'claudeCodeAgents.tasksRefreshSeconds setting missing');
  assert.equal(refresh.type, 'number');

  const jiraBaseUrl = findSetting('claudeCodeAgents.tasksJiraBaseUrl');
  assert.ok(jiraBaseUrl, 'claudeCodeAgents.tasksJiraBaseUrl setting missing');
  assert.equal(jiraBaseUrl.type, 'string');

  const statusBar = findSetting('claudeCodeAgents.tasksStatusBar');
  assert.ok(statusBar, 'claudeCodeAgents.tasksStatusBar setting missing');
});

test('package.json: claudeCodeTasks.refresh is wired into view/title for all three Tasks views', () => {
  const menuEntries = (pkg.contributes.menus['view/title'] || []).filter(
    (m) => m.command === 'claudeCodeTasks.refresh'
  );
  const whens = menuEntries.map((m) => m.when);
  for (const view of ['claudeCodeTasks.branch', 'claudeCodeTasks.mine', 'claudeCodeTasks.reviewing']) {
    assert.ok(whens.some((w) => w.includes(view)), `no view/title refresh entry for ${view}`);
  }
});
