'use strict';
// Contract: claudeCodeAgents.servePhone / servePhoneStop exist as commands only — the "serve"
// workflow is driven from the command palette/status bar, never wired into a tree-item or editor menu.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const pkg = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'package.json'), 'utf8'));

function findCommand(id) {
  return (pkg.contributes.commands || []).find((c) => c.command === id);
}

test('package.json declares claudeCodeAgents.servePhone with title "Serve for Phone (Tailscale)"', () => {
  const cmd = findCommand('claudeCodeAgents.servePhone');
  assert.ok(cmd, 'claudeCodeAgents.servePhone is not declared in contributes.commands');
  assert.equal(cmd.title, 'Serve for Phone (Tailscale)');
});

test('package.json declares claudeCodeAgents.servePhoneStop with title "Stop Serving for Phone"', () => {
  const cmd = findCommand('claudeCodeAgents.servePhoneStop');
  assert.ok(cmd, 'claudeCodeAgents.servePhoneStop is not declared in contributes.commands');
  assert.equal(cmd.title, 'Stop Serving for Phone');
});

test('servePhone/servePhoneStop are not wired into any editor/title or view/title menu', () => {
  const menus = pkg.contributes.menus || {};
  const ids = ['claudeCodeAgents.servePhone', 'claudeCodeAgents.servePhoneStop'];
  for (const group of ['editor/title', 'view/title']) {
    for (const entry of menus[group] || []) {
      assert.ok(
        !ids.includes(entry.command),
        `${entry.command} must not appear in contributes.menus["${group}"]`
      );
    }
  }
});
