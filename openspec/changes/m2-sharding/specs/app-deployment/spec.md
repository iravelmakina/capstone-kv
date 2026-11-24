# Spec Delta

## Purpose

The coordinator and shards run on EC2 inside the M1 network, deployed from IaC, with the
running version named by the stack.

## ADDED Requirements

### Requirement: Stacks
The repo SHALL define `velmakina-kv-artifacts` (an S3 bucket kept on stack deletion) and
`velmakina-kv-app` (coordinator, 2 shards, security groups), importing subnet and VPC IDs
from the network stack's exports.

#### Scenario: App stack deploys
- **WHEN** the app stack is deployed with an `AppVersion` whose image exists in the bucket
- **THEN** it reaches `CREATE_COMPLETE` and every node answers `GET /health` with 200

#### Scenario: No hard-coded IDs
- **WHEN** `infra/app.yaml` is searched for `vpc-` or `subnet-` literals
- **THEN** there are none; every network ID comes from `Fn::ImportValue`

### Requirement: Placement and access
The coordinator SHALL run in a public subnet with a public IP; each shard SHALL run in a
private subnet in a different AZ with no public IP, and SHALL accept traffic only from the
coordinator's security group.

#### Scenario: Shards unreachable from outside
- **WHEN** the shards' security group rules are read
- **THEN** the only inbound rule allows port 8080 from the coordinator's security group

### Requirement: Image delivery without internet
Each instance SHALL get the app image named by `AppVersion` from the artifacts bucket
through the S3 gateway endpoint and run it as a container that restarts on failure.

#### Scenario: Version named by the stack
- **WHEN** the app stack is updated with a different `AppVersion`
- **THEN** every node runs that image, and the stack's parameters show which version runs

#### Scenario: Routine deploy keeps the instances
- **WHEN** the app stack is updated with only a different `AppVersion`
- **THEN** no instance is replaced: the AMI is the one the stack was created with until an image upgrade is asked for explicitly (ADR-0017)

#### Scenario: Data survives a container restart
- **WHEN** a shard's container is restarted
- **THEN** its data directory on the host is reused and its keys are still there
