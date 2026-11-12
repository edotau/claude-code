# Access Control

The tailnet policy file (HuJSON, admin console → **Access Controls**) is deny-by-default. Write new access rules as
grants (SKILL.md → Authoring defaults); ACLs appear here only so you can read and migrate existing policies.

**Canonical reference:** <https://tailscale.com/docs/reference/grants-vs-acls> — WebFetch it for the full conversion
table, migration guidance or a capability schema you haven't seen. When a grant doesn't behave as expected, fetch
<https://tailscale.com/docs/reference/troubleshooting/grants>.

## ACL → grant conversion

```json
// ACL (legacy — for reading existing policies)
"acls": [
  {
    "action": "accept",
    "src": ["group:engineering"],
    "dst": ["tag:server:*"]
  }
]

// Grant (use this for new policies)
"grants": [
  {
    "src": ["group:engineering"],
    "dst": ["tag:server"],
    "ip": ["*"]
  }
]
```

Grants drop `"action"` (every grant is implicitly accept) and move the port spec out of the `dst` string into a
separate `"ip"` field. `"ip": ["*"]` means all ports; use `["tcp:22", "tcp:443"]` to restrict.

## Policy file sections

Top-level keys: `groups`, `tagOwners`, `grants`, `acls`, `ssh`, `autoApprovers` (plus `nodeAttrs`, `postures`).

```json
"groups": {
  "group:engineering": ["alice@example.com", "bob@example.com"]
},
"tagOwners": {
  "tag:server": ["group:ops"],
  "tag:ci": ["group:engineering"]
}
```

Tags give non-user devices (servers, CI runners, IoT) a service identity: the device is owned by the tag, not a user.
Apply tags via auth key at registration or from the admin console. Tagged devices have key expiry disabled by default.
Naming conventions: `tag:prod-app`, `tag:staging-db`, `tag:prod-emea-web`.

### Grants with application capabilities

```json
"grants": [
  {
    "src": ["group:engineering"],
    "dst": ["tag:nas"],
    "app": {
      "tailscale.com/cap/drive": [{
        "shares": ["docs", "media"]
      }]
    }
  }
]
```

SSH access is **not** a grant capability; it lives in the top-level `"ssh"` section.

### SSH rules

```json
"ssh": [
  {
    "action": "check",
    "src": ["group:ops"],
    "dst": ["tag:prod"],
    "users": ["root"],
    "checkPeriod": "8h"
  },
  {
    "action": "accept",
    "src": ["autogroup:member"],
    "dst": ["autogroup:self"],
    "users": ["autogroup:nonroot"]
  }
]
```

Actions: `accept`, `check` (user must re-sign in with their identity provider; default `checkPeriod` is 12 hours),
`deny`.

### Auto-approvers

```json
"autoApprovers": {
  "routes": {
    "10.0.0.0/8": ["tag:infra"]
  },
  "exitNode": ["tag:exit"]
}
```

## Targets and selectors

Usable as sources or destinations in grants/ACLs:

- Users `alice@example.com`, groups `group:engineering`, tags `tag:server`, IPs/CIDRs `192.168.1.0/24`.
- Autogroups: `autogroup:member` (all users), `autogroup:self` (same device), `autogroup:internet` (exit node
  traffic), `autogroup:nonroot` (non-root SSH users).

## Default policy

New tailnets start with "allow all": every member reaches every device on all ports, and can SSH to all devices as
non-root users. Tighten it for production.

## Just-in-time and least-privilege access

Grant narrow standing access with tags and groups, and layer time-bound elevation on top:

- **Accessbot** — JIT request-and-approve workflow in Slack; access expires automatically. The primary JIT tool.
- **Device posture attributes** — key-value pairs on devices that grants gate on (`srcPosture`), so access depends
  on device state, not just identity.
- **SSH check mode** (`"action": "check"`, `checkPeriod`) — re-authentication for high-risk sessions.

Fetch the pages below for current Accessbot setup and posture-attribute syntax before writing configuration.

| If the user wants to… | Fetch |
|---|---|
| Stop handing out standing admin access and grant elevated access only when needed | <https://tailscale.com/docs/features/access-control/just-in-time-access> |
| Add an approval step (for example in Slack) before someone gets elevated access | <https://tailscale.com/docs/features/tailscale-accessbot-jit> |
| Lock down production access to least-privilege for a compliance audit | <https://tailscale.com/docs/use-cases/infrastructure-access/manage-least-priviledged-and-jit-access> |
