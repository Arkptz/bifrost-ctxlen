#!/usr/bin/env bash
# Run CI jobs locally via nektos/act. The flake-check job is skipped (its
# nix-installer-action needs systemd, unavailable in act's container) — run
# `nix flake check` in your devshell instead.
#
# Usage:
#   ./scripts/ci-local.sh              # run all act-compatible jobs
#   ./scripts/ci-local.sh --job fmt    # run a single job
#   ./scripts/ci-local.sh -- --verbose # pass extra flags through to act
set -euo pipefail

WORKFLOW=".github/workflows/ci.yml"
DEFAULT_JOBS=(fmt lint test vuln typos)
SINGLE_JOB=""
EXTRA_ARGS=()

while [ $# -gt 0 ]; do
    case "$1" in
        --job) SINGLE_JOB="$2"; shift 2 ;;
        --) shift; EXTRA_ARGS+=("$@"); break ;;
        *) EXTRA_ARGS+=("$1"); shift ;;
    esac
done

missing=()
command -v docker >/dev/null 2>&1 || missing+=("docker")
command -v act >/dev/null 2>&1 || missing+=("act")
command -v gh >/dev/null 2>&1 || missing+=("gh")
if [ ${#missing[@]} -gt 0 ]; then
    echo "ERROR: missing required tools: ${missing[*]}" >&2
    echo "  act:    provided by the dev shell — run 'nix develop'." >&2
    echo "  docker: a HOST prerequisite — install it and ensure the daemon is running." >&2
    echo "  gh:     a HOST prerequisite — install it and run 'gh auth login'." >&2
    exit 1
fi

GITHUB_TOKEN="$(gh auth token 2>/dev/null || true)"
if [ -z "$GITHUB_TOKEN" ]; then
    echo "ERROR: 'gh auth token' is empty — run 'gh auth login' first." >&2
    exit 1
fi
export GITHUB_TOKEN

run_job() {
    echo "=== act: $1 ==="
    act push -W "$WORKFLOW" -j "$1" -s GITHUB_TOKEN "${EXTRA_ARGS[@]+"${EXTRA_ARGS[@]}"}"
}

if [ -n "$SINGLE_JOB" ]; then
    run_job "$SINGLE_JOB"
else
    for job in "${DEFAULT_JOBS[@]}"; do run_job "$job"; done
    echo
    echo "Done. flake-check was skipped — run 'nix flake check' directly."
fi
