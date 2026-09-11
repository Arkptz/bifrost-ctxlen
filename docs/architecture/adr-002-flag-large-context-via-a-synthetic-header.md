---
author: owner
code_paths:
- internal/plugin/plugin.go
date: 2026-09-11
status: accepted
tags:
- bifrost
- routing
- plugins
---
# Flag large-context requests with a synthetic header, estimated by payload size

How this plugin tells Bifrost's routing rules that a request is large, and why
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

(a) **Mechanism**: a `PreRequestHook` measures the request and writes a header
    into `BifrostContextKeyRequestHeaders`. The routing decision itself stays in
    ordinary admin-UI rules matching `headers["x-ctx-large"] == "1"`. The plugin
    answers "is this large", never "where should it go" — thresholds and targets
    stay operator-editable without rebuilding anything.

(b) **Measurement**: `len(json.Marshal(messages)) / 4`, the conventional
    bytes-per-token approximation and the same fallback upstream's own draft
    uses. A real tokenizer was rejected deliberately: it means a heavyweight
    dependency with per-model vocabularies, and any library also linked by the
    host widens the shared-package surface that Go's plugin runtime
    version-checks — the exact coupling that makes a plugin fail to load. For a
    coarse threshold, an estimate that is never wrong by more than roughly a
    factor of two is sufficient.

(c) **Scope**: only chat and responses payloads are measured. Embeddings,
    transcription, image generation and the rest are not context-window bound
    and estimate to zero.

(d) **The header is written on every request**, `"0"` included. The map starts
    as a copy of the client's own headers, so writing only when the request is
    large would let any caller send the header themselves and choose their own
    route. Unconditional overwrite makes the client's value irrelevant.

(e) **The map is replaced, not mutated.** It is shared with other hooks and read
    concurrently; an in-place write is a data race.

(f) **Config is read atomically.** `PUT /api/plugins/ctxlen` rewrites the config
    of a live plugin, so the threshold and header name live in atomics rather
    than plain fields.

## Consequences

Operators tune the threshold and the routing target from the admin UI; only a
change to the measurement itself needs a rebuild. The plugin must be registered
with `placement: pre_builtin` — the default `post_builtin` runs after routing,
where the header is useless, and nothing warns about that.

The estimate will misjudge requests near the threshold, particularly
non-English text and heavy tool-call payloads, whose bytes-per-token ratio
differs. This is acceptable for a threshold meant to separate "enormous" from
"normal"; it would not be acceptable for billing.

When upstream merges the `context_length` routing variable, this plugin becomes
unnecessary: the rule can measure the request directly and the header, the
plugin and its ABI coupling all go away.

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
