# ADR-0017: Pin the instances' AMI

- Status: **Accepted** — 2026-11-24 by Iryna Velmakina (author). Drafted with Claude Code from the AI design review verdicts (`docs/m2/ai-review.md`, §1.3) agreed in chat.
- Date: 2026-11-24
- Supersedes: the AMI line of ADR-0014 ("Amazon Linux 2023 AMI from the public SSM parameter").

## Context

`infra/app.yaml` takes `ImageId` as an `AWS::SSM::Parameter::Value<AWS::EC2::Image::Id>`
pointing at `/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64`.
CloudFormation reads that parameter again on **every** stack update. Once AWS publishes a
new Amazon Linux image (several times a month), the next routine deploy, even one that only
changes `AppVersion`, changes `ImageId` on all three instances. `ImageId` can't be changed
in place, so every instance is **replaced**: new root volumes, and every shard's log, the
only copy of its data in M2, is gone.

## Options

| | 1. Keep "latest" | 2. Hard-code an AMI ID in the template | 3. Concrete ID as a parameter: looked up once, then kept |
| --- | --- | --- | --- |
| Routine deploy can replace instances | **yes** | no | no |
| New stack gets a current image | yes | no, the ID ages | yes |
| Upgrading the image | silent | edit the template | deliberate: pass a new ID |
| Works across regions | yes | no | yes |

## Decision

**Option 3.** `ImageId` becomes a plain `AWS::EC2::Image::Id` parameter with no default.

- `make up-app` on a **new** stack reads the current AL2023 ID from the public SSM parameter
  once and passes it.
- On an **update** it passes `UsePreviousValue=true`, so the stack keeps the image it was
  created with.
- An image upgrade is a deliberate action: `make up-app IMAGE_ID=ami-…`. Its change set
  shows the replacement before anything happens.

**Runner-up:** option 2. Simpler, but the ID ages and is region-specific.

## Accepted downsides

- A long-lived stack doesn't get OS patches by itself. In M2 the app stack lives for one
  lab session, so each new session starts on a current image anyway.

## Risks

| Risk | Severity | Mitigation |
| --- | --- | --- |
| An upgrade replaces shards and loses their data | Medium (M2 has no replicas) | Only on an explicit `IMAGE_ID`; the change set shows `Replacement: True` first. M3's replicas make it survivable |

## Related

- Implemented in: `infra/app.yaml`, `Makefile` (`up-app`).
- Source: AI design review §1.3 (`docs/m2/ai-review.md`).
