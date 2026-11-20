// Pi guard extension (internal/pi/guard.go renders __BIN__ and __MODULES__); runs the harness's PreToolUse guards.
import { execFileSync } from "node:child_process"
import { writeFileSync } from "node:fs"
import { resolve } from "node:path"

const BIN: string = __BIN__
const MODULES: { name: string; matcher: string; budgetMs: number }[] = __MODULES__
const TOOLS: Record<string, string> = { bash: "Bash", edit: "Edit", write: "Write", read: "Read", grep: "Grep", find: "Glob", ls: "Read" }
const NO_PATH_DEFAULT_CWD = new Set(["grep", "find", "ls"]) // these tools search the cwd when path is omitted

// toolInput is the Claude-shaped tool_input, or a string naming why the call cannot be judged (the caller blocks).
function toolInput(tool: string, input: any, cwd: string): Record<string, unknown> | string {
  if (typeof input !== "object" || input === null) return "tool input is not an object"
  if (tool === "bash") return typeof input.command === "string" ? { command: input.command } : "bash call has no command"
  const raw = typeof input.path === "string" && input.path ? input.path : NO_PATH_DEFAULT_CWD.has(tool) ? "." : ""
  if (!raw) return tool + " call has no path"
  return { ...input, file_path: resolve(cwd, raw) }
}

// denied is the block reason ("" allows), or null when stdout is not a hook document.
function denied(stdout: string): string | null {
  if (!stdout.trim()) return "" // a silent allow prints nothing
  let out: any
  try {
    out = JSON.parse(stdout).hookSpecificOutput ?? {}
  } catch {
    return null
  }
  if (out.permissionDecision === "ask") return out.permissionDecisionReason || "approval required; blocked" // no one to ask
  return out.permissionDecision === "deny" ? out.permissionDecisionReason || "denied" : ""
}

// verdict is one module's block reason, or "" to allow; every failure to run or read the module blocks.
function verdict(m: { name: string; budgetMs: number }, payload: string): string {
  const stuck = (why: string) => "pi guard: " + m.name + " did not complete (" + why + "); blocked"
  try {
    const out = execFileSync(BIN, ["hook", "PreToolUse", "--" + m.name],
      { input: payload, timeout: m.budgetMs, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] })
    return denied(out) ?? stuck("malformed output")
  } catch (err: any) {
    if (err.status === 2) return String(err.stderr ?? "").trim() || "pi guard: " + m.name + " blocked the call"
    if (err.code === "EPIPE" && err.status === 0) return denied(String(err.stdout ?? "")) ?? stuck("malformed output")
    return stuck(err.signal ? "timed out (" + err.signal + ")" : String(err.code ?? "exit " + err.status))
  }
}

// judge is the block reason for a tool call, or "" to let it run.
function judge(name: string, input: any, cwd: string, sid: string): string {
  const tool = TOOLS[name]
  if (!tool) return "pi guard: tool " + name + " is not mapped" // unknown and package tools are never allowed
  const ti = toolInput(name, input, cwd)
  if (typeof ti === "string") return "pi guard: " + ti
  const payload = JSON.stringify({ hook_event_name: "PreToolUse", tool_name: tool, tool_input: ti, cwd, session_id: sid,
    harness_agent: "pi" })
  for (const m of MODULES) {
    if (!new RegExp("^(?:" + m.matcher + ")$", "i").test(tool)) continue
    const reason = verdict(m, payload)
    if (reason) return reason
  }
  return ""
}

export default function (pi: any) {
  const file = process.env.HARNESS_PI_GUARD_NONCE_FILE, nonce = process.env.HARNESS_PI_GUARD_NONCE
  if (!file || !nonce) throw new Error("pi guard: nonce env is not set") // a guard that cannot prove it loaded aborts the run
  writeFileSync(file, nonce, { mode: 0o600 })
  pi.on("tool_call", async (ev: any, ctx: any) => {
    try {
      const reason = judge(String(ev.toolName), ev.input, String(ctx?.cwd ?? ""), String(ctx?.sessionManager?.getSessionId?.() ?? ""))
      return reason ? { block: true, reason } : undefined
    } catch (err: any) {
      return { block: true, reason: "pi guard: " + String(err?.message ?? err) + "; blocked" }
    }
  })
}
