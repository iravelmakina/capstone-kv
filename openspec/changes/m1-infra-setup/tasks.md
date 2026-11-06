# Tasks

> Groups 1–3 come before the spec gate (fact-finding only, no code). Nothing from
> group 4 on starts until 3.5 is done.

## 1. Tooling and access

- [x] 1.1 Install AWS CLI v2, `cfn-lint`, `gh`; all three print a version
- [x] 1.2 Create profile `academy` (us-east-1); turn off OpenSpec telemetry
- [x] 1.3 Author stores Academy credentials; `aws sts get-caller-identity --profile academy` shows `voclabs`
- [ ] 1.4 Author runs `gh auth login`; the GitHub repo is created and set as `origin`

## 2. Read-only discovery (before the gate)

- [x] 2.1 Identity: role `voclabs` (`ReadOnlyAccess` + inline `Pvoclabs`); no `LabRole`, instance role `Inventory-App-Role`
- [x] 2.2 us-east-1 and us-west-2 allowed; t3.small in every AZ except `use1-az3`; `use1-az1/2/4` written into ADR-0004
- [x] 2.3 Dry-run probes: VPC, IGW, SG, gateway and interface endpoints, Instance Connect Endpoint, EIP, t3.small/medium allowed; t3.large and up denied
- [x] 2.4 Simulator works on `voclabs`: IAM create role/policy, PassRole (except `Inventory-App-Role`) and ECS denied; reading `voclabs` itself explicitly denied
- [x] 2.5 Evidence in `docs/m1/evidence/` (6 files, redacted), indexed in its `README.md`
- [x] 2.6 Lecturer confirmed "LabRole" is a generic name; in this lab it's `voclabs` and `Inventory-App-Role`
- [x] 2.7 Author reads the lab budget from the lab page

## 3. Spec gate

- [x] 3.1 Region: us-east-1 (ADR-0004)
- [x] 3.2 ADR-0005 confirmed buildable in the Academy lab
- [x] 3.3 AI design review run in a fresh session; author agreed to the accepted and refuted points (`docs/m1/ai-review.md`); ADRs 0003–0006, design and specs revised
- [x] 3.4 Author accepted proposal, specs, design and ADRs 0002–0006 in chat (2026-11-05); statuses set to `Accepted`; `openspec validate --strict` passes
- [ ] 3.5 Commit spec, ADRs and evidence only, push to GitHub; `git show --stat HEAD` shows no templates (committed `3769f20`; push pending)

## 4. Permissions dissection

- [x] 4.1 Real `iam create-role` and `iam create-policy` attempts: both implicitly denied, nothing created (evidence 60–61)
- [x] 4.2 Author reviews the drafted `docs/m1/labrole-dissection.md`

## 5. IAM template

- [x] 5.1 Write `infra/iam-readonly.yaml` per ADR-0006; `cfn-lint` passes
- [x] 5.2 Check both policies with `simulate-custom-policy` against the data-action list in ADR-0006 (all `explicitDeny`) and a few view actions (`allowed`); record results
- [x] 5.3 `validate-template` passes (evidence 71); the refusal is shown by real `create-role`/`create-policy` attempts (60–61), since a change set alone creates nothing and wouldn't prove it; `docs/m1/iam-readonly.md` explains the refusal and what the template would do
- [ ] 5.4 After ADR-0007 is accepted and committed: list every view action by name (no action wildcards); `cfn-lint` passes; the simulator shows the listed actions `allowed`, the dropped ones (`lambda:ListFunctions`, `ec2:DescribeInstanceAttribute`, `rds:DescribeDBInstances`) not `allowed`

## 6. Network stack

- [x] 6.1 Write `infra/network.yaml` (all names prefixed `velmakina-`); `cfn-lint` passes
- [x] 6.2 Write `Makefile` (`diff`, `up` with confirmation, `down`, `drift`, `verify`, `rebuild`, `lint`); `make lint` runs
- [x] 6.3 `make diff`, then `make up`; check every network spec scenario with `describe-*` and save outputs (evidence 80–88)
- [x] 6.4 `make drift` shows IN_SYNC; change a tag in the console, it shows MODIFIED; revert. `make verify` passes

## 7. Recreatability

- [x] 7.1 `make rebuild`; `docs/m1/recreate-log.md` has timings and every first-attempt failure
- [x] 7.2 After fixes, rebuild again with no manual steps

## 8. Quotas and budget

- [x] 8.1 Author reviews the drafted `docs/m1/quotas.md`
- [x] 8.2 Author reviews the drafted `docs/m1/budget.md` (lab budget $60 filled in)
- [ ] 8.3 Close the cost loop: compare spend shown on the lab page with the estimate

## 9. Review and README

- [x] 9.1 AI critique from a fresh session saved in `docs/m1/ai-review.md`, with verdicts on 3 points
- [ ] 9.2 `README.md` drafted (bring up, verify, what changed); check it works from a clean clone after the first deploy
- [x] 9.3 `AGENT_WORKLOG.md` has a row for every agent-made file
- [ ] 9.4 Author reviews the drafted `docs/m1/control-questions.md` and rewrites it in own words

## Workflow follow-up

- After the M1 defense: `openspec archive m1-infra-setup`.
