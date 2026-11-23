# ADR-0015: CI/CD pipeline

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the decision list agreed in chat.
- Date: 2026-11-24

## Context

The brief: one pipeline that deploys infra updates (via IaC) and app updates, carried
through M5. The lab denies CodePipeline, CodeBuild, CodeDeploy and creating an OIDC
provider or IAM roles (M1 evidence 08, 62), so no AWS-native pipeline and no keyless
login. Lab credentials expire every 3-hour session. The lecturer (20.11): keep the
benchmark in its own job, separate from tests. The lecturer (02.11): the stack should
describe which app version runs, so a rollback is a parameter change.

## Options

| | 1. GitHub Actions + lab keys as secrets | 2. Self-hosted runner on EC2 | 3. Deploy from my laptop (`make`) |
| --- | --- | --- | --- |
| A real pipeline | yes | yes | no |
| AWS credentials | copied each session into secrets | instance role is read-only: can't deploy | my local profile |
| Runs when I push | yes (deploy only while secrets are fresh) | yes | no |

## Decision

**Option 1, GitHub Actions** (repo `iravelmakina/capstone-kv`), two workflows:

| Workflow | Trigger | Jobs |
| --- | --- | --- |
| `ci-cd.yml` | every push and PR; deploy jobs only on `main` | **test** (`go vet`, `go test`, contract test, `cfn-lint`) → **build** (image, `docker save`, upload to S3 under the SHA) → **deploy-infra** (network + artifacts stacks via change sets) → **deploy-app** (app stack with `AppVersion=<sha>`) → **smoke** (SSM Run Command on the coordinator: create table, put, read, exists, delete) |
| `benchmark.yml` | manual (`workflow_dispatch`) | starts the client, runs go-ycsb workloads A and B, uploads results to S3 and as a workflow artifact |

- **Credentials:** repo secrets `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
  `AWS_SESSION_TOKEN`, refreshed by me at the start of a session. Never in the repo.
- **Rollback:** re-run deploy-app with an earlier SHA (the image is still in S3).
- **Every stack update goes through a change set** (M1 infra-lifecycle spec), executed automatically on `main`.

**Runner-up:** option 3. It's the fallback when secrets are stale; same `make` targets.

## Accepted downsides

- Deploys fail outside a lab session until I refresh the secrets; tests and builds still run.
- Long-lived secrets in GitHub would be a risk; these expire in 3 hours, which limits it.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| Secrets leak through logs | Medium | Never echo them; GitHub masks secrets; keys expire each session |
| Network stack changed by a careless push to `main` | Medium | Change set printed in the job log; network changes are rare and additive (ADR-0003) |

## Related

- Implemented in: `.github/workflows/ci-cd.yml`, `.github/workflows/benchmark.yml`, `Makefile`.
- Uses: ADR-0009 (image), ADR-0014 (stacks).
