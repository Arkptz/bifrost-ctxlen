# bifrost-ctxlen

A [Bifrost](https://github.com/maximhq/bifrost) plugin that reports each
request's estimated context size, so routing rules in the admin UI can send the
big ones somewhere that handles them.

```sh
make verify        # build the .so and prove it loads
make test          # unit-test the hooks
nix develop        # toolchain pinned to the version the host needs
```

## Why it exists

Bifrost's routing rules are CEL expressions over a **fixed** variable set —
`model`, `provider`, `headers[...]`, `virtual_key_*` and a handful more — built
by a single hardcoded `cel.NewEnv()`. There is no extension point, so a rule
like `context_length > 500000` cannot be written today. Upstream has an unmerged
draft PR adding exactly that variable.

What a plugin *can* do is write into the request-headers map that the routing
plugin already reads to populate its `headers[...]` variable. So this plugin
measures the request and publishes the number; every threshold stays in ordinary
rules you manage from the UI:

```text
int(headers["x-ctx-tokens"]) > 500000   ->  your long-context provider
int(headers["x-ctx-tokens"]) > 900000   ->  something bigger still
```

It publishes the count rather than a yes/no flag on purpose. A boolean bakes one
threshold into the plugin: changing it means editing config, a second tier is
impossible, and every rule can only ask the question the plugin already decided.
A number lets rules ask their own questions, as many as you like, with no plugin
change. `int()` on a header was verified against a live gateway first.

## Setup

Register the plugin with `placement: pre_builtin`. Without it the plugin
defaults to `post_builtin` and runs **after** the routing plugin, so the header
arrives too late to affect anything:

```json
{
  "name": "ctxlen",
  "path": "/var/lib/bifrost/plugins/bifrost-ctxlen.so",
  "placement": "pre_builtin",
  "enabled": true,
  "config": {
    "header": "x-ctx-tokens"
  }
}
```

`header` is optional and defaults to the value above. It is editable at runtime
through `PUT /api/plugins/ctxlen` and the admin UI — the plugin reads it
atomically, so a change takes effect without a restart. There is no threshold to
configure: that lives in the rules.

Then add a routing rule comparing `int(headers["x-ctx-tokens"])`.

## How the measurement works

Text is priced per byte class, and the whole prompt is measured: messages, tool
definitions and system instructions. Tools matter as much as messages, because
an agent client resends its entire toolset every turn.

The familiar "4 per token" is **4 characters of English prose**, and applying it
to `len(json.Marshal(...))` — which is **bytes** — conflates the two units. Go
writes non-ASCII into JSON as raw UTF-8, so one Cyrillic character arrives as
two bytes and gets charged double. Prose is also the wrong corpus: code, JSON
and tool schemas tokenize denser. Both errors point the same way, and against
what providers actually billed for 12 real requests the flat divisor came out
**30-39% low** — the dangerous direction, because a request that looks small
gets routed to a short-context provider and fails mid-session.

| Byte class | Bytes per token |
|---|---|
| ASCII | 2.55 |
| non-ASCII | 3.85 |
| per message | +3 tokens of framing |

3.85 bytes per non-ASCII token is 1.93 *characters* per token for two-byte
Cyrillic, which independently matches the 1.5-2 chars/token that o200k is
documented to reach on Russian. Worst-case error over the corpus is **3.7%**,
against 39% before.

**Non-text parts are priced by modality, not by bytes.** No provider bills an
image by the characters that carried it: OpenAI charges 85 tokens at
`detail: low` and 765 for a 1024×1024 at `detail: high`, Anthropic
`⌈w/28⌉·⌈h/28⌉` capped at 1568, Gemini 258 per tile or a flat 1120 on Gemini 3.
Counting bytes is wrong in both directions, and by two orders of magnitude each
way. The plugin therefore subtracts the media bytes from the text estimate and
adds a per-modality cost instead:

| Part | Charged |
|------|---------|
| Image, inline or by URL | 1600 tokens — the top of the documented range, so images err high |
| Audio | decoded bytes / 500 (~16 kB/s at Gemini's 32 tokens/s) |
| Inline document | decoded bytes / 20 (~1500–3000 tokens per PDF page) |
| Document by URL or file id | 2000 tokens — one page; its real size is not in the request |

Measured on this build, a flat byte count versus what ships:

| Request | Flat bytes/4 | This estimator |
|---------|-------------:|---------------:|
| 1 MB inline screenshot | 349,549 | ~1,600 |
| Same image by `https://` URL | 24 | ~1,600 |
| 400 kB of tool definitions | 8 | ~157,000 |

A real tokenizer would be more accurate and considerably worse in practice: it
means a heavyweight dependency with per-model vocabularies, and any library also
linked by the host widens the shared-package surface that Go's plugin runtime
version-checks. For a coarse "is this request enormous" question an estimate
that is never wrong by more than about a factor of two is enough — and for media
it is the *modality*, not the tokenizer, that supplies that factor.

Only chat and responses payloads are measured. Embeddings, transcription and the
rest are not context-window bound, and estimate to zero.

## Two details that are easy to get wrong

**The header is written on every request**, `"0"` included. The map starts as a
copy of the *client's* headers, so a value written only for large requests would
let any caller claim their own size and pick their own route. Overwriting
unconditionally makes the client's value irrelevant — and a rule comparing
`int(headers[...])` needs a value present on every request anyway.

**The map is replaced, not mutated.** It is shared with other hooks and read
concurrently, so writing into the existing map is a data race.

Both are covered by tests.

## Before deploying

A Go plugin fails at runtime rather than at build time, in three ways.

**The host must be a dynamic build.** A statically linked Go binary cannot
`dlopen` anything.

**The versions must match exactly — all of them.** The plugin runtime compares a
hash of every package shared with the host, across the whole transitive closure.
A patch bump in an indirect dependency this module never imports directly is
enough to produce `plugin was built with a different version of package ...`.
Check what the host links against and match it:

```sh
go version -m /path/to/bifrost-http | grep -E 'maximhq/bifrost|^\S+: go'
```

If the host is built from patched sources, the plugin has to be built against
*those* sources — a published module at the same version number is a different
tree.

**Every gateway upgrade requires rebuilding the plugin.** Treat the host and its
plugins as one atomic deployment.

`make verify` dlopens the artifact and resolves every symbol. That check proves
agreement with the core version in `go.mod`; it cannot prove agreement with a
patched, self-built host.

## Operational notes

The gateway stores an absolute path to the `.so`. Under Nix that path is in the
store and nothing references it, so garbage collection will delete it and the
gateway will not find its plugin on the next start. Install the artifact to a
stable location, or register a GC root.

If a plugin fails to load the gateway may not start, which means the admin UI is
not there to disable it. Know how to set `config_plugins.enabled = false`
directly in the database before you need to.

## Layout

| Path | Why it is there |
|------|-----------------|
| `main.go` | The shim. `package main`, no `func main` — `-buildmode=plugin` compiles a directory and the loader resolves package-level symbols out of it. |
| `hooks_assert.go` | Compile-time proof that each exported hook has the right name and signature. |
| `internal/plugin/` | The measurement and the hook logic. |
| `ci/loader/` | Harness that `plugin.Open`s the built `.so` and resolves every symbol. |

Keeping the logic out of `package main` means the same code can instead be
compiled *into* a bifrost binary, which is the escape hatch if the `.so` version
coupling stops being worth it. See `docs/architecture/adr-001-*`.
