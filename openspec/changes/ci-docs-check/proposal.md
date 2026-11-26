# Proposal

> **Approved** by the author on 2026-11-26. Drafted with Claude Code at the author's request.
> Decision: ADR-0024.

## Why

Documentation-only commits skip `ci-cd.yml` (ADR-0019), so they get no check at all:
`openspec validate` runs only when someone remembers to run it locally, and a broken link
between ADRs, specs and evidence goes unnoticed.

## What Changes

- A new workflow `.github/workflows/docs.yml`, triggered by changes under `docs/`,
  `openspec/` or any Markdown file: `openspec validate --all --strict`, and a check that
  every relative Markdown link points to an existing file. No AWS, no secrets.
- A comment in `ci-cd.yml` linking its ignore list to the new trigger list.
- ADR-0024.

This is pipeline housekeeping, not milestone work, so it has its own change and branch.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `delivery-pipeline`: documentation changes get a check of their own.

## Impact

- New file: `.github/workflows/docs.yml`. One comment line in `ci-cd.yml`.
- No AWS resources, no cost.
