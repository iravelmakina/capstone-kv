# Recreate log

Proof that the network stack can be destroyed and rebuilt from the repo alone (brief task 8).
Stack `velmakina-kv-network`, AWS Academy lab, us-east-1.

## First deployment — 2026-11-05

| Step | Command | Result | Time |
| --- | --- | --- | --- |
| Attempt 1 | `CONFIRM=yes make up` | **Failed before reaching AWS**: `--change-set-name: expected one argument` | 1 s |
| Attempt 2 | `CONFIRM=yes make up` | `CREATE_COMPLETE`, 21 resources, no replacements | 97 s |

**What broke on the first attempt:** the Makefile relied on `.ONESHELL` (run a whole recipe
in one shell). macOS ships GNU Make **3.81**, which ignores it, so each recipe line ran in
its own shell and the change-set name set on one line was empty on the next. Nothing was
created in AWS. **Fix:** recipes rewritten as single shell commands joined with `; \`,
which works on any GNU Make. A smaller bug fixed at the same time: a stack left in
`REVIEW_IN_PROGRESS` (an unexecuted first change set) is now treated as a CREATE, not an UPDATE.

Checks after deployment (evidence 80–88, `network-checks.txt`): every network spec scenario
holds, `make verify` passes, `make drift` reports all resources `IN_SYNC`.

## After the lab session ended — 2026-11-05, ~22:15 UTC

The lab session timed out. The CLI then failed with `ExpiredToken` until new
credentials were pasted from **AWS Details**. The stack **survived**: still
`CREATE_COMPLETE` with its original creation time (2026-11-05 13:20 UTC), `make verify` OK,
`make drift` all `IN_SYNC`. Ending a session expires the credentials; it does not delete
CloudFormation stacks or VPC resources.

## Teardown and rebuild — 2026-11-05 (UTC)

Command: `make rebuild` (delete the stack, wait, then create it again from the template;
the script prints the seconds for each half). No manual steps, no prompts.

| Run | Started (UTC) | Teardown | Rebuild | Total | Failures |
| --- | --- | --- | --- | --- | --- |
| 1 | 22:19:43 | 63 s | 97 s | 160 s | none |
| 2 | 22:22:29 | 32 s | 97 s | 129 s | none |

Both rebuilds created the same 21 resources ("Add", no replacements) and ended in
`CREATE_COMPLETE`. After run 2: `make verify` OK, `make drift` all `IN_SYNC`, 11 exports,
same subnet CIDRs and AZ IDs as the first deployment (evidence 89–91, `network-checks.txt`).
Only the IDs changed (new `vpc-…`, `subnet-…`), which is why other stacks must read them
through exports (ADR-0003) and never hard-code them.

**How precise the times are:** the AWS CLI waiters check the stack every 30 s, so each half
is rounded up to the next check. The 63 s vs 32 s teardown is one extra check, not a real
difference to explain.

**What broke across all attempts:** the Make 3.81 `.ONESHELL` bug (first deployment) and
expired credentials after the lab session ended. Nothing broke in the rebuilds themselves.
