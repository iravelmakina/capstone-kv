# Tasks

## 1. Spec

- [x] 1.1 Author accepts ADR-0024, the proposal and the spec delta in chat; `openspec validate ci-docs-check --strict` passes; spec-only commit

## 2. Workflow

- [x] 2.1 `.github/workflows/docs.yml` (ADR-0024) and a comment in `ci-cd.yml` linking the two path lists; tested locally, including a broken link
- [ ] 2.2 A docs-only push runs `docs` green and starts no `ci-cd` run; a PR to `main`

## 3. Close

- [ ] 3.1 After the merge: `openspec archive ci-docs-check`
