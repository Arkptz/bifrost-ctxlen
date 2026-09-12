---
author: owner
code_paths:
- internal/plugin/plugin.go
- internal/plugin/transport.go
- internal/plugin/observability_test.go
- README.md
date: 2026-09-11
related:
- ADR-003
- ADR-004
- ADR-005
status: accepted
tags:
- bifrost
- routing
- plugins
---
# Publish the estimated context size as a header, measured from payload size

How this plugin tells Bifrost's routing rules how large a request is, and why
the measurement is an estimate.

## Context

Requests whose context is very large need a different provider from ordinary
ones: the usual upstream either refuses them or, worse, returns an empty stream
that looks like success. Deciding that per request is exactly what routing rules
are for.

Bifrost's routing rules are CEL expressions over a fixed variable set, built by
one hardcoded `cel.NewEnv()` in the routing plugin. Nothing lets a plugin
register a new variable, so `context_length > 500000` cannot be expressed;
upstream has a draft PR adding that variable, unmerged for months.

A plugin can, however, write into the request-headers map in the context, which
the routing plugin reads to populate its `headers[...]` variable. That map is
not among the reserved context keys, so writes from a hook are permitted.

## Decision
(a) **Mechanism**: the measurement is written into `BifrostContextKeyRequestHeaders` as a decimal string. Rules compare it with `int(headers["x-ctx-tokens"]) > N`. The plugin answers "how large", never "where should it go".

    The count, not a boolean. A yes/no flag would bake one threshold into the plugin: moving it means a config change, a second tier is impossible, and every rule is stuck with the question the plugin already answered. Reporting the number lets rules ask their own questions, as many as needed, with no plugin change — and the plugin keeps no threshold at all. CEL's int() conversion on a header was verified against the live gateway before committing to this.

    One total header turned out not to be enough: a rule may need to ask *why* a request is large. The full breakdown is published as `x-ctxlen-*` headers — messages, tools, byte classes, per-modality counts — so a rule can route on the component rather than the sum. See ADR-005 for the publishing surface and its integer-only guarantee.

(b) **Measurement**: an estimate, priced per byte class and modality and calibrated against what providers actually billed (ADR-003). The first cut — `len(json.Marshal(messages)) / 4`, the conventional bytes-per-token approximation — measured 30-39% low against billed tokens on real traffic, in the dangerous direction; ADR-003 records why and what replaced it. A real tokenizer was rejected deliberately: it means a heavyweight dependency with per-model vocabularies, and any library also linked by the host widens the shared-package surface that Go's plugin runtime version-checks — the exact coupling that makes a plugin fail to load. For a coarse threshold, an estimate that is never wrong by more than roughly a factor of two is sufficient.

    The bytes are counted from the raw request body in an `HTTPTransportPreHook`, one linear pass, before Bifrost parses the request; serializing the parsed payload survives only as a fallback for callers with no transport hook (ADR-004).

(c) **Scope**: only chat and responses payloads are measured. Embeddings, transcription, image generation and the rest are not context-window bound and estimate to zero. Within chat and responses, the whole prompt is measured — messages, tool definitions and system instructions — because an agent client resends its entire toolset every turn.
## Consequences
Operators set thresholds and targets entirely in routing rules; only a change to the measurement itself needs a rebuild.

### Positive
- Publishing the count rather than a boolean keeps every threshold question answerable by a rule — a new tier is a new rule, not a plugin change.
- The `x-ctxlen-*` breakdown lets a rule route on the component (non-Latin text, image-heavy, tool-heavy) rather than the sum alone.
- The synthetic-header mechanism needs no upstream change: it rides on the request-headers map the routing plugin already reads.

### Negative
- The plugin must be registered with `placement: pre_builtin` — the default `post_builtin` runs after routing, where the header is useless, and nothing warns about that.
- The estimate will misjudge requests near the threshold: worst case single-digit percent on the calibration corpus (ADR-003), looser off it, and provider pricing changes land outside the fixture until someone recalibrates. Acceptable for separating "enormous" from "normal"; not acceptable for billing.
- When upstream merges the `context_length` routing variable, this plugin becomes unnecessary: the rule can measure the request directly and the header, the plugin and its ABI coupling all go away.
## Alternatives considered

**Count tokens before the gateway**, in the client or a small proxy, and send
the header from there. Zero ABI coupling and no plugin at all. Rejected because
every client would need the logic and any client could then spoof the header;
a gateway-side hook is the single trustworthy place. Still the cheaper option if
this ever becomes the only reason to run a plugin.

**Ask the provider to count.** Bifrost can issue a token-count request, and
Anthropic-compatible upstreams expose an endpoint for it. Rejected: it adds a
network round-trip to the latency of every request to answer a question a cheap
local estimate answers well enough.