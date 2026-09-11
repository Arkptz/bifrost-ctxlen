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

(a) **Mechanism**: a `PreRequestHook` measures the request and writes the count
    into `BifrostContextKeyRequestHeaders` as a decimal string. Rules compare it
    with `int(headers["x-ctx-tokens"]) > N`. The plugin answers "how large",
    never "where should it go".

    The count, not a boolean. A yes/no flag would bake one threshold into the
    plugin: moving it means a config change, a second tier is impossible, and
    every rule is stuck with the question the plugin already answered. Reporting
    the number lets rules ask their own questions, as many as needed, with no
    plugin change — and the plugin keeps no threshold at all. CEL's int()
    conversion on a header was verified against the live gateway before
    committing to this.

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
    as a copy of the client's own headers, so writing only for large requests
    would let any caller claim a size and choose their own route. Unconditional
    overwrite makes the client's value irrelevant, and a rule comparing
    `int(headers[...])` needs a value on every request regardless.

(e) **The map is replaced, not mutated.** It is shared with other hooks and read
    concurrently; an in-place write is a data race.

(f) **Config is read atomically.** `PUT /api/plugins/ctxlen` rewrites the config
    of a live plugin, so the header name lives in an atomic rather than a plain
    field.

## Consequences

Operators set thresholds and targets entirely in routing rules; only a change to
the measurement itself needs a rebuild. The plugin must be registered
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
