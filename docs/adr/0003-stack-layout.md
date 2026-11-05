# ADR-0003: Stack layout

- Status: **Accepted** — 2026-11-05 by Iryna Velmakina (author). Drafted with Claude Code; revised after an independent AI design review (`docs/m1/ai-review.md`).
- Date: 2026-11-05

## Context

The network changes rarely. From M2 the app is redeployed often. The IAM template can't
be deployed at all. App deploys must not be able to break the network, and the network
must be able to grow (e.g. a 4th AZ) without tearing down app stacks.

## Options

| | One stack | Separate stacks + exports | Nested stacks | Separate stacks + SSM parameters |
| --- | --- | --- | --- | --- |
| App deploy can break network | yes | no | partly | no |
| Teardown | one command | app first, then network | one command | any order |
| Protects in-use values | — | yes: CFN won't change or delete an export that is imported | — | no lock; EC2 still refuses to delete a subnet or VPC with live ENIs, but stale values go unnoticed |
| Extra parts | none | none | S3 bucket for child templates | parameters |

## Decision

**Separate stacks linked by exports.** `velmakina-kv-network` now, app stacks in M2.
`infra/iam-readonly.yaml` stands alone and is never deployed.

**One export per value, never lists.** A list export (e.g. all private subnets joined)
changes when a subnet is added, and CloudFormation refuses that while any stack imports
it. Per-value exports make growth genuinely additive and give M3 the subnet→AZ mapping it
needs for one replica per AZ.

**Runner-up:** SSM parameters. Reconsider if the export lock keeps blocking network changes.

## Accepted downsides

- Teardown order is fixed: app, then network.
- An existing export can't change while another stack imports it.
- More exports to name and maintain.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| A needed subnet change is blocked by imports | Medium | Add, don't replace (ADR-0004 spare blocks); otherwise delete app stacks first |
| Teardown run in the wrong order | Low | `make down` deletes app stacks before the network |
| Stateful resources (M2+: EBS volumes, S3 buckets) lost or blocking teardown | Medium | M2 sets `DeletionPolicy` (`Snapshot` for volumes, `Retain` or an empty-bucket step for S3) |

## Related

- Implemented in: `infra/network.yaml` (Outputs with `Export`), `Makefile` (`down` order).
- Export names (prefix `<stack>-`): `VpcId`, `VpcCidr`, `PublicSubnetA/B/C`,
  `PrivateSubnetA/B/C`, `AzIdA/B/C`. Security groups are added in M2.
