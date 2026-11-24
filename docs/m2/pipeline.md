# CI/CD pipeline runs (ADR-0015)

Workflow: [`.github/workflows/ci-cd.yml`](../../.github/workflows/ci-cd.yml). Repository:
https://github.com/iravelmakina/capstone-kv. Every run is linked; logs stay on GitHub.

```
test ──▶ build ──────────┐
   └──▶ deploy-infra ────┴──▶ deploy-app ──▶ smoke
```

| Job | Runs on | Does |
| --- | --- | --- |
| test | every push and PR | `go vet`, `gofmt`, `go test -race` (incl. the contract test), `cfn-lint` |
| build | `main` | `make release`: image of the commit to `s3://…/releases/<sha>/` (skipped if it exists) |
| deploy-infra | `main` | `make up`, `make up-artifacts`: change sets; "no changes" is not an error |
| deploy-app | `main` | `make up-app`: app stack with `AppVersion=<sha>` |
| smoke | after deploy-app | `make smoke-aws`: the release's `smoke.sh` on the coordinator through SSM, against localhost |

## Runs

### 1. Push to a feature branch: tests only — 2026-11-24

[Run 45301418178](https://github.com/iravelmakina/capstone-kv/actions/runs/45301418178),
`m2-sharding` at `9905d85`.

| Job | Result |
| --- | --- |
| test | ✅ success |
| build, deploy-infra, deploy-app, smoke | skipped (not `main`), as designed |

### 2. Push to main: first full deploy from CI — 2026-11-24, 01:01–01:05 UTC

[Run 45301802414](https://github.com/iravelmakina/capstone-kv/actions/runs/45301802414),
`main` fast-forwarded to `9905d85`. App stack did not exist before the run.

| Job | Time (UTC) | Result | What the log shows |
| --- | --- | --- | --- |
| test | 01:01:18–01:02:07 | ✅ | all checks pass |
| deploy-infra | 01:02:09–01:02:25 | ✅ | network and artifacts: "didn't contain changes … nothing to execute" |
| build | 01:02:09–01:02:49 | ✅ | `released 9905d8544314 to s3://velmakina-kv-artifacts-<ACCOUNT_ID>/releases/9905d8544314/` |
| deploy-app | 01:02:51–01:04:33 | ✅ | change set: Add 2 security groups, Coordinator, Shard1, Shard2 → `CREATE_COMPLETE` |
| smoke | 01:04:35–01:05:22 | ✅ | on the coordinator via SSM: create table 201, CREATE 200, READ 200 + value, EXISTS 200, DELETE 204, READ/EXISTS after delete 404 → `SSM status: Success` |

Total 4 min 12 s from push to a smoke-tested deployment. Nothing was run by hand.

## Still to show (task 7.3)

- A code change on `main` updates the running stack to the new `AppVersion`.
- A template change prints its change set (Modify, no replacement) before it applies.
- A rollback: a manual run with an earlier version redeploys it without a build.
