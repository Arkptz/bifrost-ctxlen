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

The estimate is `len(json.Marshal(messages)) / 4` — the usual bytes-per-token
rule of thumb, and the same fallback upstream's own draft uses.

A real tokenizer would be more accurate and considerably worse in practice: it
means a heavyweight dependency with per-model vocabularies, and any library also
linked by the host widens the shared-package surface that Go's plugin runtime
version-checks. For a coarse "is this request enormous" question an estimate
that is never wrong by more than about a factor of two is enough.

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
