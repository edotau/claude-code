---
name: network-browser
description: Browser work in this harness, locally or across machines - which browser to use and how to set it up (`claude-code network browser`), opening a URL where the person actually sits (Mac, WSL, VS Code Remote-SSH, a Tailscale host, `$BROWSER`, xdg-open), and driving a browser with the Claude in Chrome tools (their own signed-in Chrome) or the Claude desktop app's built-in browser pane. Use it whenever a task needs a real browser or a link opened, even if the person never says "browser" - "open this link", "show me the page", "log in to the Databricks UI", "the URL didn't open", "open it on my Mac", "use chrome", "set up the extension", "click through this site", "fill this form in my browser", "screenshot this page", "which browser do I have".
allowed-tools: mcp__claude-in-chrome, mcp__Claude_Browser, mcp__remote-devices__Claude_Browser
---

# Network browser

Three jobs, in order: know the machine's browsers, open links where the person sits, then drive a browser when
a task needs clicks. Loading this skill pre-approves the browser tools for the turn it loads in (`allowed-tools`);
the auto-mode classifier may still review some Chrome actions.

## 1. Know the browsers

`claude-code network browser` lists the browsers, the system default (Linux: from `xdg-settings`/`xdg-mime`) and
each one's control paths; an installed browser that is not ready gets an indented `fix:` line with the exact next
step (`fix` field with `--format json`). Run it on the machine where the browser is.

| Column | Meaning |
| --- | --- |
| `host` | the native-messaging host the Claude in Chrome extension talks through: `ready`, `no-host`, `unsupported` |
| `extension` | the Claude extension: `ready`, `disabled`, `no-extension`, `no-host`, `not-installed`, `unsupported` |
| `cdp` | Chrome DevTools Protocol, the WSL control path: `available`, `ready`, `not-installed`, `unsupported` |

Chromium-family browsers (chrome, edge, brave, arc, vivaldi, chromium) can run the Claude extension; Safari and
Firefox have no Claude extension. `--setup --browser <b>` acts where it can: on WSL it starts the Windows browser with
CDP and registers the chrome-devtools MCP; elsewhere it opens the extension install page in the named browser (Linux:
`gtk-launch`, the browser's binary, else `xdg-open`) or prints the fix. It never writes the native host: `claude
--chrome` does. `--dry-run` shows the actions first.

## 2. Open a link where the person sits

Use `claude-code network browser --open <url>` (or a file path) rather than calling `open` or `xdg-open` yourself:
it knows where the screen is. Install writes `~/.claude/bin/browser`, which runs the same verb; it does not set
`$BROWSER`. To route other tools' links (CLIs that honour `$BROWSER`) the same way, the person adds
`export BROWSER=~/.claude/bin/browser` to their shell rc on that machine; `echo $BROWSER` shows whether it is set.
The route the verb picks, first match wins:

| Situation | Where the link opens |
| --- | --- |
| `BROWSER=none` | printed, for the person to click |
| `$BROWSER` names another program | that program |
| A sign-in link (device code, OAuth) | this machine's desktop (WSL: the Windows desktop of that box), never VS Code |
| A live VS Code window is attached (Remote-SSH included) | the person's own machine, through that window |
| Local session, no SSH | this desktop: `open` (macOS), `wslview` or PowerShell `Start-Process` (WSL), `xdg-open` (Linux) |
| SSH with no VS Code window | printed |

When the person works from a Mac into WSL or a Linux box over VS Code Remote-SSH or Tailscale SSH, the VS Code row is
what puts the page on their Mac screen. If it printed instead, no VS Code window is attached: give them the URL, or
open it on their machine with `claude-code --host <mac-host> network browser --open <url>` (runs the verb on that
tailnet host over ssh; `--host auto` uses `CLAUDE_REMOTE_HOST`). Sign-in links are the exception: they stay on the
machine that asked, so a login run on a remote box opens on that box's desktop or prints. To sign in from your own
screen, start it from your own machine: `claude-code --host <box> databricks login` runs it on the box and forwards
the callback port back, and `--host-browser` opens the box's browser instead (`dbx-expert` skill).

## 3. Drive a browser

| Browser | Tools | Read first |
| --- | --- | --- |
| The person's own Chrome, with their sign-ins | `mcp__claude-in-chrome__*` | the `chrome-browser` skill |
| The Claude desktop app's browser pane | `mcp__Claude_Browser__*` (or `mcp__remote-devices__Claude_Browser__*`) | the `built-in-browser` skill |

The Chrome tools act in the Chrome on the machine where the extension runs, usually the person's Mac, not the remote
box this session may be on. If the tools do not answer, run `claude-code network browser` there and fix the row whose
`extension` or `host` is not `ready` with `--setup`.

- Load every browser tool you need in one ToolSearch call, look at the open tabs first, and work in a new tab.
- Never trigger an alert, confirm or prompt dialog: it blocks the extension until the person dismisses it.
- After two or three failed attempts at the same action, stop and ask the person how to proceed.
