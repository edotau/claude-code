# Databricks errors: symptom to command

Find the symptom, run the command, read the cause. All `claude-code` verbs: `claude-code <verb> -h`.

| Symptom | Likely cause | Run |
| --- | --- | --- |
| 401 or 403 on every call | Token expired, user login dead, or the wrong identity | `claude-code doctor`, then `claude-code databricks check --profile <p>` |
| 401 and the status line shows `login:<profile>` | The user login is dead; a paired service principal still covers headless runs | `claude-code databricks login --profile <p>`; over SSH `claude-code --remote <host> databricks login --profile <p>` |
| 401 only in headless runs | Service-principal secret missing or rejected | `claude-code doctor` (`source=` on the row), then `claude-code vault set` |
| 401 and doctor shows `source=env <NAME>` | An env var shadows the vault entry | Unset the variable, or fix its value |
| 403 on one endpoint only | The identity lacks permission on that endpoint | Ask the workspace owner; not a harness fault |
| 404 or `ENDPOINT_NOT_FOUND` | Model dropped or renamed in that workspace | `claude-code models discover --profile <p> --apply --verify`, then pick a served id |
| 400 "This endpoint <id> is deprecated" | The endpoint is retired | The proxy drops it from menus and the auto pool at the first such reply; `models discover --profile <p> --apply --verify` sweeps the rest; `--undrop <id> --apply` lifts it (a model Databricks re-enables stays dropped until then) |
| 429 | Rate limit or quota on the workspace | The proxy cools that provider for `Retry-After` (30 s default, 5 min cap) and moves on; `claude-code gateway proxy --status` |
| 503 or 502 | Endpoint scaling or workspace outage | Token mint and listing retry 3 times with capped, jittered backoff that honours `Retry-After`; model traffic fails over through the proxy |
| 400 "context length", "maximum context length", `context_length_exceeded` | Prompt over the model's window | The proxy rewrites it into the form agents compact on; if the agent cannot compact, pick a larger window (see wires.md) |
| 400 on an `anthropic-beta` value | Databricks rejects some beta flags | Send none for the 1-hour cache (`ttl` only); the proxy strips `advanced-tool-use-2025-11-20` |
| 400 on `store` | A GPT model on the chat wire rejects `store` | Use the model's Responses wire, or Pi's `supportsStore: false` compat (set by the harness) |
| "stream disconnected" | The endpoint failed mid-stream; a non-streaming probe will not show it | Re-run with a real streamed `agents ask`; if it repeats, drop the id (below) |
| Model prints raw tool-call markup as text | Known for one open-weights model on the chat wire | `claude-code models discover --drop <id> --apply` so no pool picks it; `--undrop` later |
| Slow first token | Cold or scaled-to-zero endpoint, long prompt, or a busy pool | Retry once; `claude-code gateway resolve all --model auto --all-profiles` shows the pool and held rows |
| "model not served" at launch | Fresh listing lacks the name | `claude-code gateway resolve <agent> --model <m> --profile <p>`; `--fallback pin\|agents\|all` to fall back |

After any fix, confirm with a streamed `claude-code agents ask`, not `curl`.
