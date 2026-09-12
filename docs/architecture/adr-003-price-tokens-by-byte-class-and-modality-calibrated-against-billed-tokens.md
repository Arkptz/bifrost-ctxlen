---
author: owner
code_paths:
- internal/plugin/plugin.go
- internal/plugin/calibration_test.go
- internal/plugin/testdata/calibration.json
date: 2026-09-12
related:
- ADR-002
status: accepted
tags:
- estimation
- calibration
- tokens
---
# Price tokens by byte class and modality, calibrated against billed tokens

<!-- Brief introduction: what this document is about -->

## Context

The first estimator divided a serialized payload length by four: `len(json.Marshal(messages)) / 4`. Measured against what providers actually billed for twelve real requests, it ran 30-39% low — the dangerous direction, because a request that looks small is routed to a short-context provider and fails mid-session. Two unit errors compounded. "Four per token" is four *characters* of English prose (OpenAI's help centre and Gemini's docs both state characters, and both warn that other languages differ), and it was applied to *bytes*: Go serializes non-ASCII JSON as raw UTF-8, so one Cyrillic character arrives as two bytes and was charged double. The corpus was wrong too — this traffic is code, JSON and tool schemas, which tokenize denser than prose. All three errors point the same way: down.

The estimate also under-measured the prompt itself. Only `messages` was serialized, but an agent client resends its entire toolset and its system instructions on every turn; requests built exactly the way this plugin exists to catch were the ones most under-reported.

Media was priced as text, which is wrong in both directions and by two orders of magnitude each way: a 1 MB inline screenshot is ~1.4 MB of base64 and scored ~350k tokens against a real cost near 1.6k, while the same picture referenced by an 80-character URL scored ~20.

A real tokenizer would fix the arithmetic and break the plugin: a heavyweight dependency with per-model vocabularies, and any library also linked by the host widens the shared-package surface that Go's plugin runtime version-checks — the exact coupling that makes a `.so` refuse to load (ADR-001).
## Decision

(a) **Text is priced per byte class, not by one flat divisor, and the classes follow UTF-8 sequence length.** One linear pass counts lead bytes and attributes each sequence's full width to its class, so the buckets sum to the buffer length. Four divisors convert the counts to tokens — 2.55 bytes per ASCII token, 3.85 for 2-byte scripts, 2.5 for 3-byte, 1.5 for 4-byte — plus 3 tokens of framing per message, matching OpenAI's cookbook.

Three non-ASCII classes and not one, because bytes-per-token is not a single number: measured across FLORES-200 it spans roughly 4.5x, from about 5.8 bytes/token for Cyrillic under o200k down to 1.3 for emoji, which have no single token and shatter into byte fragments. A lone non-ASCII divisor fitted on two-byte Cyrillic therefore underprices CJK by roughly a third and emoji by more. Each constant sits at or below the low end of its measured range, so the estimate errs high in tokens — the safe direction, since underestimating is what routes an oversized request to a short-context provider. 3.85 for the two-byte class is 1.93 *characters* per token for Cyrillic, which independently matches the 1.5-2 chars/token o200k is documented to achieve on Russian. Worst-case error over the corpus fell from 39% to 3.7%.

(b) **The constants are calibration artifacts, not folklore, and are pinned by tests.** `testdata/calibration.json` holds the profile of twelve real requests — byte classes, message and tool counts, and the `prompt_tokens` the provider billed — captured from a gateway's `/api/logs`. The transcripts themselves are not in this repository: they are session logs full of content, and this repo is public; the profile is all the arithmetic consumes. `TestEstimateMatchesBilledTokens` fails when a constant drifts, with asymmetric bands (+15% over, -10% under — underestimation is the direction that breaks routing), and `TestThresholdDecisionsHoldOnRealTraffic` asserts the decision a routing threshold would actually make. The defect survived the original suite precisely because every unit test asserted against bands derived from the estimator itself; the oracle had to be external.

(c) **The whole prompt is measured**: messages, tool definitions and system instructions. Tools matter as much as messages, because an agent client resends its entire toolset every turn.

(d) **Non-text parts are priced by modality, never by the bytes that carried them**, with their bytes subtracted from the text count. No provider bills an image by its base64 characters: OpenAI charges 85 tokens at `detail: low` and up to 1445 for tiled images, Anthropic `ceil(w/28)*ceil(h/28)` capped at 1568, Gemini 258 per tile or a flat 1120 on Gemini 3. Images are therefore charged a flat 1600 tokens — the top of the documented range, so images err high rather than letting a long request look small. Audio is charged by decoded size at 500 bytes per token (about 16 kB/s at the highest documented per-second rate); inline documents by decoded size at 20 bytes per token (1500-3000 tokens per typical PDF page); a document referenced by URL or file id gets one page's worth, 2000 tokens, because its real size is not in the request at all.

(e) **A payload that cannot be measured is reported, never swallowed.** An unmeasurable payload estimates to zero, which makes a large request look small; it is published as a visible warning instead (ADR-005).
## Consequences

The estimate is accurate enough to route on: worst case 3.7% against billed tokens on the calibration corpus, with the failure direction that matters (underestimation) held to 10% by the calibration test. It is not a billing meter — the bands are deliberately loose.

### Positive
- Worst-case error is single-digit percent, against 30-39% low before, so a rule near a threshold is no longer guessing.
- The constants are re-fittable from production: ADR-005 publishes every input the arithmetic used, so the next calibration can be assembled from live logs rather than a captured replay.
- Media-heavy requests are priced by what a provider will actually bill, in both directions — a screenshot no longer inflates a small request, and a URL-referenced image no longer hides a large one.

### Negative
- The constants describe one traffic profile: twelve requests, largely Cyrillic text plus code and tool schemas. Different traffic (CJK, mostly prose) may need re-fitting, and provider pricing changes — image costs, new models' densities — land outside the fixture until someone recalibrates.
- The estimate is a routing input, not a billing meter; off-corpus, per-request error can still reach double digits.
