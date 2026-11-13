# Access Control

The tailnet policy file (HuJSON, admin console → **Access Controls**) is deny-by-default. Write new rules as grants
(SKILL.md → Authoring defaults); ACLs appear here only to read and migrate existing policies.

WebFetch <https://tailscale.com/docs/reference/grants-vs-acls> for the full conversion table or an unfamiliar
capability schema; for a misbehaving grant, <https://tailscale.com/docs/reference/troubleshooting/grants>.

## ACL → grant conversion

```json
// ACL (legacy)
"acls": [{ "action": "accept", "src": ["group:engineering"], "dst": ["tag:server:*"] }]

// Grant
"grants": [{ "src": ["group:engineering"], "dst": ["tag:server"], "ip": ["*"] }]
```

Grants drop `"action"` (implicitly accept) and move the port spec out of `dst` into `"ip"`: `["*"]` is all ports,
`["tcp:22", "tcp:443"]` restricts.

## Policy file sections

Top-level keys: `groups`, `tagOwners`, `grants`, `acls`, `ssh`, `autoApprovers`, `nodeAttrs`, `postures`. New tailnets
start "allow all" (every member reaches every device, SSH as non-root); tighten for production.

```json
"groups": { "group:engineering": ["alice@example.com", "bob@example.com"] },
"tagOwners": { "tag:server": ["group:ops"], "tag:ci": ["group:engineering"] }
```

Tags give non-user devices (servers, CI, IoT) a service identity: the device is owned by the tag, not a user. Apply
them via auth key at registration or in the admin console; tagged devices have key expiry disabled by default.

### Grants with application capabilities

```json
"grants": [{
  "src": ["group:engineering"],
  "dst": ["tag:nas"],
  "app": { "tailscale.com/cap/drive": [{ "shares": ["docs", "media"] }] }
}]
```

SSH is **not** a grant capability; it lives in the top-level `"ssh"` section.

### SSH rules

```json
"ssh": [
  { "action": "check", "src": ["group:ops"], "dst": ["tag:prod"], "users": ["root"], "checkPeriod": "8h" },
  { "action": "accept", "src": ["autogroup:member"], "dst": ["autogroup:self"], "users": ["autogroup:nonroot"] }
]
```

Actions: `accept`, `check` (re-sign-in with the identity provider; default `checkPeriod` 12 hours), `deny`.

### Auto-approvers

```json
"autoApprovers": { "routes": { "10.0.0.0/8": ["tag:infra"] }, "exitNode": ["tag:exit"] }
```

## Targets and selectors

Sources/destinations: users `alice@example.com`, groups `group:engineering`, tags `tag:server`, IPs/CIDRs.
Autogroups: `autogroup:member` (all users), `autogroup:self` (same device), `autogroup:internet` (exit node traffic),
`autogroup:nonroot` (non-root SSH users).

## Just-in-time and least-privilege access

Grant narrow standing access with tags and groups; layer time-bound elevation on top:

- **Accessbot** — JIT request-and-approve in Slack; access expires automatically.
- **Device posture attributes** — key-value pairs grants gate on (`srcPosture`).
- **SSH check mode** (`"action": "check"`, `checkPeriod`) — re-authentication for high-risk sessions.

Fetch before writing Accessbot or posture config:

| If the user wants to… | Fetch |
|---|---|
| Replace standing admin access with elevated access on demand | <https://tailscale.com/docs/features/access-control/just-in-time-access> |
| Add an approval step (e.g. Slack) | <https://tailscale.com/docs/features/tailscale-accessbot-jit> |
| Least-privilege production access for an audit | <https://tailscale.com/docs/use-cases/infrastructure-access/manage-least-priviledged-and-jit-access> |
