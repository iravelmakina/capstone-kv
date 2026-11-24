# Spec Delta

## Purpose

One pipeline builds, tests and deploys both infrastructure and app; the benchmark runs
separately.

## ADDED Requirements

### Requirement: Checks on every push
Every push and pull request SHALL run `go vet`, the tests, the contract test and
`cfn-lint`, unless all of its changed files are documentation (ADR-0019).

#### Scenario: Failing test blocks
- **WHEN** a test fails
- **THEN** the workflow fails and no build or deploy job runs

### Requirement: Deploy from main
A push to `main` that changes anything other than documentation SHALL build the image,
upload it under the commit SHA, deploy the network and artifacts stacks, deploy the app
stack with `AppVersion` set to that SHA, and run a smoke test; every stack update SHALL go
through a change set.

#### Scenario: Documentation-only change
- **WHEN** a commit on `main` changes only files under `docs/` or `openspec/`, or Markdown files
- **THEN** no job runs, and the app stack keeps running the `AppVersion` it had

#### Scenario: Infra change deployed
- **WHEN** a commit on `main` changes a template
- **THEN** the pipeline prints the change set and applies it

#### Scenario: App change deployed
- **WHEN** a commit on `main` changes only Go code
- **THEN** the app stack is updated with the new SHA and the smoke test passes

#### Scenario: Failed change set fails the job
- **WHEN** creating a change set fails for any reason other than "no changes"
- **THEN** the deploy job fails

#### Scenario: Smoke test checks the intended version
- **WHEN** the smoke test runs after a deploy
- **THEN** it compares the version each node reports with the version the pipeline meant to deploy, not with the stack's own output

#### Scenario: Rollback
- **WHEN** the deploy job is re-run with an earlier SHA
- **THEN** the app stack runs that version

### Requirement: Credentials stay out of the repo
AWS credentials SHALL come only from repository secrets.

#### Scenario: No keys in the repo
- **WHEN** the repository is searched for access key patterns
- **THEN** there are none

### Requirement: Benchmark is a separate workflow
The benchmark SHALL run in its own manually triggered workflow, not in the test or deploy
jobs.

#### Scenario: Manual benchmark
- **WHEN** the benchmark workflow is started by hand
- **THEN** it runs the workloads and stores the results, without running deploys
