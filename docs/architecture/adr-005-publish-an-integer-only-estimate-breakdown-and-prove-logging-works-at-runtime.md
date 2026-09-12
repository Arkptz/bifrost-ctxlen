---
author: owner
code_paths:
- internal/plugin/plugin.go
- internal/plugin/observability_test.go
- README.md
date: 2026-09-12
related:
- ADR-003
- ADR-002
status: accepted
tags:
- observability
- security
- logging
---
# Publish an integer-only estimate breakdown, and prove logging works at runtime

<!-- Brief introduction: what this document is about -->

## Context

An estimator nobody can see is indistinguishable from a wrong one. The estimate used to be invisible in production: Bifrost does not log inbound headers (they carry credentials), the routing engine's trace prints `x-ctx-tokens=<present>` without the value, and a routing rule only ever reveals which side of a threshold a request landed on. An estimator that drifted stayed wrong until someone stood up a gateway against a mock upstream and read the header back.

Two properties of the publishing channels raise the stakes. The values land in a database humans query — the admin UI's Plugin Logs tab is backed by a table where operators run ad-hoc drift queries — and the request itself is attacker-controlled input: a client names its tools and writes its message text, so anything derived from request content reaching a published field is both a content leak and a forgery vector (a tool named `x est=1` would corrupt the drift query).
## Decision

(a) **The estimate publishes itself three ways.** Every field of the measurement goes out as an `x-ctxlen-*` request header, so a routing rule can compare any component and not just the total; one line per request on the admin UI's Plugin Logs tab; and the total as a `ctxlen.estimate` trace attribute for OTEL and Langfuse. The log line is forensics for one request; the trace attribute is the aggregate.

(b) **Every published value is an integer, except `kind`, which comes from a closed set** (`chat`, `responses`, `other`). That is a mechanical guarantee, not a convention: nothing derived from request content — no message text, no tool name, no URL — can reach a header or a log row, so the plugin can neither leak content into a database humans query nor let a client forge a field.

(c) **The log line is fixed-arity and schema-versioned.** `ctxlen/1 est=... kind=... msgs=...` — every field printed, zeros included, because three extra bytes are cheaper than an awk script that silently shifts a column. The leading `ctxlen/1` is both the grep anchor and the format version, so a consumer can refuse a version it does not know instead of misreading a reordered field. The same field set feeds the headers and the log line, so the two cannot disagree about what was measured.

(d) **The published arithmetic is self-checking.** The line carries the answer and every input the arithmetic used: `est = text + frame + media` and `frame = msgs*3` are checkable by eye, which is what makes the constants auditable against what the provider billed without access to the content.

(e) **An unmeasurable payload logs `warn=payload_unmarshalable` instead of an `est=`**, distinguished by the second token so a parser can branch before it looks for `est=`. That case estimates to zero, which would make a large request look small; a value a parser would believe is worse than none.

(f) **Drift is one SQL query.** The gateway records the provider's billed `prompt_tokens` as a plain column on the same log row, so the ratio of `est` to billed is a substring regex and a percentile, with no join. The alert band — p95 outside ±10% — is the same band the calibration test enforces, so production and the fixture hold the estimator to one number.

(g) **The plugin proves, once per process, that `ctx.Log` actually records.** `ctx.Log` is a silent no-op on a context the host did not scope to the plugin (core returns early when `pluginScope` is nil) — code that looks instrumented and emits nothing, which is exactly the failure this surface exists to prevent. The first request after startup checks that the write landed in `GetPluginLogs()`; if it did not, the hook returns an error — the one channel that still works when logging does not. The check is non-blocking (the host logs the error and continues), runs once rather than per request (`GetPluginLogs` deep-copies), and runs after the headers are published, so a broken gateway upgrade costs observability, never routing.
## Consequences

The estimate is auditable in production and safe to publish, and a host regression that silently disables plugin logging surfaces as a hook error instead of as missing data nobody notices.

### Positive
- Every input the arithmetic used is published, so the constants are checkable against provider billing without access to request content, and the next calibration can be assembled from live logs rather than a captured replay.
- Leak-proof and forgery-proof by construction, pinned by a test (`TestPublishedValuesNeverCarryRequestContent`) rather than by review discipline.
- The drift check closes the loop that caught the original defect: billed tokens found the 30-39% underestimate, and now the same comparison runs continuously.

### Negative
- One log line per inference request lands in a table the gateway keeps; each line is short and fixed-arity, but the volume is real.
- The log format is an unstructured string, so its shape is the interface: any field change must bump the schema token, and consumers that never check the version will misread a reordered line.
- The trace attribute carries only the total; component-level drift (images only, say) needs the log line, not the trace.