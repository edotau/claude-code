'use strict';
// Contract: the Tasks page's declared surface — container, the one webview, commands, settings.
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

test('package.json: a claudeCodeTasks activitybar container exists with the tasks icon', () => {
  const tasks = pkg.contributes.viewsContainers.activitybar.find((c) => c.id === 'claudeCodeTasks');
  assert.ok(tasks, 'claudeCodeTasks activitybar container missing');
  assert.equal(tasks.icon, 'media/tasks.svg');
});

test('package.json: claudeCodeTasks declares exactly one webview view, claudeCodeTasks.page', () => {
  const views = pkg.contributes.views.claudeCodeTasks || [];
  assert.deepEqual(views.map((v) => v.id), ['claudeCodeTasks.page']);
  assert.equal(views[0].type, 'webview');
  assert.ok(pkg.activationEvents.includes('onView:claudeCodeTasks.page'));
});

test('package.json: a Panel container claudeCodeTasksPanel hosts the claudeCodeTasks.panel webview', () => {
  const panel = (pkg.contributes.viewsContainers.panel || []).find((c) => c.id === 'claudeCodeTasksPanel');
  assert.ok(panel, 'claudeCodeTasksPanel panel container missing');
  const views = pkg.contributes.views.claudeCodeTasksPanel || [];
  assert.deepEqual(views.map((v) => v.id), ['claudeCodeTasks.panel']);
  assert.equal(views[0].type, 'webview');
  assert.ok(pkg.activationEvents.includes('onView:claudeCodeTasks.panel'));
});

test('package.json: tasksOpenIn is a panel|sidebar|editor enum defaulting to panel, and Show Tasks exists', () => {
  const prop = findSetting('claudeCodeAgents.tasksOpenIn');
  assert.deepEqual(prop.enum, ['panel', 'sidebar', 'editor']);
  assert.equal(prop.default, 'panel');
  assert.ok(findCommand('claudeCodeTasks.show'), 'claudeCodeTasks.show command missing');
  assert.equal(findCommand('claudeCodeTasks.popOut').icon, '$(link-external)');
});

test('package.json: the three tree views are gone', () => {
  const ids = (pkg.contributes.views.claudeCodeTasks || []).map((v) => v.id);
  for (const old of ['claudeCodeTasks.branch', 'claudeCodeTasks.mine', 'claudeCodeTasks.reviewing']) assert.ok(!ids.includes(old), `${old} must be removed`);
  assert.equal(pkg.contributes.menus['view/item/context'], undefined);
});

test('package.json: refresh/openInEditor/open/startAgent/setupToken commands exist', () => {
  for (const id of ['claudeCodeTasks.refresh', 'claudeCodeTasks.openInEditor', 'claudeCodeTasks.open', 'claudeCodeTasks.startAgent', 'claudeCodeTasks.setupToken']) {
    assert.ok(findCommand(id), `${id} command missing`);
  }
});

test('package.json: Open in New Window is the only Tasks view-title button (drag the tab to the editor instead; refresh is in the page)', () => {
  const entries = (pkg.contributes.menus['view/title'] || []).filter((m) => m.command.startsWith('claudeCodeTasks.'));
  for (const view of ['claudeCodeTasks.page', 'claudeCodeTasks.panel']) {
    const cmds = entries.filter((m) => m.when === `view == ${view}`).map((m) => m.command).sort();
    assert.deepEqual(cmds, ['claudeCodeTasks.popOut'], `view/title entries for ${view}`);
  }
});

test('package.json: the Tasks page settings exist with their defaults', () => {
  assert.equal(findSetting('claudeCodeAgents.tasksDefaultSource').default, 'jira');
  assert.deepEqual(findSetting('claudeCodeAgents.tasksDefaultSource').enum, ['jira', 'github', 'bitbucket']);
  assert.deepEqual(findSetting('claudeCodeAgents.tasksVisibleSources').default, ['jira', 'github', 'bitbucket']);
  assert.deepEqual(findSetting('claudeCodeAgents.tasksJiraProjects').default, []);
  assert.equal(findSetting('claudeCodeAgents.tasksDefaultQuery.jira').default, 'is:issue is:open assignee:@me');
  assert.deepEqual(findSetting('claudeCodeAgents.tasksViewMode').enum, ['list', 'board']);
  assert.equal(findSetting('claudeCodeAgents.tailscaleBinary').default, 'tailscale');
  assert.deepEqual(findSetting('claudeCodeAgents.tasksGitHubRepos').default, []);
  assert.deepEqual(findSetting('claudeCodeAgents.tasksBitbucketRepos').default, []);
  assert.equal(findSetting('claudeCodeAgents.tasksDefaultQuery.github').default, 'is:open');
  assert.equal(findSetting('claudeCodeAgents.tasksDefaultQuery.bitbucket').default, 'is:pr is:open review-requested:@me');
  assert.equal(findSetting('claudeCodeAgents.tasksPageSize').default, 36);
  for (const id of ['claudeCodeAgents.tasksRefreshSeconds', 'claudeCodeAgents.tasksJiraBaseUrl', 'claudeCodeAgents.tasksStatusBar', 'claudeCodeAgents.bitbucketUrl']) {
    assert.ok(findSetting(id), `${id} setting missing`);
  }
});

test('extension.js: settings writes keep every source — no handler hard-codes the GitHub/Bitbucket pair', () => {
  const src = require('node:fs').readFileSync(require('node:path').join(__dirname, '..', 'extension.js'), 'utf8');
  assert.match(src, /const TASK_SOURCES = \['jira', 'github', 'bitbucket'\];/);
  assert.doesNotMatch(src, /\['github', 'bitbucket'\]\.filter/);
  assert.doesNotMatch(src, /msg\.source === 'github' \|\| msg\.source === 'bitbucket'/);
});

test('renderElsewhereHtml: a pointer page with Bring back here / Show it, scoped to the view that asked', () => {
  const { renderElsewhereHtml } = require('../lib/tasks-view');
  const floating = renderElsewhereHtml({ floating: true, viewType: 'claudeCodeTasks.panel', nonce: 'n' });
  assert.match(floating, /open in a separate window/);
  assert.match(floating, /"claudeCodeTasks\.panel"/);
  assert.match(floating, /type: 'bringBack'/);
  assert.match(floating, /type: 'revealElsewhere'/);
  assert.match(renderElsewhereHtml({ floating: false }), /open in an editor tab/);
  assert.doesNotMatch(floating, /class="row head"/); // never a second live list
});

test('extension.js: views render the pointer page while the editor tab owns Tasks, and the dispose handler re-focuses them', () => {
  const src = require('node:fs').readFileSync(require('node:path').join(__dirname, '..', 'extension.js'), 'utf8');
  assert.match(src, /this\.panel \? renderElsewhereHtml\(/);
  assert.match(src, /executeCommand\(`\$\{viewType\}\.focus`\)/);
  assert.match(src, /'workbench\.action\.closePanel'/);
});
