'use strict';
// USD per 1M tokens from the harness's claude-api skill (cached 2026-10-06); non-Anthropic models stay unpriced.
// Cache write defaults to 1.25× input and cache read to 0.1× input unless Anthropic publishes a figure.
const ANTHROPIC_MODELS = [
  { pattern: /fable-5/, input: 10, output: 50, cacheRead: 0.25 },
  { pattern: /mythos-5/, input: 10, output: 50, cacheRead: 0.25 },
  { pattern: /opus-5-5/, input: 4, output: 20, cacheRead: 0.2 },
  { pattern: /opus-5|opus-4-[678]/, input: 5, output: 25 },
  { pattern: /sonnet-5-5/, input: 2, output: 10, cacheRead: 0.2 },
  { pattern: /sonnet-5/, input: 2, output: 10 },
  { pattern: /sonnet-4-6/, input: 3, output: 15 },
  { pattern: /haiku-5-5/, input: 0.1, output: 0.5 },
  { pattern: /haiku-4-5/, input: 1, output: 5 },
  // Older or unversioned names fall back to the tier's last published rate.
  { pattern: /opus/, input: 5, output: 25 },
  { pattern: /sonnet/, input: 3, output: 15 },
  { pattern: /haiku/, input: 1, output: 5 },
];

/** USD/1M-token rates {input, output, cacheRead, cacheWrite} for an Anthropic model name, or null otherwise. */
function priceFor(provider, model) {
  if (provider !== 'anthropic' || !model) return null;
  const m = ANTHROPIC_MODELS.find((t) => t.pattern.test(model));
  if (!m) return null;
  return {
    input: m.input,
    output: m.output,
    cacheRead: m.cacheRead != null ? m.cacheRead : m.input * 0.1,
    cacheWrite: m.cacheWrite != null ? m.cacheWrite : m.input * 1.25,
  };
}

module.exports = { priceFor };
