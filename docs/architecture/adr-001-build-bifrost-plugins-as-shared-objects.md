---
author: owner
code_paths:
- go.mod
- flake.nix
- main.go
- hooks_assert.go
- ci/loader/main.go
- renovate.json5
date: 2026-09-11
status: accepted
tags:
- go
- bifrost
- plugins
- toolchain
---
# Build Bifrost plugins as shared objects, behind a compile-in escape hatch

How this template builds a Bifrost gateway plugin, and what the choice costs.

## Context

Bifrost loads plugins two ways. A plugin can be compiled *into* the
`bifrost-http` binary as a built-in, or built as a Go `-buildmode=plugin`
shared object and `dlopen`ed at startup from a path in the gateway's database.

The two differ in one property that dominates every other consideration. Go's
plugin runtime compares a hash of every package shared between the host process
and the plugin, and that hash covers the entire transitive dependency closure.
A plugin therefore loads only when it was built against byte-identical sources,
with the same compiler and the same libc. Upstream states it plainly: a plugin
must be rebuilt on every Bifrost upgrade, and Bifrost ships patches every two to
three days.

This template also serves a concrete first use case. Bifrost's routing rules are
CEL expressions over a fixed variable set, built by a single hardcoded
`cel.NewEnv(...)`; nothing lets a plugin register a new variable. The sanctioned
way for a plugin to influence routing is to write a synthetic header into the
request-headers map in the context, which the routing plugin reads to populate
its `headers[...]` variable, so operators keep managing the actual routing as
ordinary rules in the admin UI.

## Decision

(a) **Build mode**: `-buildmode=plugin`, producing a `.so`. It is what upstream
    documents, what their examples use, and it keeps the plugin in its own
    repository with its own release cycle — the shape the other templates here
    already have.

(b) **Escape hatch**: the plugin's behaviour lives in `internal/plugin`, an
    ordinary importable package, and `main.go` is a thin shim exporting the
    symbols the loader resolves. Putting the logic in `package main` would have
    been shorter and would have permanently foreclosed the alternative. As
    written, the same package compiles into a patched `bifrost-http` whenever
    the ABI coupling stops being worth its operational cost.

(c) **Version pinning**: `go.mod` requires a specific published
    `github.com/maximhq/bifrost/core`, with no `replace`. This is treated as an
    ABI contract, not a dependency: it may only move together with a rebuild of
    the host.

(d) **Toolchain**: Go 1.27.0, obtained through a nixpkgs overlay that rebuilds
    `go_1_27` from the go.dev source tarball. nixpkgs currently ships 1.27 as a
    release candidate, and Go orders `1.27rc2` *before* `1.27.0`, so every
    bifrost module's `go 1.27.0` directive is rejected. A monorepo build can
    rewrite those directives in its own tree; a module consumed from the module
    cache cannot, so the toolchain itself has to be genuine. `GOTOOLCHAIN=local`
    keeps the devshell from silently downloading a different compiler, which is
    exactly how an unloadable artefact gets produced.

(e) **Typo protection**: `hooks_assert.go` binds every exported hook to its
    expected function type at compile time. The loader looks hooks up
    *optionally*, so a misspelled name or a drifted signature yields a plugin
    that loads, reports healthy, and silently never runs that hook. The asserts
    live in the package rather than a `_test.go` file so that building the `.so`
    fails too, not only `go test`.

(f) **Load verification**: `ci/loader` `plugin.Open`s the built artefact and
    resolves each symbol, and CI runs it. Unit tests exercise hooks in-process
    and prove nothing about loading. The harness imports `core/schemas`
    deliberately — that shared package is what arms the runtime's version check;
    without it the harness would pass while verifying nothing.

(g) **Dependency updates**: the whole `gomod` manager sits behind
    `dependencyDashboardApproval`, grouped as one set, and `lockFileMaintenance`
    is disabled — the one place this template departs from its siblings.
    Nudging indirect dependencies is precisely the change that breaks
    `plugin.Open`.

## Consequences

Plugin and gateway become a single atomic deployment: upgrading one without the
other produces a runtime failure, not a build failure. CI catches disagreement
with the *published* core version; it cannot catch disagreement with a
self-built, patched host, which needs a check built from the host's own tree.

The `.so` path also carries operational obligations that a compiled-in plugin
would not have. The gateway stores an absolute path, so under Nix the artefact
needs a GC root or a stable install location. A plugin that fails to load can
take the gateway down with it, including the admin UI that would otherwise
disable it, so disabling it directly in the database is the documented recovery.

If these costs outgrow the benefit — and the benefit is thin, since the ability
to update a plugin without rebuilding the host does not survive exact version
coupling — decision (b) makes switching a matter of adding a `require`, a
`replace` and a registration line to the host build, without touching the
plugin's code.

## Alternatives considered

**Compile into a forked `bifrost-http`.** Eliminates the version coupling
entirely: a mismatch becomes a compile error. Rejected as the default because it
makes every plugin a patch against upstream and gives up independent release
cycles. Retained as (b), and the better choice for a deployment that already
patches Bifrost.

**Count tokens ahead of the gateway.** For the motivating use case specifically,
a client or a small proxy can compute the header before Bifrost ever sees the
request, with no ABI coupling at all. Cheaper when that is the only requirement;
it stops working as soon as a plugin needs access to gateway internals.
