'use strict';
// argv builders for the claude-code binary and ssh — kept pure (plain arrays, no execFile) so they
// are unit-testable, and so the extension never shells out through a string.

/** Agent CLIs the dispatch command and the extension's quick-pick both allow. */
const AGENTS = ['claude', 'codex', 'opencode', 'gemini', 'copilot'];

/** argv for `<binary> agents tree --days N [--all] --format json`. */
function treeArgv(binary, opts) {
  const { days, all } = opts || {};
  const argv = [binary, 'agents', 'tree', '--days', String(days != null ? days : 1)];
  if (all) argv.push('--all');
  argv.push('--format', 'json');
  return argv;
}

/** argv for `<binary> dispatch --list --host H --format json`. */
function dispatchListArgv(binary, host) {
  return [binary, 'dispatch', '--list', '--host', host, '--format', 'json'];
}

/** argv for `<binary> dispatch --host H --agent A [--dir D] <task>`. Throws on an unknown agent. */
function dispatchStartArgv(binary, opts) {
  const { host, agent, dir, task } = opts || {};
  if (!AGENTS.includes(agent)) {
    throw new Error(`dispatch: --agent ${JSON.stringify(agent)} must be one of ${AGENTS.join('|')}`);
  }
  const argv = [binary, 'dispatch', '--host', host, '--agent', agent];
  if (dir) argv.push('--dir', dir);
  argv.push(task);
  return argv;
}

/** argv for `<binary> web view <agentId> [--session S] --format json` (the click-through detail pane). */
function viewArgv(binary, agentId, sessionId) {
  const argv = [binary, 'web', 'view', agentId];
  if (sessionId) argv.push('--session', sessionId);
  argv.push('--format', 'json');
  return argv;
}

/** argv for `<binary> web view <agentId> --session S --follow` (text/plain event stream, no --format). */
function viewFollowArgv(binary, agentId, sessionId) {
  const argv = [binary, 'web', 'view', agentId];
  if (sessionId) argv.push('--session', sessionId);
  argv.push('--follow');
  return argv;
}

/** shellArgs for an `ssh -t host tmux attach -t session` terminal. */
function attachShellArgs(host, session) {
  return ['-t', host, 'tmux', 'attach', '-t', session];
}

/** The attach command line shown to the user as text (matches the Go CLI's AttachCommand). */
function attachCommandLine(host, session) {
  return ['ssh', ...attachShellArgs(host, session)].join(' ');
}

module.exports = { AGENTS, treeArgv, dispatchListArgv, dispatchStartArgv, viewArgv, viewFollowArgv, attachShellArgs, attachCommandLine };
