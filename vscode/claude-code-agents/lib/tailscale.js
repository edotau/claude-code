'use strict';
// Tailscale exit node for the Task Sources section. The peer list comes from the harness (`<binary> network tailscale
// --format json`, CLI-only, no API key); the active exit node and the switch use the local `tailscale` CLI, also keyless.

function harnessStatusArgs() {
  return ['network', 'tailscale', '--format', 'json'];
}

function cliStatusArgs() {
  return ['status', '--json'];
}

/** `--exit-node=<ip|hostname>`; an empty target clears the exit node. LAN access stays reachable while routed. */
function setExitNodeArgs(target) {
  const t = String(target || '').trim();
  return ['set', `--exit-node=${t}`, ...(t ? ['--exit-node-allow-lan-access=true'] : [])];
}

function dnsLabel(dnsName) {
  return String(dnsName || '').replace(/\.$/, '');
}

/** Harness status JSON → `{self, backendState, options}`; options = peers advertising an exit node (`ExitNodeOption`) or in use. */
function parseHarnessStatus(json) {
  const data = typeof json === 'string' ? JSON.parse(json || '{}') : json || {};
  const options = (data.Peers || [])
    .filter((p) => p.ExitNodeOption || p.ExitNode)
    .map((p) => ({ dnsName: dnsLabel(p.DNSName), hostName: dnsLabel(p.DNSName).split('.')[0], ip: (p.IPs || [])[0] || '', os: p.OS || '', online: !!p.Online, selected: !!p.ExitNode }))
    .sort((a, b) => Number(b.online) - Number(a.online) || a.hostName.localeCompare(b.hostName));
  return { self: dnsLabel(data.DNSName), backendState: data.BackendState || '', options };
}

/** Raw `tailscale status --json` → the active exit node's first IP (or null) and its advertised peers (ExitNodeOption). */
function parseCliStatus(json) {
  const data = typeof json === 'string' ? JSON.parse(json || '{}') : json || {};
  const active = data.ExitNodeStatus && Array.isArray(data.ExitNodeStatus.TailscaleIPs) ? String(data.ExitNodeStatus.TailscaleIPs[0] || '').replace(/\/\d+$/, '') : null;
  const advertised = Object.values(data.Peer || {})
    .filter((p) => p.ExitNodeOption || p.ExitNode)
    .map((p) => ({ dnsName: dnsLabel(p.DNSName), hostName: p.HostName || dnsLabel(p.DNSName).split('.')[0], ip: (p.TailscaleIPs || [])[0] || '', os: p.OS || '', online: !!p.Online, selected: !!p.ExitNode }));
  return { activeIp: active, advertised };
}

function run(execFileImpl, binary, args) {
  const execFile = execFileImpl || require('node:child_process').execFile;
  return new Promise((resolve, reject) => {
    execFile(binary, args, { timeout: 15000, maxBuffer: 8 * 1024 * 1024 }, (err, stdout, stderr) => {
      if (err) {
        err.stderr = stderr;
        if (err.code === 'ENOENT') err.message = `${binary} not found`;
        return reject(err);
      }
      resolve(String(stdout || ''));
    });
  });
}

/** `{ok, self, exitNode, options}`: harness list, CLI selection; `{ok:false, message}` when neither answers. */
async function status({ binary = 'claude-code', tailscaleBinary = 'tailscale', execFileImpl } = {}) {
  let harness = null;
  let cli = null;
  const errors = [];
  try {
    harness = parseHarnessStatus(await run(execFileImpl, binary, harnessStatusArgs()));
  } catch (err) {
    errors.push((err.stderr && String(err.stderr).trim().split('\n')[0]) || err.message);
  }
  try {
    cli = parseCliStatus(await run(execFileImpl, tailscaleBinary, cliStatusArgs()));
  } catch (err) {
    errors.push((err.stderr && String(err.stderr).trim().split('\n')[0]) || err.message);
  }
  if (!harness && !cli) return { ok: false, message: errors[0] || 'Tailscale unavailable' };
  const base = harness || { self: '', backendState: 'Running', options: [] };
  const byIp = new Map(base.options.map((o) => [o.ip, o]));
  for (const p of (cli && cli.advertised) || []) if (p.ip && !byIp.has(p.ip)) byIp.set(p.ip, p); // peers the harness list lacks
  const options = [...byIp.values()].map((o) => ({ ...o, selected: cli && cli.activeIp ? o.ip === cli.activeIp : !!o.selected }));
  options.sort((a, b) => Number(b.selected) - Number(a.selected) || Number(b.online) - Number(a.online) || a.hostName.localeCompare(b.hostName));
  if (base.backendState && base.backendState !== 'Running') return { ok: false, message: `Tailscale is ${base.backendState.toLowerCase()}`, self: base.self, options, exitNode: null };
  return { ok: true, self: base.self, options, exitNode: options.find((o) => o.selected) || null };
}

async function setExitNode(target, { tailscaleBinary = 'tailscale', execFileImpl } = {}) {
  await run(execFileImpl, tailscaleBinary, setExitNodeArgs(target));
  return { ok: true };
}

module.exports = { harnessStatusArgs, cliStatusArgs, setExitNodeArgs, parseHarnessStatus, parseCliStatus, status, setExitNode };
