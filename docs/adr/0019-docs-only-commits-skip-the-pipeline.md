# ADR-0019: Documentation-only commits skip the pipeline

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code at the author's request.
- Date: 2026-11-24
- Supersedes: in ADR-0015, the `ci-cd.yml` trigger "every push and PR". The rest of ADR-0015 stands.

## Context

Every push to `main` runs the whole pipeline: build a release named by the commit, deploy
the stacks, restart the app, smoke-test. A commit that changes only documentation (ADR
text, evidence, OpenSpec files, the worklog) does the same, so:

- it needs a live lab session and fresh secrets, or the deploy jobs fail;
- it starts instances, which cost money, to ship no change in behaviour;
- so documentation stays on the feature branch, and closing a milestone (merging the last
  docs, `openspec archive`) needs a lab session.

The lecturer's advice (20.11, the defense around 58:00) must keep holding: the stack
describes which app version runs (the commit SHA as a CloudFormation parameter); nothing is
updated around the stack. His criticism there was of a pipeline that updated the code on
running instances while the stack stayed the same.

## Options

| | 1. Keep: every push deploys | 2. Skip the workflow for docs-only commits (`paths-ignore`) | 3. Docs in a separate repository |
| --- | --- | --- | --- |
| Docs commit on `main` needs a lab session | yes | no | no |
| Stack still names the running version | yes | yes | yes |
| Anything updated outside CloudFormation | no | no | no |
| Spec, ADRs and code reviewed together | yes | yes | **no**: the brief wants them in one repo |

## Decision

**Option 2.** `ci-cd.yml` ignores pushes and pull requests whose changed files are **all**
under `docs/`, under `openspec/`, or Markdown files (`**/*.md`: README, CLAUDE.md, the
worklog). Such a commit runs nothing: no tests, no build, no deploy.

- A commit that touches **any** other file (Go code, `api/*.yaml`, templates, the Makefile,
  scripts, workflows) runs the whole pipeline as before, including its Markdown files.
- **What the stack says:** `AppVersion` is the SHA of the last commit that was deployed, so
  after a docs-only commit it is an earlier commit than the tip of `main`. That is still
  exact: that commit's image is what runs, and the docs commit changed nothing it contains.
- Rollback, change sets and the smoke test are unchanged.
- `benchmark.yml` stays manual and is unaffected.

**Runner-up:** option 1. Simplest rule ("`main` = what runs"), but it costs a lab session
per docs fix.

## Accepted downsides

- The tip of `main` and the running version can differ by docs-only commits. `git log
  <AppVersion>..main` shows exactly which ones, all documentation.
- A docs-only commit gets no CI check. Nothing in those paths is built or tested today
  (`openspec validate` runs locally, by hand).

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A file the app or tests read is put under an ignored path | Low | The contract tests read `api/*.yaml`, which isn't ignored; anything new the build reads goes outside `docs/` and isn't Markdown |

## Related

- Implemented in: `.github/workflows/ci-cd.yml`.
- Uses: ADR-0015 (pipeline), ADR-0009 (release named by the commit).
