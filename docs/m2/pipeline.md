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

### 3. Code change on main: the running stack moves to the new version — 01:09–01:13 UTC

[Run 45302907063](https://github.com/iravelmakina/capstone-kv/actions/runs/45302907063),
commit `8b25677` (build version in `/health`; the smoke test now checks it).

| Job | Result | What the log shows |
| --- | --- | --- |
| build | ✅ | `released 8b2567781f08` |
| deploy-app | ✅ | change set: `Modify` Coordinator, Shard1, Shard2 (user data changed) → `UPDATE_COMPLETE` |
| smoke | ✅ | `ok running version (8b2567781f08)`; `create table (409)`: the table from run 2 is still there |

The instances were updated in place (stop/start): CloudFormation's events show only
`UPDATE_IN_PROGRESS`/`UPDATE_COMPLETE` for them, no replacement or delete, and the table
registered in run 2 survived on the coordinator's disk.

### 4. Template change: change set first, no replacement — 01:14–01:19 UTC

[Run 45303571836](https://github.com/iravelmakina/capstone-kv/actions/runs/45303571836),
commit `4c14222` (artifacts bucket: abort incomplete multipart uploads after a day).

| Job | Result | What the log shows |
| --- | --- | --- |
| deploy-infra | ✅ | network: no changes; artifacts: `Modify | ArtifactsBucket | AWS::S3::Bucket | False` → `UPDATE_COMPLETE` |
| build, deploy-app, smoke | ✅ | new release `4c14222e30db` running (every commit is a release) |

Checked afterwards: the bucket has both lifecycle rules (`expire-old-releases`,
`abort-incomplete-uploads`).

### 5. Rollback: a manual run with an earlier version — 01:20–01:24 UTC

[Run 45304257527](https://github.com/iravelmakina/capstone-kv/actions/runs/45304257527),
`workflow_dispatch` on `main` with `version = 8b2567781f08`.

| Job | Result | What the log shows |
| --- | --- | --- |
| build | ⏭️ skipped | the release already exists in S3; nothing is rebuilt |
| deploy-app | ✅ | `ParameterKey=AppVersion,ParameterValue=8b2567781f08` → `Modify` ×3 → `UPDATE_COMPLETE` |
| smoke | ✅ | `ok running version (8b2567781f08)` |

A rollback is one parameter: the stack says which release runs, and the release is still
in S3 (ADR-0015, the lecturer's advice from the 02.11 defenses).

### After the demos

The app stack was deleted with `make down-app`; the network and artifacts stacks stay.

### 6. Benchmark workflow: first attempt failed — 2026-11-24, 11:03–11:08 UTC

Before it, the ci-cd run for `30e1846` had failed twice at the AWS login (lab credentials in
the secrets had expired overnight); after a new lab session and fresh secrets, the rerun
([run 45378620997](https://github.com/iravelmakina/capstone-kv/actions/runs/45378620997),
attempt 3) was green.

[Run 45380561150](https://github.com/iravelmakina/capstone-kv/actions/runs/45380561150)
(`benchmark.yml`): release found, app stack with the client deployed, then the benchmark step
failed 2 minutes into the load; the stack was still deleted (`if: always()`).

**Cause:** `bench.sh` uploaded each result to S3 from the client, but the instance role can
read S3 and not write it (M1 evidence 63); with `set -e` the script stopped at the first
upload. **Fix:** the client prints only the final summaries into the SSM output, and
`make bench-aws` stores them and uploads them with its own credentials. The baseline was
then run from a laptop with the same targets: `docs/m2/benchmark.md`.

## Notes

- **Every commit on `main` is a release**, including a template-only or docs-only one, so
  each push to `main` also restarts the app. Deliberate (the version names the commit), but
  docs-only changes stay on the feature branch until there's code to ship with them.
- **Deploys need fresh secrets**: the lab credentials expire with the 3-hour session.
