---
author: owner
code_paths:
- internal/plugin/transport.go
- internal/plugin/plugin.go
- hooks_assert.go
- internal/plugin/transport_test.go
date: 2026-09-12
related:
- ADR-003
- ADR-002
status: accepted
tags:
- transport
- performance
- bifrost
---
# Measure the raw request body instead of re-serializing the payload

<!-- Brief introduction: what this document is about -->

## Context

The estimate has to run on every inference request, inside the gateway, before routing. The first implementation measured by re-serializing what Bifrost had already parsed: `json.Marshal` of the messages and tools, then counting the bytes. That marshal was 76% of this plugin's total cost and nearly all of its allocations — to reproduce bytes that were already on the wire.

The transport layer hands a plugin those bytes: `fasthttpToHTTPRequest` copies the (decompressed) body into `HTTPRequest.Body`, and does so on every request whether or not this plugin exports a transport hook, because a loaded `.so` registers as an HTTP transport plugin regardless. The copy was being paid for and thrown away.

Measuring the wire body is not identical to measuring a marshal: the body carries top-level request parameters and the client's own JSON escaping rather than Go's. Priced against billed tokens over the calibration corpus the two agree to within hundredths of a percent — JSON escaping never touches valid non-ASCII, and the ASCII overhead is a stable fraction the constants absorb — and the non-ASCII split, which the whole estimator depends on, is byte-exact (`TestBodyMeasurementMatchesByteClasses`). The equivalence was verified before the switch, not assumed after it.

One wrinkle: the transport does not retain every body. Payloads over the large-payload threshold (10 MB by default) and requests of unknown length arrive with an empty `Body`. `Content-Length` is not a dependable substitute either — the decompression middleware DELETES that header before the retain decision is made, so a gzipped or chunked request arrives with neither a body nor a declared size. An absent body must therefore be distinguished from an empty one: treating the two alike published a three-token estimate for payloads worth hundreds of thousands, which is exactly the underestimate that routes an oversized request to a short-context provider.
## Decision

(a) **`HTTPTransportPreHook` measures the raw body before Bifrost parses it.** One linear pass over the bytes, counting ASCII and non-ASCII — the units ADR-003 prices — with zero allocation. It measures and stashes only; the estimate is still published by `PreRequestHook`, which is where routing can see it.

(b) **The measurement travels in the context under a `BifrostContextKey`, as a struct pointer, holding counts rather than the body.** Not a plain string key: the transport middleware sweeps every string-keyed user value into `HTTPRequest.PathParams`, so a string key would quietly become a path parameter. Not the body slice: `HTTPRequest` is pooled and `ReleaseHTTPRequest` nils `Body` as soon as the hook returns, so holding it would alias memory the next request is about to reuse.

(c) **`json.Marshal` survives only as a fallback**, for a request that reaches `PreRequestHook` without a transport measurement — an SDK embedding this package, or a realtime websocket. The two paths are held equivalent by differential tests, not hope: byte-exact on the non-ASCII split, and a ratio band on the totals.

(d) **Only inference paths are measured** (`/chat/completions`, `/responses`, `/messages`, `/generateContent`). The hook fires on every route the middleware covers, including the admin API; scanning a config write costs time and says nothing.

(e) **A body that was not retained falls back to serializing**, not to the wire size. The transport hook records whether it actually counted the body; when it did not, `PreRequestHook` re-serializes the parsed payload rather than trust an absent `Content-Length`. Only if a declared length genuinely survives is it used, treated as ASCII so the estimate errs high. Reporting zero — the original behaviour — is the misroute this plugin exists to prevent.

(f) **The hook never short-circuits.** It observes; returning a response would answer the request from a plugin whose job is to measure it.

(g) **`hooks_assert.go` binds the new hook's name and signature at compile time.** The loader resolves hooks optionally, so a typo or a drifted signature yields a plugin that loads, reports healthy, and silently never measures anything.
## Consequences

The hot path no longer serializes anything: measured against the marshal it replaces, 711 µs → 75 µs at 220 kB and 4.0 ms → 0.5 ms at 1.4 MB, with effectively no allocations on the scanning path. Measurement and publication are decoupled — the transport hook counts, `PreRequestHook` prices and publishes — which is also what lets the fallback coexist.

### Positive
- Measuring costs one linear scan of bytes the gateway had already copied; the marshal — most of the plugin's cost — is gone from the hot path.
- The fallback keeps SDK embedders and realtime websockets measurable instead of silently estimating zero.
- Equivalence between the two paths is pinned by tests, including the exact case that broke the first estimator: non-ASCII byte classes.

### Negative
- Two code paths exist forever, and they are only empirically equivalent — the wire body is not Go's serialization, so any change to what is measured needs the differential tests re-run.
- Requests whose body was not retained lose the cheap path entirely and pay the marshal, which is the price of not guessing their size.
- The fallback path keeps its marshal cost, so callers without a transport hook still pay it.