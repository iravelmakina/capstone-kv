# Evidence

Raw CLI output from the AWS Academy lab (profile `academy`, role `voclabs`, us-east-1),
collected 2026-11-05/06: read-only calls, the policy simulator and `--dry-run`, two real
IAM create attempts that were refused, and checks of the deployed stack. Account IDs and
session names are redacted; resource IDs (`vpc-…`, `subnet-…`) aren't secrets and stay. Each file is split into numbered sections (`===== [NN] … =====`); the docs cite
those numbers. Analysis: [`../labrole-dissection.md`](../labrole-dissection.md),
[`../quotas.md`](../quotas.md), [`../budget.md`](../budget.md).

| File | Sections | What it shows |
| --- | --- | --- |
| [`identity-and-roles.txt`](identity-and-roles.txt) | 01–07, 09–10, 52–53, 60–61, 95 | Caller identity; no `LabRole`; `voclabs` and `Inventory-App-Role` policies and trust; real `iam create-role`/`create-policy` attempts (refused) |
| [`simulator.txt`](simulator.txt) | 08, 20, 51, 62–63, 66, 70, 72–73 | Policy simulator: M1/M2 actions, services M2–M5 need, instance-side actions, resource-scope check; the two policies in `infra/iam-readonly.yaml` (70), again after denying user data in launch templates and launch configurations (72), and after scoping the view policy to `velmakina-*` resources (73) |
| [`dry-runs.txt`](dry-runs.txt) | 30–42, 44, 64–65, 71 | VPC, IGW, security group, endpoints, Instance Connect, Elastic IP, instance sizes, launch template, volume; template validation incl. both M1 templates (71) |
| [`region-and-network.txt`](region-and-network.txt) | 11–15, 54, 67 | Region probe; us-east-1 AZs and t3.small offerings; existing VPCs, stacks, IGWs; the lab's own resources (incl. its NAT Gateway) |
| [`quotas.txt`](quotas.txt) | 19, 46–50 | Service Quotas: VPC, EC2, EBS, CloudFormation, Elastic IPs |
| [`network-checks.txt`](network-checks.txt) | 80–93 | The deployed stack checked against the network spec: VPC and DNS, subnets and AZs, route tables, S3 endpoint, exports, `make verify`, `make drift`; two timed `make rebuild` runs and checks after them; drift demo (tag changed → `MODIFIED`, reverted → `IN_SYNC`) |
| [`cost.txt`](cost.txt) | 45, 68, 94 | Pricing API blocked by an SCP; Cost Explorer readable; Budgets not creatable; the lab NAT runs between sessions and the lab-page meter lags |
