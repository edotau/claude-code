# The Four Principles — in depth

LLM-written code fails in four characteristic ways. These principles are the discipline
lens that catches them; each maps to a detector that makes the mechanical part of the check
deterministic.

> **Attribution.** The four principles are adapted from Andrej Karpathy's observations on
> LLM coding pitfalls (X post, 2025). The detector tooling, anti-pattern gallery, and
> enforcement levels in this skill are original.

---

## #1 — Think Before Coding

> *State assumptions explicitly. If uncertain, ask. If multiple interpretations exist,
> present them — don't pick silently.*

The failure: an ambiguous requirement gets one interpretation, silently. The code looks
finished, but it answered a question nobody confirmed. The damage is invisible until it
ships against the wrong assumption.

**In review:** a change that picked an interpretation without surfacing it is a
**question**, not a defect — ask which interpretation was intended.

**Detector:** `claude-code review assumptions` on the plan. Flags minimizing language, hopeful
phrasing, absolute scope, and plan blocks with no verification step.

---

## #2 — Simplicity First

> *No abstractions for single-use code. If you write 200 lines and it could be 50,
> rewrite it.*

The failure: speculative generality. A factory for one product, a Protocol with one
implementer, error handling for states that can't occur, a config knob nobody turns. Each
adds surface area and reading cost for flexibility that never pays off.

**In review:** *would a senior engineer call this overcomplicated?* If yes → **should-fix**.
The bar is "minimum code that solves the actual problem", not the imagined future one.

**Detector:** `claude-code review complexity`. Per-function complexity/length/nesting
(estimate for Python, heuristics for TS/JS; Go not analyzed), class density, and premature ABC/Protocol in small files.

---

## #3 — Surgical Changes

> *Every changed line should trace directly to the request.*

The failure: drive-by edits. Reformatting an untouched function, swapping quote styles,
churning comments, "while I'm here" refactors. They inflate the diff, bury the real change,
and make review and `git blame` harder.

**In review:** diff noise is a **nit** — or **should-fix** when it hides the substantive
change. The test: can each changed line be traced to the stated task?

**Detector:** `claude-code review diff`. Reports a noise ratio over whitespace/comment churn,
docstring adds, and quote-style swaps.

---

## #4 — Goal-Driven Execution

> *Define success criteria. Loop until verified. Don't say what to do — give success
> criteria and watch it go.*

The failure: "looks right." Work declared done with no concrete check — no test added, no
command run, no observable criterion. It passes review on vibes and fails in production.

**In review:** missing verification is **should-fix**. Ask for the concrete check: a test,
an assertion, an exit code, a status code, a metric threshold.

**Detector:** `claude-code review goals`. Scores each plan step 0–3 on verification quality and
flags a missing final/end-to-end check.

---

## How the lens fits the review

Run the detectors **first** (mechanical pass), then apply judgment on top. The detectors
are advisory — they point at hot spots; you decide severity. A clean detector run does not
mean the change is good; it means the mechanical violations are absent and you can spend
attention on correctness, architecture, and the security pass (`security-reviewer`).
