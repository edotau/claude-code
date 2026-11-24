// Pi guard extension (internal/pi/guard.go renders the __ constants): the only tools Pi has are these, each judged first.
import { execFile, execFileSync, spawn } from "node:child_process"
import { promisify } from "node:util"
import { existsSync, realpathSync, writeFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import * as pkg from "@earendil-works/pi-coding-agent"

// Pi imports an -e file by its real path but reports the path as given, so both sides compare as real paths
const SELF: string = realpathSync(fileURLToPath(import.meta.url))
const BIN: string = __BIN__
const execFileAsync = promisify(execFile)
const ERR_CHARS = 2000 // the most of a failed action's stderr a tool error carries
const VERIFIED_PI: string = __PI_VERSION__ // the Pi version whose tool code this guard was checked against
const BUDGET_MS: number = __BUDGET_MS__
const FACTORIES: Record<string, string> = { bash: "createBashToolDefinition", edit: "createEditToolDefinition",
  write: "createWriteToolDefinition", read: "createReadToolDefinition", grep: "createGrepToolDefinition",
  find: "createFindToolDefinition", ls: "createLsToolDefinition" }

// hookEnv is the inherited env minus every HARNESS_HOOK_* switch (none can skip) and the HARNESS_PI_GUARD_* nonce.
function hookEnv(): Record<string, string> {
  const env: Record<string, string> = {}
  for (const [k, v] of Object.entries(process.env)) if (v !== undefined && !/^HARNESS_(HOOK|PI_GUARD)_/i.test(k)) env[k] = v
  return env
}

// Step is one argv Go built from a verify line, with the GOOS/GOARCH/CGO_ENABLED assignments it carries.
type Step = { env: string[]; argv: string[] }
const strArray = (a: any): boolean => Array.isArray(a) && a.every((s: any) => typeof s === "string")
const isStep = (s: any): boolean => strArray(s?.env) && strArray(s?.argv) && s.argv.length > 0

// judge sends the raw call to the harness, which normalizes the path and runs every hook; only {allow:true} allows.
type Verdict = { block?: string; path?: string; run?: string[]; steps?: Step[]; unset?: string[]; usage?: string; notice?: string }
function judge(tool: string, input: any, cwd: string, sid: string): Verdict {
  const stuck = (why: string) => ({ block: "pi guard: " + why + "; blocked" })
  try {
    const out = execFileSync(BIN, ["hook", "PreToolUse", "--pi-guard"], { input: JSON.stringify({ tool, input, cwd, session_id: sid }),
      env: hookEnv(), timeout: BUDGET_MS, encoding: "utf8", stdio: ["pipe", "pipe", "pipe"] })
    const v = JSON.parse(out)
    if (typeof v.block === "string" && v.block) return { block: v.block }
    if (typeof v.usage === "string" && v.usage && v.allow !== true) return { usage: v.usage }
    if (v.allow !== true) return stuck("unrecognized verdict")
    if (v.unset !== undefined && !strArray(v.unset)) return stuck("malformed unset") // fail closed: never run with it
    return allowVerdict(v)
  } catch (err: any) {
    return stuck(err.signal ? "timed out (" + err.signal + ")" : String(err.code ?? err.message ?? "exit " + err.status))
  }
}

// allowVerdict keeps only the well-typed fields of an allow; a malformed run or steps is dropped, so nothing runs.
function allowVerdict(v: any): Verdict {
  return { path: typeof v.path === "string" ? v.path : "", run: strArray(v.run) ? v.run : undefined,
    steps: Array.isArray(v.steps) && v.steps.every(isStep) ? v.steps : undefined, unset: v.unset ?? [],
    notice: typeof v.notice === "string" && v.notice ? v.notice : undefined }
}

// running holds the tool calls in flight; handback checks it, so a handback is the last thing the run does.
const running = new Set<string>()

// The contract package's enums (contract.TestsValues, contract.Methods); a Go test pins these spellings.
const HANDBACK_TESTS = ["judged", "executed", "unknown"]
const HANDBACK_METHODS = ["solo", "fleet"]

// handbackError names the first missing or wrong handback field, or "" when the call is complete.
function handbackError(p: any): string {
  if (!["DONE", "DONE_WITH_CONCERNS", "NEEDS_CONTEXT", "BLOCKED"].includes(p?.status)) return "status must be DONE, DONE_WITH_CONCERNS, NEEDS_CONTEXT or BLOCKED"
  if (typeof p.summary !== "string" || !p.summary.trim()) return "summary must be a non-empty string"
  if (!Array.isArray(p.unfixed) || p.unfixed.some((s: any) => typeof s !== "string")) return "unfixed must be an array of strings (empty if none)"
  return enumError("tests", p.tests, HANDBACK_TESTS) || enumError("method", p.method, HANDBACK_METHODS)
}

// enumError is "" for an omitted or listed value, else the retry text naming the field.
function enumError(field: string, v: any, allowed: string[]): string {
  return v === undefined || allowed.includes(v) ? "" : field + " must be one of " + allowed.join(", ") + " (or omitted)"
}

async function tracked<T>(id: string, fn: () => Promise<T>): Promise<T> {
  running.add(id)
  try { return await fn() } finally { running.delete(id) }
}

function guarded(name: string, def: any) {
  const inner = guardedRun(name, def)
  return { ...inner, execute: (id: string, ...rest: any[]) => tracked(id, () => inner.execute(id, ...(rest as [any, any, any, any]))) }
}

// guardedRun is def with execute wrapped: it runs only after the hooks allow, on the exact path they judged.
function guardedRun(name: string, def: any) {
  return { ...def, async execute(id: string, params: any, signal: any, onUpdate: any, ctx: any) {
    const sid = String(ctx?.sessionManager?.getSessionId?.() ?? "")
    const v = judge(name, params, String(ctx?.cwd || process.cwd()), sid)
    if (v.block) throw new Error(v.block)
    if (name === "bash") return withNotice(await def.execute(id, params, signal, onUpdate, ctx), v.notice)
    // fail closed: Pi's read tries other spellings of a missing file (NFD, curly quote); a missing path is never read
    if (name === "read" && !existsSync(v.path!)) throw new Error("File not found: " + v.path)
    return withNotice(await def.execute(id, { ...params, path: v.path }, signal, onUpdate, ctx), v.notice)
  } }
}

// withNotice appends the judge's notice (the contract budget line) to a tool result as one more text part.
function withNotice(res: any, notice?: string): any {
  if (!notice || !Array.isArray(res?.content)) return res
  return { ...res, content: [...res.content, { type: "text", text: notice }] }
}

// ACTIONS are plain JSON Schema tools: the model picks the action and typed arguments, Go builds the argv.
const ACTIONS: Record<string, any> = {
  gate: { label: "Gate", executionMode: "sequential",
    promptSnippet: "gate: run the repo's build, vet and test gate (use it instead of typing go test in bash)",
    description: "Run the repo's scoped build, vet and test gate; returns {gate,exit,changed,failures}",
    parameters: { type: "object", additionalProperties: false,
      properties: { scope: { type: "array", items: { type: "string" }, description: "dirs to limit the gate to" } } } },
  handback: { label: "Hand back", executionMode: "sequential", promptSnippet: "handback: report the task's outcome; call it once, last",
    constrainedSampling: { type: "json_schema", strict: "prefer" },
    description: "Finish: report the outcome of the task. Call once, last.",
    parameters: { type: "object", additionalProperties: false, required: ["status", "summary", "unfixed"],
      properties: { status: { enum: ["DONE", "DONE_WITH_CONCERNS", "NEEDS_CONTEXT", "BLOCKED"] },
        summary: { type: "string" }, unfixed: { type: "array", items: { type: "string" } },
        tests: { enum: HANDBACK_TESTS }, method: { enum: HANDBACK_METHODS } } } },
}

// stopTree ends the action's process tree: taskkill /T on Windows, SIGTERM (the gate reaps its tree) elsewhere.
function stopTree(child: any) {
  if (!child?.pid) return
  if (process.platform !== "win32") { child.kill("SIGTERM"); return }
  execFile("taskkill", ["/T", "/F", "/PID", String(child.pid)], { timeout: BUDGET_MS }, (err) => { if (err) child.kill() })
}

// action is a tool whose command, if any, is the argv Go returned after judging it; the extension builds none.
function action(name: string) {
  return { name, ...ACTIONS[name], async execute(id: string, params: any, signal: any, _onUpdate: any, ctx: any) {
    const cwd = String(ctx?.cwd || process.cwd())
    const v = judge(name, params, cwd, String(ctx?.sessionManager?.getSessionId?.() ?? ""))
    if (v.block) throw new Error(v.block)
    if (v.usage) throw new Error(name + ": " + v.usage)
    if (signal?.aborted) throw new Error(name + ": cancelled")
    if (name === "handback") { // a usage error of this tool, not a guard block: the model retries it last
      const bad = handbackError(params)
      if (bad) throw new Error("handback: " + bad + "; call it again with every field")
      if (running.size > 0) throw new Error("handback: other tool calls are still running; call handback last, alone")
    }
    if (!v.run) return { content: [{ type: "text", text: v.notice ?? "recorded" }], details: {} }
    const text = await tracked(id, () => runAction(name, v.run!, cwd, signal))
    return withNotice({ content: [{ type: "text", text }], details: {} }, v.notice)
  } }
}

// runAction runs Go's argv abortably: a cancelled call stops the tree; a failed launch or exit is a tool error.
async function runAction(name: string, run: string[], cwd: string, signal: any): Promise<string> {
  const p = execFileAsync(BIN, run, { cwd, env: hookEnv(), encoding: "utf8", maxBuffer: 64 << 20 })
  const stop = () => stopTree(p.child) // Node's abort kills only the child; the gate stops its own tree on SIGTERM
  signal?.addEventListener("abort", stop, { once: true })
  if (signal?.aborted) stop() // cancelled before the listener existed
  try { return (await p).stdout }
  catch (err: any) { throw new Error(name + " failed: " + String(err.stderr || err.message || err.code).slice(0, ERR_CHARS)) }
  finally { signal?.removeEventListener("abort", stop) }
}

// VERIFY_MS plus BUDGET_MS stays under the 5-minute idle watch (pi_test pins it); post-run verify covers the rest.
const VERIFY_MS = 4 * 60_000
// PROXY_KEY is the proxy session token's env (internal/pi KeyRef): verify steps run edited code, so they never get it.
const PROXY_KEY = "HARNESS_PROXY_API_KEY"

// verifyEnv is hookEnv without the proxy token and the credentials Go named, plus the step's GOOS/GOARCH/CGO_ENABLED.
function verifyEnv(step: Step, unset: string[]): Record<string, string> {
  const env = hookEnv()
  for (const name of [PROXY_KEY, ...unset]) delete env[name]
  for (const kv of step.env) if (kv.includes("=")) env[kv.slice(0, kv.indexOf("="))] = kv.slice(kv.indexOf("=") + 1)
  return env
}

// runStep runs one argv in its own group (execFile drops detached); launch failure (rtk on Windows) or timeout throws.
function runStep(step: Step, unset: string[], cwd: string, ms: number): Promise<{ code: number; out: string }> {
  return new Promise((resolve, reject) => {
    const child = spawn(step.argv[0], step.argv.slice(1), { cwd, env: verifyEnv(step, unset), detached: process.platform !== "win32",
      windowsHide: true, stdio: ["ignore", "pipe", "pipe"] })
    let out = ""
    const add = (s: string) => { out = masked(out + s).slice(-4 * ERR_CHARS) } // mask before the cut: no key fragment
    child.stdout.setEncoding("utf8").on("data", add).on("error", () => {}) // a pipe error must not crash Pi mid-settle
    child.stderr.setEncoding("utf8").on("data", add).on("error", () => {})
    const timer = setTimeout(() => { killTree(child); reject(new Error("timed out")) }, Math.max(1, ms))
    child.on("error", (err) => { clearTimeout(timer); reject(err) })
    child.on("close", (code) => { clearTimeout(timer); code === null ? reject(new Error("killed")) : resolve({ code, out }) })
  })
}

// killTree kills the step's group (taskkill /T on Windows); with no group of its own, the step alone.
function killTree(child: any) {
  if (process.platform === "win32") stopTree(child)
  else try { process.kill(-child.pid, "SIGKILL") } catch { child.kill("SIGKILL") }
  child.stdout?.destroy(); child.stderr?.destroy(); child.unref() // an escaped grandchild must not keep Pi alive
}

// masked is s with the proxy token's value cut out, should a step print its inherited env.
function masked(s: string): string {
  const key = process.env[PROXY_KEY]
  return key ? s.split(key).join("[redacted]") : s
}

// verifyFailure runs the steps Go built from the run's verify line: the failing output, or "" (passed or not run).
async function verifyFailure(line: string, cwd: string, sid: string): Promise<string> {
  const v = judge("verify", { verify: line }, cwd, sid)
  if (v.block || v.usage || !v.steps?.length) {
    process.stderr.write("pi guard: verify before settle not run: " + (v.block || v.usage || "no steps") + "\n")
    return ""
  }
  const deadline = Date.now() + VERIFY_MS
  for (const s of v.steps) {
    let r: { code: number; out: string }
    try { r = await runStep(s, v.unset ?? [], cwd, deadline - Date.now()) } catch (err: any) { // a fixing turn cannot help either
      const why = err?.code === "ENOENT" ? "cannot launch " + s.argv[0] + " (not on PATH)" : s.argv[0] + ": " + (err?.message ?? err)
      process.stderr.write("pi guard: verify before settle skipped: " + why + "\n")
      return ""
    }
    if (r.code !== 0) {
      return "The run's verify check failed (exit " + r.code + "). Fix the cause without changing the check, then finish.\n$ " +
        line + "\n" + r.out.slice(-ERR_CHARS)
    }
  }
  return ""
}

// verifyBeforeSettle gives a failing --verify one fixing turn per run; the post-run verify is still the record.
function verifyBeforeSettle(pi: any, line: string) {
  let tried = false
  pi.on("agent_before_settle", async (ev: any, ctx: any) => {
    if (tried || ev?.outcome !== "completed") return undefined
    tried = true
    const fail = await verifyFailure(line, String(ctx?.cwd || process.cwd()), String(ctx?.sessionManager?.getSessionId?.() ?? ""))
    if (!fail) return undefined
    return { entries: [...(ev.entries ?? []), { type: "custom_message", customType: "harness-verify", content: fail, display: true }], continue: true }
  })
}

export default function (pi: any) {
  // fail closed: a Pi this guard was not checked against may normalize or validate differently; throwing aborts the run
  if ((pkg as any).VERSION !== VERIFIED_PI) throw new Error("pi guard: Pi " + (pkg as any).VERSION + " is not the verified " + VERIFIED_PI)
  const names = String(process.env.HARNESS_PI_GUARD_TOOLS ?? "").split(",").filter(Boolean)
  const file = process.env.HARNESS_PI_GUARD_NONCE_FILE, nonce = process.env.HARNESS_PI_GUARD_NONCE
  if (!names.length || !file || !nonce) throw new Error("pi guard: tool or nonce env is not set") // cannot prove it loaded: abort
  for (const name of names) {
    if (ACTIONS[name]) { pi.registerTool(action(name)); continue }
    const make = (pkg as any)[FACTORIES[name] ?? ""]
    if (typeof make !== "function") throw new Error("pi guard: no tool factory for " + name)
    pi.registerTool(guarded(name, make(process.cwd())))
  }
  // pi.getAllTools() throws while extensions load, so a tool registered ahead of the guard is caught once Pi is bound
  const reals = new Map<string, string>()
  const real = (p: string): string => { // fail closed: a path that does not resolve is "" and never matches SELF
    if (!reals.has(p)) { try { reals.set(p, realpathSync(p)) } catch { reals.set(p, "") } }
    return reals.get(p)!
  }
  const ours = (name: string): boolean => {
    try { const p = pi.getAllTools().find((t: any) => t.name === name)?.sourceInfo?.path; return typeof p === "string" && real(p) === SELF }
    catch { return false } // fail closed
  }
  // codemode runs no tool itself: its nested calls come back through tool_call and the guarded tools' execute
  const codemode = process.env.HARNESS_PI_GUARD_CODEMODE === "1"
  const builtinCodemode = (): boolean => {
    try { return pi.getAllTools().find((t: any) => t.name === "codemode")?.sourceInfo?.path === "builtin:codemode" }
    catch { return false } // fail closed: a package that registers codemode replaces the builtin and is blocked
  }
  const allowed = (name: string): boolean => names.includes(name) ? ours(name) : codemode && name === "codemode" && builtinCodemode()
  // second layer: a call is judged only when the tool Pi runs under that name is the guard's own registration
  pi.on("tool_call", (ev: any) => allowed(String(ev.toolName)) ? undefined
    : { block: true, reason: "pi guard: tool " + ev.toolName + " is not a guarded tool" })
  // fail closed: the nonce is written only after every guarded name resolves to this file, so a shadowed tool means no guard
  pi.on("session_start", () => {
    if (names.every(ours) && (!codemode || builtinCodemode())) writeFileSync(file, nonce, { mode: 0o600 })
  })
  const verify = String(process.env.HARNESS_PI_GUARD_VERIFY ?? "") // empty: no verify set, no handler
  if (verify) verifyBeforeSettle(pi, verify)
}
