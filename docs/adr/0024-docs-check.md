# ADR-0024: A light check for documentation commits

- Status: **Accepted** — 2026-11-26 by Iryna Velmakina (author). Drafted with Claude Code at the author's request.
- Date: 2026-11-26
- Supersedes: in ADR-0019, the accepted downside "A docs-only commit gets no CI check". The
  rest of ADR-0019 stands.

## Context

ADR-0019 makes `ci-cd.yml` skip commits that change only documentation, so docs can land on
`main` without a lab session. The cost: those commits get no check at all, and
`openspec validate` runs only when someone remembers to run it locally.

Two things in the docs can break and can be checked without AWS:

- the OpenSpec structure (`openspec validate --strict`);
- relative links between documents (ADRs, evidence files, specs).

The filter in `ci-cd.yml` is `paths-ignore` on the workflow's trigger, so on a docs-only
commit **no job in that file starts**. A docs check can't simply be one more job there.

## Options

| | 1. Keep: check by hand | 2. Separate `docs.yml`, filtered with `paths` | 3. One workflow: no trigger filter, a "what changed" job, a condition on every other job |
| --- | --- | --- | --- |
| Docs-only commit gets checked | no | yes | yes |
| Needs AWS or a lab session | — | no | no for the check |
| Touches the deploy workflow | no | no | **yes**: every deploy job gets a new condition |
| Extra logic | none | none: GitHub's own path filter | a change-detection job and conditions |

## Decision

**Option 2.** A new workflow `.github/workflows/docs.yml`:

- **Trigger:** `push` and `pull_request` with `paths` `docs/**`, `openspec/**`, `**/*.md`,
  the same list `ci-cd.yml` ignores; plus manual `workflow_dispatch`. It runs whenever docs
  change, so on a mixed commit it runs next to `ci-cd.yml`.
- **One job, no secrets, no AWS:**
  - `npx -y @fission-ai/openspec@1.16.0 validate --all --strict --no-interactive`, the
    version the repo already uses (CLAUDE.md);
  - a relative-link check: every `](path)` in the repository's Markdown that isn't a URL
    must point to an existing file. Anchors (`#…`) are ignored; web URLs aren't fetched.

**Runner-up:** option 3. Reconsider if the two path lists drift apart or more checks need
to be shared between the workflows.

## Accepted downsides

- The path list lives in two files (`ci-cd.yml` ignores it, `docs.yml` uses it). A comment
  in each points to the other.
- The link check is a simple pattern: reference-style links (`[x][1]`) and paths containing
  `)` aren't checked, and web links never are (they fail for reasons outside the repo).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| The check fails for a reason the commit didn't cause (a new CLI version) | Low | OpenSpec version pinned to 1.16.0 |
| A broken web link goes unnoticed | Low | Accepted: web links are rare here and can be checked by hand before a defense |

## Related

- Implemented in: `.github/workflows/docs.yml` (task 7.3 of `m3-replication`).
- Uses: ADR-0019 (docs-only commits skip `ci-cd.yml`), ADR-0015 (pipeline).
