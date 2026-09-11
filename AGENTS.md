# Project conventions (bifrost-ctxlen)

## Stack

- Language: Go 1.27.0 (pinned via the `go.mod` `go` directive + a Nix overlay that
  builds the real 1.27.0 — nixpkgs still ships a release candidate)
- Artifact: a `-buildmode=plugin` shared object, `dlopen`ed by `bifrost-http`
- Purpose: estimate request context size and record it as a header, so Bifrost's
  CEL routing rules can match on it (`headers["x-ctx-large"] == "1"`)
- Test runner: `go test -race` (`gotestsum` for nicer output in the dev shell)
- Formatter: `gofumpt` (strict superset of `gofmt`; enforced by pre-commit + agent hooks)
- Linter: `golangci-lint` v2 (`gosec`/`gocritic`/`errorlint`/`modernize`/`revive`; zero-warning policy)
- Vulnerabilities: `govulncheck` (scans the dependency graph for known CVEs)

## Commands

```bash
make plugin                          # build the .so
make verify                          # build it AND prove it loads (ci/loader)
go test -race ./...                  # run tests (race detector)
golangci-lint run ./...              # lint (must pass clean)
gofumpt -l .                         # format check (empty output = clean)
govulncheck ./...                    # supply-chain vuln scan
```

## Critical rules

- NEVER bump a Go dependency on its own. Every version here is an ABI contract with
  the host binary: the plugin runtime hashes the whole shared transitive closure, so
  even an indirect patch bump yields `plugin was built with a different version of
  package ...` at load time. Dependencies move only together with a host rebuild.
- ALWAYS add an assert to `hooks_assert.go` when exporting a new hook. The loader
  resolves hooks *optionally*, so a typo silently disables one and the plugin still
  reports healthy.
- ALWAYS put behaviour in `internal/plugin`, never in `main.go`. The root is a shim so
  the same code can be compiled into a bifrost binary instead of loaded as a `.so`.
- NEVER read a mutable setting from a plain struct field — `PUT /api/plugins/<name>`
  reconfigures a LIVE plugin, so use an atomic or a mutex.
- NEVER mutate the context's request-headers map in place, and never set a synthetic
  header only when it is absent: the map starts as a copy of the client's headers, so
  in-place writes race and conditional writes let callers spoof the value.
- ALWAYS run `gofumpt -w .` before committing — pre-commit + the agent hooks enforce it.
- NEVER commit with `golangci-lint` warnings — the zero-warning policy is enforced.
- NEVER ignore a returned `error` — check it, wrap it with `fmt.Errorf("...: %w", err)`,
  or explicitly discard it with a comment saying why.
- NEVER hand-edit `go.sum` — run `go mod tidy`.

## Agent setup in this project

- `CLAUDE.md` re-exports `AGENTS.md` (`@AGENTS.md`) so Claude Code auto-loads these conventions. Per-agent permissions/MCP are the developer's own setup, not shipped with this template.
- Code-structure questions use `graphify update <path>` (AST, free) to build `graphify-out/graph.json` + the graphify MCP. `graphify-out/` is a regenerable local build artifact — gitignored, never committed.
- `dg` (DecisionGraph) governs decisions/docs — see below.

---

<!-- BEGIN dg-section (generated from _shared/AGENTS.dg-section.md; do not edit) -->

# Decision Graph

This project uses `dg` to track decisions, policies, specs, and operational
knowledge as a markdown knowledge graph under `docs/`.

**Command reference is in the CLI, not here.** Run `dg guide` for the workflow,
`dg --help` for all subcommands, and `dg new --help` / `dg set --help` for flags.
Do not rely on a copy of the help text — query the tool.

## Rules an agent must follow

- **Ask first, create later.** For vague requests, ask 3-5 clarifying questions
  (via `AskUserQuestion` / `ask_user`, never a wall of plain text) and wait for
  answers. Goal: complete documents with no TBD/FIXME. Only use TBD/FIXME if the
  user says they don't know or tells you to proceed without it.
- **Human-gated vs agent-creatable.** ADR / POL / OPP / INC / SPEC / PROC are
  human-gated: propose, get confirmation, then create. `TASK` docs you may create
  autonomously.
- **Always mutate through `dg`** (`dg new` / `dg set`) — never hand-edit files
  under `docs/` or `.dg/`. `dg` assigns IDs, validates schema, and keeps the
  graph consistent. For multi-line / table / special-character content, get the
  path (`dg show ID --json`), edit the file, then `dg validate`.
- **Field assignment footgun:** `key=value` sets a scalar; `key+=value` appends
  to an array. Only `supersedes` / `superseded_by` are scalar. `enables`,
  `enabled_by`, `triggers`, `triggered_by`, `implements`, `depends_on`,
  `related`, `conflicts_with`, `tags`, `code_paths` are arrays — use `+=`.
  (`dg set ADR-001 superseded_by=ADR-010` ✓ scalar; `dg set OPP-001 enabled_by+=ADR-010` ✓ array.)
- **Quality:** problem-focused titles; cross-link with the most specific relation;
  add yourself to authors.

## End of session

Run `dg validate` (also a pre-commit hook) and `dg lint` to catch orphans and
dangling refs before finishing. `dg hooks stop` summarizes outstanding issues
(run it manually unless your agent has a native Stop hook).

<!-- END dg-section -->
