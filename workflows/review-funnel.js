export const meta = {
  name: "review-funnel",
  description:
    "Tiered review funnel: Sonnet dimension finders -> Sonnet refuter -> reasoning-tier verifier for survivors only; returns confirmed findings ranked by severity",
  whenToUse:
    'Thorough multi-dimension code review at ~1/3 the verify tokens (cheap refuters gate the expensive verifier). Pass args to scope: ["path", ...] or {paths: [...], dimensions: [...]}. Omit paths to review the working-tree diff.',
  phases: [
    {
      title: "Find",
      detail: "one Sonnet finder per review dimension over the target paths",
    },
    {
      title: "Refute",
      detail:
        "Sonnet refuter per finding — kills misreads, cosmetic, already-handled",
    },
    {
      title: "Verify",
      detail: "reasoning-tier verifier (effort high) for refute survivors only",
    },
  ],
};

// ── Dimensions ──────────────────────────────────────────────────────────────
// Override with args.dimensions: [{key, prompt}, ...]. Keys must be unique — they label agents.
const DEFAULT_DIMENSIONS = [
  {
    key: "correctness",
    prompt:
      "logic bugs, off-by-ones, wrong conditionals, broken error paths, unhandled nil/None/empty cases that produce wrong output or a crash",
  },
  {
    key: "security",
    prompt:
      "secrets in code, injection (shell/SQL/path), unsafe input handling, credential leaks in logs or errors, permission/authorization gaps",
  },
  {
    key: "regressions",
    prompt:
      "contract breaks: changed signatures/flags/outputs with un-updated callers, invariants the surrounding code or docs rely on that this code violates",
  },
];

// Normalize args -> {paths, dimensions}. Accepts: undefined | [path,...] | {paths, dimensions}.
function resolveScope(a) {
  if (Array.isArray(a))
    return { paths: a.filter(Boolean), dimensions: DEFAULT_DIMENSIONS };
  if (a && typeof a === "object") {
    return {
      paths: Array.isArray(a.paths) ? a.paths.filter(Boolean) : [],
      dimensions:
        Array.isArray(a.dimensions) && a.dimensions.length
          ? a.dimensions
          : DEFAULT_DIMENSIONS,
    };
  }
  return { paths: [], dimensions: DEFAULT_DIMENSIONS };
}

const FINDINGS_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["findings"],
  properties: {
    findings: {
      type: "array",
      items: {
        type: "object",
        additionalProperties: false,
        required: ["file", "line", "title", "detail", "severity"],
        properties: {
          file: { type: "string", description: "repo-relative path" },
          line: {
            type: "integer",
            description: "1-indexed line the finding anchors to",
          },
          title: { type: "string", description: "one-line claim, <=80 chars" },
          detail: {
            type: "string",
            description:
              "the defect + concrete failure scenario (inputs/state -> wrong outcome)",
          },
          severity: {
            type: "string",
            enum: ["critical", "high", "medium", "low"],
          },
        },
      },
    },
  },
};

const REFUTE_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["refuted", "reason"],
  properties: {
    refuted: {
      type: "boolean",
      description:
        "true ONLY when you can concretely show the finding is wrong, cosmetic, or already handled",
    },
    reason: { type: "string" },
  },
};

const VERDICT_SCHEMA = {
  type: "object",
  additionalProperties: false,
  required: ["confirmed", "reason", "severity"],
  properties: {
    confirmed: { type: "boolean" },
    reason: { type: "string" },
    severity: {
      type: "string",
      enum: ["critical", "high", "medium", "low"],
      description: "corrected severity (may differ from the finder's)",
    },
    suggestedFix: {
      type: "string",
      description: "one-paragraph minimal fix, when confirmed",
    },
  },
};

const { paths, dimensions } = resolveScope(args);
const scopeText = paths.length
  ? `Review ONLY these paths (recurse into directories): ${paths.join(", ")}`
  : "Review the current working-tree diff: run `git diff HEAD --stat` then read the changed files. If the tree is clean, review the HEAD commit (`git show HEAD`).";
log(
  `Funnel over ${paths.length ? paths.join(", ") : "working-tree diff"} — dimensions: ${dimensions.map((d) => d.key).join(", ")}`,
);

// ── The funnel: find -> per-finding refute -> verify survivors ──────────────
// pipeline(): dimension A's findings refute/verify while dimension B is still finding.
// Sequential per finding by design — half the verify tokens vs parallel 2x reasoning-tier.
const funneled = await pipeline(
  dimensions,
  (d) =>
    agent(
      `You are one dimension of a code-review fan-out. Read code, report findings — do NOT edit anything.\n\n` +
        `${scopeText}\n\n` +
        `Your ONE dimension — report only findings of this kind: ${d.prompt}\n\n` +
        `Rules: every finding needs a concrete failure scenario (inputs/state -> wrong outcome), not a style opinion. ` +
        `Anchor each to the exact file:line. An empty findings list is a valid result — do not pad.`,
      {
        label: `find:${d.key}`,
        phase: "Find",
        schema: FINDINGS_SCHEMA,
        agentType: "code-workers",
        effort: "medium",
      },
    ),
  (found, d) =>
    parallel(
      (found?.findings || []).map(
        (f) => () =>
          agent(
            `Adversarially REFUTE this code-review finding. Read the actual code at ${f.file}:${f.line} and around it.\n\n` +
              `Finding [${f.severity}] (${d.key}): ${f.title}\n${f.detail}\n\n` +
              `Set refuted=true ONLY when you can concretely show it is wrong (the reviewer misread the code), ` +
              `cosmetic (no behavioral consequence), or already handled elsewhere (cite where). ` +
              `If you cannot cleanly refute it — including genuine uncertainty — set refuted=false so a deeper verifier judges it.`,
            {
              label: `refute:${d.key}:${f.file}`,
              phase: "Refute",
              schema: REFUTE_SCHEMA,
              agentType: "code-workers",
              effort: "medium",
            },
          ).then((r) =>
            !r || r.refuted
              ? {
                  finding: f,
                  dimension: d.key,
                  killed: r?.reason || "refuter returned no result",
                }
              : agent(
                  `Final verification of a code-review finding that survived a first adversarial pass. ` +
                    `Read ${f.file}:${f.line} and every caller/dependency needed to be certain.\n\n` +
                    `Finding [${f.severity}] (${d.key}): ${f.title}\n${f.detail}\n\n` +
                    `Confirm ONLY if the failure scenario is real and reachable. Correct the severity if miscalibrated. ` +
                    `When confirmed, give the minimal fix.`,
                  {
                    label: `verify:${d.key}:${f.file}`,
                    phase: "Verify",
                    schema: VERDICT_SCHEMA,
                    effort: "high",
                  }, // funnel stage 2 is the one deliberate reasoning-tier call
                ).then((v) => ({ finding: f, dimension: d.key, verdict: v })),
          ),
      ),
    ),
);

// ── Report ──────────────────────────────────────────────────────────────────
const all = funneled.filter(Boolean).flat().filter(Boolean);
const confirmed = all.filter((x) => x.verdict?.confirmed);
const rank = { critical: 0, high: 1, medium: 2, low: 3 };
confirmed.sort(
  (a, b) => (rank[a.verdict.severity] ?? 4) - (rank[b.verdict.severity] ?? 4),
);
const killedAtRefute = all.filter((x) => x.killed).length;
const killedAtVerify = all.filter(
  (x) => x.verdict && !x.verdict.confirmed,
).length;
log(
  `Funnel: ${all.length} found -> ${all.length - killedAtRefute} past refute -> ${confirmed.length} confirmed (refuter killed ${killedAtRefute}, verifier killed ${killedAtVerify})`,
);

return {
  confirmed: confirmed.map((x) => ({
    severity: x.verdict.severity,
    dimension: x.dimension,
    file: x.finding.file,
    line: x.finding.line,
    title: x.finding.title,
    detail: x.finding.detail,
    reason: x.verdict.reason,
    suggestedFix: x.verdict.suggestedFix || "",
  })),
  stats: {
    found: all.length,
    killedAtRefute,
    killedAtVerify,
    confirmed: confirmed.length,
  },
};
