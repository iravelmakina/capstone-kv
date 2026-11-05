# Evidence

Raw CLI output from the AWS Academy lab (profile `academy`, role `voclabs`, us-east-1),
collected 2026-11-05: read-only calls, the policy simulator and `--dry-run`, plus two real
IAM create attempts that were refused. Account IDs, session names and resource IDs are
redacted. Each file is split into numbered sections (`===== [NN] … =====`); the docs cite
those numbers. Analysis: [`../labrole-dissection.md`](../labrole-dissection.md),
[`../quotas.md`](../quotas.md), [`../budget.md`](../budget.md).

| File | Sections | What it shows |
| --- | --- | --- |
| [`identity-and-roles.txt`](identity-and-roles.txt) | 01–07, 09–10, 52–53, 60–61 | Caller identity; no `LabRole`; `voclabs` and `Inventory-App-Role` policies and trust; real `iam create-role`/`create-policy` attempts (refused) |
| [`simulator.txt`](simulator.txt) | 08, 20, 51, 62–63, 66 | Policy simulator: M1/M2 actions, services M2–M5 need, instance-side actions, resource-scope check |
| [`dry-runs.txt`](dry-runs.txt) | 30–42, 44, 64–65 | VPC, IGW, security group, endpoints, Instance Connect, Elastic IP, instance sizes, launch template, volume; template validation |
| [`region-and-network.txt`](region-and-network.txt) | 11–15, 54, 67 | Region probe; us-east-1 AZs and t3.small offerings; existing VPCs, stacks, IGWs; the lab's own resources (incl. its NAT Gateway) |
| [`quotas.txt`](quotas.txt) | 19, 46–50 | Service Quotas: VPC, EC2, EBS, CloudFormation, Elastic IPs |
| [`cost.txt`](cost.txt) | 45, 68 | Pricing API blocked by an SCP; Cost Explorer readable; Budgets not creatable |
