// Pi guard extension (internal/pi/guard.go renders the __ constants): the only tools Pi has are these, each judged first.
import { execFileSync } from "node:child_process"
import { existsSync, writeFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import * as pkg from "@earendil-works/pi-coding-agent"

const SELF: string = fileURLToPath(import.meta.url)
const BIN: string = __BIN__
const VERIFIED_PI: string = __PI_VERSION__ // the Pi version whose tool code this guard was checked against
const BUDGET_MS: number = __BUDGET_MS__
const FACTORIES: Record<string, string> = { bash: "createBashToolDefinition", edit: "createEditToolDefinition",
  write: "createWriteToolDefinition", read: "createReadToolDefinition", grep: "createGrepToolDefinition",
  find: "createFindToolDefinition", ls: "createLsToolDefinition" }

// hookEnv is the hook process's env: the inherited one minus every HARNESS_HOOK_* switch (fail closed: none can skip).
function hookEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const [k, v] of Object.entries(process.env)) if (v !== undefined && !k.toUpperCase().startsWith("HARNESS_HOOK_")) env[k] = v
  return env
}

// judge sends the raw call to the harness, which normalizes the path and runs every hook; only {allow:true} allows.
function judge(tool: string, input: any, cwd: string, sid: string): { block?: string; path?: string } {
  const stuck = (why: string) => ({ block: "pi guard: " + why + "; blocked" })
  try {
    const out = execFileSync(BIN, ["hook", "PreToolUse", "--pi-guard"], { input: JSON.stringify({ tool, input, cwd, session_id: sid }),
      env: hookEnv(), timeout: BUDGET_MS, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] })
    const v = JSON.parse(out)
    if (typeof v.block === "string" && v.block) return { block: v.block }
    return v.allow === true ? { path: typeof v.path === "string" ? v.path : "" } : stuck("unrecognized verdict")
  } catch (err: any) {
    return stuck(err.signal ? "timed out (" + err.signal + ")" : String(err.code ?? err.message ?? "exit " + err.status))
  }
}

// guarded is def with execute wrapped: it runs only after the hooks allow, on the exact path they judged.
function guarded(name: string, def: any) {
  return { ...def, async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
    const sid = String(ctx?.sessionManager?.getSessionId?.() ?? "")
    const v = judge(name, params, String(ctx?.cwd || process.cwd()), sid)
    if (v.block) throw new Error(v.block)
    if (name === "bash") return def.execute(id, params, signal, onUpdate, ctx)
    // fail closed: Pi's read tries other spellings of a missing file (NFD, curly quote); a missing path is never read
    if (name === "read" && !existsSync(v.path!)) throw new Error("File not found: " + v.path)
    return def.execute(id, { ...params, path: v.path }, signal, onUpdate, ctx)
  } }
}

export default function (pi: any) {
  // fail closed: a Pi this guard was not checked against may normalize or validate differently; throwing aborts the run
  if ((pkg as any).VERSION !== VERIFIED_PI) throw new Error("pi guard: Pi " + (pkg as any).VERSION + " is not the verified " + VERIFIED_PI)
  const names = String(process.env.HARNESS_PI_GUARD_TOOLS ?? "").split(",").filter(Boolean)
  const file = process.env.HARNESS_PI_GUARD_NONCE_FILE, nonce = process.env.HARNESS_PI_GUARD_NONCE
  if (!names.length || !file || !nonce) throw new Error("pi guard: tool or nonce env is not set") // cannot prove it loaded: abort
  for (const name of names) {
    const make = (pkg as any)[FACTORIES[name] ?? ""]
    if (typeof make !== "function") throw new Error("pi guard: no tool factory for " + name)
    pi.registerTool(guarded(name, make(process.cwd())))
  }
  // pi.getAllTools() throws while extensions load, so a tool registered ahead of the guard is caught once Pi is bound
  const ours = (name: string): boolean => {
    try { return pi.getAllTools().find((t: any) => t.name === name)?.sourceInfo?.path === SELF } catch { return false } // fail closed
  }
  // second layer: a call is judged only when the tool Pi runs under that name is the guard's own registration
  pi.on("tool_call", (ev: any) => names.includes(String(ev.toolName)) && ours(String(ev.toolName)) ? undefined
    : { block: true, reason: "pi guard: tool " + ev.toolName + " is not a guarded tool" })
  // fail closed: the nonce is written only after every guarded name resolves to this file, so a shadowed tool means no guard
  pi.on("session_start", () => { if (names.every(ours)) writeFileSync(file, nonce, { mode: 0o600 }) })
}
