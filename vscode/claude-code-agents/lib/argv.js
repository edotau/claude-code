'use strict';
// argv builders for the claude-code binary and ssh — kept pure (plain arrays, no execFile) so they
// are unit-testable, and so the extension never shells out through a string.

/** Agent CLIs the dispatch command and the extension's quick-pick both allow. */
const AGENTS = ['claude', 'codex', 'opencode', 'gemini', 'copilot'];

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

/** argv for `<binary> usage [--days N] --format json`. */
function usageArgv(binary, opts) {
  const { days } = opts || {};
  const argv = [binary, 'usage'];
  if (days != null) argv.push('--days', String(days));
  argv.push('--format', 'json');
  return argv;
}

/** argv for `<binary> profile --format json`. */
function profileArgv(binary) {
  return [binary, 'profile', '--format', 'json'];
}

/** argv for `<binary> network vscode` or, with opts.stop, `<binary> network vscode --stop`. */
function vscodeServeArgv(binary, opts) {
  const { stop } = opts || {};
  const argv = [binary, 'network', 'vscode'];
  if (stop) argv.push('--stop');
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

module.exports = {
  AGENTS,
  dispatchStartArgv,
  usageArgv,
  profileArgv,
  vscodeServeArgv,
  attachShellArgs,
  attachCommandLine,
};
